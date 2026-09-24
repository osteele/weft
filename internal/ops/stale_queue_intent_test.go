package ops

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// A dispatch pass stamps pending_status=queued before reconciling, and
// SetPendingStatus stamps the latest attempt even when it is closed. If the
// job finished first, that intent describes a run nobody asked for — and
// acting on it means issuing an sbatch or a queue append for an attempt that
// already has an exit code. Reconcile must take the remote's terminal state
// and drop the stale intent instead.
func TestReconcileStaleQueueIntentOnFinishedAttempt(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 1", "stale intent")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := db.RecordCompletionByIDWithTransition(database, jobID, 0, time.Now().Unix()); err != nil {
		t.Fatalf("record completion: %v", err)
	}
	if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	appended := false
	previousAppender := queueAppender
	t.Cleanup(func() { queueAppender = previousAppender })
	queueAppender = func(*sql.DB, *db.Job, time.Duration, string, string) error {
		appended = true
		return nil
	}
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		t.Errorf("reconcile contacted %s for a finished attempt: %s", host, cmd)
		return "", "", 0
	})

	result, err := Reconcile(database, job, db.StatusCompleted, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if appended {
		t.Error("queue append issued for an attempt that already completed")
	}
	if result.NewStatus != db.StatusCompleted {
		t.Errorf("result status = %q, want %q", result.NewStatus, db.StatusCompleted)
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusCompleted {
		t.Errorf("job status = %q, want %q", updated.Status, db.StatusCompleted)
	}
	if updated.PendingStatus != nil {
		t.Errorf("stale pending status survived: %q", *updated.PendingStatus)
	}
}
