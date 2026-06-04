package db

import (
	"testing"
)

func TestListUnsyncedQueuedJobs_ReturnsUnsynced(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	jobs, err := ListUnsyncedQueuedJobs(database, "test-host")
	if err != nil {
		t.Fatalf("list unsynced: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 unsynced job, got %d", len(jobs))
	}
	if jobs[0].ID != jobID {
		t.Errorf("expected job ID %d, got %d", jobID, jobs[0].ID)
	}
}

func TestListUnsyncedQueuedJobs_ExcludesSynced(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := UpdateLastSyncedStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("update synced status: %v", err)
	}

	jobs, err := ListUnsyncedQueuedJobs(database, "test-host")
	if err != nil {
		t.Fatalf("list unsynced: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 unsynced jobs, got %d", len(jobs))
	}
}

func TestListUnsyncedQueuedJobs_ExcludesPending(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := SetPendingStatus(database, jobID, StatusCanceled); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	jobs, err := ListUnsyncedQueuedJobs(database, "test-host")
	if err != nil {
		t.Fatalf("list unsynced: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs (pending cancel), got %d", len(jobs))
	}
}

func TestListUnsyncedQueuedJobs_IncludesPendingQueued(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := SetPendingStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("set pending queued: %v", err)
	}

	jobs, err := ListUnsyncedQueuedJobs(database, "test-host")
	if err != nil {
		t.Fatalf("list unsynced: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 pending queued job, got %d", len(jobs))
	}
	if jobs[0].ID != jobID {
		t.Errorf("expected job ID %d, got %d", jobID, jobs[0].ID)
	}
}

func TestListUnsyncedQueuedJobs_FiltersByHost(t *testing.T) {
	database := SetupTestDB(t)

	_, err := RecordQueued(database, "host-a", "/tmp", "echo a", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	_, err = RecordQueued(database, "host-b", "/tmp", "echo b", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	jobs, err := ListUnsyncedQueuedJobs(database, "host-a")
	if err != nil {
		t.Fatalf("list unsynced: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for host-a, got %d", len(jobs))
	}
	if jobs[0].Host != "host-a" {
		t.Errorf("expected host=host-a, got %q", jobs[0].Host)
	}
}
