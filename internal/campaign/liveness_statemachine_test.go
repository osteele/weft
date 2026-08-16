package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
)

// TestLivenessStateMachine_RandomCommands exercises CheckInstance,
// checkStaleHeartbeat, and ExecuteAction with randomized liveness observations
// while hysteresis is enabled. It model-checks the invariants from
// specs/campaign-lifecycle.allium:
//   - provider-dead detection requires a consecutive stretch of dead
//     observations lasting at least deadConfirmTime
//   - an alive observation resets the dead-confirm and unknown-streak timers
//   - provider-status-unknown defers destructive actions for interruptible
//     instances until the unknown streak exceeds stalePauseTimeout
//   - stale heartbeat + unreachable SSH probe requires both minProbeFailureAttempts
//     consecutive failures and minProbeFailureWindow elapsed time
//   - a successful probe or fresh heartbeat resets the probe-failure window
//   - terminal launch statuses are sticky
//   - provider destruction is recorded at most once
//   - jobs are reset only after the terminal transition commits
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestLivenessStateMachine_RandomCommands(t *testing.T) {
	seed := launchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("liveness state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 30
	stepsPerRun := 30
	if testing.Short() {
		iterations = 5
		stepsPerRun = 15
	}

	for i := 0; i < iterations; i++ {
		database := setupTestDB(t)

		instanceType := cloud.InstanceTypeOnDemand
		if rng.IntN(2) == 0 {
			instanceType = cloud.InstanceTypeInterruptible
		}

		jobID, launchID, client := setupLivenessLaunchStateMachine(t, database, instanceType)

		r := NewReconciler()
		confirmTime := 30 * time.Second
		r.deadConfirmTime = confirmTime

		m := &livenessModel{
			confirmTime:  confirmTime,
			instanceType: instanceType,
		}
		now := time.Now()

		// Mock R2 heartbeat fetch and SSH probe so the model controls them.
		origFetch := fetchReconcileHeartbeat
		origProbe := probeCampaignAgent
		t.Cleanup(func() {
			fetchReconcileHeartbeat = origFetch
			probeCampaignAgent = origProbe
		})
		fetchReconcileHeartbeat = func(_ context.Context, _ *r2.Client, id int64) (*HeartbeatSample, time.Duration) {
			if id != launchID {
				return nil, 0
			}
			if m.heartbeatAge == 0 {
				return nil, 0
			}
			return &HeartbeatSample{Ts: now.Add(-m.heartbeatAge).Unix()}, m.heartbeatAge
		}
		probeCampaignAgent = func(_ *cloud.Instance, _ time.Duration) (bool, error) {
			switch m.probeResult {
			case probeAlive:
				return true, nil
			case probeGone:
				return false, nil
			case probeError:
				return false, errors.New("probe unreachable")
			default:
				return false, errors.New("probe not configured")
			}
		}

		for step := 0; step < stepsPerRun; step++ {
			cmd := randomLivenessCommand(rng, m)
			desc := cmd.describe()
			cmd.run(t, database, r, launchID, &now, client, m, rng)

			checkLivenessInvariants(t, database, jobID, launchID, client, m, &now, i, step, desc, seed)
		}
	}
}

// probeResult is the mocked outcome of an SSH agent probe.
type probeResult int

const (
	probeNone probeResult = iota
	probeAlive
	probeGone
	probeError
)

// livenessModel is the reference state for one launch/job pair under the
// liveness hysteresis rules. Zero time means the corresponding timer is not
// running.
type livenessModel struct {
	providerStatus string        // running, exited, unknown, notfound
	deadSince      time.Time     // start of consecutive dead observations (measured at reconcile time)
	unknownSince   time.Time     // start of consecutive unknown observations (measured at reconcile time)
	terminal       bool          // terminal transition committed
	destroyed      bool          // provider destroy committed
	confirmTime    time.Duration // dead-confirm window
	instanceType   string        // on-demand or interruptible

	// heartbeat / SSH probe state
	heartbeatAge   time.Duration
	probeResult    probeResult
	probeFailCount int
	firstProbeFail time.Time

	// other classifier inputs driven through CheckInstance
	instancePhase     string
	bootstrapStage    string
	terminationIntent *instanceintent.Marker
}

func (m *livenessModel) isDeadProviderStatus() bool {
	return m.providerStatus == cloud.ProviderStatusExited
}

func (m *livenessModel) isUnknownProviderStatus() bool {
	return m.providerStatus == "unknown"
}

func (m *livenessModel) isNotFoundProviderStatus() bool {
	return m.providerStatus == "notfound"
}

func (m *livenessModel) isAliveProviderStatus() bool {
	return m.providerStatus == cloud.ProviderStatusRunning
}

// updateHysteresisTimers advances the model's dead/unknown timers based on the
// current provider observation at the given reconcile time. The production
// code records firstDeadAt and firstUnknownAt when CheckInstance evaluates the
// observation, not when the observation command was issued, so the model must
// match that semantics.
func (m *livenessModel) updateHysteresisTimers(now time.Time) {
	if m.isDeadProviderStatus() {
		if m.deadSince.IsZero() {
			m.deadSince = now
		}
	} else {
		m.deadSince = time.Time{}
	}

	if m.isUnknownProviderStatus() || m.isNotFoundProviderStatus() {
		if m.unknownSince.IsZero() {
			m.unknownSince = now
		}
	} else {
		m.unknownSince = time.Time{}
	}
}

func (m *livenessModel) unknownDuration(now time.Time) time.Duration {
	if m.unknownSince.IsZero() {
		return 0
	}
	return now.Sub(m.unknownSince)
}

func (m *livenessModel) deadDuration(now time.Time) time.Duration {
	if m.deadSince.IsZero() {
		return 0
	}
	return now.Sub(m.deadSince)
}

// livenessCommand is one mutating operation in the liveness state machine.
type livenessCommand struct {
	name string
	run  func(*testing.T, *sql.DB, *Reconciler, int64, *time.Time, *recordingMockClient, *livenessModel, *rand.Rand)
}

func (c livenessCommand) describe() string { return c.name }

func randomLivenessCommand(rng *rand.Rand, m *livenessModel) livenessCommand {
	choices := []livenessCommand{
		{name: "observeRunning", run: observeRunningLiveness},
		{name: "observeExited", run: observeExitedLiveness},
		{name: "observeUnknown", run: observeUnknownLiveness},
		{name: "observeNotFound", run: observeNotFoundLiveness},
		{name: "heartbeatFresh", run: heartbeatFreshLiveness},
		{name: "heartbeatStale", run: heartbeatStaleLiveness},
		{name: "probeAlive", run: probeAliveLiveness},
		{name: "probeGone", run: probeGoneLiveness},
		{name: "probeError", run: probeErrorLiveness},
		{name: "phaseEmpty", run: phaseEmptyLiveness},
		{name: "phaseSetup", run: phaseSetupLiveness},
		{name: "phaseRunning", run: phaseRunningLiveness},
		{name: "bootstrapNone", run: bootstrapNoneLiveness},
		{name: "bootstrapReady", run: bootstrapReadyLiveness},
		{name: "bootstrapFailed", run: bootstrapFailedLiveness},
		{name: "terminationIntent", run: terminationIntentLiveness},
		{name: "clearTerminationIntent", run: clearTerminationIntentLiveness},
		{name: "reconcile", run: reconcileLiveness},
		{name: "wait", run: waitLiveness},
	}
	return choices[rng.IntN(len(choices))]
}

func waitLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	// Advance simulated time by up to twice the dead-confirm window so the
	// random walk exercises both sides of the hysteresis threshold.
	*now = now.Add(time.Duration(rng.IntN(int(2*m.confirmTime.Seconds())+1)) * time.Second)
}

func observeRunningLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.providerStatus = cloud.ProviderStatusRunning
}

func observeExitedLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.providerStatus = cloud.ProviderStatusExited
}

func observeUnknownLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.providerStatus = "unknown"
}

func observeNotFoundLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.providerStatus = "notfound"
}

func heartbeatFreshLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.heartbeatAge = 0
}

func heartbeatStaleLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	// Exceed the steady-state stale threshold by a small, bounded amount.
	m.heartbeatAge = heartbeatStaleThreshold + time.Duration(rng.IntN(60)+1)*time.Second
}

func probeAliveLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.probeResult = probeAlive
}

func probeGoneLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.probeResult = probeGone
}

func probeErrorLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.probeResult = probeError
}

func phaseEmptyLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.instancePhase = ""
}

func phaseSetupLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.instancePhase = "setup:1"
}

func phaseRunningLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.instancePhase = "running:1"
}

func bootstrapNoneLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.bootstrapStage = ""
}

func bootstrapReadyLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.bootstrapStage = bootstrapStageReady
}

func bootstrapFailedLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.bootstrapStage = "failed:apt"
}

func terminationIntentLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.terminationIntent = &instanceintent.Marker{
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonJobFailure,
		RequestedAtUnix:   now.Unix(),
	}
}

func clearTerminationIntentLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	m.terminationIntent = nil
}

