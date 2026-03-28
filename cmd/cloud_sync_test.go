package cmd

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestSyncCloudStateWithClients_ReconcilesProviderState(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "dead-123"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	launchedAt := time.Now().Add(-2 * time.Minute).Unix()
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	var destroyed string
	destroyedOnce := false
	result := syncCloudStateWithClients(&config.Config{}, database, campaign.NewReconciler(), []cloud.Client{&cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			if destroyedOnce {
				return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusDestroyed}, nil
			}
			return &cloud.Instance{ProviderID: id, Status: ""}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyed = id
			destroyedOnce = true
			return nil
		},
	}}, nil, false)

	if result.ReconcileResult == nil || result.ReconcileResult.Reconciled != 1 {
		t.Fatalf("reconciled = %+v, want 1 provider reconciliation", result.ReconcileResult)
	}
	if destroyed != "dead-123" {
		t.Fatalf("destroyed provider ID = %q, want %q", destroyed, "dead-123")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}
}

func TestSyncCloudStateWithClientsTimeout_TimesOut(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "slow-123"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}

	result, completed := syncCloudStateWithClientsTimeout(&config.Config{}, database, campaign.NewReconciler(), []cloud.Client{&cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			time.Sleep(50 * time.Millisecond)
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
	}}, nil, 5*time.Millisecond, false)

	if completed {
		t.Fatal("expected timeout")
	}
	if result.ReconcileResult != nil || result.Updated != 0 {
		t.Fatalf("unexpected result on timeout: %+v", result)
	}
}
