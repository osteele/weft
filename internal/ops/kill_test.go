package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
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
	if updatedJob.Status != db.StatusKilled {
		t.Errorf("expected job status to be killed, got %s", updatedJob.Status)
	}
}

func TestKillJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
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
	if updatedJob.Status == db.StatusKilled {
		t.Errorf("expected job status not to be killed, got %s", updatedJob.Status)
	}
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusKilled {
		t.Errorf("expected pending status to be killed, got %v", updatedJob.PendingStatus)
	}
}

func TestKillJob_SyncError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	db.MarkRunningByID(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	// Mock: probe succeeds (session exists), then kill fails with connection error
	callCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		callCount++
		if callCount == 1 {
			return "", "", 0 // tmux has-session succeeds (session exists)
		}
		// Kill command fails (connection dropped during apply)
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := KillJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
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
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusKilled {
		t.Errorf("expected pending status to be killed, got %v", updatedJob.PendingStatus)
	}
}

func TestCancelQueuedJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test", "default")
	job, _ := db.GetJobByID(database, jobID)

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "printf", Stdout: ""},
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
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status to be canceled, got %s", updatedJob.Status)
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
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := CancelQueuedJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
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
	if updatedJob.PendingStatus == nil || *updatedJob.PendingStatus != db.StatusCanceled {
		t.Errorf("expected pending status to be canceled, got %v", updatedJob.PendingStatus)
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

// TestKillQueueRunnerJob_TildeNotSingleQuoted verifies that the kill command
// for queue-runner jobs doesn't use single-quoted tilde paths, which would
// prevent shell expansion.
func TestKillQueueRunnerJob_TildeNotSingleQuoted(t *testing.T) {
	database := db.SetupTestDB(t)
	// Create a queue-runner job (no session name, has queue name)
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test", "default")
	// Transition to running (simulating queue runner starting it)
	db.MarkQueuedJobRunning(database, jobID)
	job, _ := db.GetJobByID(database, jobID)

	var capturedCommands []string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		capturedCommands = append(capturedCommands, command)
		return "", "", 0
	})

	_, err := KillJob(database, job, DefaultOptions())
	if err != nil {
		t.Fatalf("KillJob failed: %v", err)
	}

	// Check that no command contains single-quoted tilde path
	for _, cmd := range capturedCommands {
		if strings.Contains(cmd, "'~") {
			t.Errorf("Command contains single-quoted tilde (prevents shell expansion): %s", cmd)
		}
	}
}

// TestReconcileCancelKillsRunningProcess verifies that reconciling a job with
// pending=canceled issues both a cancel command and kills any running process.
// This handles the race condition where the queue runner starts the job
// between when we set pending status and when we reconcile.
func TestReconcileCancelKillsRunningProcess(t *testing.T) {
	database := db.SetupTestDB(t)
	// Create a queued job
	jobID, _ := db.RecordQueued(database, "test-host", "/tmp", "sleep 100", "test", "default")

	// Set pending status to canceled (simulating user requesting cancel)
	if err := db.SetPendingStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("SetPendingStatus failed: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	var capturedCommands []string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		capturedCommands = append(capturedCommands, command)
		return "", "", 0
	})

	// Use production path: SyncAndReconcile
	_, err := SyncAndReconcile(database, job, ReconcileOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("SyncAndReconcile failed: %v", err)
	}

	// Verify both cancel command AND kill commands were issued
	hasCancelCommand := false
	hasKillCommand := false
	for _, cmd := range capturedCommands {
		// Cancel command appended to .commands file
		if strings.Contains(cmd, ".commands") && strings.Contains(cmd, "cancel") {
			hasCancelCommand = true
		}
		if strings.Contains(cmd, "kill") && strings.Contains(cmd, ".pid") {
			hasKillCommand = true
		}
	}

	if !hasCancelCommand {
		t.Errorf("expected cancel command to be issued; got commands: %v", capturedCommands)
	}
	if !hasKillCommand {
		t.Error("expected kill command to be issued (for case where job started running)")
	}

	// Verify job status was updated
	updatedJob, _ := db.GetJobByID(database, jobID)
	if updatedJob.Status != db.StatusCanceled {
		t.Errorf("expected job status to be canceled, got %s", updatedJob.Status)
	}
}
