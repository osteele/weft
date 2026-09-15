package ops

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// TestDispatchBackoffBaseDelaySchedule pins the schedule from the brief:
// no backoff under the age threshold, a monotone ramp above it, and a hard
// cap.
func TestDispatchBackoffBaseDelaySchedule(t *testing.T) {
	if got := dispatchBackoffBaseDelay(4 * time.Minute); got != 0 {
		t.Fatalf("age=4m delay = %v, want 0 (below threshold)", got)
	}
	if got := dispatchBackoffBaseDelay(dispatchBackoffAgeThreshold - time.Second); got != 0 {
		t.Fatalf("age just under threshold delay = %v, want 0", got)
	}

	var prev time.Duration
	ages := []time.Duration{
		dispatchBackoffAgeThreshold,
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		2 * time.Hour,
	}
	for i, age := range ages {
		got := dispatchBackoffBaseDelay(age)
		if got <= 0 {
			t.Fatalf("age=%v delay = %v, want > 0 (at/above threshold)", age, got)
		}
		if i > 0 && got < prev {
			t.Fatalf("age=%v delay = %v, want >= previous delay %v (ramp must be monotone)", age, got, prev)
		}
		prev = got
	}

	if got := dispatchBackoffBaseDelay(40 * time.Minute); got != DispatchBackoffCap {
		t.Fatalf("age=40m delay = %v, want the cap %v (brief: cap reached at ~40m)", got, DispatchBackoffCap)
	}
	if got := dispatchBackoffBaseDelay(24 * time.Hour); got > DispatchBackoffCap {
		t.Fatalf("age=24h delay = %v, must never exceed the cap %v", got, DispatchBackoffCap)
	}
}

// TestDispatchBackoffJitterStable pins jitter determinism: the same job ID
// and age must produce the same delay across repeated calls, and across
// process-independent calls (no seeding, no clock reads). A jitter that
// varied per call would make a job's eligibility flap inside one backoff
// window, which is the bug jitter exists to prevent.
func TestDispatchBackoffJitterStable(t *testing.T) {
	const jobID = int64(48213)
	age := 20 * time.Minute
	first := dispatchBackoffDelay(jobID, age)
	for i := range 5 {
		if got := dispatchBackoffDelay(jobID, age); got != first {
			t.Fatalf("call %d: dispatchBackoffDelay(%d, %v) = %v, want stable %v", i, jobID, age, got, first)
		}
	}

	// Different jobs at the same age are allowed (in fact expected) to
	// diverge — that divergence is jitter doing its job of avoiding
	// lockstep retries. But each job's own value must still be stable, and
	// jitter must stay within +/-25% of the un-jittered schedule.
	base := dispatchBackoffBaseDelay(age)
	lowerBound := time.Duration(float64(base) * 0.75)
	upperBound := time.Duration(float64(base) * 1.25)
	for _, id := range []int64{1, 2, 3, 100, 100000, 48213} {
		d := dispatchBackoffDelay(id, age)
		if d < lowerBound || d > upperBound {
			t.Fatalf("job %d: delay = %v, want within +/-25%% of base %v (bounds [%v, %v])", id, d, base, lowerBound, upperBound)
		}
	}
}

// TestDispatchBackoffDelayNeverExceedsCap verifies jitter cannot push a
// delay past DispatchBackoffCap, preserving the invariant that no queued
// job's backoff outlives the system's existing quiet-sync promise
// (AutopilotQuietBackstop).
func TestDispatchBackoffDelayNeverExceedsCap(t *testing.T) {
	for _, id := range []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 999999} {
		if got := dispatchBackoffDelay(id, 24*time.Hour); got > DispatchBackoffCap {
			t.Fatalf("job %d: jittered delay = %v, must never exceed cap %v", id, got, DispatchBackoffCap)
		}
	}
}

// TestDispatchBackoffRemaining_SkipsWithinWindowAttemptsAfter exercises the
// per-job gate function directly against a synthetic failure history: inside
// the window it reports (remaining>0, true); once the window elapses it
// reports (0, false), meaning "attempt now".
func TestDispatchBackoffRemaining_SkipsWithinWindowAttemptsAfter(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	// Simulate the realistic history the age<5m phase produces: repeated
	// real attempts (recordFailure no longer dedupes) with the same
	// detail, the run's first at t0 and its most recent shortly before
	// "now". FirstOccurredAt anchors age; OccurredAt anchors the next
	// eligible time.
	base := time.Unix(1_000_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, base.Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	runStart := base
	lastAttempt := base.Add(5*time.Minute + 50*time.Second)
	now := base.Add(6 * time.Minute)
	for _, at := range []time.Time{runStart, lastAttempt} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchFailed,
			JobID:      jobID,
			Detail:     "artifact needs staging failed: boom",
		}); err != nil {
			t.Fatalf("insert failure at %v: %v", at, err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	// age = now - runStart = 6m -> delay = dispatchBackoffDelay(jobID, 6m).
	// eligibleAt = lastAttempt + delay, which is after "now" since
	// lastAttempt is only 10s before it.
	delay := dispatchBackoffDelay(jobID, now.Sub(runStart))
	remaining, inBackoff := dispatchBackoffRemaining(database, job, now)
	if !inBackoff {
		t.Fatalf("dispatchBackoffRemaining at %v = (_, false), want true (still inside window)", now)
	}
	if remaining <= 0 || remaining > delay {
		t.Fatalf("remaining = %v, want in (0, %v]", remaining, delay)
	}

	// With no further recorded attempts (as the gate itself guarantees — a
	// skipped pass emits Deferred, not Failed), OccurredAt stays fixed at
	// lastAttempt while age keeps growing. Once the schedule caps out at
	// DispatchBackoffCap, eligibleAt <= lastAttempt + cap*1.25 (worst-case
	// jitter); any "now" well beyond that must find the window elapsed.
	farFuture := lastAttempt.Add(2 * DispatchBackoffCap)
	if _, inBackoff := dispatchBackoffRemaining(database, job, farFuture); inBackoff {
		t.Fatalf("dispatchBackoffRemaining at %v = true, want false (window elapsed)", farFuture)
	}
}
