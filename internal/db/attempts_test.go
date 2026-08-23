package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestListAttempts_OrderAndFields(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(4001)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	// Close the auto-created attempt, then create two more. Should see 3 total.
	if err := CloseAttempt(database, jobID, StatusFailed, intPtr(1), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt #1: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "host-alpha", nil, StatusRunning); err != nil {
		t.Fatalf("CreateAttempt #2: %v", err)
	}
	if err := CloseAttempt(database, jobID, StatusCompleted, intPtr(0), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt #2: %v", err)
	}
	if _, err := CreateAttempt(database, jobID, "host-beta", nil, StatusRunning); err != nil {
		t.Fatalf("CreateAttempt #3: %v", err)
	}

	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("len(attempts) = %d, want 3", len(attempts))
	}

	// Newest first — attempt_number DESC.
	if attempts[0].AttemptNumber <= attempts[1].AttemptNumber ||
		attempts[1].AttemptNumber <= attempts[2].AttemptNumber {
		t.Fatalf("not in DESC order: %d,%d,%d",
			attempts[0].AttemptNumber, attempts[1].AttemptNumber, attempts[2].AttemptNumber)
	}

	if attempts[0].Host != "host-beta" || attempts[0].Status != StatusRunning {
		t.Fatalf("latest = %+v; want host=host-beta status=running", attempts[0])
	}
	if attempts[1].Host != "host-alpha" || attempts[1].Status != StatusCompleted {
		t.Fatalf("middle = %+v; want host=host-alpha status=completed", attempts[1])
	}
	if attempts[2].Status != StatusFailed {
		t.Fatalf("oldest status = %q, want %q", attempts[2].Status, StatusFailed)
	}
}

func TestListAttempts_EmptyForMissingJob(t *testing.T) {
	database := setupTestDB(t)
	attempts, err := ListAttempts(database, 9999)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("expected 0 attempts, got %d", len(attempts))
	}
}

