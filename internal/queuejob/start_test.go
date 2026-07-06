package queuejob

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
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
	jobID, err := db.RecordQueued(database, "testhost", "/tmp", "echo test", "test job")
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

func TestStartNowRejectsRentalInstanceJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp", "echo test", "rental job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	started, err := StartNow(database, job)
	if err == nil {
		t.Fatal("StartNow accepted a rental-instance job, want clear rejection")
	}
	if started {
		t.Fatal("started = true, want false")
	}
	if !strings.Contains(err.Error(), ids.FormatInstanceID(launchID)) ||
		!strings.Contains(err.Error(), "start-now only supports inventory-host queue jobs") {
		t.Fatalf("error = %q, want rental target and inventory-host guidance", err.Error())
	}
}
