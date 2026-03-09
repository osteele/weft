package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
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
	_, err = database.Exec(`INSERT INTO jobs (id, status, command, cloud_instance_id) VALUES (1, 'queued', 'python train.py', ?)`, instanceID)
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

	// Reconcile
	n, err := ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled = %d, want 1", n)
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
	n, err := ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled = %d, want 0 (nil r2Client)", n)
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
	n, err := ReconcileCloudInstances(database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled = %d, want 0", n)
	}
}
