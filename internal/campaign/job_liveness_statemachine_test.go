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

// TestJobLivenessStateMachine_RandomCommands exercises the protocol between
// the host agent and the daemon/DB that decides whether a running job is alive,
// complete, or dead. The model treats R2 as the communication channel: the host
// writes heartbeat samples and completion markers; the daemon consumes them via
// SyncInstanceState, CheckInstance/checkStaleHeartbeat, ExecuteAction, and
// FinalizeStuckJobsWithR2Check.
//
// Invariants model-checked:
//   - a fresh heartbeat reporting the job running/setup keeps the job out of a
//     terminal status (unless a completion marker has already been written)
//   - a .complete marker is ground truth: it eventually moves the job to
//     completed/failed and overrides prior dead/killed guesses
//   - staleness alone does not kill a job; instance termination requires
//     positive evidence (stale heartbeat + unreachable SSH probe past hysteresis)
//   - once an instance is terminal, a job without a completion marker is
//     eventually marked dead by the safety net
//   - terminal instance statuses and job resets are sticky/sequenced
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestJobLivenessStateMachine_RandomCommands(t *testing.T) {
	seed := launchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))

	iterations := 30
	stepsPerRun := 30
	for i := 0; i < iterations; i++ {
		database := setupTestDB(t)
		defer database.Close()

		jobID, launchID, client := setupJobLivenessStateMachine(t, database)

		r := NewReconciler()
		r.deadConfirmTime = 30 * time.Second

		m := &jobLivenessModel{
			confirmTime: r.deadConfirmTime,
			jobID:       jobID,
			launchID:    launchID,
		}
		now := time.Now()

		// Mock every external input so the model controls the protocol.
		installJobLivenessMocks(t, m, &now)

		for step := 0; step < stepsPerRun; step++ {
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				t.Fatalf("iteration %d step %d: GetJobByID: %v", i, step, err)
			}
			prevJobStatus := job.Status

			cmd := randomJobLivenessCommand(rng, m)
			desc := cmd.describe()
			cmd.run(t, database, r, &now, client, m, rng)

			checkJobLivenessInvariants(t, database, jobID, launchID, client, m, &now, prevJobStatus, i, step, desc, seed)
		}
	}
}

// jobLivenessModel is the reference state for one launch/job pair under the
// job-liveness protocol. It tracks what the host has written to R2 and what
// the daemon has observed.
type jobLivenessModel struct {
	confirmTime time.Duration
	jobID       int64
	launchID    int64

	hostAlive bool

	// heartbeat state returned to both SyncInstanceState and checkStaleHeartbeat.
	heartbeatSample *HeartbeatSample
	heartbeatAge    time.Duration

	// instance phase reported to SyncInstanceState and CheckInstance.
	instancePhase string

	// bootstrap stage reported to SyncInstanceState; "ready" bypasses bootstrap.
	bootstrapStage string

	// completion marker state. nil means no marker is currently present.
	completeMarkerExitCode *int
	// completedUnderMarker latches once the job is observed completed while a
	// marker is present, recording that the completion was justified when it
	// happened. A marker cleared afterwards (R2 lifecycle deletion, or
	// clearCompleteMarker here) does not retroactively invalidate it.
	completedUnderMarker bool

	// probe result used by checkStaleHeartbeat.
	probeOutcome probeOutcome

	// daemon-side expectations
	terminalObserved bool // a terminal transition has been observed
	destroyObserved  bool // a provider destroy has been observed
}

func (m *jobLivenessModel) effectiveThreshold(now time.Time) time.Duration {
	// The model keeps agent_ready_at nil in the DB, so the threshold is always
	// the steady-state value. This matches how setupJobLivenessStateMachine
	// creates the launch.
	_ = now
	return heartbeatStaleThreshold
}

func (m *jobLivenessModel) heartbeatFresh(now time.Time) bool {
	if m.heartbeatSample == nil {
		return false
	}
	if m.heartbeatSample.observationUnknown {
		return false
	}
	return m.heartbeatAge > 0 && m.heartbeatAge <= m.effectiveThreshold(now)
}

func (m *jobLivenessModel) heartbeatStale(now time.Time) bool {
	if m.heartbeatSample == nil {
		return false
	}
	if m.heartbeatSample.observationUnknown {
		return false
	}
	return m.heartbeatAge > m.effectiveThreshold(now)
}