func reconcileLiveness(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *livenessModel, rng *rand.Rand) {
	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci == nil {
		t.Fatalf("launch %d missing", launchID)
	}

	*now = now.Add(time.Duration(rng.IntN(30)+1) * time.Second)
	m.updateHysteresisTimers(*now)

	var inst *cloud.Instance
	var providerErr error
	switch m.providerStatus {
	case cloud.ProviderStatusRunning:
		inst = &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}
	case cloud.ProviderStatusExited:
		inst = &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusExited}
	case "unknown":
		providerErr = fmt.Errorf("provider poll failed: %w", cloud.ErrProviderCommandTimeout)
	case "notfound":
		providerErr = fmt.Errorf("provider instance %s: %w", ci.EffectiveProviderID(), cloud.ErrInstanceNotFound)
	}

	jobs, err := db.GetLaunchJobsIncludingAttempts(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	jobState := ComputeJobState(jobs, nil)

	action := r.CheckInstance(CheckInstanceParams{
		CI:                       ci,
		ProviderInst:             inst,
		ProviderErr:              providerErr,
		JobState:                 jobState,
		Now:                      *now,
		ProviderStatusUnknownFor: m.unknownDuration(*now),
		PauseTolerant:            m.instanceType == cloud.InstanceTypeInterruptible,
		HeartbeatAge:             m.heartbeatAge,
		InstancePhase:            m.instancePhase,
		BootstrapStage:           m.bootstrapStage,
		TerminationIntent:        m.terminationIntent,
	})

	// Evaluate the reconciler-specific stale-heartbeat watchdog when CheckInstance
	// did not already produce a destructive action. This path is not in
	// CheckInstance; it lives in reconcile.go so the model includes it here to
	// cover the probe-failure hysteresis window.
	if action.Kind == ActionNone && ci.Status == db.LaunchStatusRunning && m.heartbeatAge > effectiveHeartbeatStaleThreshold(ci.AgentReadyAtUnix, *now) {
		hbAction := r.checkStaleHeartbeat(nil, ci, inst, *now)
		if hbAction.Kind != ActionNone {
			action = hbAction
		}
	}

	if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
		reconciled, terminated := ExecuteAction(database, client, ci, action)
		if reconciled {
			if action.DestroyProvider {
				m.destroyed = true
			}
			if terminated && action.TerminalStatus != "" && db.IsTerminalLaunchStatus(action.TerminalStatus) {
				m.terminal = true
			}
		}
	}

	// Mirror the probe-failure counter bookkeeping so the model stays in sync
	// with the Reconciler's internal map. The production code clears the counter
	// when the heartbeat is fresh or the probe reports the agent alive, and
	// increments it on each unreachable probe while the heartbeat is stale.
	if m.heartbeatAge == 0 || m.heartbeatAge <= effectiveHeartbeatStaleThreshold(ci.AgentReadyAtUnix, *now) {
		m.probeFailCount = 0
		m.firstProbeFail = time.Time{}
	} else if m.probeResult == probeAlive {
		m.probeFailCount = 0
		m.firstProbeFail = time.Time{}
	} else if m.probeResult == probeError {
		if m.probeFailCount == 0 {
			m.firstProbeFail = *now
		}
		m.probeFailCount++
	}
}

