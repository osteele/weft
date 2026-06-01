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

// TestForJobExplainsSourceProvenanceMismatchAsDispatchBlock validates that a
// queued job whose latest dispatch failed with a source_provenance_mismatch
// reason surfaces that reason through explain.ForJob (Layer A+B). Without
// this wiring the diagnose surface falls back to the generic "job is queued
// / wait" reply that triggered the original bug report.
func TestForJobExplainsSourceProvenanceMismatchAsDispatchBlock(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	now := time.Unix(20_000, 0)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-2*time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	reason := "source_provenance_mismatch: expected=991f6ad9, marker=d7531f4c"
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-30 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		Detail:     reason,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	x := ForJob(database, job, now)
	if x.State != "blocked" {
		t.Fatalf("State = %q, want blocked", x.State)
	}
	if !strings.Contains(x.PrimaryReason, "source_provenance_mismatch") {
		t.Fatalf("PrimaryReason = %q, want source_provenance_mismatch", x.PrimaryReason)
	}
	if !strings.Contains(x.PrimaryReason, "expected=991f6ad9") {
		t.Fatalf("PrimaryReason = %q, want expected=991f6ad9", x.PrimaryReason)
	}
	var hasReplan bool
	for _, opt := range x.Options {
		if opt.Label == "replan" {
			hasReplan = true
			break
		}
	}
	if !hasReplan {
		t.Fatalf("Options missing 'replan' choice: %+v", x.Options)
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
