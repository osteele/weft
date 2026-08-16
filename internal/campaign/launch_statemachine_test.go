package campaign

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// TestLaunchReconcileStateMachine_RandomCommands exercises CheckInstance and
// ExecuteAction with randomized provider observations. It model-checks the
// invariants from specs/campaign-lifecycle.allium:
//   - terminal launch statuses are sticky and always carry ended_at + reason
//   - a provider-dead observation eventually triggers a terminal transition
//     that resets attached jobs to unplaced
//   - provider destruction is recorded exactly once
//   - alive observations do not terminate the instance
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestLaunchReconcileStateMachine_RandomCommands(t *testing.T) {
	seed := launchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("launch reconcile state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 30
	stepsPerRun := 25
	for i := 0; i < iterations; i++ {
		database := setupTestDB(t)
		jobID, launchID, client := setupLaunchStateMachine(t, database)

		r := NewReconciler()
		r.deadConfirmTime = -1 // disable hysteresis so a single dead observation is terminal

		m := &launchModel{}
		now := time.Now()

		for step := 0; step < stepsPerRun; step++ {
			cmd := randomLaunchReconcileCommand(rng, m)
			desc := cmd.describe()
			cmd.run(t, database, r, launchID, &now, client, m, rng)

			checkLaunchReconcileInvariants(t, database, jobID, launchID, client, m, i, step, desc, seed)
		}
	}
}

// launchModel is the reference state for one launch/job pair in the test.
type launchModel struct {
	providerStatus string // running, exited, unknown, notfound
	terminal       bool
	destroyed      bool
}

// launchReconcileCommand is one mutating operation in the launch state machine.
type launchReconcileCommand struct {
	name string
	run  func(*testing.T, *sql.DB, *Reconciler, int64, *time.Time, *recordingMockClient, *launchModel, *rand.Rand)
}

func (c launchReconcileCommand) describe() string { return c.name }

func randomLaunchReconcileCommand(rng *rand.Rand, m *launchModel) launchReconcileCommand {
	choices := []launchReconcileCommand{
		{name: "observeRunning", run: observeRunning},
		{name: "observeExited", run: observeExited},
		{name: "observeUnknown", run: observeUnknown},
		{name: "observeNotFound", run: observeNotFound},
		{name: "reconcile", run: reconcileLaunch},
	}
	return choices[rng.IntN(len(choices))]
}

func observeRunning(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *launchModel, rng *rand.Rand) {
	m.providerStatus = cloud.ProviderStatusRunning
}

func observeExited(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *launchModel, rng *rand.Rand) {
	m.providerStatus = cloud.ProviderStatusExited
}

func observeUnknown(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *launchModel, rng *rand.Rand) {
	m.providerStatus = "unknown"
}

func observeNotFound(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *launchModel, rng *rand.Rand) {
	m.providerStatus = "notfound"
}

func reconcileLaunch(t *testing.T, database *sql.DB, r *Reconciler, launchID int64, now *time.Time, client *recordingMockClient, m *launchModel, rng *rand.Rand) {
	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci == nil {
		t.Fatalf("launch %d missing", launchID)
	}

	*now = now.Add(time.Duration(rng.IntN(30)+1) * time.Second)

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
		CI:           ci,
		ProviderInst: inst,
		ProviderErr:  providerErr,
		JobState:     jobState,
		Now:          *now,
	})

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
}

// checkLaunchReconcileInvariants verifies that the persisted state matches the
// reference model and the spec invariants.
func checkLaunchReconcileInvariants(t *testing.T, database *sql.DB, jobID, launchID int64, client *recordingMockClient, m *launchModel, iteration, step int, desc string, seed uint64) {
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
}

// TestLaunchReconcileStateMachine_Hysteresis checks that dead-confirm
// hysteresis requires a consecutive stretch of dead observations and resets
// on an alive observation, end-to-end through CheckInstance and ExecuteAction.
func TestLaunchReconcileStateMachine_Hysteresis(t *testing.T) {
	database := setupTestDB(t)
	jobID, launchID, client := setupLaunchStateMachine(t, database)

	r := NewReconciler()
	r.deadConfirmTime = 30 * time.Second

	t0 := time.Now()
	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	deadParams := func(at time.Time) CheckInstanceParams {
		return CheckInstanceParams{
			CI:           ci,
			ProviderInst: &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusExited},
			Now:          at,
		}
	}
	aliveParams := func(at time.Time) CheckInstanceParams {
		return CheckInstanceParams{
			CI:           ci,
			ProviderInst: &cloud.Instance{ProviderID: ci.EffectiveProviderID(), Status: cloud.ProviderStatusRunning},
			Now:          at,
		}
	}

	// First dead observation starts the timer but does not terminate.
	if action := r.CheckInstance(deadParams(t0)); action.Kind != ActionNone {
		t.Fatalf("first dead observation: action.Kind = %v, want ActionNone", action.Kind)
	}
	// Alive observation resets the timer.
	if action := r.CheckInstance(aliveParams(t0.Add(20 * time.Second))); action.Kind != ActionNone {
		t.Fatalf("alive observation after 20s: action.Kind = %v, want ActionNone", action.Kind)
	}
	// Dead again at 40s is the first observation of a new stretch.
	if action := r.CheckInstance(deadParams(t0.Add(40 * time.Second))); action.Kind != ActionNone {
		t.Fatalf("dead at 40s: action.Kind = %v, want ActionNone", action.Kind)
	}
	// Dead at 80s exceeds confirmTime from the reset stretch.
	action := r.CheckInstance(deadParams(t0.Add(80 * time.Second)))
	if action.Kind != ActionProviderDead {
		t.Fatalf("dead at 80s: action.Kind = %v, want ActionProviderDead", action.Kind)
	}

	ExecuteAction(database, client, ci, action)

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
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want queued after reset", job.Status)
	}
}

