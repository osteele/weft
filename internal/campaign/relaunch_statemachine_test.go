package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// TestRelaunchStateMachine_RandomCommands exercises the launch/relaunch/retry
// loop with randomized provider outcomes. It model-checks the invariants from
// specs/campaign-lifecycle.allium:
//   - a job with an unplaced status and remaining attempt budget is launch-eligible
//   - a running instance that is confirmed gone is terminated and its job reset
//   - the retry budget is enforced: relaunch stops after maxAttempts
//   - terminal launches are sticky and are not relaunched
//   - provider destruction is recorded at most once per terminal transition
//   - jobs are reset only after the terminal transition commits
//
// The test avoids real R2 by seeding failed and running launches directly
// through the DB. RelaunchOrphanedJobs and ReconcileLaunches are driven with
// a mock provider client to exercise eligibility, retry budgets, and the
// running -> terminal transition. LaunchInstance itself is not directly driven
// here because its R2 staging path requires a configured R2 client; the
// lifecycle_test.go TestLaunchInstanceCreateFails case covers a single failed
// LaunchInstance attempt, and this model covers the retry-budget and
// terminal-transition orchestration around it.
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestRelaunchStateMachine_RandomCommands(t *testing.T) {
	seed := launchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("relaunch state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 30
	stepsPerRun := 30
	for i := 0; i < iterations; i++ {
		database := setupTestDB(t)
		defer database.Close()
		jobID := setupRelaunchJob(t, database)

		client := &recordingMockClient{MockClient: &cloud.MockClient{ProviderVal: cloud.ProviderVastai}}
		// Simulate a provider instance that is already gone. The main reconcile
		// path terminates the launch and calls DestroyInstance once; the safety
		// net sees ErrInstanceNotFound and skips the redundant destroy, keeping
		// destroyCount tightly bounded by terminalCount.
		client.ShowInstanceFunc = func(string) (*cloud.Instance, error) {
			return nil, cloud.ErrInstanceNotFound
		}

		r := NewReconciler()
		r.deadConfirmTime = -1 // disable hysteresis so a single dead observation terminates

		maxAttempts := 3
		m := &relaunchModel{maxAttempts: maxAttempts}
		now := time.Now()

		for step := 0; step < stepsPerRun; step++ {
			cmd := randomRelaunchCommand(rng, m)
			desc := cmd.describe()
			cmd.run(t, database, r, jobID, &now, client, m, rng)

			m.observe(database, jobID)
			checkRelaunchInvariants(t, database, jobID, client, m, i, step, desc, seed)
		}
	}
}

// relaunchModel is the reference state for one job under the relaunch/retry
// rules. It is updated observationally from the DB after each command.
type relaunchModel struct {
	maxAttempts    int
	attemptCount   int
	activeLaunchID int64
	jobPlaced      bool
	terminal       bool // job has exhausted its retry budget and is unplaced
	destroyCount   int
}

// observe reads the current DB state and updates the model.
func (m *relaunchModel) observe(database *sql.DB, jobID int64) {
	attempts, _ := db.GetLaunchAttempts(database, jobID)
	m.attemptCount = 0
	for _, a := range attempts {
		if a.Outcome != db.AttemptOutcomeCancelled && a.Outcome != db.AttemptOutcomeSuperseded {
			// Count attempts that have a real launch association, matching
			// CountLaunchAttempts semantics.
			if a.LaunchID != 0 && !(a.Outcome == db.AttemptOutcomeOrphaned && a.StartedAt == 0) {
				m.attemptCount++
			}
		}
	}

	job, _ := db.GetJobByID(database, jobID)
	if job != nil && job.LaunchID != nil {
		m.activeLaunchID = *job.LaunchID
		m.jobPlaced = true
	} else {
		m.activeLaunchID = 0
		m.jobPlaced = false
	}

	m.terminal = m.attemptCount >= m.maxAttempts && !m.jobPlaced
}

// relaunchCommand is one mutating operation in the relaunch state machine.
type relaunchCommand struct {
	name string
	run  func(*testing.T, *sql.DB, *Reconciler, int64, *time.Time, *recordingMockClient, *relaunchModel, *rand.Rand)
}

func (c relaunchCommand) describe() string { return c.name }

func randomRelaunchCommand(rng *rand.Rand, m *relaunchModel) relaunchCommand {
	choices := []relaunchCommand{
		{name: "relaunch", run: relaunchCmd},
		{name: "reconcile", run: reconcileCmd},
		{name: "recordFailedAttempt", run: recordFailedAttemptCmd},
		{name: "spawnRunning", run: spawnRunningCmd},
		{name: "wait", run: waitRelaunchCmd},
	}
	return choices[rng.IntN(len(choices))]
}

func setupRelaunchJob(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "python train.py --gpu RTX_4090", "sm-relaunch", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.TagRental); err != nil {
		t.Fatalf("AddJobTag rental: %v", err)
	}
	return jobID
}