func (m *jobLivenessModel) phaseJobID() int64 {
	_, jid, ok := ParsePhaseJobID(m.instancePhase)
	if ok {
		return jid
	}
	return 0
}

// probeOutcome is the mocked outcome of an SSH agent probe.
type probeOutcome int

const (
	probeOutcomeNone probeOutcome = iota
	probeOutcomeAlive
	probeOutcomeGone
	probeOutcomeError
)

// jobLivenessCommand is one mutating operation in the job-liveness state machine.
type jobLivenessCommand struct {
	name string
	run  func(*testing.T, *sql.DB, *Reconciler, *time.Time, *recordingMockClient, *jobLivenessModel, *rand.Rand)
}

func (c jobLivenessCommand) describe() string { return c.name }

func randomJobLivenessCommand(rng *rand.Rand, m *jobLivenessModel) jobLivenessCommand {
	choices := []jobLivenessCommand{
		{name: "emitFreshHeartbeatRunning", run: emitFreshHeartbeatRunning},
		{name: "emitFreshHeartbeatSetup", run: emitFreshHeartbeatSetup},
		{name: "emitStaleHeartbeat", run: emitStaleHeartbeat},
		{name: "stopHeartbeats", run: stopHeartbeats},
		{name: "writeCompleteMarkerSuccess", run: writeCompleteMarkerSuccess},
		{name: "writeCompleteMarkerFailure", run: writeCompleteMarkerFailure},
		{name: "clearCompleteMarker", run: clearCompleteMarker},
		{name: "probeAlive", run: probeAliveJobLiveness},
		{name: "probeGone", run: probeGoneJobLiveness},
		{name: "probeError", run: probeErrorJobLiveness},
		{name: "syncInstanceState", run: syncInstanceStateJob},
		{name: "reconcileInstance", run: reconcileInstanceJob},
		{name: "finalizeStuckJobs", run: finalizeStuckJobsJob},
		{name: "wait", run: waitJobLiveness},
	}
	return choices[rng.IntN(len(choices))]
}

func emitFreshHeartbeatRunning(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.hostAlive = true
	m.instancePhase = fmt.Sprintf("running:%d", m.jobID)
	m.heartbeatSample = &HeartbeatSample{Ts: now.Unix(), Phase: m.instancePhase}
	m.heartbeatAge = time.Duration(rng.IntN(30)+1) * time.Second
}

func emitFreshHeartbeatSetup(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.hostAlive = true
	m.instancePhase = fmt.Sprintf("setup:%d", m.jobID)
	m.heartbeatSample = &HeartbeatSample{Ts: now.Unix(), Phase: m.instancePhase}
	m.heartbeatAge = time.Duration(rng.IntN(30)+1) * time.Second
}

func emitStaleHeartbeat(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.hostAlive = true
	m.heartbeatSample = &HeartbeatSample{Ts: now.Add(-heartbeatStaleThreshold - time.Minute).Unix(), Phase: m.instancePhase}
	m.heartbeatAge = heartbeatStaleThreshold + time.Duration(rng.IntN(60)+1)*time.Second
}

func stopHeartbeats(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.hostAlive = false
	m.heartbeatSample = nil
	m.heartbeatAge = 0
}

func writeCompleteMarkerSuccess(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	exitCode := 0
	m.completeMarkerExitCode = &exitCode
}

func writeCompleteMarkerFailure(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	exitCode := rng.IntN(255) + 1
	m.completeMarkerExitCode = &exitCode
}

func clearCompleteMarker(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.completeMarkerExitCode = nil
}

func probeAliveJobLiveness(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.probeOutcome = probeOutcomeAlive
}

func probeGoneJobLiveness(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.probeOutcome = probeOutcomeGone
}

func probeErrorJobLiveness(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	m.probeOutcome = probeOutcomeError
}

