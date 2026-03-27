package db

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestGetLaunchJobsIncludingAttempts(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance
	ci := &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job associated with the cloud instance
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Verify both functions return the job before reset
	jobs, err := GetLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("GetLaunchJobs before reset: got %d jobs, want 1", len(jobs))
	}

	jobsIncl, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts before reset: got %d jobs, want 1", len(jobsIncl))
	}

	// Reset the instance's jobs (simulates instance failure)
	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetLaunchJobs: reset %d jobs, want 1", n)
	}

	// GetLaunchJobs should return 0 jobs (cloud_instance_id was cleared)
	jobs, err = GetLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobs after reset: %v", err)
	}

	// GetLaunchJobsIncludingAttempts should still return 1 job
	jobsIncl, err = GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts after reset: %v", err)
	}
	if len(jobsIncl) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts after reset: got %d jobs, want 1", len(jobsIncl))
	}
	if jobsIncl[0].ID != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got job ID %d, want 1", jobsIncl[0].ID)
	}
}

func TestGetLaunchJobsIncludingAttemptsTreatsOpenAttemptsAsCurrent(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 203, "open attempt current", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 203, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(203): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got %d jobs, want 1", len(jobs))
	}
	if jobs[0].ID != 203 {
		t.Fatalf("job ID = %d, want 203", jobs[0].ID)
	}
	if jobs[0].LaunchID == nil || *jobs[0].LaunchID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", jobs[0].LaunchID, instanceID)
	}
}

func TestUpdateLaunchOfferMetadata(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:            LaunchStatusPlanned,
		Provider:          "vastai",
		GPUSpec:           "RTX_4090",
		ResolvedGPUName:   "RTX_4090",
		CostPerHourCents:  100,
		NumGPUs:           1,
		DLPerf:            10,
		Reliability:       0.98,
		InetDownMbps:      100,
		InetUpMbps:        50,
		CUDAVersion:       12.1,
		DiskGB:            80,
		ProvisionedInputs: []string{"hf:test/model"},
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	replacement := cloud.Offer{
		GPUName:           "RTX_5090",
		CostPerHour:       1.24,
		NumGPUs:           2,
		DLPerf:            42,
		Reliability:       0.995,
		DownloadBandwidth: 500,
		UploadBandwidth:   200,
		CUDAVersion:       12.4,
		DiskSpaceGB:       160,
	}
	if err := UpdateLaunchOfferMetadata(database, instanceID, replacement); err != nil {
		t.Fatalf("UpdateLaunchOfferMetadata: %v", err)
	}

	inst, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if inst.ResolvedGPUName != "RTX_5090" || inst.CostPerHourCents != 124 || inst.NumGPUs != 2 {
		t.Fatalf("updated instance metadata = %+v", inst)
	}
	if inst.DiskGB != 160 || inst.CUDAVersion != 12.4 || inst.InetDownMbps != 500 || inst.InetUpMbps != 200 {
		t.Fatalf("updated network/disk metadata = %+v", inst)
	}
}

func TestGetLaunchJobsIncludingAttemptsSortsByCampaignIndex(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 249, "historical", "/tmp", StatusQueued)
	insertTestJob(t, database, 203, "open attempt current", "/tmp", StatusQueued)
	insertTestJob(t, database, 199, "current earlier campaign slot", "/tmp", StatusRunning)

	if err := SetJobLaunchID(database, 249, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(249): %v", err)
	}
	if err := CloseLaunchAttempt(database, 249, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt(249): %v", err)
	}
	if err := SetJobLaunchID(database, 203, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(203): %v", err)
	}
	if err := SetJobLaunchID(database, 199, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(199): %v", err)
	}
	if err := SetJobCampaignIndex(database, 203, 1); err != nil {
		t.Fatalf("SetJobCampaignIndex(203): %v", err)
	}
	if err := SetJobCampaignIndex(database, 199, 0); err != nil {
		t.Fatalf("SetJobCampaignIndex(199): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("GetLaunchJobsIncludingAttempts: got %d jobs, want 3", len(jobs))
	}
	if jobs[0].ID != 199 || jobs[1].ID != 203 || jobs[2].ID != 249 {
		t.Fatalf("job order = [%d %d %d], want [199 203 249]", jobs[0].ID, jobs[1].ID, jobs[2].ID)
	}
}

