package campaign

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// setupTestDB creates an in-memory SQLite database for testing.
func setupTestDB(t *testing.T) *sql.DB {
	return db.SetupTestDB(t)
}

func TestLaunchInstanceNilR2Client(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	var createdOfferID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			createdOfferID = offerID
			return &cloud.Instance{
				ProviderID:  "12345",
				Status:      "running",
				SSHHost:     "127.0.0.1",
				SSHPort:     19999,
				CostPerHour: 0.50,
			}, nil
		},
		DestroyInstanceFunc: func(instanceID string) error {
			return nil
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}

	offer := cloud.Offer{
		ProviderID:  "999",
		Provider:    cloud.ProviderVastai,
		GPUName:     "RTX_4090",
		NumGPUs:     1,
		GPUMemGB:    24,
		CostPerHour: 0.50,
	}

	r2Cfg := cloud.R2Config{
		AccountID:       "test-account",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "test-bucket",
	}

	createOpts := cloud.CreateOpts{
		Image:      "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
		DiskGB:     50,
		SSHEnabled: true,
	}

	_, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		R2Assets{Client: nil, AgentR2Key: "agents/test-version/linux-amd64", SourceR2Keys: map[string]string{}},
		func(phase string) {},
	)
	if err == nil {
		t.Fatal("expected error for nil r2Client, got nil")
	}
	if !strings.Contains(err.Error(), "R2Assets.Client is required") {
		t.Errorf("error should mention R2Assets.Client requirement, got: %v", err)
	}

	// Verify CreateInstance was NOT called (we fail before that)
	if createdOfferID != "" {
		t.Errorf("expected no CreateInstance call, got offer ID %q", createdOfferID)
	}
}

func TestLaunchInstanceCreateFails(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return nil, errors.New("API error: insufficient balance")
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusQueued,
		Command: "python train.py",
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}

	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}
	r2Cfg := cloud.R2Config{Bucket: "test", AccountID: "test"}
	createOpts := cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, host, working_dir, status, gpu_class, gpu_mem_gb, command, tombstoned)
		 VALUES (?, '', '/tmp', ?, ?, ?, ?, 0)`,
		job.ID, db.StatusQueued, group.GPUClass, group.GPUMemGB, job.Command,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	instanceID, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		R2Assets{Client: &r2.Client{}},
		func(phase string) {},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error: insufficient balance") {
		t.Errorf("error should include create-instance failure, got: %v", err)
	}

	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get cloud instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonInfraFailure)
	}

	var host string
	var cloudInstanceID sql.NullInt64
	if err := database.QueryRow(`SELECT host, cloud_instance_id FROM jobs WHERE id = ?`, job.ID).Scan(&host, &cloudInstanceID); err != nil {
		t.Fatalf("select job: %v", err)
	}
	if cloudInstanceID.Valid {
		t.Fatalf("job cloud_instance_id = %v, want NULL", cloudInstanceID.Int64)
	}
	if host != "" {
		t.Fatalf("job host = %q, want empty", host)
	}

	attempts, err := db.GetJobCloudAttempts(database, job.ID)
	if err != nil {
		t.Fatalf("get job attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", attempts[0].Outcome, db.AttemptOutcomeOrphaned)
	}
	if attempts[0].EndedAt == nil {
		t.Fatal("attempt ended_at = nil, want non-nil")
	}
}

func TestLaunchCampaignRejectsEmptyGroups(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	result, err := LaunchCampaign(
		nil, database, nil, nil, nil, LaunchOpts{}, cloud.R2Config{}, nil,
		nil, nil,
	)
	if err == nil {
		t.Fatal("expected error for empty groups, got nil")
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %#v", result)
	}
	if !strings.Contains(err.Error(), "no instance groups to launch") {
		t.Fatalf("error = %v, want message about empty launch groups", err)
	}

	var campaigns int
	if scanErr := database.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&campaigns); scanErr != nil {
		t.Fatalf("count campaigns: %v", scanErr)
	}
	if campaigns != 0 {
		t.Fatalf("campaign count = %d, want 0", campaigns)
	}
}
