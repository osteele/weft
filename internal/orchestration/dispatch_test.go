package orchestration

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/retrypolicy"
)

func TestClassifyPass(t *testing.T) {
	pausedWait := 42 * time.Second

	tests := []struct {
		name        string
		result      *GroupedAutoPilotResult
		err         error
		wantOutcome PassOutcome
		wantWait    time.Duration
	}{
		{
			name:        "paused",
			err:         ErrAutopilotPaused,
			wantOutcome: OutcomePaused,
			wantWait:    pausedWait,
		},
		{
			name:        "busy",
			err:         ErrAutopilotBusy,
			wantOutcome: OutcomeBusy,
			wantWait:    AutopilotCooldownContend,
		},
		{
			name:        "error",
			err:         errors.New("boom"),
			wantOutcome: OutcomeError,
			wantWait:    AutopilotCooldownError,
		},
		{
			name:        "nil result is idle",
			result:      nil,
			wantOutcome: OutcomeIdle,
			wantWait:    AutopilotCooldownIdle,
		},
		{
			name:        "placed counts as progress",
			result:      &GroupedAutoPilotResult{Placed: 1},
			wantOutcome: OutcomeProgress,
			wantWait:    AutopilotCooldownProgress,
		},
		{
			name:        "launched counts as progress",
			result:      &GroupedAutoPilotResult{Launched: 1},
			wantOutcome: OutcomeProgress,
			wantWait:    AutopilotCooldownProgress,
		},
		{
			name:        "rebalanced counts as progress",
			result:      &GroupedAutoPilotResult{Rebalanced: 1},
			wantOutcome: OutcomeProgress,
			wantWait:    AutopilotCooldownProgress,
		},
		{
			name:        "overload move counts as progress",
			result:      &GroupedAutoPilotResult{OverloadMoved: 1},
			wantOutcome: OutcomeProgress,
			wantWait:    AutopilotCooldownProgress,
		},
		{
			name:        "blocked-only with no progress",
			result:      &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: "no offers from providers for gpu=A40"}},
			wantOutcome: OutcomeBlocked,
			wantWait:    AutopilotCooldownBlocked,
		},
		{
			name: "progress wins over blocked",
			result: &GroupedAutoPilotResult{
				Launched:       1,
				BlockedReasons: map[int64]string{1: "no offers from providers for gpu=A40"},
			},
			wantOutcome: OutcomeProgress,
			wantWait:    AutopilotCooldownProgress,
		},
		{
			name:        "empty result is idle",
			result:      &GroupedAutoPilotResult{},
			wantOutcome: OutcomeIdle,
			wantWait:    AutopilotCooldownIdle,
		},
		{
			name:        "a pass blocked only on a retry backoff gets the slower recheck",
			result:      &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: "reuse backoff 45s remaining (after 2 failed submit(s))"}},
			wantOutcome: OutcomeBlocked,
			wantWait:    AutopilotCooldownBackoff,
		},
		{
			name: "a market blocker alongside a backoff keeps the short cooldown",
			result: &GroupedAutoPilotResult{BlockedReasons: map[int64]string{
				1: "reuse backoff 45s remaining (after 2 failed submit(s))",
				2: "no offers from providers for gpu=A40",
			}},
			wantOutcome: OutcomeBlocked,
			wantWait:    AutopilotCooldownBlocked,
		},
		{
			name:        "waiting-kind reasons are not a blocked pass",
			result:      &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: "inventory-tagged: waiting for on-prem host"}},
			wantOutcome: OutcomeIdle,
			wantWait:    AutopilotCooldownIdle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOutcome, gotWait := ClassifyPass(tc.result, tc.err, pausedWait)
			if gotOutcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", gotOutcome, tc.wantOutcome)
			}
			if gotWait != tc.wantWait {
				t.Errorf("wait = %s, want %s", gotWait, tc.wantWait)
			}
		})
	}
}