func TestSetAttemptLaunch_NormalizesPendingPlacement(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(1001)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	if err := SetAttemptPendingStatus(database, jobID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if err := SetAttemptLaunch(database, jobID, launchID); err != nil {
		t.Fatalf("SetAttemptLaunch: %v", err)
	}

	var gotLaunchID int64
	var gotHost, gotPending string
	if err := database.QueryRow(
		`SELECT COALESCE(launch_id, 0), host, COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&gotLaunchID, &gotHost, &gotPending); err != nil {
		t.Fatalf("query attempt: %v", err)
	}

	if gotLaunchID != launchID {
		t.Fatalf("launch_id = %d, want %d", gotLaunchID, launchID)
	}
	if gotHost != LaunchHost(launchID) {
		t.Fatalf("host = %q, want %q", gotHost, LaunchHost(launchID))
	}
	if gotPending != StatusQueued {
		t.Fatalf("pending_status = %q, want %q", gotPending, StatusQueued)
	}
}

func TestMarkAttemptQueuedByIDDoesNotCloneTerminalCloudAttempt(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud", "")
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
	attemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		    SET status = ?, start_time = ?, end_time = ?, exit_code = ?, cloud_outcome = ?
		  WHERE id = ?`,
		StatusFailed, int64(100), int64(200), 1, AttemptOutcomeFailed, attemptID,
	); err != nil {
		t.Fatalf("seed terminal attempt: %v", err)
	}

	if err := MarkAttemptQueuedByID(database, jobID); err != nil {
		t.Fatalf("MarkAttemptQueuedByID: %v", err)
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if count != 1 {
		t.Fatalf("attempt count = %d, want 1; stale queued sync must not clone terminal attempts", count)
	}
	got, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got == nil || got.Status != StatusFailed {
		t.Fatalf("job status = %v, want failed", got)
	}
	if got.LatestRunID == nil || *got.LatestRunID != attemptID {
		t.Fatalf("latest run = %v, want original attempt %d", got.LatestRunID, attemptID)
	}
}

func TestAbandonDuplicateTerminalCloudAttempts(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud", "")
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
	keepAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		    SET status = ?, start_time = ?, end_time = ?, exit_code = ?, failure_reason = ?, cloud_outcome = ?
		  WHERE id = ?`,
		StatusFailed, int64(100), int64(200), 1, "boot failed", AttemptOutcomeFailed, keepAttemptID,
	); err != nil {
		t.Fatalf("seed authoritative terminal attempt: %v", err)
	}
	for attemptNumber := 2; attemptNumber <= 3; attemptNumber++ {
		if _, err := database.Exec(
			`INSERT INTO job_attempts (
				job_id, attempt_number, host, launch_id, target_id, status, queued_at,
				start_time, end_time, exit_code, failure_reason, cloud_outcome
			)
			SELECT job_id, ?, host, launch_id, target_id, status, queued_at,
			       start_time, end_time, exit_code, failure_reason, cloud_outcome
			  FROM job_attempts
			 WHERE id = ?`,
			attemptNumber, keepAttemptID,
		); err != nil {
			t.Fatalf("insert duplicate attempt %d: %v", attemptNumber, err)
		}
	}

	groups, err := FindDuplicateTerminalCloudAttemptGroups(database)
	if err != nil {
		t.Fatalf("FindDuplicateTerminalCloudAttemptGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("duplicate group count = %d, want 1: %+v", len(groups), groups)
	}
	if groups[0].JobID != jobID || groups[0].LaunchID != launchID || groups[0].Count != 3 || groups[0].KeepAttemptID != keepAttemptID {
		t.Fatalf("duplicate group = %+v, want job=%d launch=%d count=3 keep=%d", groups[0], jobID, launchID, keepAttemptID)
	}

	n, err := AbandonDuplicateTerminalCloudAttempts(database)
	if err != nil {
		t.Fatalf("AbandonDuplicateTerminalCloudAttempts: %v", err)
	}
	if n != 2 {
		t.Fatalf("abandoned rows = %d, want 2", n)
	}
	groups, err = FindDuplicateTerminalCloudAttemptGroups(database)
	if err != nil {
		t.Fatalf("FindDuplicateTerminalCloudAttemptGroups after repair: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("duplicate groups after repair = %+v, want none", groups)
	}

	var activeCount, abandonedCount int
	var abandonedReason string
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND abandoned_at IS NULL`,
		jobID,
	).Scan(&activeCount); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(abandoned_reason), '')
		   FROM job_attempts
		  WHERE job_id = ? AND abandoned_at IS NOT NULL`,
		jobID,
	).Scan(&abandonedCount, &abandonedReason); err != nil {
		t.Fatalf("count abandoned attempts: %v", err)
	}
	if activeCount != 1 || abandonedCount != 2 || abandonedReason != AttemptAbandonedDuplicateSameLaunchTerminal {
		t.Fatalf("active=%d abandoned=%d reason=%q, want active=1 abandoned=2 reason=%q",
			activeCount, abandonedCount, abandonedReason, AttemptAbandonedDuplicateSameLaunchTerminal)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.LatestRunID == nil || *job.LatestRunID != keepAttemptID {
		t.Fatalf("latest run after repair = %v, want kept attempt %d", job.LatestRunID, keepAttemptID)
	}
	if job.RetryCount != 0 {
		t.Fatalf("retry count after repair = %d, want 0", job.RetryCount)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ID != keepAttemptID {
		t.Fatalf("ListAttempts after repair = %+v, want only kept attempt %d", attempts, keepAttemptID)
	}
}

func TestAbandonedAttemptDoesNotDriveJobStatus(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	firstAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if err := CloseAttempt(database, jobID, StatusCompleted, intPtr(0), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt first: %v", err)
	}
	secondAttemptID, err := CreateAttempt(database, jobID, "cool100", nil, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}
	if err := AbandonAttempt(database, secondAttemptID, AttemptAbandonedMoveSourceWon, nil); err != nil {
		t.Fatalf("AbandonAttempt: %v", err)
	}

	physicalLatest, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID physical: %v", err)
	}
	if physicalLatest != secondAttemptID {
		t.Fatalf("physical latest attempt = %d, want abandoned attempt %d", physicalLatest, secondAttemptID)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != firstAttemptID {
		t.Fatalf("authoritative attempt = %d, want source attempt %d", authoritative, firstAttemptID)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.LatestRunID == nil || *job.LatestRunID != firstAttemptID {
		t.Fatalf("job latest run = %v, want %d", job.LatestRunID, firstAttemptID)
	}
	if job.Host != "cool30" || job.Status != StatusCompleted {
		t.Fatalf("job status = host %q status %q, want cool30 completed", job.Host, job.Status)
	}
}

func TestRetryAfterAbandonedAttemptBecomesAuthoritative(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	firstAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if err := CloseAttempt(database, jobID, StatusCompleted, intPtr(0), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt first: %v", err)
	}
	secondAttemptID, err := CreateAttempt(database, jobID, "cool100", nil, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}
	if err := AbandonAttempt(database, secondAttemptID, AttemptAbandonedMoveSourceWon, nil); err != nil {
		t.Fatalf("AbandonAttempt: %v", err)
	}
	thirdAttemptID, err := CreateAttempt(database, jobID, "studio", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt third: %v", err)
	}

	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != thirdAttemptID {
		t.Fatalf("authoritative attempt = %d, want retry attempt %d", authoritative, thirdAttemptID)
	}
	if authoritative == firstAttemptID {
		t.Fatalf("retry attempt should supersede older completed attempt")
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.LatestRunID == nil || *job.LatestRunID != thirdAttemptID {
		t.Fatalf("job latest run = %v, want %d", job.LatestRunID, thirdAttemptID)
	}
	if job.Host != "studio" || job.Status != StatusQueued {
		t.Fatalf("job status = host %q status %q, want studio queued", job.Host, job.Status)
	}
}

func TestAbandonMoveLoserRecordsIntentID(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	attemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := AbandonMoveLoser(database, intent.ID, attemptID, AttemptAbandonedMoveTargetWon); err != nil {
		t.Fatalf("AbandonMoveLoser: %v", err)
	}

	var reason string
	var intentID int64
	if err := database.QueryRow(
		`SELECT abandoned_reason, COALESCE(abandoned_by_intent_id, 0) FROM job_attempts WHERE id = ?`,
		attemptID,
	).Scan(&reason, &intentID); err != nil {
		t.Fatalf("query abandoned fields: %v", err)
	}
	if reason != AttemptAbandonedMoveTargetWon || intentID != intent.ID {
		t.Fatalf("abandoned fields = reason %q intent %d, want %q intent %d", reason, intentID, AttemptAbandonedMoveTargetWon, intent.ID)
	}
}

func TestOpenMoveTargetAttemptHiddenUntilAccepted(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := SetJobPlacementReasons(database, jobID, []string{`waiting for "output/nsweep_reps.tar" from wj5070 (running)`}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if targetAttemptID == sourceAttemptID {
		t.Fatalf("target attempt reused source id %d", sourceAttemptID)
	}

	physicalLatest, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID physical: %v", err)
	}
	if physicalLatest != targetAttemptID {
		t.Fatalf("physical latest = %d, want target %d", physicalLatest, targetAttemptID)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != sourceAttemptID {
		t.Fatalf("authoritative = %d, want source %d while intent open", authoritative, sourceAttemptID)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "cool30" || job.LatestRunID == nil || *job.LatestRunID != sourceAttemptID {
		t.Fatalf("job = host %q latest %v, want source cool30/%d", job.Host, job.LatestRunID, sourceAttemptID)
	}
	if len(job.PlacementReasons) != 0 {
		t.Fatalf("placement reasons after target attempt = %v, want cleared", job.PlacementReasons)
	}

	if err := SetJobPlacementReasons(database, jobID, []string{`waiting for "output/nsweep_reps.tar" from wj5070 (running)`}); err != nil {
		t.Fatalf("SetJobPlacementReasons before confirm: %v", err)
	}
	if err := ConfirmMoveTargetAccepted(database, intent.ID, "accepted"); err != nil {
		t.Fatalf("ConfirmMoveTargetAccepted: %v", err)
	}
	authoritative, err = GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID after confirm: %v", err)
	}
	if authoritative != targetAttemptID {
		t.Fatalf("authoritative after confirm = %d, want target %d", authoritative, targetAttemptID)
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("source abandoned reason: %v", err)
	}
	if sourceReason != AttemptAbandonedMoveTargetAccepted {
		t.Fatalf("source abandoned reason = %q, want %q", sourceReason, AttemptAbandonedMoveTargetAccepted)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after confirm: %v", err)
	}
	if len(job.PlacementReasons) != 0 {
		t.Fatalf("placement reasons after confirm = %v, want cleared", job.PlacementReasons)
	}
}

func TestMoveSourceCompletionAbandonsTargetAttempt(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := CloseAttempt(database, jobID, StatusCompleted, intPtr(0), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt source: %v", err)
	}

	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateObsoleted {
		t.Fatalf("intent state = %q, want obsoleted", gotIntent.State)
	}
	var targetReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&targetReason); err != nil {
		t.Fatalf("target abandoned reason: %v", err)
	}
	if targetReason != AttemptAbandonedMoveSourceWon {
		t.Fatalf("target abandoned reason = %q, want %q", targetReason, AttemptAbandonedMoveSourceWon)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != sourceAttemptID {
		t.Fatalf("authoritative = %d, want source %d", authoritative, sourceAttemptID)
	}
}

func TestMoveTargetCompletionAbandonsSourceAttempt(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusRunning)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = 0, end_time = ? WHERE id = ?`,
		StatusCompleted, time.Now().Unix(), targetAttemptID,
	); err != nil {
		t.Fatalf("complete target attempt: %v", err)
	}

	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != MoveIntentStateConfirmed {
		t.Fatalf("intent state = %q, want confirmed", gotIntent.State)
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("source abandoned reason: %v", err)
	}
	if sourceReason != AttemptAbandonedMoveTargetWon {
		t.Fatalf("source abandoned reason = %q, want %q", sourceReason, AttemptAbandonedMoveTargetWon)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != targetAttemptID {
		t.Fatalf("authoritative = %d, want target %d", authoritative, targetAttemptID)
	}
}

