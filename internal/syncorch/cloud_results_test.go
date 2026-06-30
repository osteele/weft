package syncorch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/status"
)

func TestListCloudAttemptSyncCandidates_AttemptScoped(t *testing.T) {
	database := db.SetupTestDB(t)

	liveQueuedJobID, liveQueuedRunID := createPlacedCloudJob(t, database, db.LaunchStatusRunning)
	terminalQueuedJobID, _ := createPlacedCloudJob(t, database, db.LaunchStatusFailed)
	syncedCompletedJobID, syncedCompletedRunID := createPlacedCloudJob(t, database, db.LaunchStatusCompleted)
	incompleteCompletedJobID, incompleteCompletedRunID := createPlacedCloudJob(t, database, db.LaunchStatusCompleted)

	setCloudAttemptTerminal(t, database, syncedCompletedRunID, db.StatusCompleted, true)
	setCloudAttemptTerminal(t, database, incompleteCompletedRunID, db.StatusCompleted, false)

	candidates, err := listCloudAttemptSyncCandidates(database)
	if err != nil {
		t.Fatalf("listCloudAttemptSyncCandidates: %v", err)
	}
	got := make(map[int64]cloudAttemptSyncCandidate, len(candidates))
	for _, c := range candidates {
		got[c.JobID] = c
	}

	if c, ok := got[liveQueuedJobID]; !ok {
		t.Fatalf("live queued job %d missing from candidates: %+v", liveQueuedJobID, candidates)
	} else if c.RunID != liveQueuedRunID {
		t.Fatalf("live queued run_id = %d, want %d", c.RunID, liveQueuedRunID)
	}
	if _, ok := got[terminalQueuedJobID]; ok {
		t.Fatalf("queued job on terminal launch %d included; candidates: %+v", terminalQueuedJobID, candidates)
	}
	if _, ok := got[syncedCompletedJobID]; ok {
		t.Fatalf("fully synced terminal job %d included; candidates: %+v", syncedCompletedJobID, candidates)
	}
	if c, ok := got[incompleteCompletedJobID]; !ok {
		t.Fatalf("incomplete terminal job %d missing from candidates: %+v", incompleteCompletedJobID, candidates)
	} else if c.RunID != incompleteCompletedRunID {
		t.Fatalf("incomplete terminal run_id = %d, want %d", c.RunID, incompleteCompletedRunID)
	}
}

func createPlacedCloudJob(t *testing.T, database *sql.DB, finalLaunchStatus string) (int64, int64) {
	t.Helper()
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if finalLaunchStatus != db.LaunchStatusRunning {
		if err := db.UpdateLaunchStatus(database, launchID, finalLaunchStatus); err != nil {
			t.Fatalf("UpdateLaunchStatus(%s): %v", finalLaunchStatus, err)
		}
	}
	runID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	return jobID, runID
}

