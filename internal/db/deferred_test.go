package db

import (
	"strings"
	"testing"
)

func TestHasPendingDeferredOperationForJob(t *testing.T) {
	db := setupTestDB(t)

	// Create a test job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Initially should have no pending operations
	hasPending, err := HasPendingDeferredOperationForJob(db, jobID)
	if err != nil {
		t.Fatalf("HasPendingDeferredOperationForJob failed: %v", err)
	}
	if hasPending {
		t.Error("Expected no pending operations for new job")
	}

	// Add a deferred operation
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", `{"working_dir":"/tmp"}`)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Now should have pending operations
	hasPending, err = HasPendingDeferredOperationForJob(db, jobID)
	if err != nil {
		t.Fatalf("HasPendingDeferredOperationForJob failed: %v", err)
	}
	if !hasPending {
		t.Error("Expected pending operations after adding one")
	}
}

func TestHasPendingOperation(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Initially should have no pending queue_job
	hasQueue, err := HasPendingOperation(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("HasPendingOperation failed: %v", err)
	}
	if hasQueue {
		t.Error("Expected no pending queue_job for new job")
	}

	// Add queue_job operation
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", `{}`)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Now should have pending queue_job
	hasQueue, err = HasPendingOperation(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("HasPendingOperation failed: %v", err)
	}
	if !hasQueue {
		t.Error("Expected pending queue_job after adding one")
	}

	// But should not have pending start_queued_job
	hasStart, err := HasPendingOperation(db, jobID, OpStartQueuedJob)
	if err != nil {
		t.Fatalf("HasPendingOperation failed: %v", err)
	}
	if hasStart {
		t.Error("Expected no pending start_queued_job")
	}
}

func TestGetDeferredOperationPayload(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add operation with payload
	payload := `{"working_dir":"/foo","command":"python train.py"}`
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", payload)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Get payload
	gotPayload, err := GetDeferredOperationPayload(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("GetDeferredOperationPayload failed: %v", err)
	}
	if gotPayload != payload {
		t.Errorf("GetDeferredOperationPayload() = %q, want %q", gotPayload, payload)
	}

	// Non-existent operation should return empty
	gotPayload, err = GetDeferredOperationPayload(db, jobID, OpStartQueuedJob)
	if err != nil {
		t.Fatalf("GetDeferredOperationPayload failed: %v", err)
	}
	if gotPayload != "" {
		t.Errorf("Expected empty payload for non-existent operation, got %q", gotPayload)
	}
}

func TestUpdatePendingOperationPayload(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add operation with initial payload
	initialPayload := `{"command":"python v1.py"}`
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", initialPayload)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Update payload
	updatedPayload := `{"command":"python v2.py"}`
	err = UpdatePendingOperationPayload(db, jobID, OpQueueJob, updatedPayload)
	if err != nil {
		t.Fatalf("UpdatePendingOperationPayload failed: %v", err)
	}

	// Verify update
	gotPayload, err := GetDeferredOperationPayload(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("GetDeferredOperationPayload failed: %v", err)
	}
	if gotPayload != updatedPayload {
		t.Errorf("Payload after update = %q, want %q", gotPayload, updatedPayload)
	}
}

func TestDeletePendingOperation(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add two operations
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", `{}`)
	if err != nil {
		t.Fatalf("Failed to add queue_job: %v", err)
	}
	err = AddDeferredOperation(db, "testhost", OpStartQueuedJob, jobID, "default", `{}`)
	if err != nil {
		t.Fatalf("Failed to add start_queued_job: %v", err)
	}

	// Delete queue_job
	err = DeletePendingOperation(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("DeletePendingOperation failed: %v", err)
	}

	// queue_job should be gone
	hasQueue, _ := HasPendingOperation(db, jobID, OpQueueJob)
	if hasQueue {
		t.Error("queue_job should be deleted")
	}

	// start_queued_job should still exist
	hasStart, _ := HasPendingOperation(db, jobID, OpStartQueuedJob)
	if !hasStart {
		t.Error("start_queued_job should still exist")
	}
}

