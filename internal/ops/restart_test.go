package ops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestRestartJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create original job (completed)
	origJobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "original job")
	db.MarkRunningByID(database, origJobID)
	db.RecordCompletionByID(database, origJobID, 0, time.Now().Unix())
	origJob, _ := db.GetJobByID(database, origJobID)

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "printf", Stdout: ""},
	})

	params := RestartJobParams{
		OriginalJob: origJob,
	}
	result, err := RestartJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("RestartJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.JobID == origJobID {
		t.Error("expected new job ID to differ from original")
	}

	newJob, _ := db.GetJobByID(database, result.JobID)
	if newJob.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", newJob.Status)
	}
	if newJob.Command != origJob.Command {
		t.Errorf("expected command to be %q, got %q", origJob.Command, newJob.Command)
	}
}

func TestRestartJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create original job
	origJobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "original job")
	db.RecordCompletionByID(database, origJobID, 0, time.Now().Unix())
	origJob, _ := db.GetJobByID(database, origJobID)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	params := RestartJobParams{
		OriginalJob: origJob,
	}
	result, err := RestartJob(database, params, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("RestartJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	newJob, _ := db.GetJobByID(database, result.JobID)
	if newJob.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", newJob.Status)
	}
	if newJob.PendingStatus == nil || *newJob.PendingStatus != db.StatusQueued {
		t.Errorf("expected pending status to be queued, got %v", newJob.PendingStatus)
	}
}

// Note: RestartJob only makes a single SSH call via Reconcile (AppendCommand),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestRestartJob_QuickTimeout.

func TestRestartJob_OverrideParams(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create original job
	origJobID, _ := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "original job")
	db.RecordCompletionByID(database, origJobID, 0, time.Now().Unix())
	origJob, _ := db.GetJobByID(database, origJobID)

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "printf", Stdout: ""},
	})

	params := RestartJobParams{
		OriginalJob: origJob,
		WorkingDir:  "/new/dir",
		Command:     "echo new",
		Description: "new description",
	}
	result, err := RestartJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("RestartJob failed: %v", err)
	}

	newJob, _ := db.GetJobByID(database, result.JobID)
	if newJob.WorkingDir != "/new/dir" {
		t.Errorf("expected working dir to be /new/dir, got %s", newJob.WorkingDir)
	}
	if newJob.Command != "echo new" {
		t.Errorf("expected command to be 'echo new', got %s", newJob.Command)
	}
	if newJob.Description != "new description" {
		t.Errorf("expected description to be 'new description', got %s", newJob.Description)
	}
}

func TestRestartJob_RefreshesProjectMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	cfg := "inputs = [\"hf:config-model\"]\n\n[outputs]\ndirs = [\"results/\"]\n"
	if err := os.WriteFile(filepath.Join(workDir, ".weft.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	origJobID, _ := db.RecordJobStarting(database, "test-host", workDir, "python train.py", "original job")
	if err := db.SetJobInputs(database, origJobID, []string{"hf:explicit-model"}); err != nil {
		t.Fatalf("set original inputs: %v", err)
	}
	db.RecordCompletionByID(database, origJobID, 0, time.Now().Unix())
	origJob, _ := db.GetJobByID(database, origJobID)

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := RestartJob(database, RestartJobParams{OriginalJob: origJob}, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("RestartJob failed: %v", err)
	}

	newJob, err := db.GetJobByID(database, result.JobID)
	if err != nil {
		t.Fatalf("get restarted job: %v", err)
	}
	if len(newJob.Inputs) != 2 || newJob.Inputs[0] != "hf:config-model" || newJob.Inputs[1] != "hf:explicit-model" {
		t.Fatalf("restarted job inputs = %v, want [hf:config-model hf:explicit-model]", newJob.Inputs)
	}
	if len(newJob.OutputDirs) != 1 || newJob.OutputDirs[0] != "results/" {
		t.Fatalf("restarted job output dirs = %v, want [results/]", newJob.OutputDirs)
	}
}
