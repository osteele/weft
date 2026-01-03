package ops

import (
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
)

func TestKillJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	mockSSHCommands(t, []sshMockResponse{
		// ProbeRemoteStatus (TmuxSessionExistsQuickTimeout)
		{Contains: "tmux has-session", Stdout: "NO\n"},
		// applyKillToRemote
		{Contains: "tmux kill-session", Stdout: ""},
	})

	result, err := KillJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("KillJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job 1 killed" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusDead {
		t.Errorf("expected job status to be dead, got %s", updatedJob.Status)
	}
}

func TestKillJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock immediate connection failure (no sync attempted)
	cleanup := ssh.SetExecCommand(mockSSHExecCommand(func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	}))
	t.Cleanup(cleanup)

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("KillJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}
	if result.Message != "Job 1 kill pending (host unreachable)" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status == db.StatusDead {
		t.Errorf("expected job status not to be dead, got %s", updatedJob.Status)
	}
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusDead {
		t.Errorf("expected pending status to be dead, got %v", updatedJob.PendingStatus)
	}
}

func TestKillJob_SyncError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock: probe succeeds (session exists), then kill fails with connection error
	callCount := 0
	cleanup := ssh.SetExecCommand(mockSSHExecCommand(func(host, command string) (string, string, int) {
		callCount++
		if callCount == 1 {
			return "", "", 0 // tmux has-session succeeds (session exists)
		}
		// Kill command fails (connection dropped during apply)
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	}))
	t.Cleanup(cleanup)

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("KillJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusDead {
		t.Errorf("expected pending status to be dead, got %v", updatedJob.PendingStatus)
	}
}

func TestCancelQueuedJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test", "default")
	job, _ := db.GetJobByID(database, jobID)

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "flock", Stdout: ""},
	})

	result, err := CancelQueuedJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("CancelQueuedJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job 1 canceled" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusDead {
		t.Errorf("expected job status to be dead, got %s", updatedJob.Status)
	}
	if updatedJob.PendingStatus != nil {
		t.Errorf("expected pending status to be nil, got %v", updatedJob.PendingStatus)
	}
}

func TestCancelQueuedJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test", "default")
	job, _ := db.GetJobByID(database, jobID)

	// Mock immediate connection failure (no sync attempted)
	cleanup := ssh.SetExecCommand(mockSSHExecCommand(func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	}))
	t.Cleanup(cleanup)

	result, err := CancelQueuedJob(database, job, ExecuteOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("CancelQueuedJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	// CancelQueuedJob doesn't set Deferred flag, it returns a pending message
	if result.Message != "Job 1 cancel pending (host unreachable)" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusQueued {
		t.Errorf("expected job status to remain queued, got %s", updatedJob.Status)
	}
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusDead {
		t.Errorf("expected pending status to be dead, got %v", updatedJob.PendingStatus)
	}
}

// Note: CancelQueuedJob only makes a single SSH call (removeFromQueueFile),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestCancelQueuedJob_QuickTimeout.

func TestCancelQueuedJob_NotQueued(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	_, err := CancelQueuedJob(database, job, DefaultOptions())
	if err == nil {
		t.Fatal("expected error for non-queued job")
	}
}
