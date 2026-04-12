package orchestration

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestKillOrCancelCloudJob_SetsRequestedStatusOnTerminalInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test cloud job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	if err := db.SetRequestedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("SetRequestedStatus(queued): %v", err)
	}

	message, err := KillOrCancelCloudJob(database, jobID, db.StatusKilled)
	if err != nil {
		t.Fatalf("KillOrCancelCloudJob: %v", err)
	}
	if !strings.Contains(message, "killed") {
		t.Fatalf("message = %q, expected kill wording", message)
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.EffectiveStatus() != db.StatusKilled {
		t.Fatalf("effective status = %q, want %q", updated.EffectiveStatus(), db.StatusKilled)
	}
}