// The backoff recheck interval must not exceed the longest retry backoff it is
// waiting on. If it did, a job would become eligible and then sit idle for the
// remainder of the interval on every single backoff — the recheck would add
// more latency than the backoff itself.
func TestBackoffCooldownFits(t *testing.T) {
	longest := retrypolicy.BackoffDelayClamped(retrypolicy.MaxPlacementAttempts())
	if AutopilotCooldownBackoff > longest {
		t.Fatalf("AutopilotCooldownBackoff = %s, longer than the longest retry backoff %s; "+
			"every backoff would expire into a wait", AutopilotCooldownBackoff, longest)
	}
	if AutopilotCooldownBackoff <= AutopilotCooldownBlocked {
		t.Fatalf("AutopilotCooldownBackoff = %s, not slower than the market cooldown %s; "+
			"it exists to recheck local deadlines less often than the market",
			AutopilotCooldownBackoff, AutopilotCooldownBlocked)
	}
}

func TestTimerRunsPass(t *testing.T) {
	const marketReason = "no offers from providers for gpu=A40 vram>=20GB"
	const budgetReason = "run-rate target exceeded: target $2.00/hr, current $1.80/hr + requested $0.50/hr = $2.30/hr (headroom $0.20/hr, job needs $0.50/hr)"
	const reuseReason = "could not reuse running instances: wi12 has 0 free GPU slots"
	const backoffReason = "reuse backoff 45s remaining (after 2 failed submit(s))"

	tests := []struct {
		name    string
		outcome PassOutcome
		blocked map[int64]string
		want    bool
	}{
		{name: "progress revisits soon", outcome: OutcomeProgress, want: true},
		{name: "error retries", outcome: OutcomeError, want: true},
		{name: "busy retries", outcome: OutcomeBusy, want: true},
		{name: "paused rechecks", outcome: OutcomePaused, want: true},
		{name: "idle waits for a change", outcome: OutcomeIdle, want: false},
		{
			name:    "market blocker needs a fresh provider query",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: marketReason},
			want:    true,
		},
		{
			name:    "run-rate blocker clears via the database",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: budgetReason},
			want:    false,
		},
		{
			name:    "reuse rejections clear via the database",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: reuseReason},
			want:    false,
		},
		{
			name:    "one market blocker among budget blockers still needs the timer",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: budgetReason, 2: marketReason},
			want:    true,
		},
		{
			name:    "budget blocker with reuse detail attached stays database-observable",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: budgetReason + "; " + reuseReason},
			want:    false,
		},
		{
			name:    "unrecognized blocker keeps the timer",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: "something nobody classified yet"},
			want:    true,
		},
		{
			// A retry backoff expires on a clock with no accompanying write,
			// so the wake path cannot see it clear.
			name:    "backoff countdown keeps the timer",
			outcome: OutcomeBlocked,
			blocked: map[int64]string{1: backoffReason},
			want:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TimerRunsPass(tc.outcome, tc.blocked); got != tc.want {
				t.Errorf("TimerRunsPass(%q, %v) = %v, want %v", tc.outcome, tc.blocked, got, tc.want)
			}
		})
	}
}

// A blocked pass whose reasons are all database-observable must still be
// reachable through the wake path, or suppressing its timer would strand the
// job. The run-rate gate's input is the committed spend, so the snapshot has
// to carry it: a launch ending changes no count, only the rate.
func TestWakeSnapshotCarriesRunRateSoLaunchEndingIsObservable(t *testing.T) {
	database := db.SetupTestDB(t)

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 150,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	running, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read running snapshot: %v", err)
	}
	if running.RunRateCentsPerHour != 150 {
		t.Fatalf("RunRateCentsPerHour = %d, want 150", running.RunRateCentsPerHour)
	}

	if err := db.UpdateLaunchStatus(database, launchID, db.LaunchStatusCompleted); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}
	after, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read completed snapshot: %v", err)
	}
	if after == running {
		t.Fatal("snapshot unchanged after the launch ended; a run-rate-blocked job would never be revisited")
	}
	if after.RunRateCentsPerHour != 0 {
		t.Fatalf("RunRateCentsPerHour = %d, want 0 after the launch ended", after.RunRateCentsPerHour)
	}
}

func TestReadWakeSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)

	initial, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}
	if !initial.Quiet() {
		t.Fatalf("fresh database should be quiet, got %+v", initial)
	}

	if _, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "unplaced", "A100"); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	afterJob, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read job snapshot: %v", err)
	}
	if afterJob.UnplacedJobs != initial.UnplacedJobs+1 {
		t.Fatalf("UnplacedJobs = %d, want %d", afterJob.UnplacedJobs, initial.UnplacedJobs+1)
	}
	if afterJob.ActiveJobs != initial.ActiveJobs+1 {
		t.Fatalf("ActiveJobs = %d, want %d", afterJob.ActiveJobs, initial.ActiveJobs+1)
	}
	if afterJob.Quiet() {
		t.Fatal("a queued job must not read as quiet")
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRetryNoOffers}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	afterLifecycle, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read lifecycle snapshot: %v", err)
	}
	if afterLifecycle.LifecycleID <= afterJob.LifecycleID {
		t.Fatalf("LifecycleID = %d, want > %d", afterLifecycle.LifecycleID, afterJob.LifecycleID)
	}

	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	afterLaunch, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read launch snapshot: %v", err)
	}
	if afterLaunch.LiveLaunches != afterLifecycle.LiveLaunches+1 {
		t.Fatalf("LiveLaunches = %d, want %d", afterLaunch.LiveLaunches, afterLifecycle.LiveLaunches+1)
	}

	placementJob, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python place.py", "placing", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU placement: %v", err)
	}
	if _, err := db.CreatePlacementIntent(database, placementJob, "test"); err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	moveJob, err := db.RecordQueuedWithGPU(database, db.LaunchHost(launchID), t.TempDir(), "python move.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU move: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{JobID: moveJob, TargetKind: db.MoveTargetNew}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	afterIntents, err := ReadWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read intent snapshot: %v", err)
	}
	if afterIntents.OpenPlacementIntents != afterLaunch.OpenPlacementIntents+1 {
		t.Fatalf("OpenPlacementIntents = %d, want %d", afterIntents.OpenPlacementIntents, afterLaunch.OpenPlacementIntents+1)
	}
	if afterIntents.OpenMoveIntents != afterLaunch.OpenMoveIntents+1 {
		t.Fatalf("OpenMoveIntents = %d, want %d", afterIntents.OpenMoveIntents, afterLaunch.OpenMoveIntents+1)
	}
}

// fakeClock and scriptedWaiter make WaitForInvalidation deterministic: the
// waiter advances the same clock the loop reads, so a test can assert on the
// exact wall-clock time a wait ended without sleeping.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) {
	if d > 0 {
		c.now = c.now.Add(d)
	}
}

// waiterStep is one scripted reply from the change source.
type waiterStep struct {
	// after is how long the waiter blocks before replying. It is capped by the
	// caller's maxWait, which is what makes deadlines observable.
	after time.Duration
	// changed reports a state-change notification rather than a timeout. It is
	// only delivered when after <= maxWait.
	changed bool
	err     error
}

type scriptedWaiter struct {
	clock *fakeClock
	steps []waiterStep
	// calls records the maxWait of each call, so tests can assert the loop
	// narrowed its wait to the nearest deadline.
	calls []time.Duration
}

func (w *scriptedWaiter) Wait(_ context.Context, maxWait time.Duration) (bool, error) {
	w.calls = append(w.calls, maxWait)
	if len(w.steps) == 0 {
		// Nothing left to say: behave as a plain timeout so the loop's own
		// deadlines terminate the test rather than the script running dry.
		w.clock.advance(maxWait)
		return false, nil
	}
	step := w.steps[0]
	w.steps = w.steps[1:]
	if maxWait > 0 && step.after > maxWait {
		// The caller's deadline arrives first; the notification does not.
		w.clock.advance(maxWait)
		return false, nil
	}
	w.clock.advance(step.after)
	return step.changed, step.err
}

func TestWaitForInvalidationIgnoresChangesThatDoNotMoveTheSnapshot(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	waiter := &scriptedWaiter{clock: clock, steps: []waiterStep{
		{after: time.Second, changed: true},
		{after: time.Second, changed: true},
	}}

	snapshot, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:       waiter,
		ReadSnapshot: func() (WakeSnapshot, error) { return baseline, nil },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Debounce:     2 * time.Second,
		Now:          clock.Now,
	})

	if reason != WakeBackstop {
		t.Fatalf("reason = %q, want %q — unrelated writes must not end the wait", reason, WakeBackstop)
	}
	if snapshot != baseline {
		t.Fatalf("snapshot = %+v, want the unchanged baseline", snapshot)
	}
}