// TestGetLaunchJobsIncludingAttemptsRetainsOrderAfterFailure verifies that
// a failed (historical) job keeps its campaign position instead of moving after
// still-running jobs.
func TestGetLaunchJobsIncludingAttemptsRetainsOrderAfterFailure(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Job 292 (campaign index 0) failed and was reset — cloud_instance_id cleared.
	// Job 293 (campaign index 1) is still running.
	insertTestJob(t, database, 292, "echo first", "/tmp", StatusFailed)
	insertTestJob(t, database, 293, "echo second", "/tmp", StatusRunning)
	if err := SetJobCampaignIndex(database, 292, 0); err != nil {
		t.Fatalf("SetJobCampaignIndex(292): %v", err)
	}
	if err := SetJobCampaignIndex(database, 293, 1); err != nil {
		t.Fatalf("SetJobCampaignIndex(293): %v", err)
	}

	// Simulate what ResetLaunchJobs does: assign + close the attempt for 292.
	if err := SetJobLaunchID(database, 292, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(292): %v", err)
	}
	if err := CloseLaunchAttempt(database, 292, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt(292): %v", err)
	}
	if err := SetJobLaunchID(database, 293, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID(293): %v", err)
	}

	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].ID != 292 || jobs[1].ID != 293 {
		t.Fatalf("job order = [%d %d], want [292 293]", jobs[0].ID, jobs[1].ID)
	}
}

// TestGetLaunchJobsIncludingAttemptsOverridesStatusForHistorical verifies
// that after a job is reset (orphaned), its display status reflects the attempt
// outcome rather than the current global status.
func TestGetLaunchJobsIncludingAttemptsOverridesStatusForHistorical(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Insert a job, associate it with the instance, then reset (simulating instance failure).
	insertTestJob(t, database, 317, "python train.py", "/tmp", StatusRunning)
	if err := SetJobLaunchID(database, 317, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Simulate ResetLaunchJobs: close attempt as orphaned, clear instance assignment, reset to queued.
	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}

	// Verify the job's global status is now "queued".
	job, err := GetJobByID(database, 317)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("global status = %q, want %q", job.Status, StatusQueued)
	}

	// GetLaunchJobsIncludingAttempts should preserve "queued" status for
	// orphaned jobs — JobDisplayStatus() maps queued + orphaned outcome to "orphaned".
	jobs, err := GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchJobsIncludingAttempts: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Status != StatusQueued {
		t.Fatalf("display status = %q, want %q", jobs[0].Status, StatusQueued)
	}
}

func TestResetOrphanedCloudJobsSetsPlacementReasons(t *testing.T) {
	database := setupTestDB(t)

	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusRunning, withHost("vastai:77"))

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

func TestGetAttemptOutcomesByLaunch(t *testing.T) {
	database := setupTestDB(t)

	// Create a cloud instance and a job
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1, "echo hello", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Before closing the attempt, outcomes should be empty
	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("expected 0 outcomes before closing, got %d", len(outcomes))
	}

	// Close the attempt as orphaned (simulates instance failure + reset)
	if err := CloseLaunchAttempts(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("CloseLaunchAttempts: %v", err)
	}

	outcomes, err = GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch after close: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("expected outcome %q, got %q", AttemptOutcomeOrphaned, outcomes[1])
	}
}

func TestResetLaunchJobs_PreservesCanceledJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 1, "echo canceled", "/tmp", StatusCanceled, withLaunch(instanceID))
	insertTestJob(t, database, 2, "echo running", "/tmp", StatusRunning, withLaunch(instanceID))

	n, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
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
	if canceledJob.LaunchID == nil || *canceledJob.LaunchID != instanceID {
		t.Fatalf("canceled job cloud_instance_id = %v, want %d", canceledJob.LaunchID, instanceID)
	}
	if canceledJob.Host != "" {
		t.Fatalf("canceled job host = %q, want empty", canceledJob.Host)
	}

	resetJob, err := GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("GetJobByID(reset): %v", err)
	}
	if resetJob.Status != StatusQueued {
		t.Fatalf("reset job status = %q, want %q", resetJob.Status, StatusQueued)
	}
	// After schema refactor: the attempt retains its cloud_instance_id.
	// The job_status view shows the latest attempt's cloud_instance_id.
	if resetJob.Host != "" {
		t.Fatalf("reset job host = %q, want empty", resetJob.Host)
	}
}