func setCloudAttemptTerminal(t *testing.T, database *sql.DB, runID int64, jobStatus string, completeMetadata bool) {
	t.Helper()
	now := time.Now().Unix()
	var startTime any = int64(0)
	var endTime any = now
	var exitCode any = int64(0)
	var lastSyncedStatus any
	if completeMetadata {
		startTime = now - 60
		lastSyncedStatus = jobStatus
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, start_time = ?, end_time = ?, exit_code = ?, last_synced_status = ?
		 WHERE id = ?`,
		jobStatus, startTime, endTime, exitCode, lastSyncedStatus, runID,
	); err != nil {
		t.Fatalf("set terminal attempt: %v", err)
	}
}

func TestImportCloudTimeseriesFileUsesExplicitRunID(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "host1", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning: %v", err)
	}
	firstRunID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}

	if err := db.RequeueByID(database, jobID); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatalf("UpdateQueuedToRunning second run: %v", err)
	}
	secondRunID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}

	path := filepath.Join(t.TempDir(), "timeseries.jsonl")
	if err := os.WriteFile(path, []byte(`{"ts":5000,"cpu_pct":10}
`), 0o644); err != nil {
		t.Fatalf("write timeseries: %v", err)
	}

	if err := importCloudTimeseriesFile(database, jobID, firstRunID, path, "single"); err != nil {
		t.Fatalf("importCloudTimeseriesFile: %v", err)
	}

	firstSamples, err := db.GetTimeseriesByRun(database, firstRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun first: %v", err)
	}
	if len(firstSamples) != 1 || firstSamples[0].Ts != 5000 {
		t.Fatalf("first run samples = %+v, want ts=5000", firstSamples)
	}
	secondSamples, err := db.GetTimeseriesByRun(database, secondRunID)
	if err != nil {
		t.Fatalf("GetTimeseriesByRun second: %v", err)
	}
	if len(secondSamples) != 0 {
		t.Fatalf("second run samples = %+v, want none", secondSamples)
	}
}

func TestMarkStartedJobFromMarkerClearsSatisfiedPendingStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "studio", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("SetPendingStatus: %v", err)
	}

	const markerStartTime = int64(1760000000)
	if err := markStartedJobFromMarker(database, jobID, markerStartTime); err != nil {
		t.Fatalf("markStartedJobFromMarker: %v", err)
	}

	var (
		status           string
		pendingStatus    sql.NullString
		lastSyncedStatus sql.NullString
		startTime        sql.NullInt64
	)
	if err := database.QueryRow(
		`SELECT status, pending_status, last_synced_status, start_time
		 FROM job_attempts
		 WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&status, &pendingStatus, &lastSyncedStatus, &startTime); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if status != db.StatusRunning {
		t.Fatalf("status = %q, want %q", status, db.StatusRunning)
	}
	if pendingStatus.Valid {
		t.Fatalf("pending_status = %q, want NULL", pendingStatus.String)
	}
	if !lastSyncedStatus.Valid || lastSyncedStatus.String != db.StatusRunning {
		t.Fatalf("last_synced_status = %v, want %q", lastSyncedStatus, db.StatusRunning)
	}
	if !startTime.Valid || startTime.Int64 != markerStartTime {
		t.Fatalf("start_time = %v, want %d", startTime, markerStartTime)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.EffectiveStatus() != db.StatusRunning {
		t.Fatalf("EffectiveStatus = %q, want %q", job.EffectiveStatus(), db.StatusRunning)
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
// re-read and re-rejected on every sync pass: the .processed marker must be
// written so the .complete marker is not reprocessed.
func TestMarkRejectedCompletionProcessed_TransitionRejectionWritesMarker(t *testing.T) {
	w := &fakeMarkerWriter{}
	rejection := fmt.Errorf("record completion: %w", &status.InvalidTransitionError{
		From:   status.Completed,
		To:     status.Failed,
		Reason: "no rule exists for this transition",
	})

	if !markRejectedCompletionProcessed(context.Background(), w, 42, 7, rejection) {
		t.Fatal("markRejectedCompletionProcessed = false, want true for transition-validation rejection")
	}
	want := r2keys.JobAttemptProcessed(42, 7)
	if len(w.keys) != 1 || w.keys[0] != want {
		t.Fatalf("PutMarker keys = %v, want [%s]", w.keys, want)
	}
}

// TestMarkRejectedCompletionProcessed_TransientErrorLeavesMarkerUnprocessed
// verifies that transient errors (DB I/O, etc.) do NOT mark the completion
// processed, so a later sync pass can retry ingestion.
func TestMarkRejectedCompletionProcessed_TransientErrorLeavesMarkerUnprocessed(t *testing.T) {
	w := &fakeMarkerWriter{}
	if markRejectedCompletionProcessed(context.Background(), w, 42, 7, errors.New("database is locked")) {
		t.Fatal("markRejectedCompletionProcessed = true, want false for transient error")
	}
	if len(w.keys) != 0 {
		t.Fatalf("PutMarker keys = %v, want none", w.keys)
	}
}