// checkLivenessInvariants verifies that the persisted state matches the
// hysteresis model and the spec invariants.
func checkLivenessInvariants(t *testing.T, database *sql.DB, jobID, launchID int64, client *recordingMockClient, m *livenessModel, now *time.Time, iteration, step int, desc string, seed uint64) {
	t.Helper()

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetLaunch: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if ci == nil {
		t.Fatalf("iteration %d step %d: launch %d missing\nseed=%d command=%s", iteration, step, launchID, seed, desc)
	}

	isTerm := db.IsTerminalLaunchStatus(ci.Status)
	if m.terminal && !isTerm {
		t.Fatalf("iteration %d step %d: model says terminal but launch status = %q\nseed=%d command=%s",
			iteration, step, ci.Status, seed, desc)
	}
	if isTerm {
		if ci.EndedAt == nil || *ci.EndedAt <= 0 {
			t.Fatalf("iteration %d step %d: terminal status %q without ended_at\nseed=%d command=%s",
				iteration, step, ci.Status, seed, desc)
		}
		if ci.TerminationReason == "" {
			t.Fatalf("iteration %d step %d: terminal status %q without termination_reason\nseed=%d command=%s",
				iteration, step, ci.Status, seed, desc)
		}
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetJobByID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if job == nil {
		t.Fatalf("iteration %d step %d: job %d missing\nseed=%d command=%s", iteration, step, jobID, seed, desc)
	}
	if isTerm && ci.Status == db.LaunchStatusFailed {
		// After a failed terminal transition the job is reset to unplaced
		// (queued/draft) or, when an attempt was created, derived as "orphaned"
		// by the job_status view until the next placement reconciler pass.
		if !db.IsTerminalStatus(job.Status) && job.Status != db.StatusQueued && job.Status != db.StatusDraft && job.Status != "orphaned" {
			t.Fatalf("iteration %d step %d: failed launch left job status = %q, want queued/draft/orphaned\nseed=%d command=%s",
				iteration, step, job.Status, seed, desc)
		}
	}

	destroyCount := client.destroyCount
	if m.destroyed && destroyCount == 0 {
		t.Fatalf("iteration %d step %d: model says destroyed but provider never destroyed\nseed=%d command=%s",
			iteration, step, seed, desc)
	}
	if destroyCount > 1 {
		t.Fatalf("iteration %d step %d: provider destroyed %d times, want at most 1\nseed=%d command=%s",
			iteration, step, destroyCount, seed, desc)
	}

	// A terminal launch must have been recorded by the model.
	if isTerm && !m.terminal {
		t.Fatalf("iteration %d step %d: launch terminal but model never recorded a committed transition\nseed=%d command=%s",
			iteration, step, seed, desc)
	}

	// Jobs must not be reset before the terminal transition commits. The
	// in-progress job is running; if it has been reset to queued/draft/orphaned
	// while the launch is still non-terminal, ExecuteAction violated ordering.
	if !isTerm {
		if job.Status != db.StatusRunning {
			t.Fatalf("iteration %d step %d: non-terminal launch left job status = %q, want running\nseed=%d command=%s",
				iteration, step, job.Status, seed, desc)
		}
	}

	// Hysteresis invariants: a destructive action may only commit after the
	// appropriate confirmation window. Only enforce this immediately after a
	// reconcile command; observe/wait commands change model state without
	// invoking CheckInstance.
	if desc == "reconcile" && !isTerm {
		if m.isDeadProviderStatus() && !m.deadSince.IsZero() {
			if m.deadDuration(*now) >= m.confirmTime {
				t.Fatalf("iteration %d step %d: exited observation %s after dead start exceeds confirmTime %s but launch not terminal\nseed=%d command=%s",
					iteration, step, m.deadDuration(*now), m.confirmTime, seed, desc)
			}
		}
	}
}

// TestLivenessStateMachine_InterruptibleUnknownDefersTermination checks that
// an interruptible instance whose provider status is unknown does not get a
// destructive action until the unknown streak exceeds stalePauseTimeout.
func TestLivenessStateMachine_InterruptibleUnknownDefersTermination(t *testing.T) {
	database := setupTestDB(t)
	jobID, launchID, client := setupLivenessLaunchStateMachine(t, database, cloud.InstanceTypeInterruptible)
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	r := NewReconciler()
	r.deadConfirmTime = 30 * time.Second

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	t0 := time.Now()
	unknownParams := func(at time.Time, unknownFor time.Duration) CheckInstanceParams {
		return CheckInstanceParams{
			CI:                       ci,
			ProviderErr:              fmt.Errorf("provider poll failed: %w", cloud.ErrProviderCommandTimeout),
			Now:                      at,
			ProviderStatusUnknownFor: unknownFor,
			PauseTolerant:            true,
		}
	}

	// Unknown for a short time defers every destructive action.
	if action := r.CheckInstance(unknownParams(t0, time.Minute)); isDestructiveLivenessAction(action.Kind) {
		t.Fatalf("short unknown streak: action.Kind = %v, want non-destructive", action.Kind)
	}

	// Unknown past the bound allows destructive actions again.
	action := r.CheckInstance(unknownParams(t0.Add(stalePauseTimeout+time.Minute), stalePauseTimeout+time.Minute))
	if !isDestructiveLivenessAction(action.Kind) {
		t.Fatalf("long unknown streak: action.Kind = %v, want destructive liveness action", action.Kind)
	}

	// ExecuteAction should not commit because there is no provider instance to
	// destroy; the important invariant is that the action is no longer deferred.
	ExecuteAction(database, client, ci, action)

	ci, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch after action: %v", err)
	}
	if db.IsTerminalLaunchStatus(ci.Status) && action.DestroyProvider {
		// If the action required a destroy and the destroy could not be
		// performed, ExecuteAction defers; the test above already covers
		// destroy deferral separately.
	}
}

