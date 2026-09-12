package ops

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

// An installed worker that owns its environment must keep that declaration
// across queue-entry rebuilds: losing it would put the target project's uv
// sync back in front of the worker command (wb129).
func TestSetupPolicySurvivesQueueEntryRebuild(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:        "studio",
		WorkingDir:  "/tmp/project",
		Command:     "agent-execution-worker execute --provider omp",
		SetupPolicy: "none",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !job.SetupSkipsDetected() {
		t.Fatalf("stored setup policy = %q, want none", job.SetupPolicy)
	}

	entry, err := queueEntryForJob(database, job, nil, "")
	if err != nil {
		t.Fatalf("queueEntryForJob: %v", err)
	}
	if entry.SetupPolicy != "none" {
		t.Fatalf("rebuilt entry setup policy = %q, want none", entry.SetupPolicy)
	}

	commandJob := commandJobForQueueEntry(entry)
	if commandJob.SetupPolicy != "none" {
		t.Fatalf("command job setup policy = %q, want none", commandJob.SetupPolicy)
	}
}
