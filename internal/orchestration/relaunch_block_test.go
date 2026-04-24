package orchestration

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestHydrateRelaunchBlockedReasons_AppliesToQueuedUnplacedJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:     db.EventRelaunchSkippedMaxAttempts,
		JobID:         jobID,
		AttemptNumber: 1,
		MaxAttempts:   3,
		Detail:        "first retry budget exceeded: elapsed 12h >= limit 45m",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	HydrateRelaunchBlockedReasons(database, jobs)

	if len(jobs) != 1 {
		t.Fatalf("ListUnplacedJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0].QueueBlockedReason != "first retry budget exceeded: elapsed 12h >= limit 45m" {
		t.Fatalf("QueueBlockedReason = %q", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateRelaunchBlockedReasons_DoesNotApplyToInventoryQueuedJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchSkippedMaxAttempts,
		JobID:     jobID,
		Detail:    "first retry budget exceeded: elapsed 12h >= limit 45m",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	jobs, err := db.ListQueued(database, "cool30")
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	HydrateRelaunchBlockedReasons(database, jobs)

	if len(jobs) != 1 {
		t.Fatalf("ListQueued returned %d jobs, want 1", len(jobs))
	}
	if jobs[0].QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateRelaunchBlockedReasons_IgnoresStalePriorQueueEpoch(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = 1000 WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("set initial queued_at: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchSkippedMaxAttempts,
		JobID:     jobID,
		OccurredAt: func() int64 {
			return 1100
		}(),
		Detail: "first retry budget exceeded: elapsed 12h >= limit 45m",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent old: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = 2000 WHERE job_id = ? AND end_time IS NULL`, jobID); err != nil {
		t.Fatalf("set new queued_at: %v", err)
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	HydrateRelaunchBlockedReasons(database, jobs)

	if len(jobs) != 1 {
		t.Fatalf("ListUnplacedJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0].QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty for stale skip event", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateRelaunchBlockedReasons_IgnoresStaleOfferError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	queuedAt := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ? AND end_time IS NULL`, queuedAt, jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}

	staleUnix := time.Now().Add(-30 * time.Minute).Unix()
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchSkippedOfferError,
		JobID:      jobID,
		OccurredAt: staleUnix,
		Detail:     "vastai SSL: UNEXPECTED_EOF_WHILE_READING",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent stale offer_error: %v", err)
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	HydrateRelaunchBlockedReasons(database, jobs)

	if len(jobs) != 1 {
		t.Fatalf("ListUnplacedJobs returned %d jobs, want 1", len(jobs))
	}
	if jobs[0].QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty for stale offer_error", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateRelaunchBlockedReasons_KeepsFreshOfferError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = 1000 WHERE job_id = ? AND end_time IS NULL`, jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET created_at = 1000 WHERE id = ?`, jobID); err != nil {
		t.Fatalf("set created_at: %v", err)
	}

	freshUnix := time.Now().Add(-30 * time.Second).Unix()
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchSkippedOfferError,
		JobID:      jobID,
		OccurredAt: freshUnix,
		Detail:     "vastai SSL: UNEXPECTED_EOF_WHILE_READING",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent fresh offer_error: %v", err)
	}

	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ListUnplacedJobs returned %d jobs, want 1", len(jobs))
	}
	HydrateRelaunchBlockedReasons(database, jobs)

	if jobs[0].QueueBlockedReason == "" {
		t.Fatalf("QueueBlockedReason empty, want fresh offer_error to surface")
	}
}
