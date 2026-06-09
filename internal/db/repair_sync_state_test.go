package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestRepairSyncStateClearsTerminalInventoryPendingStatuses(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()

	insertTestJob(t, database, 101, "echo stale", "/tmp", StatusCanceled,
		withHost("cool30"), withStartTime(now-20), withEndTime(now-10))
	setAttemptPendingStatus(t, database, 101, StatusCanceled, now-5)

	insertTestJob(t, database, 102, "echo failed", "/tmp", StatusCompleted,
		withHost("cool30"), withStartTime(now-30), withEndTime(now-20), withExitCode(1))
	setAttemptPendingStatus(t, database, 102, StatusKilled, now-5)
	setAttemptLastSyncedStatus(t, database, 102, StatusCompleted)

	insertTestJob(t, database, 103, "echo running", "/tmp", StatusRunning, withHost("cool30"))
	setAttemptPendingStatus(t, database, 103, StatusKilled, now-5)

	insertTestJob(t, database, 104, "echo other host", "/tmp", StatusCanceled,
		withHost("studio"), withStartTime(now-20), withEndTime(now-10))
	setAttemptPendingStatus(t, database, 104, StatusCanceled, now-5)

	found, err := FindStaleTerminalPendingStatuses(database, "cool30")
	if err != nil {
		t.Fatalf("FindStaleTerminalPendingStatuses: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d rows, want 2: %+v", len(found), found)
	}
	if found[0].JobID != 101 || found[1].JobID != 102 {
		t.Fatalf("found job IDs = %d,%d; want 101,102", found[0].JobID, found[1].JobID)
	}

	n, err := ClearStaleTerminalPendingStatuses(database, "cool30")
	if err != nil {
		t.Fatalf("ClearStaleTerminalPendingStatuses: %v", err)
	}
	if n != 2 {
		t.Fatalf("affected %d rows, want 2", n)
	}

	for _, jobID := range []int64{101, 102} {
		assertAttemptPendingCleared(t, database, jobID)
	}
	assertAttemptPendingEquals(t, database, 103, StatusKilled)
	assertAttemptPendingEquals(t, database, 104, StatusCanceled)

	var lastSynced string
	if err := database.QueryRow(
		`SELECT COALESCE(last_synced_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		102,
	).Scan(&lastSynced); err != nil {
		t.Fatalf("query last_synced_status: %v", err)
	}
	if lastSynced != StatusCompleted {
		t.Fatalf("last_synced_status = %q, want %q", lastSynced, StatusCompleted)
	}
}

func TestRepairSyncStateSkipsCloudAttempts(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()

	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 201, "echo cloud", "/tmp", StatusCanceled,
		withLaunch(launchID), withStartTime(now-20), withEndTime(now-10))
	setAttemptPendingStatus(t, database, 201, StatusCanceled, now-5)

	found, err := FindStaleTerminalPendingStatuses(database, "")
	if err != nil {
		t.Fatalf("FindStaleTerminalPendingStatuses: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found cloud rows = %+v, want none", found)
	}

	n, err := ClearStaleTerminalPendingStatuses(database, "")
	if err != nil {
		t.Fatalf("ClearStaleTerminalPendingStatuses: %v", err)
	}
	if n != 0 {
		t.Fatalf("affected %d rows, want 0", n)
	}
	assertAttemptPendingEquals(t, database, 201, StatusCanceled)
}

func setAttemptPendingStatus(t *testing.T, database *sql.DB, jobID int64, status string, ts int64) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_status = ?, pending_at = ?
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		status, ts, jobID,
	); err != nil {
		t.Fatalf("set pending status for job %d: %v", jobID, err)
	}
}

func setAttemptLastSyncedStatus(t *testing.T, database *sql.DB, jobID int64, status string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE job_attempts SET last_synced_status = ?
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		status, jobID,
	); err != nil {
		t.Fatalf("set last_synced_status for job %d: %v", jobID, err)
	}
}

func assertAttemptPendingCleared(t *testing.T, database *sql.DB, jobID int64) {
	t.Helper()
	var pending sql.NullString
	var pendingAt sql.NullInt64
	if err := database.QueryRow(
		`SELECT pending_status, pending_at FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&pending, &pendingAt); err != nil {
		t.Fatalf("query pending for job %d: %v", jobID, err)
	}
	if pending.Valid || pendingAt.Valid {
		t.Fatalf("job %d pending = %v/%v, want NULL/NULL", jobID, pending, pendingAt)
	}
}

func assertAttemptPendingEquals(t *testing.T, database *sql.DB, jobID int64, want string) {
	t.Helper()
	var pending string
	if err := database.QueryRow(
		`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&pending); err != nil {
		t.Fatalf("query pending for job %d: %v", jobID, err)
	}
	if pending != want {
		t.Fatalf("job %d pending = %q, want %q", jobID, pending, want)
	}
}
