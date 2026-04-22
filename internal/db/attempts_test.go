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
			CloudNeeds: []string{"output/model.pt:1046"},
			CloudAfter: []JobDependencyRef{{JobID: 1046}},
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