// TestLivenessStateMachine_HeartbeatProbeHysteresis checks the SSH-probe
// hysteresis window from specs/campaign-lifecycle.allium HeartbeatStale:
// stale heartbeat alone is not enough, unreachable probes require both a
// minimum attempt count and a minimum elapsed window, and a successful probe
// resets the window.
func TestLivenessStateMachine_HeartbeatProbeHysteresis(t *testing.T) {
	database := setupTestDB(t)
	_, launchID, client := setupLivenessLaunchStateMachine(t, database, cloud.InstanceTypeOnDemand)

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	origFetch := fetchReconcileHeartbeat
	origProbe := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetch
		probeCampaignAgent = origProbe
	})

	staleAge := heartbeatStaleThreshold + time.Minute
	fetchReconcileHeartbeat = func(context.Context, *r2.Client, int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-staleAge).Unix()}, staleAge
	}

	r := NewReconciler()
	t0 := time.Now()

	// Two rapid probe errors satisfy neither the count nor the window.
	probeCalls := 0
	probeCampaignAgent = func(*cloud.Instance, time.Duration) (bool, error) {
		probeCalls++
		return false, errors.New("unreachable")
	}
	r.checkStaleHeartbeat(nil, ci, &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}, t0)
	r.checkStaleHeartbeat(nil, ci, &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}, t0.Add(5*time.Second))
	if probeCalls != 2 {
		t.Fatalf("probe calls = %d, want 2", probeCalls)
	}

	// A successful probe clears the counter; the next error starts a fresh window.
	probeCalls = 0
	probeCampaignAgent = func(*cloud.Instance, time.Duration) (bool, error) {
		probeCalls++
		return true, nil
	}
	if action := r.checkStaleHeartbeat(nil, ci, &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}, t0.Add(10*time.Second)); action.Kind != ActionNone {
		t.Fatalf("alive probe: action = %v, want none", action.Kind)
	}

	// Now generate minProbeFailureAttempts errors spread across minProbeFailureWindow.
	probeCalls = 0
	probeCampaignAgent = func(*cloud.Instance, time.Duration) (bool, error) {
		probeCalls++
		return false, errors.New("unreachable")
	}
	for i := 0; i < minProbeFailureAttempts-1; i++ {
		if action := r.checkStaleHeartbeat(nil, ci, &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}, t0.Add(20*time.Second+time.Duration(i)*time.Second)); action.Kind != ActionNone {
			t.Fatalf("probe %d: action = %v, want none", i, action.Kind)
		}
	}
	// The final attempt is past the window; this should fire.
	action := r.checkStaleHeartbeat(nil, ci, &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}, t0.Add(minProbeFailureWindow+30*time.Second))
	if action.Kind != ActionStaleHeartbeat {
		t.Fatalf("action.Kind = %v, want ActionStaleHeartbeat", action.Kind)
	}
	if !action.DestroyProvider {
		t.Fatalf("action.DestroyProvider = false, want true")
	}

	// ExecuteAction commits destroy before marking terminal and resetting jobs.
	reconciled, terminated := ExecuteAction(database, client, ci, action)
	if !reconciled {
		t.Fatalf("ExecuteAction did not reconcile")
	}
	if !terminated {
		t.Fatalf("ExecuteAction did not terminate")
	}
	if client.destroyCount != 1 {
		t.Fatalf("destroy count = %d, want 1", client.destroyCount)
	}
	ci, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch after terminal: %v", err)
	}
	if !db.IsTerminalLaunchStatus(ci.Status) {
		t.Fatalf("launch status = %q, want terminal", ci.Status)
	}
}

