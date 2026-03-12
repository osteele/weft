package db

import (
	"strings"
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

func TestResetOrphanedCloudJobsSetsPlacementReasons(t *testing.T) {
	database := setupTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO jobs (id, host, tombstoned, status, command, working_dir)
		 VALUES (1, 'vastai:77', 0, 'running', 'echo hello', '/tmp')`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	n, err := ResetOrphanedCloudJobs(database)
	if err != nil {
		t.Fatalf("ResetOrphanedCloudJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetOrphanedCloudJobs reset %d jobs, want 1", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" || job.Status != StatusQueued {
		t.Fatalf("job = %+v, want queued unplaced", job)
	}
	if got := strings.Join(job.PlacementReasons, "\n"); got != "cloud instance 77 no longer active; job returned to unplaced queue" {
		t.Fatalf("PlacementReasons = %v", job.PlacementReasons)
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

func TestResetCloudInstanceJobs_DoesNotRewriteCompletedAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	for _, stmt := range []struct {
		id     int64
		status string
	}{
		{id: 1, status: StatusCompleted},
		{id: 2, status: StatusRunning},
	} {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
			 VALUES (?, ?, ?, 0, ?, 'echo test', '/tmp')`,
			stmt.id, instanceID, CloudInstanceHost(instanceID), stmt.status,
		); err != nil {
			t.Fatalf("insert job %d: %v", stmt.id, err)
		}
		if err := InsertJobCloudAttempt(database, stmt.id, instanceID); err != nil {
			t.Fatalf("InsertJobCloudAttempt(%d): %v", stmt.id, err)
		}
	}
	if err := CloseJobCloudAttempt(database, 1, AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseJobCloudAttempt(completed): %v", err)
	}

	if _, err := ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetCloudInstanceJobs: %v", err)
	}

	outcomes, err := GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance: %v", err)
	}
	if outcomes[1] != AttemptOutcomeCompleted {
		t.Fatalf("completed job outcome = %q, want %q", outcomes[1], AttemptOutcomeCompleted)
	}
	if outcomes[2] != AttemptOutcomeOrphaned {
		t.Fatalf("reset job outcome = %q, want %q", outcomes[2], AttemptOutcomeOrphaned)
	}
}

