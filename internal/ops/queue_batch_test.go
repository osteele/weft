package ops

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// TestBatchSyncDeadReportDoesNotKillRecentlyQueuedJob validates the fix for the
// race condition where the TUI's batch sync marks recently-queued jobs as dead.
//
// Scenario: A job is queued and appended to the remote commands file
// (last_synced_status=queued), but the queue runner hasn't processed the command
// yet. The agent reports DEAD because the job isn't in the runner's state.
// The batch sync must NOT mark this job as failed.
func TestBatchSyncDeadReportDoesNotKillRecentlyQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "recently queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued, got %s", result.Status)
	}
}

// TestBatchSyncDeadReportKillsRunningJob validates that the batch sync DOES mark
// a running job as dead when the agent reports DEAD (process disappeared).
func TestBatchSyncDeadReportKillsRunningJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "running job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusFailed {
		t.Fatalf("expected status failed, got %s", result.Status)
	}
}

// TestBatchSyncDeadProtectsQueuedJobWithSyncedStatus validates that a queued job
// whose last_synced_status is "queued" (set at INSERT time) is protected from
// being marked dead by batch sync. This is the normal case for newly-queued jobs.
func TestBatchSyncDeadProtectsQueuedJobWithSyncedStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "new job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	// last_synced_status is "queued" — set at INSERT time by RecordQueuedWithGPU

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateDead},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates (job should be protected), got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued (protected), got %s", result.Status)
	}
}

// TestBatchSyncRunningTransitionsQueuedJob validates that a queued job is
// correctly marked running when the agent reports it as RUNNING.
func TestBatchSyncRunningTransitionsQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "queue to run")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	mock := mockQueueRemote{metadata: "start_time=1700000000\n"}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{
		jobID: {State: queueStateRunning, GPUDevices: "0,1"},
	}

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 update, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusRunning {
		t.Fatalf("expected status running, got %s", result.Status)
	}
}

// TestBatchSyncSkipsUnknownJobs validates that jobs not in the agent's response
// are silently skipped (no status change).
func TestBatchSyncSkipsUnknownJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo test", "unknown job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)
	jobByID := map[int64]*db.Job{jobID: job}
	statuses := map[int64]queueBatchStatus{} // agent doesn't know about this job

	updated, err := applyBatchStatuses(database, []int64{jobID}, jobByID, statuses, time.Second)
	if err != nil {
		t.Fatalf("applyBatchStatuses: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updates, got %d", updated)
	}

	result, _ := db.GetJobByID(database, jobID)
	if result.Status != db.StatusQueued {
		t.Fatalf("expected status queued, got %s", result.Status)
	}
}
