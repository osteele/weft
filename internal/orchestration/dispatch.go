package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
)

// Autopilot dispatch policy, shared by every runner (headless `weft autopilot
// run`, the daemon, and the TUI). The runners differ in how they wait — a
// blocking loop, a bubbletea tick — but they must agree on *when* another pass
// is worth running, so that decision lives here and only here.
//
// The policy has two halves:
//
//   - ClassifyPass turns a finished pass into an outcome and a cooldown.
//   - TimerRunsPass decides whether the cooldown expiring is by itself a
//     reason to replan, or whether the runner should keep waiting for an
//     observed state change instead.
//
// Replanning unchanged state costs provider API calls and log noise and
// produces the same answer, so the default for a pass that changed nothing is
// to wait for evidence. The exceptions are blockers no local write announces:
// the rental market moving, and a retry backoff elapsing. blockreason.Recheck
// tells the two apart, because they deserve different cadences.

// PassOutcome is the classification of a finished autopilot pass. The string
// values are also the JSON contract emitted by `weft autopilot run --json`.
type PassOutcome string

const (
	OutcomeProgress PassOutcome = "progress"
	OutcomeIdle     PassOutcome = "idle"
	OutcomeBlocked  PassOutcome = "blocked"
	OutcomeError    PassOutcome = "error"
	OutcomePaused   PassOutcome = "paused"
	OutcomeBusy     PassOutcome = "busy"

	// The blocked refinements carry the typed recheck need ClassifyPass
	// derived from the pass's verdicts, so TimerRunsPass decides the timer
	// without re-parsing reason strings. OutcomeBlocked itself is the
	// fallback when no typed need was computed; its cooldown still varies
	// with the string classification, but the outcome value stays "blocked".
	OutcomeBlockedMarket   PassOutcome = "blocked-market"
	OutcomeBlockedDeadline PassOutcome = "blocked-deadline"
	OutcomeBlockedNone     PassOutcome = "blocked-none"
)

// AutopilotCooldownBackoff is the recheck interval for a pass blocked only on
// retry backoffs. Such a pass is waiting out a local deadline, not watching the
// market, and a recheck that lands before the deadline costs a provider offer
// fetch (the offer snapshot cache is far shorter than this) to re-derive the
// same answer.
//
// It is calibrated against the schedule in internal/retrypolicy, currently
// 15s/30s/60s/2m: long enough that a job cycling through the schedule is
// rechecked a couple of times rather than a dozen, and short enough that it
// never exceeds the longest backoff it is waiting on — a recheck interval above
// that would make every backoff expire into a wait. TestBackoffCooldownFits
// pins that relationship, so lengthening the retry schedule prompts a revisit.
const AutopilotCooldownBackoff = 60 * time.Second

// AutopilotQuietBackstop bounds how long a runner will go without a pass while
// its change detection reports nothing actionable. Change detection is not
// provably complete — host inventory lives in YAML outside the database, and a
// missed filesystem event is silent — so a long backstop converts any such gap
// from "stuck forever" into "late by at most this much". It is a safety net,
// not a polling interval; making it short would give back the savings it
// exists to protect.
const AutopilotQuietBackstop = 10 * time.Minute

// ClassifyPass turns a finished pass into its outcome and the cooldown to
// apply before the next one.
//
// When the result carries a settled typed recheck need (result.Recheck), it
// drives both the cooldown and the refined blocked outcome. Otherwise the
// persisted reason strings are classified, preserving the pre-typed behavior.
func ClassifyPass(result *GroupedAutoPilotResult, err error, pausedWait time.Duration) (PassOutcome, time.Duration) {
	switch {
	case errors.Is(err, ErrAutopilotPaused):
		return OutcomePaused, pausedWait
	case errors.Is(err, ErrAutopilotBusy):
		return OutcomeBusy, AutopilotCooldownContend
	case err != nil:
		return OutcomeError, AutopilotCooldownError
	}
	if result == nil {
		return OutcomeIdle, AutopilotCooldownIdle
	}
	if result.Placed > 0 || result.Launched > 0 || result.Rebalanced > 0 || result.OverloadMoved > 0 {
		return OutcomeProgress, AutopilotCooldownProgress
	}
	if AutoPilotBlockedReasonCount(result.BlockedReasons) > 0 {
		if need, computed := result.recheckNeed(); computed {
			switch need {
			case blockreason.RecheckDeadline:
				return OutcomeBlockedDeadline, AutopilotCooldownBackoff
			case blockreason.RecheckMarket:
				return OutcomeBlockedMarket, AutopilotCooldownBlocked
			default:
				return OutcomeBlockedNone, AutopilotCooldownBlocked
			}
		}
		// A pass blocked only on retry backoffs is waiting out a local
		// deadline; rechecking it at the market's cadence just burns offer
		// fetches on an answer that cannot have changed yet.
		if blockreason.RecheckFor(result.BlockedReasons) == blockreason.RecheckDeadline {
			return OutcomeBlocked, AutopilotCooldownBackoff
		}
		return OutcomeBlocked, AutopilotCooldownBlocked
	}
	return OutcomeIdle, AutopilotCooldownIdle
}