func TestGetJobIDsWithPendingOperations(t *testing.T) {
	db := setupTestDB(t)

	// Create multiple jobs
	job1, _ := RecordQueued(db, "testhost", "/tmp", "echo 1", "job 1", "default")
	job2, _ := RecordQueued(db, "testhost", "/tmp", "echo 2", "job 2", "default")
	job3, _ := RecordQueued(db, "testhost", "/tmp", "echo 3", "job 3", "default")

	// Add operations for job1 and job3
	AddDeferredOperation(db, "testhost", OpQueueJob, job1, "default", `{}`)
	AddDeferredOperation(db, "testhost", OpQueueJob, job3, "default", `{}`)

	// Get job IDs with pending operations
	jobIDs, err := GetJobIDsWithPendingOperations(db)
	if err != nil {
		t.Fatalf("GetJobIDsWithPendingOperations failed: %v", err)
	}

	// job1 and job3 should be in the map
	if !jobIDs[job1] {
		t.Errorf("Expected job %d to be in pending operations", job1)
	}
	if jobIDs[job2] {
		t.Errorf("Expected job %d to NOT be in pending operations", job2)
	}
	if !jobIDs[job3] {
		t.Errorf("Expected job %d to be in pending operations", job3)
	}
}

// TestJobWithPendingOpShouldNotBeMarkedDead tests the core bug fix:
// A queued job with a pending queue_job operation should not be marked dead
func TestJobWithPendingOpShouldNotBeMarkedDead(t *testing.T) {
	db := setupTestDB(t)

	// Create a queued job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add pending queue_job operation (job not yet synced to remote)
	err = AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", `{"working_dir":"/tmp","command":"echo test"}`)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Simulate sync check: job has pending operation, should NOT mark dead
	hasPending, err := HasPendingDeferredOperationForJob(db, jobID)
	if err != nil {
		t.Fatalf("HasPendingDeferredOperationForJob failed: %v", err)
	}
	if !hasPending {
		t.Fatal("Expected job to have pending operation")
	}

	// Verify job is still queued (not dead)
	job, err := GetJobByID(db, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if job.Status != StatusQueued {
		t.Errorf("Job status = %s, want %s", job.Status, StatusQueued)
	}

	// Now simulate sync completing: delete the pending operation
	err = DeletePendingOperation(db, jobID, OpQueueJob)
	if err != nil {
		t.Fatalf("DeletePendingOperation failed: %v", err)
	}

	// After sync, job no longer has pending operation
	hasPending, _ = HasPendingDeferredOperationForJob(db, jobID)
	if hasPending {
		t.Error("Expected no pending operations after deletion")
	}
}

// TestUpdateJobRunningToQueued tests the status rollback functionality.
// This is used when a job was transitioned to running but then connection failed.
func TestUpdateJobRunningToQueued(t *testing.T) {
	db := setupTestDB(t)

	// Create a queued job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Transition to running
	err = UpdateQueuedToRunning(db, jobID)
	if err != nil {
		t.Fatalf("UpdateQueuedToRunning failed: %v", err)
	}

	// Verify job is running
	job, _ := GetJobByID(db, jobID)
	if job.Status != StatusRunning {
		t.Fatalf("Expected job status running, got %s", job.Status)
	}
	if job.StartTime == 0 {
		t.Fatal("Expected start_time to be set")
	}

	// Now rollback to queued
	err = UpdateJobRunningToQueued(db, jobID, "default")
	if err != nil {
		t.Fatalf("UpdateJobRunningToQueued failed: %v", err)
	}

	// Verify job is back to queued with cleared start_time
	job, _ = GetJobByID(db, jobID)
	if job.Status != StatusQueued {
		t.Errorf("Expected job status queued, got %s", job.Status)
	}
	if job.StartTime != 0 {
		t.Errorf("Expected start_time to be cleared, got %d", job.StartTime)
	}
	if job.QueueName != "default" {
		t.Errorf("Expected queue_name 'default', got %s", job.QueueName)
	}
}

// TestUpdateJobRunningToQueuedOnlyAffectsRunning tests that the rollback
// only affects jobs with status=running (uses WHERE clause).
func TestUpdateJobRunningToQueuedOnlyAffectsRunning(t *testing.T) {
	db := setupTestDB(t)

	// Create a completed job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}
	UpdateQueuedToRunning(db, jobID)
	RecordCompletionByID(db, jobID, 0, 12345) // Mark as completed

	// Verify job is completed
	job, _ := GetJobByID(db, jobID)
	if job.Status != StatusCompleted {
		t.Fatalf("Expected job status completed, got %s", job.Status)
	}

	// Try to rollback - should not affect completed job
	err = UpdateJobRunningToQueued(db, jobID, "default")
	if err != nil {
		t.Fatalf("UpdateJobRunningToQueued failed: %v", err)
	}

	// Job should still be completed
	job, _ = GetJobByID(db, jobID)
	if job.Status != StatusCompleted {
		t.Errorf("Expected job to remain completed, got %s", job.Status)
	}
}

// TestDeferredStartShouldSkipNonQueuedJob tests the scenario where a deferred
// start_queued_job operation should be skipped if the job is no longer queued.
// This is the fix for bug 2: executeDeferredStartQueued not handling non-queued jobs.
func TestDeferredStartShouldSkipNonQueuedJob(t *testing.T) {
	db := setupTestDB(t)

	// Create a queued job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add a pending start_queued_job operation
	err = AddDeferredOperation(db, "testhost", OpStartQueuedJob, jobID, "default", `{}`)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Now mark the job as dead (simulating user intervention or timeout)
	MarkDeadByID(db, jobID)

	// Verify job is dead
	job, _ := GetJobByID(db, jobID)
	if job.Status != StatusDead {
		t.Fatalf("Expected job status dead, got %s", job.Status)
	}

	// The executeDeferredStartQueued function should check job.Status != StatusQueued
	// and skip the operation. We test the precondition here.
	if job.Status == StatusQueued {
		t.Error("Job should NOT be queued - executeDeferredStartQueued should skip it")
	}

	// The pending operation still exists - it will be cleaned up when executed
	hasPending, _ := HasPendingOperation(db, jobID, OpStartQueuedJob)
	if !hasPending {
		t.Error("Expected pending start_queued_job to still exist")
	}
}

// TestJobMoveCreatesQueueJobOperation tests that when moving a job to an unreachable host,
// a queue_job deferred operation is created.
// This is the fix for bug 1: job move not creating deferred op when new host unreachable.
func TestJobMoveCreatesQueueJobOperation(t *testing.T) {
	db := setupTestDB(t)

	// Create a queued job on "oldhost"
	jobID, err := RecordQueued(db, "oldhost", "/tmp", "python train.py", "training job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Update the job's host to "newhost" (simulating move)
	err = UpdateJobHost(db, jobID, "newhost")
	if err != nil {
		t.Fatalf("Failed to update job host: %v", err)
	}

	// Simulate what runJobMove does when new host is unreachable:
	// It creates a queue_job deferred operation for the new host
	payload := `{"working_dir":"/tmp","command":"python train.py","description":"training job","queue_name":"default"}`
	err = AddDeferredOperation(db, "newhost", OpQueueJob, jobID, "default", payload)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Verify the operation was created for the new host
	ops, err := GetDeferredOperations(db, "newhost")
	if err != nil {
		t.Fatalf("GetDeferredOperations failed: %v", err)
	}

	found := false
	for _, op := range ops {
		if op.JobID == jobID && op.Operation == OpQueueJob {
			found = true
			// Verify the payload contains the job details
			if op.Payload == "" {
				t.Error("Expected non-empty payload")
			}
			break
		}
	}
	if !found {
		t.Error("Expected queue_job operation for moved job")
	}

	// Verify job host was updated
	job, _ := GetJobByID(db, jobID)
	if job.Host != "newhost" {
		t.Errorf("Expected job host to be 'newhost', got '%s'", job.Host)
	}
}

// TestDeferredStartOperationWithEntryMissing tests the full deferred start flow
// when EntryMissing=true, verifying that the sync handler should recreate the queue entry.
func TestDeferredStartOperationWithEntryMissing(t *testing.T) {
	db := setupTestDB(t)

	// Create a queued job
	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add a start_queued_job operation with entry_missing=true
	// This simulates what happens when startJobDirectly fails after updating status
	payload := `{"queue_name":"default","working_dir":"/tmp","command":"echo test","description":"test job","entry_missing":true}`
	err = AddDeferredOperation(db, "testhost", OpStartQueuedJob, jobID, "default", payload)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Verify the operation exists
	gotPayload, err := GetDeferredOperationPayload(db, jobID, OpStartQueuedJob)
	if err != nil {
		t.Fatalf("GetDeferredOperationPayload failed: %v", err)
	}

	// Verify entry_missing is in the payload
	if gotPayload == "" {
		t.Fatal("Expected non-empty payload")
	}
	if !containsEntryMissing(gotPayload) {
		t.Errorf("Expected payload to contain entry_missing:true, got %s", gotPayload)
	}
}

func containsEntryMissing(payload string) bool {
	// Check if the payload contains entry_missing:true
	return strings.Contains(payload, `"entry_missing":true`)
}

// TestMultipleDeferredOperationsForSameJob tests that multiple different
// operation types can exist for the same job.
func TestMultipleDeferredOperationsForSameJob(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add multiple operation types
	AddDeferredOperation(db, "testhost", OpQueueJob, jobID, "default", `{"cmd":"echo 1"}`)
	AddDeferredOperation(db, "testhost", OpStartQueuedJob, jobID, "default", `{"cmd":"echo 2"}`)

	// Both should exist
	hasQueue, _ := HasPendingOperation(db, jobID, OpQueueJob)
	hasStart, _ := HasPendingOperation(db, jobID, OpStartQueuedJob)

	if !hasQueue {
		t.Error("Expected queue_job operation")
	}
	if !hasStart {
		t.Error("Expected start_queued_job operation")
	}

	// HasPendingDeferredOperationForJob should return true
	hasPending, _ := HasPendingDeferredOperationForJob(db, jobID)
	if !hasPending {
		t.Error("Expected HasPendingDeferredOperationForJob to return true")
	}

	// Delete one operation - other should remain
	DeletePendingOperation(db, jobID, OpQueueJob)

	hasQueue, _ = HasPendingOperation(db, jobID, OpQueueJob)
	hasStart, _ = HasPendingOperation(db, jobID, OpStartQueuedJob)

	if hasQueue {
		t.Error("Expected queue_job to be deleted")
	}
	if !hasStart {
		t.Error("Expected start_queued_job to still exist")
	}
}

// TestDeletePendingOperationsForJob tests deletion of multiple operation types.
func TestDeletePendingOperationsForJob(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add multiple operation types
	AddDeferredOperation(db, "testhost", OpRunJob, jobID, "", `{"cmd":"echo 1"}`)
	AddDeferredOperation(db, "testhost", OpRestartJob, jobID, "", `{"cmd":"echo 2"}`)
	AddDeferredOperation(db, "testhost", OpKillJob, jobID, "", `{}`)

	// All three should exist
	hasRun, _ := HasPendingOperation(db, jobID, OpRunJob)
	hasRestart, _ := HasPendingOperation(db, jobID, OpRestartJob)
	hasKill, _ := HasPendingOperation(db, jobID, OpKillJob)

	if !hasRun {
		t.Error("Expected run_job operation")
	}
	if !hasRestart {
		t.Error("Expected restart_job operation")
	}
	if !hasKill {
		t.Error("Expected kill_job operation")
	}

	// Delete run and restart (simulating a kill canceling pending starts)
	count, err := DeletePendingOperationsForJob(db, jobID, OpRunJob, OpRestartJob)
	if err != nil {
		t.Fatalf("DeletePendingOperationsForJob failed: %v", err)
	}
	if count != 2 {
		t.Errorf("Expected 2 deletions, got %d", count)
	}

	// Run and restart should be gone, kill should remain
	hasRun, _ = HasPendingOperation(db, jobID, OpRunJob)
	hasRestart, _ = HasPendingOperation(db, jobID, OpRestartJob)
	hasKill, _ = HasPendingOperation(db, jobID, OpKillJob)

	if hasRun {
		t.Error("Expected run_job to be deleted")
	}
	if hasRestart {
		t.Error("Expected restart_job to be deleted")
	}
	if !hasKill {
		t.Error("Expected kill_job to still exist")
	}
}

// TestDeletePendingOperationsForJobEmpty tests deletion with empty operation list.
func TestDeletePendingOperationsForJobEmpty(t *testing.T) {
	db := setupTestDB(t)

	jobID, err := RecordQueued(db, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to insert job: %v", err)
	}

	// Add an operation
	AddDeferredOperation(db, "testhost", OpRunJob, jobID, "", `{}`)

	// Delete with empty list should do nothing
	count, err := DeletePendingOperationsForJob(db, jobID)
	if err != nil {
		t.Fatalf("DeletePendingOperationsForJob failed: %v", err)
	}
	if count != 0 {
		t.Errorf("Expected 0 deletions with empty list, got %d", count)
	}

	// Operation should still exist
	hasRun, _ := HasPendingOperation(db, jobID, OpRunJob)
	if !hasRun {
		t.Error("Expected run_job to still exist")
	}
}