func TestPlacementDisplayQueuedAt_RestoredAfterNoStartMoveUsesSourceTime(t *testing.T) {
	database := setupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	jobID := int64(2101)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	if err := SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceQueuedAt := int64(1_000)
	var sourceAttemptID int64
	if err := database.QueryRow(`SELECT id FROM job_attempts WHERE job_id = ? AND launch_id = ?`, jobID, src).Scan(&sourceAttemptID); err != nil {
		t.Fatalf("source attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE id = ?`, sourceQueuedAt, sourceAttemptID); err != nil {
		t.Fatalf("set source queued_at: %v", err)
	}

	if err := TransferJobLaunchID(database, jobID, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	var targetAttemptID int64
	if err := database.QueryRow(`SELECT id FROM job_attempts WHERE job_id = ? AND launch_id = ?`, jobID, dst).Scan(&targetAttemptID); err != nil {
		t.Fatalf("target attempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE id = ?`,
		StatusCanceled, int64(2_000), AttemptOutcomeOrphaned, targetAttemptID,
	); err != nil {
		t.Fatalf("mark target orphaned: %v", err)
	}

	if err := SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("restore SetJobLaunchID source: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`, int64(3_000), jobID, src); err != nil {
		t.Fatalf("set restored queued_at: %v", err)
	}

	got, err := PlacementDisplayQueuedAt(database, []int64{jobID})
	if err != nil {
		t.Fatalf("PlacementDisplayQueuedAt: %v", err)
	}
	if got[jobID] != sourceQueuedAt {
		t.Fatalf("display queued_at = %d, want original source %d", got[jobID], sourceQueuedAt)
	}
}

func TestPlacementDisplayQueuedAt_RealMoveUsesLatestTime(t *testing.T) {
	database := setupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	jobID := int64(2102)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)
	if err := SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	if err := TransferJobLaunchID(database, jobID, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}
	latestQueuedAt := int64(4_000)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ? AND launch_id = ? AND end_time IS NULL`, latestQueuedAt, jobID, dst); err != nil {
		t.Fatalf("set latest queued_at: %v", err)
	}

	got, err := PlacementDisplayQueuedAt(database, []int64{jobID})
	if err != nil {
		t.Fatalf("PlacementDisplayQueuedAt: %v", err)
	}
	if got[jobID] != latestQueuedAt {
		t.Fatalf("display queued_at = %d, want latest %d", got[jobID], latestQueuedAt)
	}
}

func TestNormalizePendingPlacementForLaunch_SkipsCompletedNonOrphaned(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	plannedLaunchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusPlanned})
	if err != nil {
		t.Fatalf("CreateLaunch(planned): %v", err)
	}

	jobQueued := int64(2001)
	insertTestJob(t, database, jobQueued, "echo queued", "/tmp", StatusQueued, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobQueued, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(queued): %v", err)
	}

	jobCompleted := int64(2002)
	insertTestJob(t, database, jobCompleted, "echo completed", "/tmp", StatusCompleted, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobCompleted, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(completed): %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ? WHERE job_id = ?`, AttemptOutcomeCompleted, jobCompleted); err != nil {
		t.Fatalf("set cloud_outcome completed: %v", err)
	}

	jobOrphaned := int64(2003)
	insertTestJob(t, database, jobOrphaned, "echo orphaned", "/tmp", StatusCompleted, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobOrphaned, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(orphaned): %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ? WHERE job_id = ?`, AttemptOutcomeOrphaned, jobOrphaned); err != nil {
		t.Fatalf("set cloud_outcome orphaned: %v", err)
	}

	jobPlanned := int64(2004)
	insertTestJob(t, database, jobPlanned, "echo planned", "/tmp", StatusQueued, withLaunch(plannedLaunchID))
	if err := SetAttemptPendingStatus(database, jobPlanned, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(planned): %v", err)
	}

	normalized, err := NormalizePendingPlacementForLaunch(database, launchID)
	if err != nil {
		t.Fatalf("NormalizePendingPlacementForLaunch: %v", err)
	}
	if normalized != 2 {
		t.Fatalf("normalized = %d, want 2", normalized)
	}

	checkPending := func(jobID int64, want string) {
		t.Helper()
		var got string
		if err := database.QueryRow(
			`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
			jobID,
		).Scan(&got); err != nil {
			t.Fatalf("query pending_status for job %d: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("job %d pending_status = %q, want %q", jobID, got, want)
		}
	}

	checkPending(jobQueued, StatusQueued)
	checkPending(jobCompleted, StatusPendingPlacement)
	checkPending(jobOrphaned, StatusQueued)
	checkPending(jobPlanned, StatusPendingPlacement)
}

func TestNormalizeStalePendingPlacementNoLaunch(t *testing.T) {
	database := setupTestDB(t)

	staleID := int64(3001)
	insertTestJob(t, database, staleID, "echo stale", "/tmp", StatusQueued)
	if err := SetAttemptPendingStatus(database, staleID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(stale): %v", err)
	}
	stalePendingAt := time.Now().Unix() - stalePendingPlacementNoLaunchMaxAgeSeconds - 10
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		stalePendingAt, staleID,
	); err != nil {
		t.Fatalf("set stale pending_at: %v", err)
	}

	freshID := int64(3002)
	insertTestJob(t, database, freshID, "echo fresh", "/tmp", StatusQueued)
	if err := SetAttemptPendingStatus(database, freshID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(fresh): %v", err)
	}
	freshPendingAt := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		freshPendingAt, freshID,
	); err != nil {
		t.Fatalf("set fresh pending_at: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	anchoredID := int64(3003)
	insertTestJob(t, database, anchoredID, "echo anchored", "/tmp", StatusQueued, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, anchoredID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(anchored): %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		stalePendingAt, anchoredID,
	); err != nil {
		t.Fatalf("set anchored pending_at: %v", err)
	}

	updated, err := NormalizeStalePendingPlacementNoLaunch(database)
	if err != nil {
		t.Fatalf("NormalizeStalePendingPlacementNoLaunch: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	checkPending := func(jobID int64, want string) {
		t.Helper()
		var got string
		if err := database.QueryRow(
			`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
			jobID,
		).Scan(&got); err != nil {
			t.Fatalf("query pending_status for job %d: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("job %d pending_status = %q, want %q", jobID, got, want)
		}
	}

	checkPending(staleID, StatusQueued)
	checkPending(freshID, StatusPendingPlacement)
	checkPending(anchoredID, StatusPendingPlacement)
}

func TestSetJobLaunchID_PreservesCloudDependencyMetadata(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(4001)
	insertTestJob(t, database, jobID, "echo stage", "/tmp", StatusQueued)
	meta := &JobMetadata{
		Dependencies: &JobDependencyMetadata{
			CloudNeeds:         []string{"output/model.pt:1046"},
			CloudAfter:         []JobDependencyRef{{JobID: 1046}},
			ResolvedCloudAfter: []JobDependencyResolvedRef{{JobID: 1046, RunID: 2048}},
		},
		Source: &JobSourceMetadata{
			Hash: "manifest-a",
			Pin:  &JobSourcePinMetadata{Hash: "manifest-a", Roots: []JobSourcePinRootMetadata{{Hash: "root-a", R2Key: "root.tar.gz"}}},
			Execution: &JobSourceExecutionMetadata{
				DispatchMode: "pinned_inventory_manifest", Verification: SourceVerificationVerified,
			},
		},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Metadata == nil || job.Metadata.Dependencies == nil {
		t.Fatalf("expected dependency metadata, got %+v", job)
	}
	if got := job.Metadata.Dependencies.CloudNeeds; len(got) != 1 || got[0] != "output/model.pt:1046" {
		t.Fatalf("cloud_needs = %v", got)
	}
	if got := job.Metadata.Dependencies.CloudAfter; len(got) != 1 || got[0].JobID != 1046 {
		t.Fatalf("cloud_after = %v", got)
	}
	if got := job.Metadata.Dependencies.ResolvedCloudAfter; len(got) != 0 {
		t.Fatalf("resolved_cloud_after = %v, want not carried forward", got)
	}
	if job.Metadata.Source == nil || job.Metadata.Source.Pin == nil || job.Metadata.Source.Pin.Hash != "manifest-a" {
		t.Fatalf("source pin was not carried forward: %+v", job.Metadata.Source)
	}
	if job.Metadata.Source.Execution != nil {
		t.Fatalf("source execution evidence crossed attempt boundary: %+v", job.Metadata.Source.Execution)
	}
}

// TestSetJobMetadata_PersistsForUnplacedJob is a regression test for the bug
// where submission metadata (cloud dependencies, disk floor) was silently
// dropped for jobs submitted without a host: an unplaced job has no
// job_attempts row, so the attempt-scoped UPDATE matched zero rows and
// succeeded silently. SetJobMetadata now falls back to the jobs table.
func TestSetJobMetadata_PersistsForUnplacedJob(t *testing.T) {
	database := setupTestDB(t)

	// Unplaced job: no host, so recordQueuedWithGPU creates no attempt row.
	jobID, err := RecordQueued(database, "", "/tmp/project", "echo consume", "consumer")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	meta := &JobMetadata{
		Dependencies: &JobDependencyMetadata{
			CloudNeeds: []string{"output/model.pt:1046"},
			CloudAfter: []JobDependencyRef{{JobID: 1046}},
		},
		Disk: &JobDiskMetadata{DiskGB: 120},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Metadata == nil || job.Metadata.Dependencies == nil {
		t.Fatalf("expected metadata to survive for unplaced job, got %+v", job)
	}
	if got := job.Metadata.Dependencies.CloudNeeds; len(got) != 1 || got[0] != "output/model.pt:1046" {
		t.Fatalf("cloud_needs = %v, want [output/model.pt:1046]", got)
	}
	if job.Metadata.Disk == nil || job.Metadata.Disk.DiskGB != 120 {
		t.Fatalf("disk metadata lost: %+v", job.Metadata.Disk)
	}
}

// TestSetJobMetadata_UnknownJob verifies SetJobMetadata reports an error
// instead of silently succeeding when the job does not exist.
func TestSetJobMetadata_UnknownJob(t *testing.T) {
	database := setupTestDB(t)
	err := SetJobMetadata(database, 999999, &JobMetadata{
		Dependencies: &JobDependencyMetadata{CloudNeeds: []string{"output/x.pt:1"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown job")
	}
}

// Covers spec invariant PendingPlacementClearedOnTerminalLaunch: once a launch
// reaches a terminal status, any pending_placement on attempts referencing it
// must be cleared, otherwise AssignJobHost will refuse to re-place the job.
func TestTrigger_LaunchTerminal_ClearsPendingPlacement(t *testing.T) {
	for _, terminalStatus := range []string{LaunchStatusFailed, LaunchStatusCancelled, LaunchStatusCompleted} {
		t.Run(terminalStatus, func(t *testing.T) {
			database := setupTestDB(t)
			launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
			if err != nil {
				t.Fatalf("CreateLaunch: %v", err)
			}

			jobID := int64(7001)
			insertTestJob(t, database, jobID, "echo pending", "/tmp", StatusQueued, withLaunch(launchID))
			if err := SetAttemptPendingStatus(database, jobID, StatusPendingPlacement); err != nil {
				t.Fatalf("SetAttemptPendingStatus: %v", err)
			}

			if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, terminalStatus, launchID); err != nil {
				t.Fatalf("UPDATE launches: %v", err)
			}

			var pending sql.NullString
			var pendingAt sql.NullInt64
			if err := database.QueryRow(
				`SELECT pending_status, pending_at FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
				jobID,
			).Scan(&pending, &pendingAt); err != nil {
				t.Fatalf("query attempt: %v", err)
			}
			if pending.Valid {
				t.Fatalf("pending_status = %q, want NULL after launch %s", pending.String, terminalStatus)
			}
			if pendingAt.Valid {
				t.Fatalf("pending_at = %d, want NULL after launch %s", pendingAt.Int64, terminalStatus)
			}
		})
	}
}