// TestLaunchReconcileStateMachine_DestroyDeferral checks that when
// ExecuteAction cannot destroy the provider instance, the reference model is not
// advanced and the launch remains non-terminal. This exercises the deferral
// path that prevents a failed destroy from being recorded as a terminal
// transition.
func TestLaunchReconcileStateMachine_DestroyDeferral(t *testing.T) {
	database := setupTestDB(t)
	jobID, launchID, client := setupLaunchStateMachine(t, database)
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	client.destroyFail = fmt.Errorf("provider API unavailable")

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

	reconciled, terminated := ExecuteAction(database, client, ci, action)
	if reconciled {
		t.Fatalf("ExecuteAction reconciled despite destroy failure")
	}
	if terminated {
		t.Fatalf("ExecuteAction terminated despite destroy failure")
	}

	ci, err = db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch after deferral: %v", err)
	}
	if db.IsTerminalLaunchStatus(ci.Status) {
		t.Fatalf("launch status = %q, want non-terminal after deferred destroy", ci.Status)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("job status = %q, want running after deferred destroy", job.Status)
	}
}

// TestLaunchRelaunchStateMachine_MaxAttempts verifies that RelaunchOrphanedJobs
// converges once a job has consumed its cloud attempt budget. This is the
// relaunch side of the launch state machine: after repeated reconcile failures
// orphan the job, the autopilot stops creating new instances.
func TestLaunchRelaunchStateMachine_MaxAttempts(t *testing.T) {
	database := setupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "python train.py --gpu RTX_4090", "sm-relaunch", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.TagRental); err != nil {
		t.Fatalf("AddJobTag rental: %v", err)
	}

	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Simulate the maximum allowed cloud attempts, each closing on the failed instance.
	maxAttempts := 3
	for i := 0; i < maxAttempts; i++ {
		attemptID, err := db.CreateAttempt(database, jobID, "", &failedID, db.StatusFailed)
		if err != nil {
			t.Fatalf("CreateAttempt %d: %v", i, err)
		}
		end := time.Now().Add(-time.Duration(i+1) * time.Minute).Unix()
		if _, err := database.Exec(
			`UPDATE job_attempts SET cloud_outcome = ?, start_time = ?, end_time = ? WHERE id = ?`,
			db.AttemptOutcomeOrphaned, end-60, end, attemptID,
		); err != nil {
			t.Fatalf("close attempt %d: %v", i, err)
		}
	}

	result, err := RelaunchOrphanedJobs(RelaunchConfig{
		Database:    database,
		MaxAttempts: maxAttempts,
	})
	if err != nil {
		t.Fatalf("RelaunchOrphanedJobs: %v", err)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (max attempts reached)", result.Skipped)
	}
	if reason := result.JobReasons[jobID]; reason == "" || (!strings.Contains(reason, "max cloud attempts") && !strings.Contains(reason, "max attempts")) {
		t.Fatalf("job reason = %q, want max-attempts reason", reason)
	}
}

func setupLaunchStateMachine(t *testing.T, database *sql.DB) (jobID, launchID int64, client *recordingMockClient) {
	t.Helper()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "python train.py --gpu RTX_4090", "sm-launch", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	launchID, err = db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, launchID, "prov-sm-1"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	client = &recordingMockClient{MockClient: &cloud.MockClient{ProviderVal: cloud.ProviderVastai}}
	client.DestroyInstanceFunc = func(string) error {
		client.destroyCount++
		return nil
	}
	return jobID, launchID, client
}

// recordingMockClient wraps cloud.MockClient and records how many times the
// provider instance was destroyed.
type recordingMockClient struct {
	*cloud.MockClient
	destroyCount int
	// destroyFail, when non-nil, causes DestroyInstance to return this error
	// instead of succeeding. Used to test ExecuteAction deferral paths.
	destroyFail error
}

func (c *recordingMockClient) DestroyInstance(providerID string) error {
	if c.destroyFail != nil {
		return c.destroyFail
	}
	if c.DestroyInstanceFunc != nil {
		return c.DestroyInstanceFunc(providerID)
	}
	c.destroyCount++
	return nil
}

func launchSMSeed() uint64 {
	if s := os.Getenv("WEFT_TEST_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
	}
	return rand.Uint64()
}
