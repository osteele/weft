package db

import (
	"testing"
	"time"
)

// These tests cover the triggers added in
// 00006_terminal_attempt_closing_fields.sql, which enforce
// TerminalJobsHaveEndTime (specs/job-lifecycle.allium) and
// ClosedCloudAttemptHasOutcome (specs/campaign-lifecycle.allium) at the
// DB layer, so a writer that forgets to stamp end_time or cloud_outcome
// cannot leak the wedged state that wi2317 and the 837-row prod scan
// surfaced.

func TestTrigger_AutoStampEndTime_OnUpdateToTerminal(t *testing.T) {
	database := setupTestDB(t)
	insertTestJob(t, database, 8001, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8001, "myhost", nil, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Writer forgets to stamp end_time. The trigger must do it.
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE id = ?`, StatusFailed, attemptID); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var endTime *int64
	if err := database.QueryRow(`SELECT end_time FROM job_attempts WHERE id = ?`, attemptID).Scan(&endTime); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if endTime == nil || *endTime <= 0 {
		t.Fatalf("end_time = %v, want auto-stamped positive unix ts", endTime)
	}
	if time.Now().Unix()-*endTime > 5 {
		t.Fatalf("end_time too old: %d", *endTime)
	}
}

func TestTrigger_CarveOut_CloudBackfillPending_LeavesEndTimeNull(t *testing.T) {
	// Cloud attempt awaiting agent backfill: last_synced_status is NULL,
	// status reflects local guess, end_time should NOT be auto-stamped so
	// the eventual sync can rewrite from R2.
	database := setupTestDB(t)
	launch, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 8003, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8003, "", &launch, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	// Explicitly clear last_synced_status to model the backfill-pending state.
	if _, err := database.Exec(`UPDATE job_attempts SET last_synced_status = NULL WHERE id = ?`, attemptID); err != nil {
		t.Fatalf("clear last_synced_status: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE id = ?`, StatusFailed, attemptID); err != nil {
		t.Fatalf("UPDATE status: %v", err)
	}

	var endTime *int64
	if err := database.QueryRow(`SELECT end_time FROM job_attempts WHERE id = ?`, attemptID).Scan(&endTime); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if endTime != nil {
		t.Fatalf("end_time = %v, want NULL (backfill-pending carve-out)", *endTime)
	}
}

func TestTrigger_AutoDeriveCloudOutcome_OnCompletedZeroExit(t *testing.T) {
	database := setupTestDB(t)
	launch, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 8004, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8004, "", &launch, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Writer closes the attempt with status + exit_code + end_time but
	// forgets cloud_outcome (the vastai_sweep.go pattern).
	exit := int64(0)
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
		StatusCompleted, exit, now, StatusCompleted, attemptID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var outcome *string
	if err := database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE id = ?`, attemptID).Scan(&outcome); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if outcome == nil || *outcome != string(AttemptOutcomeCompleted) {
		t.Fatalf("cloud_outcome = %v, want %q", outcome, AttemptOutcomeCompleted)
	}
}

func TestTrigger_AutoDeriveCloudOutcome_OnCompletedNonZeroExit(t *testing.T) {
	database := setupTestDB(t)
	launch, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 8005, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8005, "", &launch, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
		StatusCompleted, 1, now, StatusCompleted, attemptID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var outcome *string
	if err := database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE id = ?`, attemptID).Scan(&outcome); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if outcome == nil || *outcome != string(AttemptOutcomeFailed) {
		t.Fatalf("cloud_outcome = %v, want %q (non-zero exit → failed)", outcome, AttemptOutcomeFailed)
	}
}

func TestTrigger_AutoDeriveCloudOutcome_RespectsExplicitValue(t *testing.T) {
	// Writers that explicitly stamp cloud_outcome with a value not
	// derivable from status (e.g. orphaned/superseded/preempted) must not
	// be overwritten by the auto-derive trigger.
	database := setupTestDB(t)
	launch, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 8006, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8006, "", &launch, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ?, last_synced_status = ? WHERE id = ?`,
		StatusCanceled, now, AttemptOutcomeOrphaned, StatusCanceled, attemptID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var outcome string
	if err := database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE id = ?`, attemptID).Scan(&outcome); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if outcome != string(AttemptOutcomeOrphaned) {
		t.Fatalf("cloud_outcome = %q, want %q (explicit value preserved)", outcome, AttemptOutcomeOrphaned)
	}
}

func TestTrigger_AutoDeriveCloudOutcome_BackfillPendingCarveOut(t *testing.T) {
	// Cloud attempt awaiting backfill (last_synced_status NULL) must NOT
	// get cloud_outcome auto-stamped.
	database := setupTestDB(t)
	launch, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 8007, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8007, "", &launch, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET last_synced_status = NULL WHERE id = ?`, attemptID); err != nil {
		t.Fatalf("clear last_synced_status: %v", err)
	}

	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE id = ?`,
		StatusFailed, now, attemptID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var outcome *string
	if err := database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE id = ?`, attemptID).Scan(&outcome); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if outcome != nil {
		t.Fatalf("cloud_outcome = %v, want NULL (backfill-pending carve-out)", *outcome)
	}
}

func TestTrigger_OnPremAttemptDoesNotGetCloudOutcome(t *testing.T) {
	// Attempt with launch_id NULL is on-prem; cloud_outcome must stay NULL.
	database := setupTestDB(t)
	insertTestJob(t, database, 8008, "x", "/tmp", StatusQueued)
	attemptID, err := CreateAttempt(database, 8008, "host", nil, StatusRunning)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ? WHERE id = ?`,
		StatusCompleted, attemptID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var outcome *string
	if err := database.QueryRow(`SELECT cloud_outcome FROM job_attempts WHERE id = ?`, attemptID).Scan(&outcome); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if outcome != nil {
		t.Fatalf("cloud_outcome = %v, want NULL for on-prem attempt", *outcome)
	}
}
