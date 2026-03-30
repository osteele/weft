package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
)

func TestReconcileLaunches_DeadInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusQueued, instanceID)

	// Mock client that reports the instance as dead
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Use zero deadConfirmTime so the instance is terminated immediately (no hysteresis wait).
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: -1}
	result, err := r.ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify instance is now failed
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	// Verify job was reset to queued (unplaced)
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_status WHERE id = 1`).Scan(&jobStatus); err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if jobStatus != db.StatusQueued {
		t.Errorf("job status = %q, want %q", jobStatus, db.StatusQueued)
	}

	// Verify attempt was closed
	attempts, err := db.GetLaunchAttempts(database, 1)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Errorf("expected 1 attempt with outcome %q, got %v", db.AttemptOutcomeOrphaned, attempts)
	}
}

func TestReconcileLaunches_GraceDetection(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// With nil r2Client, grace detection is skipped — no reconciliation
	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0 (nil r2Client)", result.Reconciled)
	}

	// Instance should still be running
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileLaunches_GraceSyncsJobCompletions(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job in "running" status associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// Override grace check to simulate R2 grace marker detection
	origGrace := reconcileCheckR2GraceStatus
	t.Cleanup(func() { reconcileCheckR2GraceStatus = origGrace })
	reconcileCheckR2GraceStatus = func(_ *r2.Client, ci *db.Launch, dbConn *sql.DB) bool {
		_ = db.SetLaunchGraceStarted(dbConn, ci.ID, time.Now().Add(5*time.Minute).Unix())
		return true
	}

	// Override job completion sync to track which jobs were synced
	var syncedJobIDs []int64
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, jobID int64) bool {
		syncedJobIDs = append(syncedJobIDs, jobID)
		return true
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify instance transitioned to grace
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusGrace {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusGrace)
	}

	// Verify job completion sync was called for the running job
	if len(syncedJobIDs) != 1 || syncedJobIDs[0] != 1 {
		t.Errorf("synced job IDs = %v, want [1]", syncedJobIDs)
	}
}

func TestReconcileLaunches_TerminalTransitionSyncsJobCompletions(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job in "running" status associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	// Provider reports instance as exited (dead)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Override job completion sync to track calls and mark job completed in DB
	var syncedJobIDs []int64
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID int64) bool {
		syncedJobIDs = append(syncedJobIDs, jobID)
		// Simulate recording the completion so ExecuteAction's ResetLaunchJobs skips it
		dbConn.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
			db.StatusCompleted, time.Now().Unix(), jobID)
		return true
	}

	rec := NewReconciler()
	rec.deadConfirmTime = -1 // skip hysteresis
	result, err := rec.ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify job completion sync was called for the running job
	if len(syncedJobIDs) != 1 || syncedJobIDs[0] != 1 {
		t.Errorf("synced job IDs = %v, want [1]", syncedJobIDs)
	}

	// Verify the job was NOT re-queued (it was marked completed by the mock sync)
	var jobStatus string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&jobStatus)
	if jobStatus != string(db.StatusCompleted) {
		t.Errorf("job status = %q, want %q (should not be re-queued)", jobStatus, db.StatusCompleted)
	}
}

func TestReconcileLaunches_SyncFailureOrphansJob(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "H100_SXM",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "33840670"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create two jobs: one completed, one still running
	for _, id := range []int{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, id); err != nil {
			t.Fatalf("create job %d: %v", id, err)
		}
	}
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ?, end_time = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusCompleted, instanceID, time.Now().Unix())
	database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = 2 AND end_time IS NULL`,
		db.StatusRunning, instanceID)

	// Provider reports instance as exited (dead)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Sync fails for job 2 (simulates the race: results not uploaded yet)
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, jobID int64) bool {
		return false // results not available
	}

	rec := NewReconciler()
	rec.deadConfirmTime = -1 // skip hysteresis
	_, err = rec.ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Job 2 should be orphaned (reset to queued with orphaned outcome)
	var jobStatus string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 2 ORDER BY id DESC LIMIT 1`).Scan(&jobStatus)
	if jobStatus != string(db.StatusQueued) {
		t.Errorf("job 2 status = %q, want %q (should be re-queued after orphaning)", jobStatus, db.StatusQueued)
	}

	// Check the orphaned attempt outcome
	attempts, _ := db.GetLaunchAttempts(database, 2)
	var hasOrphaned bool
	for _, a := range attempts {
		if a.Outcome == db.AttemptOutcomeOrphaned {
			hasOrphaned = true
		}
	}
	if !hasOrphaned {
		t.Errorf("expected an attempt with orphaned outcome for job 2")
	}
}

func TestReconcileLaunches_RunningInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports the instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// Reconcile — nothing should change
	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}
}

func TestReconcileLaunches_StaleHeartbeatWithoutAgentMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "stale-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	for _, jobID := range []int64{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, jobID); err != nil {
			t.Fatalf("create job %d: %v", jobID, err)
		}
		database.Exec(`UPDATE job_attempts SET status = ?, launch_id = ? WHERE job_id = ? AND end_time IS NULL`,
			db.StatusQueued, instanceID, jobID)
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})

	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, nil
	}

	var destroyedID string
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			status := cloud.ProviderStatusRunning
			if destroyed {
				status = cloud.ProviderStatusDestroyed
			}
			return &cloud.Instance{
				ProviderID: id,
				Status:     status,
				SSHHost:    "ssh6.vast.ai",
				SSHPort:    22,
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			destroyed = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}
	if destroyedID != "stale-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "stale-123")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	for _, jobID := range []int64{1, 2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.Status != db.StatusQueued {
			t.Fatalf("job %d status = %q, want %q", jobID, job.Status, db.StatusQueued)
		}
	}
}

func TestReconcileLaunches_TerminationIntent_DestroysAndMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "intent-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("assign job: %v", err)
	}

	origFetchIntent := fetchReconcileTerminationIntent
	t.Cleanup(func() {
		fetchReconcileTerminationIntent = origFetchIntent
	})
	fetchReconcileTerminationIntent = func(_ context.Context, _ *r2.Client, id int64) (*instanceintent.Marker, error) {
		if id != instanceID {
			return nil, nil
		}
		return &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonDiskFull,
			Phase:             "disk-full:246",
			JobID:             246,
			RequestedAtUnix:   time.Now().Unix(),
		}, nil
	}

	var destroyedID string
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			if destroyed {
				return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusDestroyed}, nil
			}
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			destroyed = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 2 {
		t.Fatalf("reconciled = %d, want 2", result.Reconciled)
	}
	if destroyedID != "intent-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "intent-123")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonDiskFull)
	}
	if ci.TerminationRequestedAt == nil || *ci.TerminationRequestedAt == 0 {
		t.Fatalf("termination requested at = %v, want non-nil", ci.TerminationRequestedAt)
	}
	if ci.TerminationIntent == nil || ci.TerminationIntent.TerminationReason != db.TerminationReasonDiskFull {
		t.Fatalf("termination intent = %+v, want disk_full marker", ci.TerminationIntent)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}
}

func TestReconcileLaunches_SafetyNetMarksDestroyConfirmedWhenProviderGone(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusCompleted, db.TerminationReasonCompleted); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "intent-gone-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.UpdateLaunchTerminationIntent(database, instanceID, &instanceintent.Marker{
		TerminalStatus:       db.LaunchStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-30 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-25 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("set termination intent: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return nil, cloud.ErrInstanceNotFound
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.TerminationIntent == nil || ci.TerminationIntent.DestroySucceededAtUnix == 0 {
		t.Fatalf("termination intent = %+v, want destroy_succeeded_at_unix set", ci.TerminationIntent)
	}
}

func TestReconcileLaunches_StaleHeartbeatUnreachableProbeRequiresRepeatedFailures(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "probe-err-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("assign job: %v", err)
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})

	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, context.DeadlineExceeded
	}

	var destroyCalls int
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			status := cloud.ProviderStatusRunning
			if destroyed {
				status = cloud.ProviderStatusDestroyed
			}
			return &cloud.Instance{ProviderID: id, Status: status}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalls++
			destroyed = true
			return nil
		},
	}

	reconciler := NewReconciler()
	for i := 0; i < minProbeFailureAttempts-1; i++ {
		result, err := reconciler.ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", i+1, err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconcile attempt %d = %d, want 0 before threshold", i+1, result.Reconciled)
		}
	}

	result, err := reconciler.ReconcileLaunches(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile final attempt: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconcile final attempt = %d, want 1", result.Reconciled)
	}
	if destroyCalls != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyCalls)
	}
}

func TestReconcileLaunches_GraceExpiry_DestroysProvider(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a grace-period instance with an expired deadline
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusGrace,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "99999"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Set grace deadline in the past
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	if err := db.SetLaunchGraceStarted(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set grace started: %v", err)
	}

	// Track whether DestroyInstance was called
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify DestroyInstance was called with the correct provider ID
	if destroyedID != "99999" {
		t.Errorf("DestroyInstance called with %q, want %q", destroyedID, "99999")
	}

	// Verify instance is now failed
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
}

func TestReconcileLaunches_SafetyNet_DestroysLeakedInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "leaked-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Mark it as failed (this sets ended_at to now)
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's still alive, DestroyInstance tracks the call
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	// Reconcile — the main loop won't see this instance (it's already failed),
	// but the safety-net pass should catch and destroy it.
	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1 (safety-net destroy)", result.Reconciled)
	}
	if destroyedID != "leaked-123" {
		t.Errorf("DestroyInstance called with %q, want %q", destroyedID, "leaked-123")
	}
}

func TestReconcileLaunches_SafetyNet_SkipsAlreadyDestroyed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "dead-456"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's already destroyed
	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusDestroyed}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0 (already destroyed)", result.Reconciled)
	}
	if destroyCalled {
		t.Error("DestroyInstance should not be called for already-destroyed instances")
	}
}

func TestReconcileLaunches_DeadInstanceHysteresis(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Use a short confirm time so the test doesn't need real wall-clock time.
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: 10 * time.Millisecond}

	// First call: instance appears dead but hasn't been confirmed yet.
	result, err := r.ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (first): %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("first pass: reconciled = %d, want 0 (hysteresis pending)", result.Reconciled)
	}
	ci, _ := db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("first pass: instance status = %q, want running", ci.Status)
	}

	// Wait for confirm period to elapse.
	time.Sleep(20 * time.Millisecond)

	// Second call: now confirmed dead, should terminate.
	result, err = r.ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (second): %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("second pass: reconciled = %d, want 1", result.Reconciled)
	}
	ci, _ = db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("second pass: instance status = %q, want failed", ci.Status)
	}
}

func TestReconcileCampaigns_RunningCampaignWithNoInstancesBecomesFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns: %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("completed campaigns = %d, want 1", len(completed))
	}
	if completed[0].ID != campaignID {
		t.Fatalf("completed campaign ID = %d, want %d", completed[0].ID, campaignID)
	}
	if completed[0].Status != db.CampaignStatusFailed {
		t.Fatalf("completed campaign status = %q, want %q", completed[0].Status, db.CampaignStatusFailed)
	}

	got, err := db.GetCampaign(database, campaignID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got == nil {
		t.Fatalf("campaign %d not found", campaignID)
	}
	if got.Status != db.CampaignStatusFailed {
		t.Fatalf("campaign status = %q, want %q", got.Status, db.CampaignStatusFailed)
	}
	if got.EndedAt == nil {
		t.Fatal("ended_at was not set for failed campaign")
	}
}

func TestReconcileLaunches_TransientAPIError_SkipsInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "transient-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that returns a transient error (not ErrInstanceNotFound)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return nil, fmt.Errorf("API timeout: connection reset")
		},
	}

	// Even with zero hysteresis, transient errors should NOT mark instance dead
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: -1}

	// Run multiple reconciliation passes — instance must remain running
	for i := 0; i < 5; i++ {
		result, err := r.ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
		if err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconcile pass %d: reconciled = %d, want 0 (transient error should skip)", i+1, result.Reconciled)
		}
	}

	// Verify instance is still running
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q (transient errors must not kill instance)", ci.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileLaunches_BatchFetch_UsesListAllInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "batch-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	var showCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "batch-123", Status: cloud.ProviderStatusRunning},
			}, nil
		},
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			showCalls++
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}
	if showCalls != 0 {
		t.Errorf("ShowInstance called %d times, want 0 (should use batch)", showCalls)
	}
}

func TestReconcileCampaigns_MixedTerminalInstancesBecomeFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	for _, status := range []string{db.LaunchStatusCompleted, db.LaunchStatusFailed} {
		if _, err := db.CreateLaunch(database, &db.Launch{
			CampaignID: &campaignID,
			Status:     status,
			Provider:   "vastai",
			GPUSpec:    "RTX_4090",
		}); err != nil {
			t.Fatalf("create cloud instance(%s): %v", status, err)
		}
	}

	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns: %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("completed campaigns = %d, want 1", len(completed))
	}
	if completed[0].Status != db.CampaignStatusFailed {
		t.Fatalf("campaign status = %q, want %q", completed[0].Status, db.CampaignStatusFailed)
	}
}