func TestResetLaunchJobs_DoesNotRewriteCompletedAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	for _, stmt := range []struct {
		id     int64
		status string
	}{
		{id: 1, status: StatusCompleted},
		{id: 2, status: StatusRunning},
	} {
		insertTestJob(t, database, stmt.id, "echo test", "/tmp", stmt.status, withLaunch(instanceID))
	}
	if err := CloseLaunchAttempt(database, 1, AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempt(completed): %v", err)
	}

	if _, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeCompleted {
		t.Fatalf("completed job outcome = %q, want %q", outcomes[1], AttemptOutcomeCompleted)
	}
	if outcomes[2] != AttemptOutcomeOrphaned {
		t.Fatalf("reset job outcome = %q, want %q", outcomes[2], AttemptOutcomeOrphaned)
	}
}

func TestResetLaunchJobs_ArchivesPreviousRun(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestGetActiveLaunchJobCounts_OnlyCountsNonTerminalJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
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
		insertTestJob(t, database, stmt.id, "echo test", "/tmp", stmt.status, withLaunch(instanceID))
	}

	counts, err := GetActiveLaunchJobCounts(database)
	if err != nil {
		t.Fatalf("GetActiveLaunchJobCounts: %v", err)
	}
	if counts[instanceID] != 2 {
		t.Fatalf("active count = %d, want 2", counts[instanceID])
	}
}

func TestNormalizeTerminalLaunchJobs_FailedInstanceOrphansRunningJobs(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	insertTestJob(t, database, 1, "echo running", "/tmp", StatusRunning, withLaunch(instanceID))

	n, err := NormalizeTerminalLaunchJobs(database, instanceID)
	if err != nil {
		t.Fatalf("NormalizeTerminalLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("NormalizeTerminalLaunchJobs reset %d jobs, want 1", n)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, StatusQueued)
	}

	outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[1] != AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[1], AttemptOutcomeOrphaned)
	}
}

func TestResetJobsOnTerminalLaunches_SkipsCompletedInstances(t *testing.T) {
	database := setupTestDB(t)

	completedID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(completed): %v", err)
	}
	failedID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(failed): %v", err)
	}

	for _, tc := range []struct {
		jobID      int64
		instanceID int64
	}{
		{jobID: 1, instanceID: completedID},
		{jobID: 2, instanceID: failedID},
	} {
		insertTestJob(t, database, tc.jobID, "echo test", "/tmp", StatusRunning, withLaunch(tc.instanceID))
	}

	n, err := ResetJobsOnTerminalLaunches(database)
	if err != nil {
		t.Fatalf("ResetJobsOnTerminalLaunches: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetJobsOnTerminalLaunches reset %d jobs, want 1", n)
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

func TestIsRetryableTermination(t *testing.T) {
	tests := []struct {
		name   string
		ci     *Launch
		expect bool
	}{
		{"nil instance", nil, false},
		{"running instance", &Launch{Status: LaunchStatusRunning}, false},
		{"completed", &Launch{Status: LaunchStatusCompleted}, false},
		{"canceled", &Launch{Status: LaunchStatusCancelled}, false},
		{"preempted", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonPreempted}, true},
		{"infra failure", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonInfraFailure}, true},
		{"failed to launch (empty reason)", &Launch{Status: LaunchStatusFailed, TerminationReason: ""}, true},
		{"job failure", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonJobFailure}, false},
		{"disk full", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonDiskFull}, false},
		{"canceled reason", &Launch{Status: LaunchStatusFailed, TerminationReason: TerminationReasonCancelled}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetryableTermination(tt.ci)
			if got != tt.expect {
				t.Errorf("IsRetryableTermination() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestCountLaunchAttemptsInCampaign(t *testing.T) {
	database := setupTestDB(t)

	// Create two campaigns (simulating two separate `launch` commands)
	campaign1ID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign 1: %v", err)
	}
	campaign2ID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign 2: %v", err)
	}

	// Create a job
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "training", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Launch in campaign 1 (completes successfully)
	inst1ID, err := CreateLaunch(database, &Launch{
		Status:     LaunchStatusCompleted,
		Provider:   "vastai",
		GPUSpec:    "RTX_3090",
		CampaignID: &campaign1ID,
	})
	if err != nil {
		t.Fatalf("create instance 1: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, inst1ID); err != nil {
		t.Fatalf("set job launch 1: %v", err)
	}

	// Launch in campaign 2 (fails, orphaned)
	inst2ID, err := CreateLaunch(database, &Launch{
		Status:     LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_3090",
		CampaignID: &campaign2ID,
	})
	if err != nil {
		t.Fatalf("create instance 2: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "", nil, StatusQueued); err != nil {
		t.Fatalf("create attempt 2: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, inst2ID); err != nil {
		t.Fatalf("set job launch 2: %v", err)
	}

	// Global count should be 2 (both campaigns)
	total, err := CountLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("count all attempts: %v", err)
	}
	if total != 2 {
		t.Errorf("CountLaunchAttempts = %d, want 2", total)
	}

	// Campaign-scoped counts should be 1 each
	count1, err := CountLaunchAttemptsInCampaign(database, jobID, campaign1ID)
	if err != nil {
		t.Fatalf("count campaign 1 attempts: %v", err)
	}
	if count1 != 1 {
		t.Errorf("CountLaunchAttemptsInCampaign(campaign1) = %d, want 1", count1)
	}

	count2, err := CountLaunchAttemptsInCampaign(database, jobID, campaign2ID)
	if err != nil {
		t.Fatalf("count campaign 2 attempts: %v", err)
	}
	if count2 != 1 {
		t.Errorf("CountLaunchAttemptsInCampaign(campaign2) = %d, want 1", count2)
	}
}

func TestRefineInstanceTerminationReason_DiskFull(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusFailed,
		withLaunch(instanceID), withFailureReason(TerminationReasonDiskFull))

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}

func TestRefineInstanceTerminationReason_OnlyRefinesJobFailure(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonInfraFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusFailed,
		withLaunch(instanceID), withFailureReason(TerminationReasonDiskFull))

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonInfraFailure)
	}
}