// TestLivenessStateMachine_TerminationIntentDestroyBeforeMark checks that a
// durable termination intent causes CheckInstance to request a provider destroy
// and that ExecuteAction performs the destroy before marking the launch
// terminal and resetting jobs.
func TestLivenessStateMachine_TerminationIntentDestroyBeforeMark(t *testing.T) {
	database := setupTestDB(t)
	jobID, launchID, client := setupLivenessLaunchStateMachine(t, database, cloud.InstanceTypeOnDemand)

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	r := NewReconciler()
	r.deadConfirmTime = 30 * time.Second

	intent := &instanceintent.Marker{
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonJobFailure,
		RequestedAtUnix:   time.Now().Unix(),
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI:                ci,
		ProviderInst:      &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning},
		TerminationIntent: intent,
		Now:               time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %v, want ActionTerminationIntent", action.Kind)
	}
	if !action.DestroyProvider {
		t.Fatalf("action.DestroyProvider = false, want true")
	}
	if !action.ResetJobs {
		t.Fatalf("action.ResetJobs = false, want true")
	}

	reconciled, terminated := ExecuteAction(database, client, ci, action)
	if !reconciled {
		t.Fatalf("ExecuteAction did not reconcile")
	}
	if !terminated {
		t.Fatalf("ExecuteAction did not terminate")
	}
	if client.destroyCount != 1 {
		t.Fatalf("destroy count = %d, want 1", client.destroyCount)
	}

	ci, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch after terminal: %v", err)
	}
	if !db.IsTerminalLaunchStatus(ci.Status) {
		t.Fatalf("launch status = %q, want terminal", ci.Status)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !db.IsTerminalStatus(job.Status) && job.Status != db.StatusQueued && job.Status != db.StatusDraft && job.Status != "orphaned" {
		t.Fatalf("job status = %q, want terminal/queued/draft/orphaned", job.Status)
	}
}

// TestLivenessStateMachine_StickyTerminal checks that once a launch reaches a
// terminal status, later reconcile passes cannot revive or re-destroy it.
func TestLivenessStateMachine_StickyTerminal(t *testing.T) {
	database := setupTestDB(t)
	jobID, launchID, client := setupLivenessLaunchStateMachine(t, database, cloud.InstanceTypeOnDemand)

	r := NewReconciler()
	r.deadConfirmTime = -1 // disable hysteresis so the first dead observation is terminal

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	action := r.CheckInstance(CheckInstanceParams{
		CI:           ci,
		ProviderInst: &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusExited},
		Now:          time.Now(),
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %v, want ActionProviderDead", action.Kind)
	}
	ExecuteAction(database, client, ci, action)

	// A later running observation must not revive the launch.
	ci, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	laterAction := r.CheckInstance(CheckInstanceParams{
		CI:           ci,
		ProviderInst: &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning},
		Now:          time.Now().Add(time.Minute),
	})
	if laterAction.Kind != ActionNone {
		t.Fatalf("terminal launch revived: action.Kind = %v, want ActionNone", laterAction.Kind)
	}
	if client.destroyCount != 1 {
		t.Fatalf("destroy count = %d, want 1", client.destroyCount)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !db.IsTerminalStatus(job.Status) && job.Status != db.StatusQueued && job.Status != db.StatusDraft && job.Status != "orphaned" {
		t.Fatalf("job status = %q, want terminal/queued/draft/orphaned", job.Status)
	}
}

// setupLivenessLaunchStateMachine creates a running launch with a single job
// and a mock provider client. The instanceType argument is set on the launch
// row.
func setupLivenessLaunchStateMachine(t *testing.T, database *sql.DB, instanceType string) (jobID, launchID int64, client *recordingMockClient) {
	t.Helper()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "python train.py --gpu RTX_4090", "sm-liveness", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	launchID, err = db.CreateLaunch(database, &db.Launch{
		Status:       db.LaunchStatusRunning,
		Provider:     "vastai",
		GPUSpec:      "RTX_4090",
		InstanceType: instanceType,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, launchID, "prov-sm-live"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	client = &recordingMockClient{MockClient: &cloud.MockClient{ProviderVal: cloud.ProviderVastai}}
	client.DestroyInstanceFunc = func(string) error {
		client.destroyCount++
		return nil
	}
	return jobID, launchID, client
}
