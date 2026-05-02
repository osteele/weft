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

func TestHydrateRelaunchBlockedReasons_SurfacesWaitingOnProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python eval.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchSkippedWaitingOnProducer,
		JobID:     jobID,
		Detail:    `waiting for "output/model.pt" from wj1555 (running)`,
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
	want := `waiting for "output/model.pt" from wj1555 (running)`
	if jobs[0].QueueBlockedReason != want {
		t.Fatalf("QueueBlockedReason = %q, want %q", jobs[0].QueueBlockedReason, want)
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

func TestHydrateCloudQueuedRetryBlockedReasons_SurfacesRunawayAfterOrphanedMove(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	sourceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ?`,
		db.StatusCanceled, int64(1000), db.AttemptOutcomeSuperseded, jobID); err != nil {
		t.Fatalf("close initial attempt: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, end_time, cloud_outcome)
		VALUES (?, 2, ?, ?, 1000, 1200, ?)`, jobID, sourceID, db.StatusQueued, db.AttemptOutcomeSuperseded); err != nil {
		t.Fatalf("insert source attempt: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, end_time, cloud_outcome, predecessor_attempt_id)
		VALUES (?, 3, ?, ?, 1200, 1300, ?, (SELECT id FROM job_attempts WHERE job_id = ? AND attempt_number = 2))`,
		jobID, targetID, db.StatusCanceled, db.AttemptOutcomeOrphaned, jobID); err != nil {
		t.Fatalf("insert orphaned target attempt: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, predecessor_attempt_id)
		VALUES (?, 4, ?, ?, 1400, (SELECT id FROM job_attempts WHERE job_id = ? AND attempt_number = 3))`,
		jobID, sourceID, db.StatusQueued, jobID); err != nil {
		t.Fatalf("insert restored source attempt: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayBlocked,
		OccurredAt: 1350,
		Detail:     "project=<all>; paused: repeated launch failures without progress",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	HydrateCloudQueuedRetryBlockedReasons(database, []*db.Job{job})

	want := "new-instance retry blocked: paused: repeated launch failures without progress"
	if job.QueueBlockedReason != want {
		t.Fatalf("QueueBlockedReason = %q, want %q", job.QueueBlockedReason, want)
	}
}

func TestHydrateCloudQueuedRetryBlockedReasons_IgnoresNormalQueuedCloudJob(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayBlocked,
		OccurredAt: time.Now().Unix(),
		Detail:     "project=<all>; paused: repeated launch failures without progress",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	HydrateCloudQueuedRetryBlockedReasons(database, []*db.Job{job})

	if job.QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty", job.QueueBlockedReason)
	}
}

func TestHydrateCloudQueuedRetryBlockedReasons_IgnoresFreshRetryLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch failed: %v", err)
	}
	retryID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch retry: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE job_id = ?`,
		db.StatusCanceled, int64(1000), db.AttemptOutcomeSuperseded, jobID); err != nil {
		t.Fatalf("close initial attempt: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, end_time, cloud_outcome)
		VALUES (?, 2, ?, ?, 1000, 1300, ?)`, jobID, failedID, db.StatusCanceled, db.AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("insert orphaned attempt: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, predecessor_attempt_id)
		VALUES (?, 3, ?, ?, 1400, (SELECT id FROM job_attempts WHERE job_id = ? AND attempt_number = 2))`,
		jobID, retryID, db.StatusQueued, jobID); err != nil {
		t.Fatalf("insert fresh retry attempt: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayBlocked,
		OccurredAt: 1350,
		Detail:     "project=<all>; paused: repeated launch failures without progress",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	HydrateCloudQueuedRetryBlockedReasons(database, []*db.Job{job})

	if job.QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty for fresh retry launch", job.QueueBlockedReason)
	}
}

func TestHydrateInventoryDispatchBlockedReasons_AppliesLatestFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		OccurredAt: time.Now().Unix() + 60,
		Detail:     "source sync failed: rsync extra path runs/foo: No such file or directory",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	jobs, err := db.ListQueued(database, "cool30")
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	HydrateInventoryDispatchBlockedReasons(database, jobs)

	if len(jobs) != 1 {
		t.Fatalf("ListQueued returned %d jobs, want 1", len(jobs))
	}
	want := "source sync failed: rsync extra path runs/foo: No such file or directory"
	if jobs[0].QueueBlockedReason != want {
		t.Fatalf("QueueBlockedReason = %q, want %q", jobs[0].QueueBlockedReason, want)
	}
}

func TestHydrateInventoryDispatchBlockedReasons_OKClearsPriorFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	now := time.Now().Unix()
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventQueueDispatchFailed,
		JobID:      jobID,
		OccurredAt: now + 60,
		Detail:     "source sync failed: rsync ...",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent failed: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventQueueDispatchOK,
		JobID:      jobID,
		OccurredAt: now + 120,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent ok: %v", err)
	}

	jobs, err := db.ListQueued(database, "cool30")
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	HydrateInventoryDispatchBlockedReasons(database, jobs)

	if jobs[0].QueueBlockedReason != "" {
		t.Fatalf("QueueBlockedReason = %q, want empty after dispatch.ok", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateInventoryDispatchBlockedReasons_FailureAfterOKReappears(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	now := time.Now().Unix()
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchOK, JobID: jobID, OccurredAt: now + 60,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchFailed, JobID: jobID, OccurredAt: now + 120,
		Detail: "queue append failed: ssh: connection refused",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	jobs, err := db.ListQueued(database, "cool30")
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	HydrateInventoryDispatchBlockedReasons(database, jobs)

	if jobs[0].QueueBlockedReason != "queue append failed: ssh: connection refused" {
		t.Fatalf("QueueBlockedReason = %q, want fresh failure to surface", jobs[0].QueueBlockedReason)
	}
}

func TestHydrateInventoryDispatchBlockedReasons_RespectsExistingReason(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchFailed, JobID: jobID,
		Detail: "source sync failed: rsync ...",
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}

	jobs, err := db.ListQueued(database, "cool30")
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	jobs[0].QueueBlockedReason = "gpu gate: no GPU with 26GB free"
	HydrateInventoryDispatchBlockedReasons(database, jobs)

	if jobs[0].QueueBlockedReason != "gpu gate: no GPU with 26GB free" {
		t.Fatalf("QueueBlockedReason = %q, want pre-existing reason preserved", jobs[0].QueueBlockedReason)
	}
}
