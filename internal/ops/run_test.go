package ops

import (
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
)

func TestRunJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "flock", Stdout: ""},
	})

	params := RunJobParams{
		Host:    "test-host",
		Command: "echo success",
	}
	result, err := RunJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("RunJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}

	job, _ := db.GetJobByID(database, 1)
	// RunJob queues the job; it starts when the queue runner processes it
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
}

func TestRunJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Mock immediate connection failure (no sync attempted)
	cleanup := ssh.SetExecCommand(mockSSHExecCommand(func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	}))
	t.Cleanup(cleanup)

	params := RunJobParams{
		Host:    "test-host",
		Command: "echo success",
	}
	result, err := RunJob(database, params, ExecuteOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	job, _ := db.GetJobByID(database, 1)
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
	if job.PendingStatus == nil || *job.PendingStatus != db.StatusQueued {
		t.Errorf("expected pending status to be queued, got %v", job.PendingStatus)
	}
}

// Note: RunJob only makes a single SSH call via Reconcile (AppendQueueEntry),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestRunJob_QuickTimeout.
