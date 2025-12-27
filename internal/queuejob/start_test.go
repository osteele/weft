package queuejob

import (
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// mockExecCommand returns a mock exec.Cmd that fails immediately with a connection error.
// This prevents tests from actually calling SSH.
func mockExecCommand(name string, arg ...string) *exec.Cmd {
	// Return a command that will fail with "connection refused"
	return exec.Command("sh", "-c", "echo 'ssh: connect to host testhost port 22: Connection refused' >&2; exit 255")
}

// setupTestDB creates a temporary database for testing
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "remote-jobs-queuejob-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

	// Override db path for testing
	cleanup := db.SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}

func TestStartNowWithPendingQueueJob(t *testing.T) {
	// This test verifies that StartNow handles jobs that have a pending
	// queue_job operation (not yet synced to remote queue)

	// Mock SSH to avoid real network calls
	cleanup := ssh.SetExecCommand(mockExecCommand)
	defer cleanup()

	database := setupTestDB(t)

	// Create a queued job
	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Add a pending queue_job operation (simulating job created while offline)
	payload := `{"working_dir":"/tmp","command":"echo test","description":"test job"}`
	err = db.AddDeferredOperation(database, "testhost", db.OpQueueJob, jobID, "default", payload)
	if err != nil {
		t.Fatalf("Failed to add deferred operation: %v", err)
	}

	// Verify the pending operation exists
	hasPending, err := db.HasPendingOperation(database, jobID, db.OpQueueJob)
	if err != nil {
		t.Fatalf("HasPendingOperation failed: %v", err)
	}
	if !hasPending {
		t.Fatal("Expected job to have pending queue_job operation")
	}

	// Get the job for StartNow
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}

	// StartNow should detect the pending operation and handle it
	// SSH is mocked, so it will fail with a connection error and defer the start
	deferred, _ := StartNow(database, job)

	// The pending queue_job should be deleted (operation was consumed)
	hasPending, err = db.HasPendingOperation(database, jobID, db.OpQueueJob)
	if err != nil {
		t.Fatalf("HasPendingOperation failed: %v", err)
	}
	if hasPending {
		t.Error("Expected queue_job operation to be deleted after StartNow attempt")
	}

	// Since SSH failed, the start should have been deferred
	if !deferred {
		t.Error("Expected StartNow to return deferred=true when SSH fails")
	}
}

func TestStartNowWithNonQueuedJob(t *testing.T) {
	database := setupTestDB(t)

	// Create a running job (not queued)
	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Transition to running
	err = db.UpdateQueuedToRunning(database, jobID)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// StartNow should reject non-queued jobs
	_, err = StartNow(database, job)
	if err == nil {
		t.Error("Expected error when trying to start non-queued job")
	}
}

func TestStartNowWithNilJob(t *testing.T) {
	database := setupTestDB(t)

	_, err := StartNow(database, nil)
	if err == nil {
		t.Error("Expected error when job is nil")
	}
}

// TestDeferQueuedJobStartWithEntryRemoved tests that when entryRemoved=true,
// the job status is reverted from running to queued and EntryMissing is set in payload.
// This is the fix for bug 3: startJobDirectly not rolling back status on connection failure.
func TestDeferQueuedJobStartWithEntryRemoved(t *testing.T) {
	database := setupTestDB(t)

	// Create a queued job and transition to running (simulating what startJobDirectly does)
	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Simulate what startJobDirectly does: transition to running
	err = db.UpdateQueuedToRunning(database, jobID)
	if err != nil {
		t.Fatalf("Failed to update job status: %v", err)
	}

	// Verify job is now running
	job, _ := db.GetJobByID(database, jobID)
	if job.Status != db.StatusRunning {
		t.Fatalf("Expected job status to be running, got %s", job.Status)
	}

	// Now call deferQueuedJobStart with entryRemoved=true
	// This simulates connection failure after status was updated to running
	deferred, err := deferQueuedJobStart(database, job, "default", nil, true)
	if err != nil {
		t.Fatalf("deferQueuedJobStart failed: %v", err)
	}
	if !deferred {
		t.Error("Expected deferQueuedJobStart to return true (deferred)")
	}

	// Verify job status was reverted to queued
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusQueued {
		t.Errorf("Expected job status to be queued after rollback, got %s", job.Status)
	}

	// Verify a start_queued_job operation was added with EntryMissing=true
	hasPending, _ := db.HasPendingOperation(database, jobID, db.OpStartQueuedJob)
	if !hasPending {
		t.Error("Expected pending start_queued_job operation")
	}

	payload, _ := db.GetDeferredOperationPayload(database, jobID, db.OpStartQueuedJob)
	if payload == "" {
		t.Fatal("Expected non-empty payload")
	}
	if !strings.Contains(payload, `"entry_missing":true`) {
		t.Errorf("Expected payload to contain entry_missing:true, got %s", payload)
	}
}

// TestDeferQueuedJobStartWithoutEntryRemoved tests that when entryRemoved=false,
// the job status is NOT changed (entry still exists in remote queue).
func TestDeferQueuedJobStartWithoutEntryRemoved(t *testing.T) {
	database := setupTestDB(t)

	// Create a queued job (status stays queued)
	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Call deferQueuedJobStart with entryRemoved=false
	// This simulates connection failure before entry was removed from remote queue
	deferred, err := deferQueuedJobStart(database, job, "default", nil, false)
	if err != nil {
		t.Fatalf("deferQueuedJobStart failed: %v", err)
	}
	if !deferred {
		t.Error("Expected deferQueuedJobStart to return true (deferred)")
	}

	// Verify job status is still queued (no rollback needed)
	job, _ = db.GetJobByID(database, jobID)
	if job.Status != db.StatusQueued {
		t.Errorf("Expected job status to remain queued, got %s", job.Status)
	}

	// Verify payload does NOT have EntryMissing=true
	payload, _ := db.GetDeferredOperationPayload(database, jobID, db.OpStartQueuedJob)
	if strings.Contains(payload, `"entry_missing":true`) {
		t.Errorf("Expected payload to NOT contain entry_missing:true when entryRemoved=false")
	}
}

// TestDeferQueuedJobStartNoDuplicate tests that calling deferQueuedJobStart twice
// doesn't create duplicate pending operations.
func TestDeferQueuedJobStartNoDuplicate(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// First call
	_, err = deferQueuedJobStart(database, job, "default", nil, false)
	if err != nil {
		t.Fatalf("First deferQueuedJobStart failed: %v", err)
	}

	// Second call should not add another operation
	_, err = deferQueuedJobStart(database, job, "default", nil, false)
	if err != nil {
		t.Fatalf("Second deferQueuedJobStart failed: %v", err)
	}

	// Count operations - should only be 1
	ops, err := db.GetDeferredOperations(database, "testhost")
	if err != nil {
		t.Fatalf("GetDeferredOperations failed: %v", err)
	}

	startOps := 0
	for _, op := range ops {
		if op.Operation == db.OpStartQueuedJob && op.JobID == jobID {
			startOps++
		}
	}

	if startOps != 1 {
		t.Errorf("Expected exactly 1 start_queued_job operation, got %d", startOps)
	}
}
