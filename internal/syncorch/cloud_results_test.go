package syncorch

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
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