// TimerRunsPass reports whether the cooldown expiring should itself run
// another pass, given the last pass's outcome and the reasons it blocked on.
//
// Idle passes never re-run on the timer: nothing was actionable, and anything
// that makes work actionable is a database write the runner already wakes on.
// Blocked passes re-run only when at least one blocker needs something no local
// write will announce — a fresh provider query, or a retry backoff elapsing. A
// pass blocked purely on the run-rate budget or on reuse rejections waits for
// the database change that clears it. ClassifyPass sets how long that wait is.
//
// The refined blocked outcomes (OutcomeBlockedMarket, OutcomeBlockedDeadline,
// OutcomeBlockedNone) already carry the typed recheck need; OutcomeBlocked
// falls back to classifying the reason strings.
func TimerRunsPass(outcome PassOutcome, blockedReasons map[int64]string) bool {
	switch outcome {
	case OutcomeBlockedMarket, OutcomeBlockedDeadline:
		return true
	case OutcomeBlockedNone:
		return false
	case OutcomeBlocked:
		return blockreason.RecheckFor(blockedReasons) != blockreason.RecheckNone
	case OutcomeIdle:
		return false
	default:
		// Progress, error, busy, and paused all describe a situation the
		// runner expects to revisit shortly on its own schedule.
		return true
	}
}

// WakeSnapshot is the coarse state an autopilot runner compares across change
// notifications to decide whether anything it cares about actually moved. The
// change source reports that a database file was written, which happens
// constantly for reasons the autopilot has no stake in; this is the filter.
//
// Every field must be something a pass's decisions depend on. A field that no
// decision reads adds spurious wakes; a decision input that is missing here
// makes the runner sleep through the event that would have changed its answer.
type WakeSnapshot struct {
	// LifecycleID is the newest lifecycle event. It covers the broad class of
	// "something happened to a job or instance" without enumerating it.
	LifecycleID int64
	// UnplacedJobs are the jobs awaiting placement — the autopilot's input.
	UnplacedJobs int
	// ActiveJobs are queued or running jobs. A job finishing frees on-prem
	// and rental capacity that a blocked job may be waiting for.
	ActiveJobs int
	// LiveLaunches are instances in a non-terminal state.
	LiveLaunches int
	// OpenPlacementIntents and OpenMoveIntents mark jobs another path owns;
	// the autopilot skips them, so their opening and closing changes its
	// working set.
	OpenPlacementIntents int
	OpenMoveIntents      int
	// RunRateCentsPerHour is the committed spend the run-rate gate compares
	// against the configured target. It is the direct input to the most
	// common non-market blocker, so a launch ending wakes the runner even
	// when no other count moved.
	RunRateCentsPerHour int
}