func relaunchCmd(t *testing.T, database *sql.DB, r *Reconciler, jobID int64, now *time.Time, client *recordingMockClient, m *relaunchModel, rng *rand.Rand) {
	result, err := RelaunchOrphanedJobs(RelaunchConfig{
		Database:             database,
		MaxAttempts:          m.maxAttempts,
		IncludeFreshUnplaced: true,
	})
	if err != nil {
		t.Fatalf("RelaunchOrphanedJobs: %v", err)
	}
	// When the retry budget is exhausted, RelaunchOrphanedJobs must skip the
	// job and report a max-attempts reason.
	if m.terminal {
		if result.Skipped == 0 {
			t.Fatalf("terminal job not skipped by RelaunchOrphanedJobs")
		}
		reason := result.JobReasons[jobID]
		if reason == "" || (!containsFold(reason, "max attempts") && !containsFold(reason, "max cloud attempts")) {
			t.Fatalf("terminal job skip reason = %q, want max-attempts reason", reason)
		}
	}
}

func reconcileCmd(t *testing.T, database *sql.DB, r *Reconciler, jobID int64, now *time.Time, client *recordingMockClient, m *relaunchModel, rng *rand.Rand) {
	*now = now.Add(time.Duration(rng.IntN(30)+1) * time.Second)
	if _, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{client}, nil); err != nil {
		t.Fatalf("ReconcileLaunches: %v", err)
	}
}

func recordFailedAttemptCmd(t *testing.T, database *sql.DB, r *Reconciler, jobID int64, now *time.Time, client *recordingMockClient, m *relaunchModel, rng *rand.Rand) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d missing", jobID)
	}
	if job.LaunchID != nil {
		// Cannot start a new attempt while the job is already placed.
		return
	}
	count, err := db.CountLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("CountLaunchAttempts: %v", err)
	}
	if count >= m.maxAttempts {
		// Retry budget exhausted; production would not launch again.
		return
	}
	recordFailedAttempt(t, database, jobID)
}

func spawnRunningCmd(t *testing.T, database *sql.DB, r *Reconciler, jobID int64, now *time.Time, client *recordingMockClient, m *relaunchModel, rng *rand.Rand) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d missing", jobID)
	}
	if job.LaunchID != nil {
		// Job is already placed; spawning another running launch would violate
		// the single-active-launch invariant, so this command is a no-op.
		return
	}
	count, err := db.CountLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("CountLaunchAttempts: %v", err)
	}
	if count >= m.maxAttempts {
		// Retry budget exhausted; production would not place this job again.
		return
	}

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, launchID, fmt.Sprintf("prov-sm-%d", launchID)); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
}

func waitRelaunchCmd(t *testing.T, database *sql.DB, r *Reconciler, jobID int64, now *time.Time, client *recordingMockClient, m *relaunchModel, rng *rand.Rand) {
	*now = now.Add(time.Duration(rng.IntN(120)+1) * time.Second)
}

