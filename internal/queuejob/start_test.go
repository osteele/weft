package queuejob

import (
	"os/exec"
	"testing"

	"github.com/osteele/remote-jobs/internal/db"
)

// mockExecCommand returns a mock exec.Cmd that fails immediately with a connection error.
// This prevents tests from actually calling SSH.
func mockExecCommand(name string, arg ...string) *exec.Cmd {
	// Return a command that will fail with "connection refused"
	return exec.Command("sh", "-c", "echo 'ssh: connect to host testhost port 22: Connection refused' >&2; exit 255")
}

func TestStartNowWithNonQueuedJob(t *testing.T) {
	database := db.SetupTestDB(t)

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
	database := db.SetupTestDB(t)

	_, err := StartNow(database, nil)
	if err == nil {
		t.Error("Expected error when job is nil")
	}
}
