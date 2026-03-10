package db

import (
	"testing"
)

func TestGetCloudInstanceJobsIncludingAttempts(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance
	ci := &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	}
	instanceID, err := CreateCloudInstance(database, ci)
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	// Insert a job associated with the cloud instance
	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
		 VALUES (1, ?, ?, 0, 'queued', 'echo hello', '/tmp')`,
		instanceID, CloudInstanceHost(instanceID),
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	// Record the cloud attempt (as SetJobCloudInstanceID would)
	if err := InsertJobCloudAttempt(database, 1, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	// Verify both functions return the job before reset
	jobs, err := GetCloudInstanceJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstanceJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("GetCloudInstanceJobs before reset: got %d jobs, want 1", len(jobs))
	}

	jobsIncl, err := GetCloudInstanceJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts before reset: got %d jobs, want 1", len(jobsIncl))
	}

	// Reset the instance's jobs (simulates instance failure)
	n, err := ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetCloudInstanceJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetCloudInstanceJobs: reset %d jobs, want 1", n)
	}

	// GetCloudInstanceJobs should return 0 jobs (cloud_instance_id was cleared)
	jobs, err = GetCloudInstanceJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstanceJobs after reset: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("GetCloudInstanceJobs after reset: got %d jobs, want 0", len(jobs))
	}

	// GetCloudInstanceJobsIncludingAttempts should still return 1 job
	// (found via job_cloud_attempts table)
	jobsIncl, err = GetCloudInstanceJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts after reset: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts after reset: got %d jobs, want 1", len(jobsIncl))
	}
	if jobsIncl[0].ID != 1 {
		t.Fatalf("GetCloudInstanceJobsIncludingAttempts: got job ID %d, want 1", jobsIncl[0].ID)
	}
}
