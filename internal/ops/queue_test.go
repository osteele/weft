package ops

import (
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

func TestQueueJob_IncludesArtifactEnvVars(t *testing.T) {
	database := db.SetupTestDB(t)

	// Capture the SSH command to verify artifact env vars are included
	var capturedCommand string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "jq -e") {
			return "NO\n", "", 0
		}
		if strings.Contains(command, "printf") {
			capturedCommand = command
		}
		return "", "", 0
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "my-command",
		Description: "artifact test",
		EnvVars:     []string{"MY_VAR=value"},
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	// Verify the queue command includes artifact env vars
	if !strings.Contains(capturedCommand, "RJ_JOB_ID=") {
		t.Error("expected queue command to contain RJ_JOB_ID env var")
	}
	if !strings.Contains(capturedCommand, "RJ_ARTIFACT_MANIFEST=") {
		t.Error("expected queue command to contain RJ_ARTIFACT_MANIFEST env var")
	}
	if !strings.Contains(capturedCommand, "RJ_ARTIFACT_ROOT=") {
		t.Error("expected queue command to contain RJ_ARTIFACT_ROOT env var")
	}

	// Verify job ID is correct in env var
	expectedJobID := result.JobID
	if !strings.Contains(capturedCommand, "RJ_JOB_ID="+string(rune('0'+expectedJobID))) {
		// For multi-digit IDs, just check it contains RJ_JOB_ID= followed by the ID
		if !strings.Contains(capturedCommand, "RJ_JOB_ID=1") {
			t.Errorf("expected RJ_JOB_ID to be %d", expectedJobID)
		}
	}
}

func TestAppendJobToQueue_IncludesArtifactEnvVars(t *testing.T) {
	// Capture the SSH command to verify artifact env vars are included
	var capturedCommand string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "printf") {
			capturedCommand = command
		}
		return "", "", 0
	})

	job := &db.Job{
		ID:          42,
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "test-command",
		Description: "test job",
		EnvVars:     []string{"USER_VAR=xyz"},
	}

	err := AppendJobToQueue(job, 5*time.Second)
	if err != nil {
		t.Fatalf("AppendJobToQueue failed: %v", err)
	}

	// Verify the queue command includes artifact env vars
	if !strings.Contains(capturedCommand, "RJ_JOB_ID=42") {
		t.Error("expected queue command to contain RJ_JOB_ID=42 env var")
	}
	if !strings.Contains(capturedCommand, "RJ_ARTIFACT_MANIFEST=") {
		t.Error("expected queue command to contain RJ_ARTIFACT_MANIFEST env var")
	}
	if !strings.Contains(capturedCommand, "RJ_ARTIFACT_ROOT=") {
		t.Error("expected queue command to contain RJ_ARTIFACT_ROOT env var")
	}

	// Verify user env vars are also included
	if !strings.Contains(capturedCommand, "USER_VAR=xyz") {
		t.Error("expected queue command to contain USER_VAR=xyz env var")
	}
}
