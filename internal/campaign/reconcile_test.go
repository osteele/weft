package campaign

import (
	"context"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
)

func TestReconcileCloudInstances_DeadInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job associated with this instance
	_, err = database.Exec(`INSERT INTO jobs (id, host, working_dir, status, command, cloud_instance_id) VALUES (1, '', '/tmp', 'queued', 'python train.py', ?)`, instanceID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	// Record the attempt
	if err := db.InsertJobCloudAttempt(database, 1, instanceID); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}

	// Mock client that reports the instance as dead
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "exited"}, nil
		},
	}

	// Use zero deadConfirmTime so the instance is terminated immediately (no hysteresis wait).
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), deadConfirmTime: -1}
	result, err := r.ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify instance is now failed
	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}

	// Verify job was reset to queued (unplaced)
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM jobs WHERE id = 1`).Scan(&jobStatus); err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if jobStatus != db.StatusQueued {
		t.Errorf("job status = %q, want %q", jobStatus, db.StatusQueued)
	}

	// Verify attempt was closed
	attempts, err := db.GetJobCloudAttempts(database, 1)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Errorf("expected 1 attempt with outcome %q, got %v", db.AttemptOutcomeOrphaned, attempts)
	}
}

func TestReconcileCloudInstances_GraceDetection(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "running"}, nil
		},
	}

	// With nil r2Client, grace detection is skipped — no reconciliation
	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0 (nil r2Client)", result.Reconciled)
	}

	// Instance should still be running
	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusRunning)
	}
}

func TestReconcileCloudInstances_RunningInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports the instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "running"}, nil
		},
	}

	// Reconcile — nothing should change
	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}
}

func TestReconcileCloudInstances_StaleHeartbeatWithoutAgentMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "stale-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	for _, jobID := range []int64{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, host, working_dir, status, command, cloud_instance_id) VALUES (?, '', '/tmp', 'queued', 'python train.py', ?)`, jobID, instanceID); err != nil {
			t.Fatalf("create job %d: %v", jobID, err)
		}
		if err := db.InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
			t.Fatalf("insert attempt for job %d: %v", jobID, err)
		}
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
			status := "running"
			if destroyed {
				status = "destroyed"
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

	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}
	if destroyedID != "stale-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "stale-123")
	}

	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}

	for _, jobID := range []int64{1, 2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.Status != db.StatusQueued {
			t.Fatalf("job %d status = %q, want %q", jobID, job.Status, db.StatusQueued)
		}
		if job.CloudInstanceID != nil {
			t.Fatalf("job %d cloud_instance_id = %v, want nil", jobID, job.CloudInstanceID)
		}
	}
}

func TestReconcileCloudInstances_TerminationIntent_DestroysAndMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "intent-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
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
			TerminalStatus:    db.CloudInstanceStatusFailed,
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
				return &cloud.Instance{ProviderID: id, Status: "destroyed"}, nil
			}
			return &cloud.Instance{ProviderID: id, Status: "running"}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			destroyed = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}
	if destroyedID != "intent-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "intent-123")
	}

	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
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
	if job.CloudInstanceID != nil {
		t.Fatalf("job cloud_instance_id = %v, want nil", job.CloudInstanceID)
	}
}

func TestReconcileCloudInstances_StaleHeartbeatUnreachableProbeRequiresRepeatedFailures(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "probe-err-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
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
			status := "running"
			if destroyed {
				status = "destroyed"
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
		result, err := reconciler.ReconcileCloudInstances(database, []cloud.Client{mockClient}, &r2.Client{})
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", i+1, err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconcile attempt %d = %d, want 0 before threshold", i+1, result.Reconciled)
		}
	}

	result, err := reconciler.ReconcileCloudInstances(database, []cloud.Client{mockClient}, &r2.Client{})
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

func TestReconcileCloudInstances_GraceExpiry_DestroysProvider(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a grace-period instance with an expired deadline
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusGrace,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "99999"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Set grace deadline in the past
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	if err := db.SetCloudInstanceGraceStarted(database, instanceID, pastDeadline); err != nil {
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

	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
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
	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}
}

func TestReconcileCloudInstances_SafetyNet_DestroysLeakedInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "leaked-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Mark it as failed (this sets ended_at to now)
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's still alive, DestroyInstance tracks the call
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "running"}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	// Reconcile — the main loop won't see this instance (it's already failed),
	// but the safety-net pass should catch and destroy it.
	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
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

func TestReconcileCloudInstances_SafetyNet_SkipsAlreadyDestroyed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "dead-456"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's already destroyed
	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "destroyed"}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
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

func TestReconcileCloudInstances_DeadInstanceHysteresis(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetCloudInstanceProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: "exited"}, nil
		},
	}

	// Use a short confirm time so the test doesn't need real wall-clock time.
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), deadConfirmTime: 10 * time.Millisecond}

	// First call: instance appears dead but hasn't been confirmed yet.
	result, err := r.ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (first): %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("first pass: reconciled = %d, want 0 (hysteresis pending)", result.Reconciled)
	}
	ci, _ := db.GetCloudInstance(database, instanceID)
	if ci.Status != db.CloudInstanceStatusRunning {
		t.Errorf("first pass: instance status = %q, want running", ci.Status)
	}

	// Wait for confirm period to elapse.
	time.Sleep(20 * time.Millisecond)

	// Second call: now confirmed dead, should terminate.
	result, err = r.ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (second): %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("second pass: reconciled = %d, want 1", result.Reconciled)
	}
	ci, _ = db.GetCloudInstance(database, instanceID)
	if ci.Status != db.CloudInstanceStatusFailed {
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

func TestReconcileCampaigns_MixedTerminalInstancesBecomeFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	for _, status := range []string{db.CloudInstanceStatusCompleted, db.CloudInstanceStatusFailed} {
		if _, err := db.CreateCloudInstance(database, &db.CloudInstance{
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
