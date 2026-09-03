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

// The queued lifecycle event is emitted by the attempt-insert trigger, which
// snapshots project and submitter_session from the jobs row. Those values
// must be present in the initial INSERT; writing them later in the same
// transaction would leave the event with empty routing metadata.
func TestRecordQueuedJob_QueuedEventCarriesRoutingMetadata(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:             "test-host",
		WorkingDir:       "/tmp/project",
		Command:          "echo hello",
		Project:          "routed-project",
		SubmitterSession: "agent-review-daemon/v1/call-7",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob failed: %v", err)
	}

	var project, root, session string
	err = database.QueryRow(
		`SELECT project, project_root, submitter_session
		   FROM job_lifecycle_events
		  WHERE job_id = ? AND event_kind = 'job.status_changed' AND status = 'queued'`,
		jobID,
	).Scan(&project, &root, &session)
	if err != nil {
		t.Fatalf("queued lifecycle event: %v", err)
	}
	if project != "routed-project" {
		t.Errorf("event project = %q, want %q", project, "routed-project")
	}
	if session != "agent-review-daemon/v1/call-7" {
		t.Errorf("event submitter_session = %q", session)
	}
}