// ReadWakeSnapshot reads the current wake state. A zero snapshot with a nil
// error is returned for a nil database.
func ReadWakeSnapshot(database *sql.DB) (WakeSnapshot, error) {
	var snap WakeSnapshot
	if database == nil {
		return snap, nil
	}
	if err := database.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM lifecycle_events`).Scan(&snap.LifecycleID); err != nil {
		return snap, err
	}
	if err := database.QueryRow(`
		SELECT COUNT(*)
		  FROM job_status
		 WHERE tombstoned = 0
		   AND effective_target_kind = ?
		   AND COALESCE(pending_status, status) IN (?, ?)`,
		string(db.JobTargetUnplaced), db.StatusQueued, db.StatusPendingPlacement,
	).Scan(&snap.UnplacedJobs); err != nil {
		return snap, err
	}
	if err := database.QueryRow(`
		SELECT COUNT(*)
		  FROM job_status
		 WHERE tombstoned = 0
		   AND COALESCE(pending_status, status) IN (?, ?)`,
		db.StatusQueued, db.StatusRunning,
	).Scan(&snap.ActiveJobs); err != nil {
		return snap, err
	}
	if err := database.QueryRow(`
		SELECT COUNT(*)
		  FROM launches
		 WHERE status IN (?, ?, ?, ?)`,
		db.LaunchStatusLaunching, db.LaunchStatusRunning, db.LaunchStatusPaused, db.LaunchStatusGrace,
	).Scan(&snap.LiveLaunches); err != nil {
		return snap, err
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM placement_intents WHERE state = 'open'`).Scan(&snap.OpenPlacementIntents); err != nil {
		return snap, err
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM move_intents WHERE state = 'open'`).Scan(&snap.OpenMoveIntents); err != nil {
		return snap, err
	}
	rate, err := db.SumActiveLaunchCostPerHourCents(database)
	if err != nil {
		return snap, err
	}
	snap.RunRateCentsPerHour = rate
	return snap, nil
}

// Quiet reports whether there is no work in flight and none waiting. A quiet
// system still needs periodic freshness checks, but far less often than one
// with live instances or queued jobs.
func (s WakeSnapshot) Quiet() bool {
	return s.UnplacedJobs == 0 &&
		s.ActiveJobs == 0 &&
		s.LiveLaunches == 0 &&
		s.OpenPlacementIntents == 0 &&
		s.OpenMoveIntents == 0
}

// WakeReason explains why a wait ended.
type WakeReason string

const (
	// WakeAction means observed state changed; the caller should run a pass.
	WakeAction WakeReason = "action"
	// WakeTimer means the caller's wait elapsed. Whether that warrants a pass
	// depends on the caller: a runner that only runs passes asks for this
	// wait only when TimerRunsPass said so, while the daemon also uses timer
	// wakes to refresh sync state without replanning.
	WakeTimer WakeReason = "timer"
	// WakeBackstop means AutopilotQuietBackstop elapsed with no observed
	// change. The caller should run a pass regardless of its timer policy.
	WakeBackstop WakeReason = "backstop"
	// WakeDone means the context was canceled.
	WakeDone WakeReason = "done"
)

// ChangeWaiter observes coarse state-change notifications. *dbwatch.Source
// satisfies it; tests supply a scripted implementation.
type ChangeWaiter interface {
	// Wait blocks until state changed, maxWait elapsed, or ctx was canceled.
	Wait(ctx context.Context, maxWait time.Duration) (bool, error)
}

// InvalidationOptions configures WaitForInvalidation.
type InvalidationOptions struct {
	// Waiter observes change notifications. A nil Waiter degrades to a plain
	// sleep, so a runner with no filesystem watch still makes progress.
	Waiter ChangeWaiter

	// ReadSnapshot reads current wake state. Required when Waiter is set.
	ReadSnapshot func() (WakeSnapshot, error)

	// Baseline is the snapshot the wait compares against.
	Baseline WakeSnapshot

	// Wait is the timer-driven pass cooldown. It is armed only when
	// TimerRunsPass is true.
	Wait time.Duration

	// TimerRunsPass mirrors the TimerRunsPass policy for the previous pass.
	TimerRunsPass bool

	// QuietWait, when positive, returns WakeTimer after this long even if
	// TimerRunsPass is false. The daemon uses it to keep its sync cadence
	// while leaving the replan decision to TimerRunsPass; a runner that only
	// runs passes leaves it zero.
	QuietWait time.Duration

	// Backstop bounds the total quiet wait. Zero disables it, which should be
	// reserved for tests — a runner with no backstop depends on change
	// detection being complete.
	Backstop time.Duration

	// Debounce is how long to let further changes accumulate after the first
	// observed one, so a burst of writes produces a single pass.
	Debounce time.Duration

	// OnQuietTimeout, when set, runs on a timer expiry that would otherwise
	// not end the wait. It returns true when it changed observable state
	// (e.g. a cloud sync that updated rows), which ends the wait with
	// WakeAction. The headless runner uses it to refresh cloud state without
	// committing to a replan.
	OnQuietTimeout func(context.Context) bool

	// OnError reports a snapshot read failure. The wait treats a failed read
	// as an observed change, since it cannot show that nothing moved.
	OnError func(error)

	// Now defaults to time.Now. Tests override it together with a Waiter that
	// advances the same clock.
	Now func() time.Time
}

func (o InvalidationOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o InvalidationOptions) reportError(err error) {
	if o.OnError != nil {
		o.OnError(err)
	}
}

// WaitForInvalidation blocks until something warrants another pass, returning
// the snapshot the caller should treat as its new baseline.
//
// A change that leaves the snapshot equal to the baseline is discarded: the
// database was written, but not in a way that changes what a pass would
// decide. This is what keeps an unrelated write burst from replanning.
func WaitForInvalidation(ctx context.Context, opts InvalidationOptions) (WakeSnapshot, WakeReason) {
	if opts.Waiter == nil || opts.ReadSnapshot == nil {
		if sleepUntilDone(ctx, opts.Wait) {
			return opts.Baseline, WakeDone
		}
		return opts.Baseline, WakeTimer
	}

	start := opts.now()
	var timerDeadline, quietDeadline, backstopDeadline, debounceDeadline time.Time
	if opts.TimerRunsPass && opts.Wait > 0 {
		timerDeadline = start.Add(opts.Wait)
	}
	if opts.QuietWait > 0 {
		quietDeadline = start.Add(opts.QuietWait)
	}
	if opts.Backstop > 0 {
		backstopDeadline = start.Add(opts.Backstop)
	}

	var pending *WakeSnapshot
	for {
		now := opts.now()
		if reached(timerDeadline, now) || reached(quietDeadline, now) {
			return opts.Baseline, WakeTimer
		}
		if reached(backstopDeadline, now) {
			return opts.Baseline, WakeBackstop
		}
		if pending != nil && reached(debounceDeadline, now) {
			return *pending, WakeAction
		}

		nextWait := opts.Wait
		for _, deadline := range []time.Time{timerDeadline, quietDeadline, backstopDeadline} {
			nextWait = earlier(nextWait, deadline, now)
		}
		if pending != nil {
			nextWait = earlier(nextWait, debounceDeadline, now)
		}

		changed, err := opts.Waiter.Wait(ctx, nextWait)
		if ctx.Err() != nil {
			return opts.Baseline, WakeDone
		}
		if err != nil {
			// The watch failed, so absence of a notification proves nothing.
			// Fall back to running a pass rather than sleeping on a source
			// that may never report again.
			opts.reportError(err)
			return opts.Baseline, WakeAction
		}

		if changed {
			snap, err := opts.ReadSnapshot()
			if err != nil {
				opts.reportError(err)
				return opts.Baseline, WakeAction
			}
			if snap != opts.Baseline {
				pendingSnap := snap
				pending = &pendingSnap
				debounceDeadline = opts.now().Add(opts.Debounce)
			} else {
				// A write that changed nothing the autopilot reads. Drop any
				// pending wake with it: the state it described is gone.
				pending = nil
			}
			continue
		}

		// The waiter timed out. Re-check the deadlines at the top of the loop,
		// but first give the caller a chance to refresh external state. The
		// deadline check has to use the post-wait clock: waiting is what moved
		// it, so the reading from the top of the loop is stale by exactly the
		// interval that matters.
		if pending == nil && opts.OnQuietTimeout != nil && !anyReached(opts.now(), timerDeadline, quietDeadline, backstopDeadline) {
			if opts.OnQuietTimeout(ctx) {
				snap, err := opts.ReadSnapshot()
				if err != nil {
					opts.reportError(err)
					return opts.Baseline, WakeAction
				}
				return snap, WakeAction
			}
			if ctx.Err() != nil {
				return opts.Baseline, WakeDone
			}
		}
	}
}

func reached(deadline, now time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}

func anyReached(now time.Time, deadlines ...time.Time) bool {
	for _, deadline := range deadlines {
		if reached(deadline, now) {
			return true
		}
	}
	return false
}

// earlier narrows a wait to a deadline that falls sooner than it.
func earlier(wait time.Duration, deadline, now time.Time) time.Duration {
	if deadline.IsZero() {
		return wait
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if wait <= 0 || remaining < wait {
		return remaining
	}
	return wait
}

func sleepUntilDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		<-ctx.Done()
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
