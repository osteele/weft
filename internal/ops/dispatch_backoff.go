package ops

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Dispatch backoff for queued jobs whose remote dispatch keeps failing
// identically. Without this, ensureQueuedJobsOnRemote re-attempts every
// queued job on every daemon pass regardless of history: a job stuck behind
// an unfixed blocker (e.g. an R2-pull inventory host that cannot stage
// producer-job --needs artifacts) gets re-dispatched, and re-fails, once per
// pass indefinitely.
//
// The schedule is age-based, not count-based: it is immune to event dedupe
// and to any future event pruning, and states the intent directly — the
// longer a blocker has stood unchanged, the less often it is poked.
//
//	age < dispatchBackoffAgeThreshold  -> no backoff; attempt every pass
//	age >= dispatchBackoffAgeThreshold -> delay = clamp(age/4, minDelay, DispatchBackoffCap)
//
// A job becomes eligible again at lastAttempt + delay, where lastAttempt and
// age both come from db.LatestDispatchAttemptRun (OccurredAt and
// FirstOccurredAt respectively).
const dispatchBackoffAgeThreshold = 5 * time.Minute

// dispatchBackoffMinDelay is the shortest backoff once a run has aged past
// dispatchBackoffAgeThreshold. age/4 already exceeds this at the threshold
// (75s at exactly 5m), so the floor is a safety net rather than a binding
// constraint of the current formula.
const dispatchBackoffMinDelay = time.Minute

// DispatchBackoffCap bounds the dispatch backoff delay. It must equal
// orchestration.AutopilotQuietBackstop: that constant is the system's
// existing promise to take a fresh look at everything within this interval,
// and letting one job's backoff exceed it would silently break that
// promise.
//
// internal/ops cannot import internal/orchestration to reference
// AutopilotQuietBackstop directly — internal/orchestration already imports
// internal/ops, so the reverse import would cycle. The two constants are
// therefore defined independently; TestDispatchBackoffCapMatchesQuietBackstop
// in internal/orchestration/dispatch_test.go (which can see both packages)
// asserts they cannot drift apart.
const DispatchBackoffCap = 10 * time.Minute

// dispatchBackoffBaseDelay implements the un-jittered schedule.
func dispatchBackoffBaseDelay(age time.Duration) time.Duration {
	if age < dispatchBackoffAgeThreshold {
		return 0
	}
	delay := age / 4
	if delay < dispatchBackoffMinDelay {
		delay = dispatchBackoffMinDelay
	}
	if delay > DispatchBackoffCap {
		delay = DispatchBackoffCap
	}
	return delay
}

// dispatchBackoffJitterFactor derives a stable per-job multiplier in
// [-0.25, 0.25] from the job ID alone — never from time.Now or math/rand.
// A jitter recomputed per pass would make the same job's eligibility flap
// inside one backoff window, reproducing the lockstep-retry problem jitter
// exists to avoid (docs/ROADMAP.md). Deriving it from the job ID instead
// keeps it identical across every call for a given job and age.
func dispatchBackoffJitterFactor(jobID int64) float64 {
	h := uint64(jobID) * 2654435761 //nolint:gomnd // Knuth multiplicative hash constant
	h ^= h >> 33
	frac := float64(h%1_000_001) / 1_000_000.0 // deterministic value in [0, 1]
	return (frac*2 - 1) * 0.25
}

// dispatchBackoffDelay is the jittered schedule: dispatchBackoffBaseDelay
// adjusted by +/-25% deterministically from jobID, then clamped back to
// DispatchBackoffCap so jitter can never push a job's backoff past the
// system's quiet-sync promise.
func dispatchBackoffDelay(jobID int64, age time.Duration) time.Duration {
	base := dispatchBackoffBaseDelay(age)
	if base <= 0 {
		return 0
	}
	jittered := time.Duration(float64(base) * (1 + dispatchBackoffJitterFactor(jobID)))
	if jittered > DispatchBackoffCap {
		jittered = DispatchBackoffCap
	}
	if jittered < time.Second {
		jittered = time.Second
	}
	return jittered
}

// dispatchBackoffRemaining reports how much longer job must wait before its
// next dispatch attempt, and whether it is currently inside that window. It
// reads the same consecutive-failure run explain.LatestInventoryDispatchBlock
// uses for the diagnose surface (db.LatestDispatchAttemptRun,
// db.DispatchRunFloor), restricted to real attempts so the gate's own
// deferred bookkeeping event cannot perturb it — see
// db.LatestDispatchAttemptRun's doc comment.
func dispatchBackoffRemaining(database *sql.DB, job *db.Job, now time.Time) (time.Duration, bool) {
	if job == nil {
		return 0, false
	}
	floor := db.DispatchRunFloor(job)
	run, ok := db.LatestDispatchAttemptRun(database, job.ID, floor, now)
	if !ok {
		return 0, false
	}
	age := now.Sub(run.FirstOccurredAt)
	delay := dispatchBackoffDelay(job.ID, age)
	if delay <= 0 {
		return 0, false
	}
	eligibleAt := run.OccurredAt.Add(delay)
	if !now.Before(eligibleAt) {
		return 0, false
	}
	return eligibleAt.Sub(now), true
}

// CountQueuedDispatchBackoff returns how many inventory-host queued jobs are
// currently inside their dispatch backoff window, across all hosts. Used by
// orchestration.ReadWakeSnapshot so a job that cannot be attempted this pass
// is not counted as work in flight for Quiet().
func CountQueuedDispatchBackoff(database *sql.DB, now time.Time) (int, error) {
	if database == nil {
		return 0, nil
	}
	jobs, err := db.ListAllQueued(database)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() {
			continue
		}
		if _, inBackoff := dispatchBackoffRemaining(database, job, now); inBackoff {
			n++
		}
	}
	return n, nil
}
