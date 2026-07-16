package db

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/status"
)

// TestRecordCloudJobCompletion_RetriesTransientLock proves the completion write
// is retried on SQLITE_BUSY rather than dropped. Before the retry wrapper was
// added, a lock burst surfaced as an error the caller logged and abandoned,
// leaving a finished job stuck in "running". The lock hook injects two transient
// busy responses; the write must still land and drive the job terminal.
func TestRecordCloudJobCompletion_RetriesTransientLock(t *testing.T) {
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

	busyLeft := 2
	recordCloudCompletionLockHook = func() error {
		if busyLeft > 0 {
			busyLeft--
			return busyErr()
		}
		return nil
	}
	t.Cleanup(func() { recordCloudCompletionLockHook = nil })

	gotLaunch, err := RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RecordCloudJobCompletion returned %v, want nil (transient lock must be retried, not dropped)", err)
	}
	if busyLeft != 0 {
		t.Fatalf("busyLeft = %d, want 0 (retry loop should have consumed both busy responses)", busyLeft)
	}
	if gotLaunch != launchID {
		t.Fatalf("launch id = %d, want %d", gotLaunch, launchID)
	}
	if !JobIsTerminal(database, jobID) {
		t.Fatal("job not terminal after completion; a dropped completion would leave it running")
	}
}

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
	if _, err := RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1", "", time.Time{}, 0); err != nil {
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
	if _, err := RecordCloudJobCompletion(database, jobID, 0, 0, 0, "", "", markerTime, 0); err != nil {
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

	if _, err := RecordCloudJobCompletion(database, jobID, 0, 0, 0, "", "", time.Time{}, 0); err != nil {
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

	if _, err := RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1", "", time.Time{}, firstRunID); err != nil {
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

func TestRecordCloudJobCompletion_IgnoresAbandonedMoveTargetAttempt(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	sourceLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, sourceLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	targetLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	dst := targetLaunchID
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     MoveTargetExisting,
		TargetLaunchID: &dst,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetRunID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &targetLaunchID, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := AbandonMoveLoser(database, intent.ID, targetRunID, AttemptAbandonedMoveSourceWon); err != nil {
		t.Fatalf("AbandonMoveLoser: %v", err)
	}

	gotLaunch, err := RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, targetRunID)
	if !errors.Is(err, ErrCloudCompletionAttemptAbandoned) {
		t.Fatalf("RecordCloudJobCompletion abandoned target error = %v, want ErrCloudCompletionAttemptAbandoned", err)
	}
	if gotLaunch != 0 {
		t.Fatalf("abandoned completion launch id = %d, want 0", gotLaunch)
	}

	var targetStatus string
	var targetExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, exit_code FROM job_attempts WHERE id = ?`,
		targetRunID,
	).Scan(&targetStatus, &targetExit); err != nil {
		t.Fatalf("query target attempt: %v", err)
	}
	if targetStatus != StatusQueued || targetExit.Valid {
		t.Fatalf("target attempt = status %q exit %v, want queued/NULL after ignored completion", targetStatus, targetExit)
	}

	gotLaunch, err = RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RecordCloudJobCompletion authoritative: %v", err)
	}
	if gotLaunch != sourceLaunchID {
		t.Fatalf("authoritative completion launch id = %d, want source launch %d", gotLaunch, sourceLaunchID)
	}

	var sourceStatus string
	var sourceExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, exit_code FROM job_attempts WHERE id = ?`,
		sourceRunID,
	).Scan(&sourceStatus, &sourceExit); err != nil {
		t.Fatalf("query source attempt: %v", err)
	}
	if sourceStatus != StatusCompleted || !sourceExit.Valid || sourceExit.Int64 != 0 {
		t.Fatalf("source attempt = status %q exit %v, want completed/0", sourceStatus, sourceExit)
	}
	if err := database.QueryRow(
		`SELECT status, exit_code FROM job_attempts WHERE id = ?`,
		targetRunID,
	).Scan(&targetStatus, &targetExit); err != nil {
		t.Fatalf("query target attempt after authoritative completion: %v", err)
	}
	if targetStatus != StatusQueued || targetExit.Valid {
		t.Fatalf("target attempt after authoritative completion = status %q exit %v, want queued/NULL", targetStatus, targetExit)
	}
}