func TestTrigger_LaunchTerminal_DoesNotTouchOtherPendingStatuses(t *testing.T) {
	database := setupTestDB(t)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// A pending_status of "queued" (user intent to re-queue) must be preserved
	// when the launch fails — only pending_placement is cleared.
	jobID := int64(7101)
	insertTestJob(t, database, jobID, "echo queued-intent", "/tmp", StatusQueued, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusFailed, launchID); err != nil {
		t.Fatalf("UPDATE launches: %v", err)
	}

	var pending string
	if err := database.QueryRow(
		`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&pending); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if pending != StatusQueued {
		t.Fatalf("pending_status = %q, want %q (non-pending_placement intent must survive)", pending, StatusQueued)
	}
}

func TestTrigger_LaunchTerminal_DoesNotTouchOtherLaunches(t *testing.T) {
	database := setupTestDB(t)
	terminalID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch(terminal): %v", err)
	}
	otherID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch(other): %v", err)
	}

	jobOther := int64(7201)
	insertTestJob(t, database, jobOther, "echo other", "/tmp", StatusQueued, withLaunch(otherID))
	if err := SetAttemptPendingStatus(database, jobOther, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusFailed, terminalID); err != nil {
		t.Fatalf("UPDATE launches: %v", err)
	}

	var pending string
	if err := database.QueryRow(
		`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobOther,
	).Scan(&pending); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if pending != StatusPendingPlacement {
		t.Fatalf("pending_status = %q, want %q (other launch's attempts must be untouched)", pending, StatusPendingPlacement)
	}
}

