package db

import "testing"

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
