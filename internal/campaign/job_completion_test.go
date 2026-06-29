package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/status"
)

func TestFinalizeStuckJobsWithR2Check_RecoverFromR2(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch (instance already finished).
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning)
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix())

	// Mock CheckAndSyncJobComplete to simulate R2 having the .complete marker.
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID int64) bool {
		// Simulate recording the completion from R2.
		dbConn.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
			db.StatusCompleted, time.Now().Unix(), jobID)
		return true
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	// The job should be completed (recovered from R2), NOT dead.
	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusCompleted) {
		t.Errorf("job status = %q, want %q (should be recovered from R2, not marked dead)", status, db.StatusCompleted)
	}
}

func TestFinalizeStuckJobsWithR2Check_FallbackToDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning)
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix())

	// Mock CheckAndSyncJobComplete to simulate R2 NOT having the .complete marker.
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, _ int64) bool {
		return false
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	// The job should be dead (no R2 completion data available).
	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusDead) {
		t.Errorf("job status = %q, want %q (should be marked dead when R2 has no data)", status, db.StatusDead)
	}
}

func TestFinalizeStuckJobsWithR2Check_NilR2Client(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed launch.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// Create a job with a non-terminal attempt on the completed launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning)
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix())

	// With nil R2 client, should fall back to marking dead.
	repaired, err := FinalizeStuckJobsWithR2Check(database, nil)
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 1 {
		t.Errorf("repaired = %v, want [1]", repaired)
	}

	var status string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status)
	if status != string(db.StatusDead) {
		t.Errorf("job status = %q, want %q", status, db.StatusDead)
	}
}

func TestFinalizeStuckJobsWithR2Check_IgnoresQueuedAttemptOnCompletedLaunch(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, 1); err != nil {
		t.Fatalf("create job: %v", err)
	}
	db.CreateAttempt(database, 1, "", &instanceID, db.StatusQueued)

	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, _ int64) bool {
		t.Fatal("queued attempts must not be probed or finalized as stuck running jobs")
		return false
	}

	repaired, err := FinalizeStuckJobsWithR2Check(database, &r2.Client{})
	if err != nil {
		t.Fatalf("FinalizeStuckJobsWithR2Check: %v", err)
	}
	if len(repaired) != 0 {
		t.Fatalf("repaired = %v, want none for queued attempt", repaired)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("read attempt status: %v", err)
	}
	if status != string(db.StatusQueued) {
		t.Fatalf("job status = %q, want %q", status, db.StatusQueued)
	}
}

func TestSyncJobCompletionsFromR2_BackfillsTerminalLaunchAttempt(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	completedLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create completed launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, completedLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	completedRunID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID completed: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, cloud_outcome = ?, start_time = ?, end_time = ? WHERE id = ?`,
		db.StatusCanceled, db.AttemptOutcomeOrphaned, int64(100), int64(180), completedRunID,
	); err != nil {
		t.Fatalf("seed orphaned attempt: %v", err)
	}
	retryLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create retry launch: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &retryLaunchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt retry: %v", err)
	}

	origSync := reconcileCheckAndSyncJobCompleteRun
	t.Cleanup(func() { reconcileCheckAndSyncJobCompleteRun = origSync })
	var gotRunID int64
	reconcileCheckAndSyncJobCompleteRun = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, gotJobID, runID int64) bool {
		gotRunID = runID
		if gotJobID != jobID {
			t.Fatalf("jobID = %d, want %d", gotJobID, jobID)
		}
		if _, err := db.RecordCloudJobCompletion(dbConn, gotJobID, 0, 100, 200, "", "", time.Time{}, runID); err != nil {
			t.Fatalf("RecordCloudJobCompletion: %v", err)
		}
		return true
	}

	syncJobCompletionsFromR2(database, &r2.Client{}, completedLaunchID)

	if gotRunID != completedRunID {
		t.Fatalf("synced runID = %d, want %d", gotRunID, completedRunID)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
}

type fakeMarkerWriter struct {
	keys []string
}

func (f *fakeMarkerWriter) PutMarker(_ context.Context, key string) error {
	f.keys = append(f.keys, key)
	return nil
}

// TestMarkRejectedCompletionProcessed_TransitionRejectionWritesMarker is a
// regression test for completions rejected by transition validation being
// re-fetched and re-rejected on every reconcile pass: the .processed marker
// must be written so the .complete marker is not reprocessed.
func TestMarkRejectedCompletionProcessed_TransitionRejectionWritesMarker(t *testing.T) {
	w := &fakeMarkerWriter{}
	rejection := fmt.Errorf("record completion: %w", &status.InvalidTransitionError{
		From:   status.Completed,
		To:     status.Failed,
		Reason: "no rule exists for this transition",
	})

	if !MarkRejectedCompletionProcessed(context.Background(), w, 42, 7, rejection) {
		t.Fatal("MarkRejectedCompletionProcessed = false, want true for transition-validation rejection")
	}
	want := r2keys.JobAttemptProcessed(42, 7)
	if len(w.keys) != 1 || w.keys[0] != want {
		t.Fatalf("PutMarker keys = %v, want [%s]", w.keys, want)
	}
}

// TestMarkRejectedCompletionProcessed_TransientErrorLeavesMarkerUnprocessed
// verifies that transient errors (DB I/O, etc.) do NOT mark the completion
// processed, so a later reconcile pass can retry ingestion.
func TestMarkRejectedCompletionProcessed_TransientErrorLeavesMarkerUnprocessed(t *testing.T) {
	w := &fakeMarkerWriter{}
	if MarkRejectedCompletionProcessed(context.Background(), w, 42, 7, errors.New("database is locked")) {
		t.Fatal("MarkRejectedCompletionProcessed = true, want false for transient error")
	}
	if len(w.keys) != 0 {
		t.Fatalf("PutMarker keys = %v, want none", w.keys)
	}
}