// End-to-end: once the trigger has cleared the stale pending_placement,
// AssignJobHost can place the job again.
func TestTrigger_LaunchTerminal_UnblocksAssignJobHost(t *testing.T) {
	database := setupTestDB(t)
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID := int64(7301)
	insertTestJob(t, database, jobID, "echo stuck", "/tmp", StatusQueued, withLaunch(launchID))
	// job_status maps an orphaned attempt back to 'queued' only when the job's
	// requested_status is explicitly 'queued'.
	if _, err := database.Exec(`UPDATE jobs SET requested_status = 'queued' WHERE id = ?`, jobID); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}
	if err := SetAttemptPendingStatus(database, jobID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	// Simulate the orphan-on-destroy trigger's output on the attempt (canceled
	// + orphaned + end_time + host cleared). It does not touch pending_status,
	// so the pending_placement from above is still set.
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, cloud_outcome = ?, end_time = ?, host = '' WHERE job_id = ?`,
		StatusCanceled, AttemptOutcomeOrphaned, time.Now().Unix(), jobID,
	); err != nil {
		t.Fatalf("simulate orphan: %v", err)
	}

	// Pre-trigger: AssignJobHost refuses because effective_status is pending_placement.
	assigned, err := AssignJobHost(database, jobID, "host-alpha")
	if err != nil {
		t.Fatalf("AssignJobHost (pre-trigger): %v", err)
	}
	if assigned {
		t.Fatalf("AssignJobHost succeeded while job was pending_placement; the stuck-job bug has returned")
	}

	// Launch goes terminal → trigger clears pending_status on the attempt.
	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusFailed, launchID); err != nil {
		t.Fatalf("UPDATE launches: %v", err)
	}

	assigned, err = AssignJobHost(database, jobID, "host-alpha")
	if err != nil {
		t.Fatalf("AssignJobHost (post-trigger): %v", err)
	}
	if !assigned {
		t.Fatalf("AssignJobHost returned false after launch went terminal; trigger did not unblock placement")
	}
}

// Regression: a cloud attempt that was created with pending_status='queued'
// (the requeue intent set by RequeueFreshAttemptByID) and then is observed
// running by the cloud sync path must have pending_status cleared. Otherwise
// EffectiveStatus() keeps returning "queued" while the job is actually
// running, which mis-buckets the job (and its launch siblings) as "Launching"
// in the jobs TUI.
func TestMarkQueuedJobRunning_ClearsSatisfiedPendingStatus(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	cases := []struct {
		name        string
		pending     string
		wantCleared bool
	}{
		{"queued", StatusQueued, true},
		{"pending_placement", StatusPendingPlacement, true},
		{"running", StatusRunning, true},
		{"starting", StatusStarting, true},
		{"canceled (preserved)", StatusCanceled, false},
		{"killed (preserved)", StatusKilled, false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobID := int64(9100 + i)
			insertTestJob(t, database, jobID, "echo running-clears-pending", "/tmp", StatusQueued, withLaunch(launchID))
			if err := SetAttemptPendingStatus(database, jobID, tc.pending); err != nil {
				t.Fatalf("SetAttemptPendingStatus(%q): %v", tc.pending, err)
			}

			if err := MarkQueuedJobRunning(database, jobID); err != nil {
				t.Fatalf("MarkQueuedJobRunning: %v", err)
			}

			var status string
			var pending sql.NullString
			var pendingAt sql.NullInt64
			if err := database.QueryRow(
				`SELECT status, pending_status, pending_at FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
				jobID,
			).Scan(&status, &pending, &pendingAt); err != nil {
				t.Fatalf("query attempt: %v", err)
			}
			if status != StatusRunning {
				t.Fatalf("status = %q, want %q", status, StatusRunning)
			}
			if tc.wantCleared {
				if pending.Valid {
					t.Fatalf("pending_status = %q, want NULL (satisfied intent should be cleared)", pending.String)
				}
				if pendingAt.Valid {
					t.Fatalf("pending_at = %d, want NULL", pendingAt.Int64)
				}
			} else {
				if !pending.Valid || pending.String != tc.pending {
					t.Fatalf("pending_status = %v, want %q (stop intent must be preserved)", pending, tc.pending)
				}
			}
		})
	}
}

func TestTrigger_RunningTransitionClearsSatisfiedPendingStatus(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(9200)
	insertTestJob(t, database, jobID, "echo direct-running", "/tmp", StatusQueued)
	if err := SetAttemptPendingStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		StatusRunning, time.Now().Unix(), jobID,
	); err != nil {
		t.Fatalf("direct running update: %v", err)
	}

	var pending sql.NullString
	var pendingAt sql.NullInt64
	if err := database.QueryRow(
		`SELECT pending_status, pending_at FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&pending, &pendingAt); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if pending.Valid {
		t.Fatalf("pending_status = %q, want NULL", pending.String)
	}
	if pendingAt.Valid {
		t.Fatalf("pending_at = %d, want NULL", pendingAt.Int64)
	}
}

func TestTrigger_PendingQueuedIntentOnAlreadyRunningJobSurvives(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(9201)
	insertTestJob(t, database, jobID, "echo running-requeue-intent", "/tmp", StatusRunning)

	if err := SetAttemptPendingStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	var pending string
	if err := database.QueryRow(
		`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&pending); err != nil {
		t.Fatalf("query pending_status: %v", err)
	}
	if pending != StatusQueued {
		t.Fatalf("pending_status = %q, want %q", pending, StatusQueued)
	}
}
