package ops

import (
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

func TestQueueJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "printf", Stdout: ""},
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
		QueueName:   "default",
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job 1 added to queue" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
	if job.LastSyncedStatus != db.StatusQueued {
		t.Errorf("expected last synced status to be queued, got %q", job.LastSyncedStatus)
	}
}

func TestQueueJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
	}
	result, err := QueueJob(database, params, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("QueueJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
	// LastSyncedStatus should be empty when deferred (not synced)
	if job.LastSyncedStatus != "" {
		t.Errorf("expected last synced status to be empty (not synced), got %q", job.LastSyncedStatus)
	}
}

// Note: QueueJob only makes a single SSH call (AppendCommand),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestQueueJob_QuickTimeout.

func TestQueueJob_ExtractsGPU(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "printf", Stdout: ""},
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "train",
		Description: "gpu job",
		EnvVars:     []string{"CUDA_VISIBLE_DEVICES=0,1", "OTHER_VAR=value"},
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.GPU != "0,1" {
		t.Errorf("expected GPU to be '0,1', got %q", job.GPU)
	}
}

func TestQueueJob_DefaultQueueName(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "printf", Stdout: ""},
	})

	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp",
		Command:    "echo",
		// QueueName omitted - should default to "default"
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.QueueName != "default" {
		t.Errorf("expected queue name to be 'default', got %q", job.QueueName)
	}
}