func TestRepairOrphanedCompletedAttemptsSkipsAbandonedAttempts(t *testing.T) {
	database := SetupTestDB(t)

	sourceLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	inferredLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch inferred: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU job: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, sourceLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID job: %v", err)
	}
	peerJobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo peer", "peer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU peer: %v", err)
	}
	if err := SetJobLaunchID(database, peerJobID, sourceLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID peer: %v", err)
	}
	if _, err := CreateAttempt(database, peerJobID, "", &inferredLaunchID, StatusCompleted); err != nil {
		t.Fatalf("CreateAttempt peer inferred: %v", err)
	}

	orphanAttemptID, err := CreateAttempt(database, jobID, "", nil, StatusCompleted)
	if err != nil {
		t.Fatalf("CreateAttempt abandoned orphan: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET host = '', launch_id = NULL, abandoned_at = ?, abandoned_reason = ?
		 WHERE id = ?`,
		time.Now().Unix(), AttemptAbandonedMoveSourceWon, orphanAttemptID,
	); err != nil {
		t.Fatalf("seed abandoned orphan: %v", err)
	}

	if err := repairOrphanedCompletedAttempts(database); err != nil {
		t.Fatalf("repairOrphanedCompletedAttempts: %v", err)
	}

	var launchID sql.NullInt64
	if err := database.QueryRow(
		`SELECT launch_id FROM job_attempts WHERE id = ?`,
		orphanAttemptID,
	).Scan(&launchID); err != nil {
		t.Fatalf("query orphan attempt: %v", err)
	}
	if launchID.Valid {
		t.Fatalf("abandoned orphan launch_id = %d, want NULL", launchID.Int64)
	}
}

func TestRecordCloudJobCompletion_FinalizesLaterSameLaunchOpenAttempt(t *testing.T) {
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
	firstRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "", &launchID, StatusQueued); err != nil {
		t.Fatalf("CreateAttempt later: %v", err)
	}
	laterRunID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID later: %v", err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning later: %v", err)
	}

	if _, err := RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, firstRunID); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var laterStatus string
	var laterExit sql.NullInt64
	var laterEnd sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, exit_code, end_time FROM job_attempts WHERE id = ?`,
		laterRunID,
	).Scan(&laterStatus, &laterExit, &laterEnd); err != nil {
		t.Fatalf("query later attempt: %v", err)
	}
	if laterStatus != StatusCompleted || !laterExit.Valid || laterExit.Int64 != 0 || !laterEnd.Valid || laterEnd.Int64 != 200 {
		t.Fatalf("later attempt = status %q exit %v end %v, want completed/0/200", laterStatus, laterExit, laterEnd)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, StatusCompleted)
	}
}

func TestRecordCloudJobCompletion_SupersedesLaterOpenRetryAfterSuccess(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	firstLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
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
		`UPDATE job_attempts SET status = ?, start_time = ?, end_time = ?, cloud_outcome = ? WHERE id = ?`,
		StatusCanceled, int64(100), int64(180), AttemptOutcomeOrphaned, firstRunID,
	); err != nil {
		t.Fatalf("seed orphaned first attempt: %v", err)
	}
	secondLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
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

	if _, err := RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, firstRunID); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var firstStatus string
	var firstExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, exit_code FROM job_attempts WHERE id = ?`,
		firstRunID,
	).Scan(&firstStatus, &firstExit); err != nil {
		t.Fatalf("query first attempt: %v", err)
	}
	if firstStatus != StatusCompleted || !firstExit.Valid || firstExit.Int64 != 0 {
		t.Fatalf("first attempt = status %q exit %v, want completed/0", firstStatus, firstExit)
	}

	var secondStatus, secondOutcome string
	var secondExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, cloud_outcome, exit_code FROM job_attempts WHERE id = ?`,
		secondRunID,
	).Scan(&secondStatus, &secondOutcome, &secondExit); err != nil {
		t.Fatalf("query second attempt: %v", err)
	}
	if secondStatus != StatusCompleted || secondOutcome != AttemptOutcomeSuperseded || !secondExit.Valid || secondExit.Int64 != 0 {
		t.Fatalf("second attempt = status %q outcome %q exit %v, want completed/superseded/0", secondStatus, secondOutcome, secondExit)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, StatusCompleted)
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