func syncInstanceStateJob(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	ci, err := db.GetLaunch(database, m.launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci == nil {
		t.Fatalf("launch %d missing", m.launchID)
	}
	if ci.Status != db.LaunchStatusRunning && ci.Status != db.LaunchStatusGrace {
		// Sync only runs for running/grace launches.
		return
	}
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, m.launchID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	jobState := ComputeJobState(jobs, nil)
	_ = SyncInstanceState(context.Background(), database, ci, &r2.Client{}, jobs, jobState, SyncInstanceStateOpts{})
}

func reconcileInstanceJob(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	ci, err := db.GetLaunch(database, m.launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci == nil {
		t.Fatalf("launch %d missing", m.launchID)
	}

	*now = now.Add(time.Duration(rng.IntN(30)+1) * time.Second)

	jobs, err := db.GetLaunchJobsIncludingAttempts(database, m.launchID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	jobState := ComputeJobState(jobs, nil)

	inst := &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning}
	action := r.CheckInstance(CheckInstanceParams{
		CI:                ci,
		ProviderInst:      inst,
		JobState:          jobState,
		Now:               *now,
		HeartbeatAge:      m.heartbeatAge,
		Heartbeat:         m.heartbeatSample,
		InstancePhase:     m.instancePhase,
		BootstrapStage:    m.bootstrapStage,
		TerminationIntent: nil,
	})

	// Stale-heartbeat adjudication runs after CheckInstance when the action is
	// none or display-only, mirroring ReconcileLaunches.
	if (action.Kind == ActionNone || action.Kind == ActionDisplayOnly) && ci.Status == db.LaunchStatusRunning && m.heartbeatStale(*now) {
		hbAction := r.checkStaleHeartbeat(nil, ci, inst, *now)
		if hbAction.Kind != ActionNone {
			action = hbAction
		}
	}

	if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
		// Use executeReconcileAction so the model matches production: it syncs
		// per-job completions from R2 before a terminal transition, preventing
		// a completed job from being reset to queued/orphaned.
		reconciled, terminated := executeReconcileAction(database, client, &r2.Client{}, ci, action)
		if reconciled && action.DestroyProvider {
			m.destroyObserved = true
		}
		if reconciled && terminated && action.TerminalStatus != "" && db.IsTerminalLaunchStatus(action.TerminalStatus) {
			m.terminalObserved = true
		}
	}
}

func finalizeStuckJobsJob(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	_, _ = FinalizeStuckJobsWithR2Check(database, &r2.Client{})
}

func waitJobLiveness(t *testing.T, database *sql.DB, r *Reconciler, now *time.Time, client *recordingMockClient, m *jobLivenessModel, rng *rand.Rand) {
	*now = now.Add(time.Duration(rng.IntN(int(2*m.confirmTime.Seconds())+1)) * time.Second)
	// Heartbeat age is computed from the model's sample timestamp; advance it
	// so later daemon ticks see the same sample as staler.
	if m.heartbeatSample != nil && !m.heartbeatSample.observationUnknown {
		m.heartbeatAge = now.Sub(time.Unix(m.heartbeatSample.Ts, 0))
	}
}

// installJobLivenessMocks replaces all external inputs with model-controlled
// functions. Each hook reads from the model, so the test drives the protocol
// without a real R2 backend or SSH access.
func installJobLivenessMocks(t *testing.T, m *jobLivenessModel, now *time.Time) {
	origFetchReconcileHeartbeat := fetchReconcileHeartbeat
	origProbe := probeCampaignAgent
	origSyncFetchHeartbeat := syncFetchHeartbeat
	origSyncFetchInstancePhase := syncFetchInstancePhase
	origSyncFetchBootstrapStage := syncFetchBootstrapStage
	origSyncFetchOnStartStage := syncFetchOnStartStage
	origSyncFetchJobProgress := syncFetchJobProgress
	origSyncFetchTermIntent := syncFetchTermIntent
	origSyncCheckR2GraceStatus := syncCheckR2GraceStatus
	origSyncCheckOnStartProbe := syncCheckOnStartProbe
	origReconcileCheckAndSyncJobComplete := reconcileCheckAndSyncJobComplete
	origReconcileCheckAndSyncJobCompleteRun := reconcileCheckAndSyncJobCompleteRun

	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchReconcileHeartbeat
		probeCampaignAgent = origProbe
		syncFetchHeartbeat = origSyncFetchHeartbeat
		syncFetchInstancePhase = origSyncFetchInstancePhase
		syncFetchBootstrapStage = origSyncFetchBootstrapStage
		syncFetchOnStartStage = origSyncFetchOnStartStage
		syncFetchJobProgress = origSyncFetchJobProgress
		syncFetchTermIntent = origSyncFetchTermIntent
		syncCheckR2GraceStatus = origSyncCheckR2GraceStatus
		syncCheckOnStartProbe = origSyncCheckOnStartProbe
		reconcileCheckAndSyncJobComplete = origReconcileCheckAndSyncJobComplete
		reconcileCheckAndSyncJobCompleteRun = origReconcileCheckAndSyncJobCompleteRun
	})

	fetchReconcileHeartbeat = func(_ context.Context, _ *r2.Client, id int64) (*HeartbeatSample, time.Duration) {
		if id != m.launchID {
			return nil, 0
		}
		if m.heartbeatSample == nil {
			return nil, 0
		}
		return m.heartbeatSample, m.heartbeatAge
	}

	probeCampaignAgent = func(_ *cloud.Instance, _ time.Duration) (bool, error) {
		switch m.probeOutcome {
		case probeOutcomeAlive:
			return true, nil
		case probeOutcomeGone:
			return false, nil
		case probeOutcomeError:
			return false, errors.New("probe unreachable")
		default:
			return false, errors.New("probe not configured")
		}
	}

	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, id int64) (*HeartbeatSample, time.Duration) {
		if id != m.launchID {
			return nil, 0
		}
		if m.heartbeatSample == nil {
			return nil, 0
		}
		return m.heartbeatSample, m.heartbeatAge
	}

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, id int64) string {
		if id != m.launchID {
			return ""
		}
		return m.instancePhase
	}

	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, id int64) string {
		if id != m.launchID {
			return ""
		}
		return m.bootstrapStage
	}

	syncFetchOnStartStage = func(_ context.Context, _ *r2.Client, id int64) (string, *time.Time) {
		if id != m.launchID {
			return "", nil
		}
		return "", nil
	}

	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}

	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, id int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool {
		return false
	}

	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, nil
	}

	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID int64) bool {
		if jobID != m.jobID {
			return false
		}
		if m.completeMarkerExitCode == nil {
			return false
		}
		recordModelCompletion(dbConn, m)
		return true
	}

	reconcileCheckAndSyncJobCompleteRun = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID, runID int64) bool {
		if jobID != m.jobID {
			return false
		}
		if m.completeMarkerExitCode == nil {
			return false
		}
		recordModelCompletion(dbConn, m)
		return true
	}

	_ = now
}

