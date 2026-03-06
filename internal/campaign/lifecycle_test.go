package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// setupTestDB creates an in-memory SQLite database for testing.
func setupTestDB(t *testing.T) *sql.DB {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	schema := `
	CREATE TABLE jobs (
		id INTEGER PRIMARY KEY,
		status TEXT,
		gpu_class TEXT,
		gpu_mem_gb INTEGER,
		command TEXT,
		cloud_instance_id INTEGER,
		campaign_job_index INTEGER
	);
	CREATE TABLE cloud_instances (
		id INTEGER PRIMARY KEY,
		campaign_id INTEGER,
		status TEXT,
		provider TEXT,
		gpu_spec TEXT,
		gpu_class TEXT,
		gpu_mem_gb INTEGER,
		max_spend_cents INTEGER,
		max_time_seconds INTEGER,
		actual_spend_cents INTEGER,
		vastai_instance_id TEXT,
		provider_instance_id TEXT,
		data_center TEXT,
		created_at INTEGER,
		ready_at INTEGER,
		launched_at INTEGER,
		ended_at INTEGER,
		resolved_gpu_name TEXT,
		cost_per_hour_cents INTEGER,
		num_gpus INTEGER,
		dl_perf REAL,
		reliability REAL,
		inet_down_mbps REAL,
		inet_up_mbps REAL,
		cuda_version REAL
	);
	CREATE TABLE campaigns (
		id INTEGER PRIMARY KEY,
		status TEXT,
		created_at INTEGER,
		ended_at INTEGER
	);
	`
	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return database
}

func TestLaunchInstancePreSSHPhases(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	var createdOfferID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			createdOfferID = offerID
			return &cloud.Instance{
				ProviderID:  "12345",
				Status:      "loading",
				SSHHost:     "127.0.0.1",
				SSHPort:     19999,
				CostPerHour: 0.50,
			}, nil
		},
		WaitReadyFunc: func(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
			// Simulate timeout so we don't reach the SSH phase (which would retry for minutes)
			return nil, fmt.Errorf("instance %s not ready after %v", instanceID, timeout)
		},
		DestroyInstanceFunc: func(instanceID string) error {
			return nil
		},
	}

	job := &db.Job{
		ID:      101,
		Status:  db.StatusNeedsRental,
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

	instanceID, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		func(phase string) {},
	)

	// Should fail at WaitReady, not at SSH
	if err == nil {
		t.Fatal("expected error from wait ready, got nil")
	}
	if !strings.Contains(err.Error(), "wait ready") {
		t.Errorf("error should mention 'wait ready', got: %v", err)
	}

	// Verify pre-SSH phases completed: instance was created, offer ID was passed
	if createdOfferID != "999" {
		t.Errorf("expected offer ID 999, got %q", createdOfferID)
	}

	// Verify DB records were created
	if instanceID == 0 {
		t.Error("expected non-zero instance ID")
	}
	inst, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get cloud instance: %v", err)
	}
	if inst.Status != db.CloudInstanceStatusFailed {
		t.Errorf("instance status = %q, want %q", inst.Status, db.CloudInstanceStatusFailed)
	}
	if inst.ProviderInstanceID != "12345" {
		t.Errorf("provider_instance_id = %q, want %q", inst.ProviderInstanceID, "12345")
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
		Status:  db.StatusNeedsRental,
		Command: "python train.py",
	}

	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{job},
	}

	offer := cloud.Offer{ProviderID: "999", Provider: cloud.ProviderVastai}
	r2Cfg := cloud.R2Config{Bucket: "test"}
	createOpts := cloud.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}

	_, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		func(phase string) {},
	)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "create instance") {
		t.Errorf("error message should mention 'create instance', got: %v", err)
	}
}