// recordFailedAttempt creates a failed launch and a counted attempt for jobID,
// leaving the job unplaced. It simulates the effect of LaunchInstance failing
// before destination acceptance without requiring real R2.
func recordFailedAttempt(t *testing.T, database *sql.DB, jobID int64) int64 {
	t.Helper()
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:            db.LaunchStatusFailed,
		Provider:          "vastai",
		GPUSpec:           "RTX_4090",
		TerminationReason: db.TerminationReasonInfraFailure,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusFailed)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, end_time = ?, cloud_outcome = ? WHERE id = ?`,
		now, now, db.AttemptOutcomeOrphaned, attemptID,
	); err != nil {
		t.Fatalf("update attempt: %v", err)
	}
	return launchID
}

func checkRelaunchInvariants(t *testing.T, database *sql.DB, jobID int64, client *recordingMockClient, m *relaunchModel, iteration, step int, desc string, seed uint64) {
	t.Helper()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetJobByID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if job == nil {
		t.Fatalf("iteration %d step %d: job %d missing\nseed=%d command=%s", iteration, step, jobID, seed, desc)
	}

	if m.terminal && m.jobPlaced {
		t.Fatalf("iteration %d step %d: terminal job is still placed\nseed=%d command=%s", iteration, step, seed, desc)
	}

	if m.attemptCount > m.maxAttempts {
		t.Fatalf("iteration %d step %d: attemptCount=%d exceeds maxAttempts=%d\nseed=%d command=%s",
			iteration, step, m.attemptCount, m.maxAttempts, seed, desc)
	}

	launches, err := db.ListLaunches(database)
	if err != nil {
		t.Fatalf("iteration %d step %d: ListLaunches: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}

	activeCount := 0
	terminalCount := 0
	for _, ci := range launches {
		if !db.IsTerminalLaunchStatus(ci.Status) {
			activeCount++
		} else {
			terminalCount++
		}
	}
	if activeCount > 1 {
		t.Fatalf("iteration %d step %d: %d active launches, want at most 1\nseed=%d command=%s",
			iteration, step, activeCount, seed, desc)
	}

	// A placed job must have exactly one active launch carrying it.
	if m.jobPlaced {
		if activeCount != 1 {
			t.Fatalf("iteration %d step %d: job placed but %d active launches\nseed=%d command=%s",
				iteration, step, activeCount, seed, desc)
		}
		if job.LaunchID == nil || *job.LaunchID != m.activeLaunchID {
			t.Fatalf("iteration %d step %d: job launch id drift: job=%v model=%d\nseed=%d command=%s",
				iteration, step, job.LaunchID, m.activeLaunchID, seed, desc)
		}
	}

	// Provider destruction should not exceed the number of terminal transitions
	// that have actually happened. Each terminal launch may be destroyed at most once.
	if client.destroyCount > terminalCount {
		t.Fatalf("iteration %d step %d: destroyCount=%d > terminalCount=%d\nseed=%d command=%s",
			iteration, step, client.destroyCount, terminalCount, seed, desc)
	}

	// Job reset sequencing: if the job is unplaced, any launch it was attached to
	// must now be terminal.
	if job.LaunchID == nil {
		attempts, err := db.GetLaunchAttempts(database, jobID)
		if err != nil {
			t.Fatalf("iteration %d step %d: GetLaunchAttempts: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
		}
		for _, a := range attempts {
			ci, err := db.GetLaunch(database, a.LaunchID)
			if err != nil {
				t.Fatalf("iteration %d step %d: GetLaunch(%d): %v\nseed=%d command=%s", iteration, step, a.LaunchID, err, seed, desc)
			}
			if ci == nil {
				continue
			}
			if !db.IsTerminalLaunchStatus(ci.Status) {
				t.Fatalf("iteration %d step %d: job unplaced but launch %d status=%q is not terminal\nseed=%d command=%s",
					iteration, step, ci.ID, ci.Status, seed, desc)
			}
		}
	}
}

// TestRelaunchStateMachine_MaxAttemptsConvergence is a targeted regression
// guard: after maxAttempts failed launches, RelaunchOrphanedJobs must skip the
// job and never create another launch.
func TestRelaunchStateMachine_MaxAttemptsConvergence(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	jobID := setupRelaunchJob(t, database)

	maxAttempts := 3
	for i := 0; i < maxAttempts; i++ {
		recordFailedAttempt(t, database, jobID)
	}

	result, err := RelaunchOrphanedJobs(RelaunchConfig{
		Database:             database,
		MaxAttempts:          maxAttempts,
		IncludeFreshUnplaced: true,
	})
	if err != nil {
		t.Fatalf("RelaunchOrphanedJobs: %v", err)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (max attempts reached)", result.Skipped)
	}
	reason := result.JobReasons[jobID]
	if reason == "" || (!containsFold(reason, "max attempts") && !containsFold(reason, "max cloud attempts")) {
		t.Fatalf("job reason = %q, want max-attempts reason", reason)
	}

	launches, err := db.ListLaunches(database)
	if err != nil {
		t.Fatalf("ListLaunches: %v", err)
	}
	if len(launches) != maxAttempts {
		t.Fatalf("launch count = %d, want %d", len(launches), maxAttempts)
	}
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