func TestResetCloudInstanceJobs_ArchivesPreviousRun(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	jobID, err := RecordQueued(database, CloudInstanceHost(instanceID), "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	startTime := int64(1_700_000_100)
	endTime := int64(1_700_000_120)
	exitCode := 137
	errorDiagnosis := `{"kind":"instance_lost"}`
	if _, err := database.Exec(
		`UPDATE jobs
		 SET status = ?, session_name = ?, start_time = ?, end_time = ?, exit_code = ?,
		     error_message = ?, failure_reason = ?, error_diagnosis = ?, remote_state = ?, remote_id = ?
		 WHERE id = ?`,
		StatusRunning, "rj-88", startTime, endTime, exitCode,
		"worker disappeared", "infra_failure", errorDiagnosis, "running", "inst-remote-1", jobID,
	); err != nil {
		t.Fatalf("seed cloud job: %v", err)
	}

	n, err := ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetCloudInstanceJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued || job.Host != "" || job.CloudInstanceID != nil {
		t.Fatalf("reset job = status %q host %q cloud_instance %v, want queued empty nil", job.Status, job.Host, job.CloudInstanceID)
	}
	if job.StartTime != 0 || job.EndTime != nil || job.ExitCode != nil {
		t.Fatalf("expected runtime timestamps cleared, got start=%d end=%v exit=%v", job.StartTime, job.EndTime, job.ExitCode)
	}
	if job.ErrorMessage != "" || job.FailureReason != "" || job.ErrorDiagnosis != "" || job.RemoteState != "" || job.RemoteID != "" || job.SessionName != "" {
		t.Fatalf("expected runtime fields cleared, got err=%q reason=%q diagnosis=%q remote_state=%q remote_id=%q session=%q",
			job.ErrorMessage, job.FailureReason, job.ErrorDiagnosis, job.RemoteState, job.RemoteID, job.SessionName)
	}

	runs, err := ListJobRuns(database, jobID)
	if err != nil {
		t.Fatalf("ListJobRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 archived run, got %d", len(runs))
	}
	run := runs[0]
	if run.ArchiveReason != "cloud_reset:"+AttemptOutcomeOrphaned {
		t.Fatalf("archive_reason = %q, want %q", run.ArchiveReason, "cloud_reset:"+AttemptOutcomeOrphaned)
	}
	if run.Status != StatusRunning || run.CloudInstanceID == nil || *run.CloudInstanceID != instanceID {
		t.Fatalf("archived run = status %q cloud_instance %v, want %q %d", run.Status, run.CloudInstanceID, StatusRunning, instanceID)
	}
	if run.ErrorMessage != "worker disappeared" || run.FailureReason != "infra_failure" || run.ErrorDiagnosis != errorDiagnosis {
		t.Fatalf("archived failure fields = (%q, %q, %q), want (%q, %q, %q)",
			run.ErrorMessage, run.FailureReason, run.ErrorDiagnosis,
			"worker disappeared", "infra_failure", errorDiagnosis)
	}
	if run.RemoteID != "inst-remote-1" || run.SessionName != "rj-88" {
		t.Fatalf("archived remote/session = (%q, %q), want (%q, %q)",
			run.RemoteID, run.SessionName, "inst-remote-1", "rj-88")
	}
}

func TestGetActiveCloudInstanceJobCounts_OnlyCountsNonTerminalJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	for _, stmt := range []struct {
		id     int64
		status string
	}{
		{id: 1, status: StatusQueued},
		{id: 2, status: StatusRunning},
		{id: 3, status: StatusCompleted},
		{id: 4, status: StatusFailed},
	} {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
			 VALUES (?, ?, ?, 0, ?, 'echo test', '/tmp')`,
			stmt.id, instanceID, CloudInstanceHost(instanceID), stmt.status,
		); err != nil {
			t.Fatalf("insert job %d: %v", stmt.id, err)
		}
	}

	counts, err := GetActiveCloudInstanceJobCounts(database)
	if err != nil {
		t.Fatalf("GetActiveCloudInstanceJobCounts: %v", err)
	}
	if counts[instanceID] != 2 {
		t.Fatalf("active count = %d, want 2", counts[instanceID])
	}
}

func TestNormalizeTerminalCloudInstanceJobs_FailedInstanceOrphansRunningJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	_, err = database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
		 VALUES (1, ?, ?, 0, ?, 'echo running', '/tmp')`,
		instanceID, CloudInstanceHost(instanceID), StatusRunning,
	)
	if err != nil {
		t.Fatalf("insert running job: %v", err)
	}
	if err := InsertJobCloudAttempt(database, 1, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	n, err := NormalizeTerminalCloudInstanceJobs(database, instanceID)
	if err != nil {
		t.Fatalf("NormalizeTerminalCloudInstanceJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("NormalizeTerminalCloudInstanceJobs reset %d jobs, want 1", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, StatusQueued)
	}
	if job.CloudInstanceID != nil {
		t.Fatalf("job cloud_instance_id = %v, want nil", job.CloudInstanceID)
	}

	outcomes, err := GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance: %v", err)
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[1], AttemptOutcomeOrphaned)
	}
}

func TestResetJobsOnTerminalCloudInstances_SkipsCompletedInstances(t *testing.T) {
	database := setupTestDB(t)

	completedID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(completed): %v", err)
	}
	failedID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(failed): %v", err)
	}

	for _, tc := range []struct {
		jobID      int64
		instanceID int64
	}{
		{jobID: 1, instanceID: completedID},
		{jobID: 2, instanceID: failedID},
	} {
		_, err = database.Exec(
			`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir)
			 VALUES (?, ?, ?, 0, ?, 'echo test', '/tmp')`,
			tc.jobID, tc.instanceID, CloudInstanceHost(tc.instanceID), StatusRunning,
		)
		if err != nil {
			t.Fatalf("insert job %d: %v", tc.jobID, err)
		}
		if err := InsertJobCloudAttempt(database, tc.jobID, tc.instanceID); err != nil {
			t.Fatalf("InsertJobCloudAttempt(%d): %v", tc.jobID, err)
		}
	}

	n, err := ResetJobsOnTerminalCloudInstances(database)
	if err != nil {
		t.Fatalf("ResetJobsOnTerminalCloudInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetJobsOnTerminalCloudInstances reset %d jobs, want 1", n)
	}

	completedJob, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID(completed): %v", err)
	}
	if completedJob.Status != StatusRunning {
		t.Fatalf("completed-instance job status = %q, want %q", completedJob.Status, StatusRunning)
	}

	failedJob, err := GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("GetJobByID(failed): %v", err)
	}
	if failedJob.Status != StatusQueued {
		t.Fatalf("failed-instance job status = %q, want %q", failedJob.Status, StatusQueued)
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