func TestWaitForInvalidationReturnsActionOnRealChange(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	moved := WakeSnapshot{LifecycleID: 8, UnplacedJobs: 1}
	waiter := &scriptedWaiter{clock: clock, steps: []waiterStep{
		{after: time.Second, changed: true},
	}}

	snapshot, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:       waiter,
		ReadSnapshot: func() (WakeSnapshot, error) { return moved, nil },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Debounce:     2 * time.Second,
		Now:          clock.Now,
	})

	if reason != WakeAction {
		t.Fatalf("reason = %q, want %q", reason, WakeAction)
	}
	if snapshot != moved {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, moved)
	}
}

// A burst of writes should produce one pass, not one per write, and the pass
// should see the final state rather than the first.
func TestWaitForInvalidationDebouncesABurst(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	reads := 0
	waiter := &scriptedWaiter{clock: clock, steps: []waiterStep{
		{after: 100 * time.Millisecond, changed: true},
		{after: 100 * time.Millisecond, changed: true},
		{after: 100 * time.Millisecond, changed: true},
	}}

	snapshot, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter: waiter,
		ReadSnapshot: func() (WakeSnapshot, error) {
			reads++
			return WakeSnapshot{LifecycleID: int64(7 + reads)}, nil
		},
		Baseline: baseline,
		Wait:     AutopilotCooldownIdle,
		Backstop: AutopilotQuietBackstop,
		Debounce: 2 * time.Second,
		Now:      clock.Now,
	})

	if reason != WakeAction {
		t.Fatalf("reason = %q, want %q", reason, WakeAction)
	}
	if snapshot.LifecycleID != 10 {
		t.Fatalf("LifecycleID = %d, want 10 (the newest state in the burst)", snapshot.LifecycleID)
	}
}

func TestWaitForInvalidationTimerOnlyWhenPolicyAllows(t *testing.T) {
	for _, timerRunsPass := range []bool{true, false} {
		t.Run("timerRunsPass="+strconv.FormatBool(timerRunsPass), func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
			start := clock.now
			baseline := WakeSnapshot{LifecycleID: 7}
			waiter := &scriptedWaiter{clock: clock}

			_, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
				Waiter:        waiter,
				ReadSnapshot:  func() (WakeSnapshot, error) { return baseline, nil },
				Baseline:      baseline,
				Wait:          AutopilotCooldownBlocked,
				TimerRunsPass: timerRunsPass,
				Backstop:      AutopilotQuietBackstop,
				Debounce:      2 * time.Second,
				Now:           clock.Now,
			})

			elapsed := clock.now.Sub(start)
			if timerRunsPass {
				if reason != WakeTimer {
					t.Fatalf("reason = %q, want %q", reason, WakeTimer)
				}
				if elapsed != AutopilotCooldownBlocked {
					t.Fatalf("elapsed = %s, want the blocked cooldown %s", elapsed, AutopilotCooldownBlocked)
				}
				return
			}
			if reason != WakeBackstop {
				t.Fatalf("reason = %q, want %q", reason, WakeBackstop)
			}
			if elapsed != AutopilotQuietBackstop {
				t.Fatalf("elapsed = %s, want the backstop %s", elapsed, AutopilotQuietBackstop)
			}
		})
	}
}

// The daemon's quiet wait keeps its sync cadence without committing to a
// replan, so it must end the wait ahead of the backstop.
func TestWaitForInvalidationQuietWaitEndsBeforeBackstop(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	start := clock.now
	baseline := WakeSnapshot{LifecycleID: 7}
	waiter := &scriptedWaiter{clock: clock}

	_, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:        waiter,
		ReadSnapshot:  func() (WakeSnapshot, error) { return baseline, nil },
		Baseline:      baseline,
		Wait:          AutopilotCooldownIdle,
		TimerRunsPass: false,
		QuietWait:     5 * time.Minute,
		Backstop:      AutopilotQuietBackstop,
		Debounce:      2 * time.Second,
		Now:           clock.Now,
	})

	if reason != WakeTimer {
		t.Fatalf("reason = %q, want %q", reason, WakeTimer)
	}
	if elapsed := clock.now.Sub(start); elapsed != 5*time.Minute {
		t.Fatalf("elapsed = %s, want the quiet wait 5m", elapsed)
	}
}