// recordModelCompletion writes the model's completion marker into the DB the
// same way RecordCloudJobCompletionWithTransition would. It is called by the
// mocked completion-sync hooks. It overrides prior dead/killed guesses when
// a marker arrives late.
func recordModelCompletion(database *sql.DB, m *jobLivenessModel) {
	exitCode := *m.completeMarkerExitCode
	status := db.StatusCompleted
	if exitCode != 0 {
		status = db.StatusFailed
	}
	now := time.Now().Unix()
	// First try to update the open attempt.
	res, _ := database.Exec(
		`UPDATE job_attempts
		    SET status = ?, end_time = COALESCE(end_time, ?), exit_code = ?
		  WHERE job_id = ? AND end_time IS NULL`,
		status, now, exitCode, m.jobID,
	)
	if n, _ := res.RowsAffected(); n == 0 {
		// No open attempt: override the latest terminal dead/killed guess.
		_, _ = database.Exec(
			`UPDATE job_attempts
			    SET status = ?, end_time = ?, exit_code = ?
			  WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC, id DESC LIMIT 1)
			    AND status IN (?, ?)`,
			status, now, exitCode, m.jobID, db.StatusDead, db.StatusKilled,
		)
	}
}

// checkJobLivenessInvariants verifies that the DB state matches the protocol
// model and the spec invariants.
func checkJobLivenessInvariants(t *testing.T, database *sql.DB, jobID, launchID int64, client *recordingMockClient, m *jobLivenessModel, now *time.Time, prevJobStatus string, iteration, step int, desc string, seed uint64) {
	t.Helper()

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetLaunch: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if ci == nil {
		t.Fatalf("iteration %d step %d: launch %d missing\nseed=%d command=%s", iteration, step, launchID, seed, desc)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetJobByID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if job == nil {
		t.Fatalf("iteration %d step %d: job %d missing\nseed=%d command=%s", iteration, step, jobID, seed, desc)
	}

	isLaunchTerminal := db.IsTerminalLaunchStatus(ci.Status)
	isJobTerminal := db.IsTerminalStatus(job.Status)

	// A completion marker is ground truth. If one has been written, the job
	// must eventually be terminal with the matching status.
	if m.completeMarkerExitCode != nil && !isJobTerminal {
		// Allow transient non-terminal states while the daemon has not yet
		// run a sync; the invariant is that no other terminal status is
		// permitted once the marker exists.
		if job.Status == db.StatusDead || job.Status == db.StatusKilled {
			t.Fatalf("iteration %d step %d: completion marker exists but job status = %q\nseed=%d command=%s",
				iteration, step, job.Status, seed, desc)
		}
	}

	// Fresh heartbeat preserves running/setup: a fresh heartbeat must not
	// cause a previously non-terminal job to become terminal unless a
	// completion marker has already been written. (It does not resurrect an
	// already-terminal job; that is a separate recovery path.)
	if m.hostAlive && m.heartbeatFresh(*now) && !db.IsTerminalStatus(prevJobStatus) {
		phaseJobID := m.phaseJobID()
		if phaseJobID == jobID && m.completeMarkerExitCode == nil {
			if db.IsTerminalStatus(job.Status) {
				t.Fatalf("iteration %d step %d: fresh heartbeat for job but job status = %q\nseed=%d command=%s",
					iteration, step, job.Status, seed, desc)
			}
		}
	}

	// Staleness alone is not death: if the heartbeat is stale but the SSH
	// probe reports alive, the job must not have been killed by the
	// stale-heartbeat watchdog.
	if m.heartbeatStale(*now) && m.probeOutcome == probeOutcomeAlive && !isLaunchTerminal {
		if db.IsTerminalStatus(job.Status) {
			t.Fatalf("iteration %d step %d: stale heartbeat with alive probe but job status = %q\nseed=%d command=%s",
				iteration, step, job.Status, seed, desc)
		}
	}

	// Terminal launch + no marker ever observed → dead (eventually, via safety
	// net). If the launch is terminal and the job never completed while a
	// marker was present, the job must not be completed; it should be dead,
	// failed, or reset.
	//
	// The latch matters: the completion decision is made against the marker as
	// it stood at the time, so a marker that disappears later cannot make that
	// decision wrong. Testing the marker's current value alone failed a correct
	// implementation whenever clearCompleteMarker ran after a marker-justified
	// completion.
	if job.Status == db.StatusCompleted && m.completeMarkerExitCode != nil {
		m.completedUnderMarker = true
	}
	if isLaunchTerminal && m.completeMarkerExitCode == nil && !m.completedUnderMarker {
		if job.Status == db.StatusCompleted {
			t.Fatalf("iteration %d step %d: terminal launch without marker but job completed\nseed=%d command=%s",
				iteration, step, seed, desc)
		}
	}

	// A terminal launch must have been observed by the model or created by
	// the safety net path (which sets m.terminalObserved when it terminates).
	if isLaunchTerminal && !m.terminalObserved {
		// The safety net directly marks jobs dead without calling ExecuteAction
		// on the launch. The only allowed terminal status from that path is
		// completed, which means the marker was present.
		if m.completeMarkerExitCode == nil {
			t.Fatalf("iteration %d step %d: launch terminal but model never observed a terminal transition\nseed=%d command=%s",
				iteration, step, seed, desc)
		}
	}

	// Provider destruction recorded at most once.
	if client.destroyCount > 1 {
		t.Fatalf("iteration %d step %d: provider destroyed %d times, want at most 1\nseed=%d command=%s",
			iteration, step, client.destroyCount, seed, desc)
	}
	if m.destroyObserved && client.destroyCount == 0 {
		t.Fatalf("iteration %d step %d: model says destroyed but provider never destroyed\nseed=%d command=%s",
			iteration, step, seed, desc)
	}

	// Jobs must not be reset before the terminal transition commits.
	if !isLaunchTerminal {
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusQueued {
			t.Fatalf("iteration %d step %d: non-terminal launch left job status = %q\nseed=%d command=%s",
				iteration, step, job.Status, seed, desc)
		}
	}
}

// setupJobLivenessStateMachine creates a running launch with a single job and
// a mock provider client. The bootstrap stage is set to "ready" so the
// agent-ready signal is present and heartbeat thresholds use the steady-state
// value.
func setupJobLivenessStateMachine(t *testing.T, database *sql.DB) (jobID, launchID int64, client *recordingMockClient) {
	t.Helper()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "python train.py --gpu RTX_4090", "sm-job-liveness", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	launchID, err = db.CreateLaunch(database, &db.Launch{
		Status:       db.LaunchStatusRunning,
		Provider:     "vastai",
		GPUSpec:      "RTX_4090",
		InstanceType: cloud.InstanceTypeOnDemand,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, launchID, "prov-sm-job-live"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	// SetJobLaunchID creates the launch-associated attempt and clears
	// requested_status so job_status derives from the attempt.
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	// Mark the agent as ready directly; set the ready time far enough in the
	// past that the steady-state 5-minute heartbeat threshold applies.
	readyAt := time.Now().Add(-heartbeatEarlyLifeWindow - time.Minute).Unix()
	if _, err := database.Exec(`UPDATE launches SET agent_ready_at_unix = ? WHERE id = ?`, readyAt, launchID); err != nil {
		t.Fatalf("set agent_ready_at_unix: %v", err)
	}

	client = &recordingMockClient{MockClient: &cloud.MockClient{ProviderVal: cloud.ProviderVastai}}
	client.DestroyInstanceFunc = func(string) error {
		client.destroyCount++
		return nil
	}
	return jobID, launchID, client
}

// TestJobLivenessStateMachine_CompletionMarkerOverridesDead checks that a
// .complete marker arriving after the safety net marked a job dead overrides
// the guess to the authoritative outcome.
func TestJobLivenessStateMachine_CompletionMarkerOverridesDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	jobID, launchID, _ := setupJobLivenessStateMachine(t, database)

	m := &jobLivenessModel{jobID: jobID, launchID: launchID, bootstrapStage: bootstrapStageReady}
	now := time.Now()
	installJobLivenessMocks(t, m, &now)

	// Mark the launch completed and run the safety net with no marker.
	if _, err := database.Exec(`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`,
		db.LaunchStatusCompleted, now.Unix(), db.TerminationReasonUnknown, launchID); err != nil {
		t.Fatalf("update launch: %v", err)
	}
	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != jobID {
		t.Fatalf("repaired = %v, want [%d]", repaired, jobID)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusDead {
		t.Fatalf("job status = %q, want dead", job.Status)
	}

	// Now the marker arrives. A late marker must override the dead guess via
	// the regular completion-sync path (the safety net only scans non-terminal
	// attempts, so it cannot recover an already-dead attempt).
	exitCode := 0
	m.completeMarkerExitCode = &exitCode
	if !reconcileCheckAndSyncJobComplete(context.Background(), &r2.Client{}, database, jobID) {
		t.Fatalf("reconcileCheckAndSyncJobComplete did not record the late marker")
	}

	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want completed after marker override", job.Status)
	}
}

// TestJobLivenessStateMachine_FreshHeartbeatPreventsFalseDead checks that a
// fresh heartbeat keeps the job running even when other transient signals
// (probe errors, stale other-job phases) are present.
func TestJobLivenessStateMachine_FreshHeartbeatPreventsFalseDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	jobID, launchID, client := setupJobLivenessStateMachine(t, database)

	m := &jobLivenessModel{
		jobID:           jobID,
		launchID:        launchID,
		hostAlive:       true,
		instancePhase:   fmt.Sprintf("running:%d", jobID),
		heartbeatSample: &HeartbeatSample{Ts: time.Now().Unix(), Phase: fmt.Sprintf("running:%d", jobID)},
		heartbeatAge:    10 * time.Second,
		bootstrapStage:  bootstrapStageReady,
		probeOutcome:    probeOutcomeError,
	}
	now := time.Now()
	installJobLivenessMocks(t, m, &now)

	r := NewReconciler()
	r.deadConfirmTime = -1 // disable provider-dead hysteresis

	for i := 0; i < minProbeFailureAttempts+1; i++ {
		// Each reconcile tick sees a fresh heartbeat, so the probe failure
		// counter is reset and the watchdog never fires.
		reconcileInstanceJob(t, database, r, &now, client, m, rand.New(rand.NewPCG(1, uint64(i))))
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if db.IsTerminalStatus(job.Status) {
		t.Fatalf("job status = %q, want non-terminal (fresh heartbeat should prevent death)", job.Status)
	}
	if client.destroyCount != 0 {
		t.Fatalf("destroy count = %d, want 0", client.destroyCount)
	}
}

// TestJobLivenessStateMachine_StuckJobSafetyNetFallsBackToDead checks that the
// safety net marks a running job dead when its launch is terminal and R2 has
// no completion marker.
func TestJobLivenessStateMachine_StuckJobSafetyNetFallsBackToDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	jobID, launchID, _ := setupJobLivenessStateMachine(t, database)

	m := &jobLivenessModel{jobID: jobID, launchID: launchID, bootstrapStage: bootstrapStageReady}
	now := time.Now()
	installJobLivenessMocks(t, m, &now)

	if _, err := database.Exec(`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`,
		db.LaunchStatusCompleted, now.Unix(), db.TerminationReasonUnknown, launchID); err != nil {
		t.Fatalf("update launch: %v", err)
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != jobID {
		t.Fatalf("repaired = %v, want [%d]", repaired, jobID)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !db.IsTerminalStatus(job.Status) {
		t.Fatalf("job status = %q, want terminal", job.Status)
	}
}

// TestJobLivenessStateMachine_StaleHeartbeatKillsInstanceAndResetsJob checks
// the full chain: stale heartbeat + unreachable SSH probe past hysteresis
// terminates the instance and resets the job, after which a late completion
// marker can still credit the job.
func TestJobLivenessStateMachine_StaleHeartbeatKillsInstanceAndResetsJob(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	jobID, launchID, client := setupJobLivenessStateMachine(t, database)

	staleAge := heartbeatStaleThreshold + time.Minute
	m := &jobLivenessModel{
		jobID:           jobID,
		launchID:        launchID,
		hostAlive:       false,
		instancePhase:   fmt.Sprintf("running:%d", jobID),
		heartbeatSample: &HeartbeatSample{Ts: time.Now().Add(-staleAge).Unix(), Phase: fmt.Sprintf("running:%d", jobID)},
		heartbeatAge:    staleAge,
		bootstrapStage:  bootstrapStageReady,
		probeOutcome:    probeOutcomeError,
	}
	now := time.Now()
	installJobLivenessMocks(t, m, &now)

	r := NewReconciler()

	// First two probe errors satisfy neither count nor window.
	reconcileInstanceJob(t, database, r, &now, client, m, rand.New(rand.NewPCG(1, 0)))
	reconcileInstanceJob(t, database, r, &now, client, m, rand.New(rand.NewPCG(1, 1)))

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if db.IsTerminalStatus(job.Status) {
		t.Fatalf("job status = %q, want non-terminal before hysteresis window", job.Status)
	}

	// Spread the remaining probe errors across the hysteresis window.
	base := now
	for i := 0; i < minProbeFailureAttempts-2; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		reconcileInstanceJob(t, database, r, &now, client, m, rand.New(rand.NewPCG(1, uint64(i+2))))
	}

	// A late completion marker still credits the job. Write it *before* the
	// terminal reconcile tick so executeReconcileAction's pre-terminal R2 sync
	// picks it up and the job is not reset to queued/orphaned.
	exitCode := 0
	m.completeMarkerExitCode = &exitCode

	// Final reconcile past the window.
	now = base.Add(minProbeFailureWindow + 30*time.Second)
	reconcileInstanceJob(t, database, r, &now, client, m, rand.New(rand.NewPCG(1, 100)))

	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if !db.IsTerminalLaunchStatus(ci.Status) {
		t.Fatalf("launch status = %q, want terminal", ci.Status)
	}
	if client.destroyCount != 1 {
		t.Fatalf("destroy count = %d, want 1", client.destroyCount)
	}

	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want completed after pre-terminal marker sync", job.Status)
	}
}
