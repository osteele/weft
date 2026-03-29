package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestQueueJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "NO\n", "", 0
		}
		return "", "", 0
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
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

func TestQueueJob_PoolTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Mock pool-style timeout (host in error message, not adjacent to "connection")
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "SSH connection to test-host timed out", 1
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
	}
	result, err := QueueJob(database, params, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("QueueJob should defer on pool timeout, got error: %v", err)
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true for pool timeout")
	}
}

func TestQueueJob_ExtractsGPU(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "NO\n", "", 0
		}
		return "", "", 0
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
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "NO\n", "", 0
		}
		return "", "", 0
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

// Artifact env vars (WEFT_ARTIFACT_MANIFEST, etc.) are injected by the runner
// package at execution time, not at queue entry creation time. See
// runner.go:startJob and single.go:RunSingleJob.

func TestRequeueJob_RefreshesProjectMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	cfg := "inputs = [\"hf:config-model\"]\n\n[outputs]\ndirs = [\"results/\"]\n"
	if err := os.WriteFile(filepath.Join(workDir, ".weft.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	jobID, err := db.RecordQueued(database, "test-host", workDir, "python train.py", "needs refresh")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, time.Now().Unix(), jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	result, err := RequeueJob(database, job, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("RequeueJob failed: %v", err)
	}
	if !result.Deferred {
		t.Fatalf("expected deferred requeue")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if len(updated.Inputs) != 1 || updated.Inputs[0] != "hf:config-model" {
		t.Fatalf("updated inputs = %v, want [hf:config-model]", updated.Inputs)
	}
	if len(updated.OutputDirs) != 1 || updated.OutputDirs[0] != "results/" {
		t.Fatalf("updated output dirs = %v, want [results/]", updated.OutputDirs)
	}
}