// A quiet-timeout probe that changed external state ends the wait, since the
// writes it produced were absorbed before the loop could compare them.
func TestWaitForInvalidationQuietTimeoutProbeEndsWait(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	probed := WakeSnapshot{LifecycleID: 9}
	waiter := &scriptedWaiter{clock: clock}
	probes := 0

	snapshot, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:       waiter,
		ReadSnapshot: func() (WakeSnapshot, error) { return probed, nil },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Debounce:     2 * time.Second,
		Now:          clock.Now,
		OnQuietTimeout: func(context.Context) bool {
			probes++
			return probes >= 2
		},
	})

	if reason != WakeAction {
		t.Fatalf("reason = %q, want %q", reason, WakeAction)
	}
	if snapshot != probed {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, probed)
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 (a probe that changed nothing must not end the wait)", probes)
	}
}

// A watch error means absence of a notification proves nothing, so the wait
// must fall back to running a pass rather than sleeping on a dead source.
func TestWaitForInvalidationWatchErrorRunsPass(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	waiter := &scriptedWaiter{clock: clock, steps: []waiterStep{
		{after: time.Second, err: errors.New("watcher closed")},
	}}
	reported := 0

	_, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:       waiter,
		ReadSnapshot: func() (WakeSnapshot, error) { return baseline, nil },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Debounce:     2 * time.Second,
		Now:          clock.Now,
		OnError:      func(error) { reported++ },
	})

	if reason != WakeAction {
		t.Fatalf("reason = %q, want %q", reason, WakeAction)
	}
	if reported != 1 {
		t.Fatalf("OnError calls = %d, want 1", reported)
	}
}

func TestWaitForInvalidationSnapshotErrorRunsPass(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	baseline := WakeSnapshot{LifecycleID: 7}
	waiter := &scriptedWaiter{clock: clock, steps: []waiterStep{
		{after: time.Second, changed: true},
	}}

	_, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
		Waiter:       waiter,
		ReadSnapshot: func() (WakeSnapshot, error) { return WakeSnapshot{}, errors.New("database is locked") },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Debounce:     2 * time.Second,
		Now:          clock.Now,
		OnError:      func(error) {},
	})

	if reason != WakeAction {
		t.Fatalf("reason = %q, want %q — a failed read cannot show that nothing moved", reason, WakeAction)
	}
}

func TestWaitForInvalidationCanceledContext(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	baseline := WakeSnapshot{LifecycleID: 7}

	_, reason := WaitForInvalidation(ctx, InvalidationOptions{
		Waiter:       &scriptedWaiter{clock: clock},
		ReadSnapshot: func() (WakeSnapshot, error) { return baseline, nil },
		Baseline:     baseline,
		Wait:         AutopilotCooldownIdle,
		Backstop:     AutopilotQuietBackstop,
		Now:          clock.Now,
	})

	if reason != WakeDone {
		t.Fatalf("reason = %q, want %q", reason, WakeDone)
	}
}

func dispatchSMSeed() uint64 {
	if raw := os.Getenv("WEFT_TEST_SEED"); raw != "" {
		if seed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return seed
		}
	}
	return uint64(time.Now().UnixNano())
}

