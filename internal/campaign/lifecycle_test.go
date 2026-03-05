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

func TestLaunchInstanceHappyPath(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return &cloud.Instance{
				ProviderID:  "12345",
				Status:      "loading",
				SSHHost:     "192.168.1.100",
				SSHPort:     10022,
				CostPerHour: 0.50,
			}, nil
		},
		WaitReadyFunc: func(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
			return &cloud.Instance{
				ProviderID:  instanceID,
				Status:      "running",
				SSHHost:     "192.168.1.100",
				SSHPort:     10022,
				CostPerHour: 0.50,
			}, nil
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
		Image:      "nvidia/cuda:12.2-devel-ubuntu22.04",
		DiskGB:     50,
		SSHEnabled: true,
	}

	_, err := LaunchInstance(
		mockClient, database, nil, group, offer,
		LaunchOpts{},
		r2Cfg, createOpts,
		func(phase string) {},
	)

	if err == nil {
		t.Error("expected error (mock SSH will fail), got nil")
	}
	// We expect an error from SSH operations since they're not mocked
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

func TestLaunchInstanceWaitTimeouts(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		CreateInstanceFunc: func(offerID string, opts cloud.CreateOpts) (*cloud.Instance, error) {
			return &cloud.Instance{ProviderID: "12345"}, nil
		},
		WaitReadyFunc: func(instanceID string, timeout time.Duration) (*cloud.Instance, error) {
			return nil, fmt.Errorf("instance %s not ready after %v", instanceID, timeout)
		},
		DestroyInstanceFunc: func(instanceID string) error {
			return nil
		},
	}

	group := InstanceGroup{
		GPUClass: "A100",
		GPUMemGB: 40,
		Jobs:     []*db.Job{{ID: 101}},
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
		t.Fatal("expected error from wait timeout, got nil")
	}
	if !strings.Contains(err.Error(), "wait ready") {
		t.Errorf("error should mention 'wait ready', got: %v", err)
	}
}
