package explain

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestForJobExplainsInventoryDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now.Add(-12 * time.Minute)} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			Detail:     "source sync deferred (host unreachable): ssh timeout",
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if x.State != "blocked" {
		t.Fatalf("State = %q, want blocked", x.State)
	}
	if !x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = false, want true")
	}
	if !strings.Contains(x.PrimaryReason, "retry #2") {
		t.Fatalf("PrimaryReason = %q, want retry count", x.PrimaryReason)
	}
	if !strings.Contains(x.SuggestedAction, "replan") {
		t.Fatalf("SuggestedAction = %q, want replan", x.SuggestedAction)
	}
}

func TestForJobAutoReplanUsesStartOfDispatchBlockStreak(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{now.Add(-20 * time.Minute), now.Add(-12 * time.Minute), now.Add(-3 * time.Minute)} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			Detail:     "source sync deferred (host unreachable): ssh timeout",
		}); err != nil {
			t.Fatalf("InsertLifecycleEvent: %v", err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if !x.AutoReplanAllowed {
		t.Fatalf("AutoReplanAllowed = false, want true; suggested=%q", x.SuggestedAction)
	}
	if !strings.Contains(x.SuggestedAction, "replan") {
		t.Fatalf("SuggestedAction = %q, want replan", x.SuggestedAction)
	}
}

func TestLatestInventoryDispatchBlockClearedByOK(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-10 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		Detail:     "source sync failed",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent failed: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-5 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchOK,
		JobID:      jobID,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent ok: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if _, ok := LatestInventoryDispatchBlock(database, job, now); ok {
		t.Fatal("LatestInventoryDispatchBlock ok = true, want false after dispatch ok")
	}
}
