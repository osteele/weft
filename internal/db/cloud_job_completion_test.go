package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestNeedsCloudCompletionBackfill_TerminalIncomplete(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, start_time = ?, end_time = ?, exit_code = NULL, last_synced_status = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		StatusFailed, int64(100), int64(200), StatusRunning, jobID,
	); err != nil {
		t.Fatalf("seed terminal incomplete attempt: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true")
	}
}

func TestNeedsCloudCompletionBackfill_TerminalComplete(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1", time.Time{}, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if needs {
		t.Fatal("needs backfill = true, want false")
	}
}

func TestNeedsCloudCompletionBackfill_NonTerminal(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		StatusRunning, int64(100), jobID,
	); err != nil {
		t.Fatalf("seed running attempt: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if needs {
		t.Fatal("needs backfill = true, want false")
	}
}

// TestRecordCloudJobCompletion_ZeroEndTimeUsesMarkerLastModified verifies that
// when the completion JSON is unavailable (endTimeUnix=0) but the .complete
// marker's LastModified is provided, end_time falls back to the marker's time
// rather than wall-clock time.Now().
func TestRecordCloudJobCompletion_ZeroEndTimeUsesMarkerLastModified(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	markerTime := time.Unix(1700000000, 0)
	if _, err := RecordCloudJobCompletion(database, jobID, 0, 0, 0, "", markerTime, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var endTime, startTime sql.NullInt64
	var lastSynced sql.NullString
	if err := database.QueryRow(
		`SELECT start_time, end_time, last_synced_status FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&startTime, &endTime, &lastSynced); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if !endTime.Valid || endTime.Int64 != markerTime.Unix() {
		t.Fatalf("end_time = %v, want %d (marker LastModified)", endTime, markerTime.Unix())
	}
	if startTime.Valid && startTime.Int64 != 0 {
		t.Fatalf("start_time = %v, want NULL", startTime)
	}
	if lastSynced.Valid {
		t.Fatalf("last_synced_status = %q, want NULL (so backfill stays armed)", lastSynced.String)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true (marker-only path)")
	}
}

// TestRecordCloudJobCompletion_ZeroEndTimeNoMarker verifies that when no
// authoritative time source is available, end_time stays NULL rather than
// being silently substituted with wall-clock time.Now().
func TestRecordCloudJobCompletion_ZeroEndTimeNoMarker(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	if _, err := RecordCloudJobCompletion(database, jobID, 0, 0, 0, "", time.Time{}, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var endTime sql.NullInt64
	if err := database.QueryRow(
		`SELECT end_time FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&endTime); err != nil {
		t.Fatalf("query end_time: %v", err)
	}
	if endTime.Valid {
		t.Fatalf("end_time = %d, want NULL", endTime.Int64)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true")
	}
}

func TestRecordCloudJobCompletion_UsesRunIDInsteadOfLatestAttempt(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	firstLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch first: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, firstLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID first: %v", err)
	}
	firstRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE id = ?`,
		StatusRunning, int64(100), firstRunID,
	); err != nil {
		t.Fatalf("seed first running attempt: %v", err)
	}

	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, cloud_outcome = ?, end_time = ? WHERE id = ?`,
		StatusCanceled, AttemptOutcomeSuperseded, now, firstRunID,
	); err != nil {
		t.Fatalf("supersede first attempt: %v", err)
	}
	secondLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch second: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "", &secondLaunchID, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}
	secondRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}

	if _, err := RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1", time.Time{}, firstRunID); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var firstStatus, firstOutcome string
	var firstExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, cloud_outcome, exit_code FROM job_attempts WHERE id = ?`,
		firstRunID,
	).Scan(&firstStatus, &firstOutcome, &firstExit); err != nil {
		t.Fatalf("query first attempt: %v", err)
	}
	if firstStatus != StatusFailed || firstOutcome != AttemptOutcomeFailed || !firstExit.Valid || firstExit.Int64 != 1 {
		t.Fatalf("first attempt = status %q outcome %q exit %v, want failed/failed/1", firstStatus, firstOutcome, firstExit)
	}

	var secondStatus string
	var secondOutcome sql.NullString
	var secondExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, cloud_outcome, exit_code FROM job_attempts WHERE id = ?`,
		secondRunID,
	).Scan(&secondStatus, &secondOutcome, &secondExit); err != nil {
		t.Fatalf("query second attempt: %v", err)
	}
	if secondStatus != StatusQueued {
		t.Fatalf("second status = %q, want %q", secondStatus, StatusQueued)
	}
	if secondOutcome.Valid {
		t.Fatalf("second cloud_outcome = %q, want NULL", secondOutcome.String)
	}
	if secondExit.Valid {
		t.Fatalf("second exit_code = %d, want NULL", secondExit.Int64)
	}
}

// TestNeedsCloudCompletionBackfill_StartTimeZero is a regression guard for the
// case where an earlier marker-only sync wrote start_time=0 alongside a
// fabricated end_time. NeedsCloudCompletionBackfill must still return true so
// a later sync that finds the JSON can rewrite the row.
func TestNeedsCloudCompletionBackfill_StartTimeZero(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, start_time = 0, end_time = ?, exit_code = 0, last_synced_status = ?
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		StatusCompleted, int64(1700000000), StatusCompleted, jobID,
	); err != nil {
		t.Fatalf("seed start_time=0 row: %v", err)
	}

	needs, err := NeedsCloudCompletionBackfill(database, jobID)
	if err != nil {
		t.Fatalf("NeedsCloudCompletionBackfill: %v", err)
	}
	if !needs {
		t.Fatal("needs backfill = false, want true (start_time=0 is unknown)")
	}
}