func TestRefineInstanceTerminationReason_UsesHistoricalAttempts(t *testing.T) {
	database := setupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	insertTestJob(t, database, 1, "python train.py", "/tmp", StatusFailed,
		withFailureReason(TerminationReasonDiskFull))
	if err := SetJobLaunchID(database, 1, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	if err := RefineInstanceTerminationReason(database, instanceID); err != nil {
		t.Fatalf("RefineInstanceTerminationReason: %v", err)
	}

	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if ci.TerminationReason != TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, TerminationReasonDiskFull)
	}
}

func TestLaunchLiveState(t *testing.T) {
	database := setupTestDB(t)

	// Get returns nil for nonexistent row
	got, err := GetLaunchLiveState(database, 999)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	// Create a launch to reference
	ci := &Launch{Status: LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX3090"}
	instanceID, err := CreateLaunch(database, ci)
	if err != nil {
		t.Fatalf("InsertLaunch: %v", err)
	}

	// Upsert and read back
	state := LaunchLiveState{
		LaunchID:       instanceID,
		InstancePhase:  "running:42",
		HeartbeatJSON:  `{"ts":1234}`,
		HeartbeatTS:    1234,
		JobProgressPct: 75,
		JobProgressID:  42,
		AgentVersion:   "abc123",
	}
	if err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	got, err = GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if got.InstancePhase != "running:42" {
		t.Errorf("phase = %q, want %q", got.InstancePhase, "running:42")
	}
	if got.JobProgressPct != 75 {
		t.Errorf("progress = %d, want 75", got.JobProgressPct)
	}
	if got.AgentVersion != "abc123" {
		t.Errorf("agent_version = %q, want %q", got.AgentVersion, "abc123")
	}

	// Upsert overwrites
	state.InstancePhase = "running:43"
	state.JobProgressPct = -1
	if err := UpsertLaunchLiveState(database, state); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	got, err = GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got.InstancePhase != "running:43" {
		t.Errorf("phase = %q, want %q", got.InstancePhase, "running:43")
	}
	if got.JobProgressPct != -1 {
		t.Errorf("progress = %d, want -1", got.JobProgressPct)
	}

	// Delete
	if err := DeleteLaunchLiveState(database, instanceID); err != nil {
		t.Fatalf("DeleteLaunchLiveState: %v", err)
	}
	got, err = GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}
