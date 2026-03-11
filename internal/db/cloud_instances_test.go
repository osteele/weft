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

func TestGetAttemptOutcomesByInstance(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance and a job
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
		 VALUES (1, ?, ?, 0, 'queued', 'echo hello', '/tmp')`,
		instanceID, CloudInstanceHost(instanceID),
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := InsertJobCloudAttempt(database, 1, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	// Before closing the attempt, outcomes should be empty
	outcomes, err := GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("expected 0 outcomes before closing, got %d", len(outcomes))
	}

	// Close the attempt as orphaned (simulates instance failure + reset)
	if err := CloseJobCloudAttemptsByInstance(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("CloseJobCloudAttemptsByInstance: %v", err)
	}

	outcomes, err = GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance after close: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("expected outcome %q, got %q", AttemptOutcomeOrphaned, outcomes[1])
	}
}

func TestResetCloudInstanceJobs_PreservesCanceledJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
		 VALUES (1, ?, ?, 0, ?, 'echo canceled', '/tmp')`,
		instanceID, CloudInstanceHost(instanceID), StatusCanceled,
	); err != nil {
		t.Fatalf("insert canceled job: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
		 VALUES (2, ?, ?, 0, ?, 'echo running', '/tmp')`,
		instanceID, CloudInstanceHost(instanceID), StatusRunning,
	); err != nil {
		t.Fatalf("insert running job: %v", err)
	}

	n, err := ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetCloudInstanceJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	canceledJob, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID(canceled): %v", err)
	}
	if canceledJob.Status != StatusCanceled {
		t.Fatalf("canceled job status = %q, want %q", canceledJob.Status, StatusCanceled)
	}
	if canceledJob.CloudInstanceID == nil || *canceledJob.CloudInstanceID != instanceID {
		t.Fatalf("canceled job cloud_instance_id = %v, want %d", canceledJob.CloudInstanceID, instanceID)
	}
	if canceledJob.Host != CloudInstanceHost(instanceID) {
		t.Fatalf("canceled job host = %q, want %q", canceledJob.Host, CloudInstanceHost(instanceID))
	}

	resetJob, err := GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("GetJobByID(reset): %v", err)
	}
	if resetJob.Status != StatusQueued {
		t.Fatalf("reset job status = %q, want %q", resetJob.Status, StatusQueued)
	}
	if resetJob.CloudInstanceID != nil {
		t.Fatalf("reset job cloud_instance_id = %v, want nil", resetJob.CloudInstanceID)
	}
	if resetJob.Host != "" {
		t.Fatalf("reset job host = %q, want empty", resetJob.Host)
	}
}

func TestRefineInstanceTerminationReason_DiskFull(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := UpdateCloudInstanceStatus(database, instanceID, CloudInstanceStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateCloudInstanceStatus: %v", err)
	}

	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir, failure_reason)
		 VALUES (1, ?, ?, 0, 'failed', 'python train.py', '/tmp', ?)`,
		instanceID, CloudInstanceHost(instanceID), TerminationReasonDiskFull,
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstance: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}

func TestRefineInstanceTerminationReason_OnlyRefinesJobFailure(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := UpdateCloudInstanceStatus(database, instanceID, CloudInstanceStatusFailed, TerminationReasonInfraFailure); err != nil {
		t.Fatalf("UpdateCloudInstanceStatus: %v", err)
	}

	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir, failure_reason)
		 VALUES (1, ?, ?, 0, 'failed', 'python train.py', '/tmp', ?)`,
		instanceID, CloudInstanceHost(instanceID), TerminationReasonDiskFull,
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstance: %v", err)
	}
	if ci.TerminationReason != TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonInfraFailure)
	}
}

func TestRefineInstanceTerminationReason_UsesHistoricalAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := UpdateCloudInstanceStatus(database, instanceID, CloudInstanceStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateCloudInstanceStatus: %v", err)
	}

	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir, failure_reason)
		 VALUES (1, NULL, '', 0, 'failed', 'python train.py', '/tmp', ?)`,
		TerminationReasonDiskFull,
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := InsertJobCloudAttempt(database, 1, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetCloudInstance: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}