// TestRecordCloudJobCompletion_FailedOverridesOrphanRecoveryGuess is a
// regression test for failed R2 completions being dropped: the agent's
// .complete marker is ground truth about the job's outcome, while orphan
// recovery's dead/killed is a guess. A non-zero-exit completion arriving after
// the attempt was already closed as dead or killed must override that guess.
func TestRecordCloudJobCompletion_FailedOverridesOrphanRecoveryGuess(t *testing.T) {
	for _, priorStatus := range []string{StatusDead, StatusKilled} {
		t.Run(priorStatus, func(t *testing.T) {
			database := SetupTestDB(t)

			jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
			if err != nil {
				t.Fatalf("RecordQueuedWithGPU: %v", err)
			}
			launchID, err := CreateLaunch(database, &Launch{
				Status:   LaunchStatusCompleted,
				Provider: "vastai",
				GPUSpec:  "RTX_4090",
			})
			if err != nil {
				t.Fatalf("CreateLaunch: %v", err)
			}
			if err := SetJobLaunchID(database, jobID, launchID); err != nil {
				t.Fatalf("SetJobLaunchID: %v", err)
			}
			// Orphan recovery won the race and closed the attempt.
			if _, err := database.Exec(
				`UPDATE job_attempts
				 SET status = ?, start_time = ?, end_time = ?, cloud_outcome = ?
				 WHERE job_id = ? AND end_time IS NULL`,
				priorStatus, int64(100), int64(150), AttemptOutcomeOrphaned, jobID,
			); err != nil {
				t.Fatalf("seed orphan-recovered attempt: %v", err)
			}

			if _, err := RecordCloudJobCompletion(database, jobID, 3, 100, 200, "exit_3", "", time.Time{}, 0); err != nil {
				t.Fatalf("RecordCloudJobCompletion: %v", err)
			}

			var gotStatus, gotOutcome string
			var gotExit, gotEnd sql.NullInt64
			if err := database.QueryRow(
				`SELECT status, cloud_outcome, exit_code, end_time
				 FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
				jobID,
			).Scan(&gotStatus, &gotOutcome, &gotExit, &gotEnd); err != nil {
				t.Fatalf("query attempt: %v", err)
			}
			if gotStatus != StatusFailed {
				t.Fatalf("status = %q, want %q", gotStatus, StatusFailed)
			}
			if gotOutcome != AttemptOutcomeFailed {
				t.Fatalf("cloud_outcome = %q, want %q", gotOutcome, AttemptOutcomeFailed)
			}
			if !gotExit.Valid || gotExit.Int64 != 3 {
				t.Fatalf("exit_code = %v, want 3 (marker's exit code)", gotExit)
			}
			if !gotEnd.Valid || gotEnd.Int64 != 200 {
				t.Fatalf("end_time = %v, want 200", gotEnd)
			}
		})
	}
}

// TestRecordCloudJobCompletion_StaleFailedDoesNotOverrideCompleted verifies
// the deliberate asymmetry in the authority rule: a stale failed .complete
// marker must never overwrite an attempt that already reached authoritative
// completed. The rejection error is a transition-validation error, which the
// sync layer treats as permanent (writes the .processed marker).
func TestRecordCloudJobCompletion_StaleFailedDoesNotOverrideCompleted(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := RecordCloudJobCompletion(database, jobID, 0, 100, 200, "", "", time.Time{}, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion (completed): %v", err)
	}

	_, err = RecordCloudJobCompletion(database, jobID, 1, 100, 200, "exit_1", "", time.Time{}, 0)
	if err == nil {
		t.Fatal("stale failed completion accepted over authoritative completed, want rejection")
	}
	var ite *status.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("error = %v (%T), want *status.InvalidTransitionError", err, err)
	}

	var gotStatus string
	var gotExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, exit_code FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&gotStatus, &gotExit); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if gotStatus != StatusCompleted || !gotExit.Valid || gotExit.Int64 != 0 {
		t.Fatalf("attempt = status %q exit %v, want completed/0 (unchanged)", gotStatus, gotExit)
	}
}

// TestRecordCloudJobCompletion_UserKillPreservesStopIntent verifies that a
// completion whose kill_reason is user_kill — i.e. the non-zero exit was
// produced by weft's own kill signal — backfills metadata without rewriting
// the user's killed/canceled status as failed. (The CLI stamps killed or
// canceled immediately when the user stops a cloud job; the agent's marker
// arrives later with the kill's exit code.)
func TestRecordCloudJobCompletion_UserKillPreservesStopIntent(t *testing.T) {
	for _, priorStatus := range []string{StatusKilled, StatusCanceled} {
		t.Run(priorStatus, func(t *testing.T) {
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
			// The CLI stamped the user's stop; exit metadata is not yet known.
			if _, err := database.Exec(
				`UPDATE job_attempts
				 SET status = ?, start_time = ?, end_time = ?, exit_code = NULL
				 WHERE job_id = ? AND end_time IS NULL`,
				priorStatus, int64(100), int64(150), jobID,
			); err != nil {
				t.Fatalf("seed user-stopped attempt: %v", err)
			}

			if _, err := RecordCloudJobCompletion(database, jobID, 143, 100, 200, "", KillReasonUserKill, time.Time{}, 0); err != nil {
				t.Fatalf("RecordCloudJobCompletion: %v", err)
			}

			var gotStatus, gotOutcome string
			var gotExit sql.NullInt64
			if err := database.QueryRow(
				`SELECT status, cloud_outcome, exit_code
				 FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
				jobID,
			).Scan(&gotStatus, &gotOutcome, &gotExit); err != nil {
				t.Fatalf("query attempt: %v", err)
			}
			if gotStatus != priorStatus {
				t.Fatalf("status = %q, want %q (user stop intent preserved)", gotStatus, priorStatus)
			}
			if gotOutcome != AttemptOutcomeCancelled {
				t.Fatalf("cloud_outcome = %q, want %q", gotOutcome, AttemptOutcomeCancelled)
			}
			if !gotExit.Valid || gotExit.Int64 != 143 {
				t.Fatalf("exit_code = %v, want 143 (backfilled from marker)", gotExit)
			}
		})
	}
}

// TestRecordCloudJobCompletion_UserKillOnOpenAttemptRecordsKilled verifies
// that a user_kill completion arriving while the attempt is still open
// (the CLI's status write was lost or raced) closes it as killed, not failed.
func TestRecordCloudJobCompletion_UserKillOnOpenAttemptRecordsKilled(t *testing.T) {
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

	if _, err := RecordCloudJobCompletion(database, jobID, 137, 100, 200, "", KillReasonUserKill, time.Time{}, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	var gotStatus string
	if err := database.QueryRow(
		`SELECT status FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&gotStatus); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if gotStatus != StatusKilled {
		t.Fatalf("status = %q, want %q", gotStatus, StatusKilled)
	}
}
