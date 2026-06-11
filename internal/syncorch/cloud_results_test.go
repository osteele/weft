package syncorch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/status"
)

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
