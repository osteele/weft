package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
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

	// Mock client that simulates successful launch
	mockClient := &vastai.MockClient{
		CreateInstanceFunc: func(offerID int, opts vastai.CreateOpts) (*vastai.Instance, error) {
			return &vastai.Instance{
				ID:          12345,
				Status:      "loading",
				SSHHost:     "192.168.1.100",
				SSHPort:     10022,
				CostPerHour: 0.50,
			}, nil
		},
		WaitReadyFunc: func(instanceID int, timeout time.Duration) (*vastai.Instance, error) {
			return &vastai.Instance{
				ID:          instanceID,
				Status:      "running",
				SSHHost:     "192.168.1.100",
				SSHPort:     10022,
				CostPerHour: 0.50,
			}, nil
		},
		DestroyInstanceFunc: func(instanceID int) error {
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

	offer := vastai.Offer{
		ID:          999,
		GPUName:     "RTX_4090",
		NumGPUs:     1,
		GPUMemGB:    24,
		CostPerHour: 0.50,
	}

	r2Cfg := vastai.R2Config{
		AccountID:       "test-account",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "test-bucket",
	}

	createOpts := vastai.CreateOpts{
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

	// Mock client where CreateInstance fails
	mockClient := &vastai.MockClient{
		CreateInstanceFunc: func(offerID int, opts vastai.CreateOpts) (*vastai.Instance, error) {
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

	offer := vastai.Offer{ID: 999}
	r2Cfg := vastai.R2Config{Bucket: "test"}
	createOpts := vastai.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}

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

	// Mock client where WaitReady times out
	mockClient := &vastai.MockClient{
		CreateInstanceFunc: func(offerID int, opts vastai.CreateOpts) (*vastai.Instance, error) {
			return &vastai.Instance{ID: 12345}, nil
		},
		WaitReadyFunc: func(instanceID int, timeout time.Duration) (*vastai.Instance, error) {
			return nil, fmt.Errorf("instance %d not ready after %v", instanceID, timeout)
		},
		DestroyInstanceFunc: func(instanceID int) error {
			return nil
		},
	}

	group := InstanceGroup{
		GPUClass: "A100",
		GPUMemGB: 40,
		Jobs:     []*db.Job{{ID: 101}},
	}

	offer := vastai.Offer{ID: 999}
	r2Cfg := vastai.R2Config{Bucket: "test"}
	createOpts := vastai.CreateOpts{Image: "nvidia/cuda:12.2-devel-ubuntu22.04"}

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
