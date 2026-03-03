package ops

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestEnsureQueuedJobsOnRemote(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job with no LastSyncedStatus (simulates job recorded while host offline)
	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "unsynced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Verify precondition: LastSyncedStatus is empty
	if job.LastSyncedStatus == db.StatusQueued {
		t.Fatal("expected LastSyncedStatus != queued before test")
	}

	// Mock SSH so AppendJobToQueue succeeds
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	ensured, err := ensureQueuedJobsOnRemote(database, []*db.Job{job}, 5*time.Second)
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Errorf("expected 1 job ensured, got %d", ensured)
	}

	// Verify LastSyncedStatus is now queued
	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.LastSyncedStatus != db.StatusQueued {
		t.Errorf("expected LastSyncedStatus=queued, got %q", updated.LastSyncedStatus)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsAlreadySynced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "synced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mark as already synced
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// No SSH mock needed — should skip without making SSH calls
	ensured, err := ensureQueuedJobsOnRemote(database, []*db.Job{job}, 5*time.Second)
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (already synced), got %d", ensured)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsPendingStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "pending job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set a pending status (e.g., pending cancel)
	cancelStatus := db.StatusCanceled
	if err := db.SetPendingStatus(database, jobID, cancelStatus); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	ensured, err := ensureQueuedJobsOnRemote(database, []*db.Job{job}, 5*time.Second)
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (has pending status), got %d", ensured)
	}
}