// TestDispatchStateMachine_RandomCommands drives the dispatcher through random
// sequences of pass outcomes and change notifications, model-checking the
// invariants the policy exists to guarantee:
//
//   - the loop never waits longer than the backstop without ending
//   - a wait only ends with WakeAction when the snapshot actually moved
//   - a timer wake only happens when the policy armed one (TimerRunsPass, or
//     the daemon's quiet wait)
//   - a job that stays blocked on a market reason is always revisited within
//     the blocked cooldown, so suppression can never strand it
//   - the reported snapshot is a valid successor: either the baseline, or the
//     state the most recent read observed
//
// The seed is logged on failure; set WEFT_TEST_SEED to reproduce.
func TestDispatchStateMachine_RandomCommands(t *testing.T) {
	seed := dispatchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("dispatch state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	const marketReason = "no offers from providers for gpu=A40"
	const budgetReason = "run-rate headroom exhausted (this group needs $1.50/hr)"
	const backoffReason = "reuse backoff 45s remaining (after 2 failed submit(s))"

	outcomes := []struct {
		name    string
		result  *GroupedAutoPilotResult
		err     error
		blocked map[int64]string
	}{
		{name: "progress", result: &GroupedAutoPilotResult{Launched: 1}},
		{name: "idle", result: &GroupedAutoPilotResult{}},
		{
			name:    "market-blocked",
			result:  &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: marketReason}},
			blocked: map[int64]string{1: marketReason},
		},
		{
			name:    "budget-blocked",
			result:  &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: budgetReason}},
			blocked: map[int64]string{1: budgetReason},
		},
		{
			name:    "backoff-blocked",
			result:  &GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: backoffReason}},
			blocked: map[int64]string{1: backoffReason},
		},
		{name: "error", err: errors.New("provider unreachable")},
		{name: "busy", err: ErrAutopilotBusy},
	}

	for iteration := 0; iteration < 30; iteration++ {
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		baseline := WakeSnapshot{LifecycleID: 1}
		quietWait := time.Duration(0)
		if rng.IntN(2) == 0 {
			// Half the runs model the daemon, which also wakes on a sync
			// cadence; the other half model a pass-only runner.
			quietWait = time.Duration(1+rng.IntN(10)) * time.Minute
		}

		for step := 0; step < 30; step++ {
			pick := outcomes[rng.IntN(len(outcomes))]
			outcome, wait := ClassifyPass(pick.result, pick.err, AutopilotCooldownContend)
			timerRunsPass := TimerRunsPass(outcome, pick.blocked)

			// Script a run of notifications, some of which move the snapshot.
			steps := make([]waiterStep, 0, 4)
			moves := false
			for i := 0; i < rng.IntN(4); i++ {
				changed := rng.IntN(2) == 0
				if changed && rng.IntN(2) == 0 {
					moves = true
				}
				steps = append(steps, waiterStep{
					after:   time.Duration(rng.IntN(90)+1) * time.Second,
					changed: changed,
				})
			}
			moved := baseline
			if moves {
				moved.LifecycleID = baseline.LifecycleID + int64(1+rng.IntN(3))
			}

			waiter := &scriptedWaiter{clock: clock, steps: steps}
			start := clock.now
			got, reason := WaitForInvalidation(context.Background(), InvalidationOptions{
				Waiter:        waiter,
				ReadSnapshot:  func() (WakeSnapshot, error) { return moved, nil },
				Baseline:      baseline,
				Wait:          wait,
				TimerRunsPass: timerRunsPass,
				QuietWait:     quietWait,
				Backstop:      AutopilotQuietBackstop,
				Debounce:      2 * time.Second,
				Now:           clock.Now,
			})
			elapsed := clock.now.Sub(start)

			label := fmt.Sprintf("iteration %d step %d outcome %s quietWait %s", iteration, step, pick.name, quietWait)

			if elapsed > AutopilotQuietBackstop {
				t.Fatalf("%s: waited %s, longer than the backstop %s", label, elapsed, AutopilotQuietBackstop)
			}
			switch reason {
			case WakeAction:
				if got == baseline {
					t.Fatalf("%s: WakeAction with an unchanged snapshot", label)
				}
				if got != moved {
					t.Fatalf("%s: WakeAction returned %+v, want the observed state %+v", label, got, moved)
				}
			case WakeTimer:
				if !timerRunsPass && quietWait == 0 {
					t.Fatalf("%s: timer wake with no timer armed", label)
				}
				if got != baseline {
					t.Fatalf("%s: timer wake must report the baseline, got %+v", label, got)
				}
			case WakeBackstop:
				if elapsed != AutopilotQuietBackstop {
					t.Fatalf("%s: backstop fired after %s, want %s", label, elapsed, AutopilotQuietBackstop)
				}
			case WakeDone:
				t.Fatalf("%s: unexpected WakeDone without cancellation", label)
			}

			// A blocker that no local write announces must be revisited within
			// its own cooldown, or suppression strands the job. The market can
			// move at any moment; a retry backoff expires on a local clock and
			// earns the slower recheck.
			switch pick.name {
			case "market-blocked":
				if elapsed > AutopilotCooldownBlocked {
					t.Fatalf("%s: market-blocked pass waited %s, longer than the blocked cooldown %s",
						label, elapsed, AutopilotCooldownBlocked)
				}
			case "backoff-blocked":
				if elapsed > AutopilotCooldownBackoff {
					t.Fatalf("%s: backoff-blocked pass waited %s, longer than the backoff cooldown %s",
						label, elapsed, AutopilotCooldownBackoff)
				}
			}

			baseline = got
		}
	}
}
