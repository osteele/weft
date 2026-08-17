package ops

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRecordQueuedJob_PersistsSubmitterSession(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:             "test-host",
		WorkingDir:       "/tmp/project",
		Command:          "echo hello",
		SubmitterSession: "claude-abc-123",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	got, err := db.JobSubmitterSession(database, jobID)
	if err != nil {
		t.Fatalf("JobSubmitterSession: %v", err)
	}
	if got != "claude-abc-123" {
		t.Errorf("submitter session = %q, want %q", got, "claude-abc-123")
	}
}

// A job recorded without a submitter (plain shell, or a process that is not the
// submitter) must read back empty rather than error, so notify degrades to a
// broadcast instead of losing the notification.
func TestRecordQueuedJob_NoSubmitterSessionReadsBackEmpty(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp/project",
		Command:    "echo hello",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	got, err := db.JobSubmitterSession(database, jobID)
	if err != nil {
		t.Fatalf("JobSubmitterSession: %v", err)
	}
	if got != "" {
		t.Errorf("submitter session = %q, want empty", got)
	}
}

func TestJobSubmitterSessionUnknownJobIsEmptyNotError(t *testing.T) {
	database := db.SetupTestDB(t)

	got, err := db.JobSubmitterSession(database, 999999)
	if err != nil {
		t.Fatalf("JobSubmitterSession: %v", err)
	}
	if got != "" {
		t.Errorf("submitter session = %q, want empty", got)
	}
}
