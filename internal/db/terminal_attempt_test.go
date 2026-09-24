package db

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// completeOneAttempt runs a job through queued -> running -> completed and
// returns its id, its attempt id, and the recorded finish time.
func completeOneAttempt(t *testing.T, database *sql.DB) (jobID, attemptID, endTime int64) {
	t.Helper()
	jobID, err := RecordQueued(database, "host-alpha", "/tmp", "echo hi", "settled job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	if err := database.QueryRow(
		`SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`, jobID,
	).Scan(&attemptID); err != nil {
		t.Fatalf("read attempt id: %v", err)
	}
	endTime = time.Now().Unix()
	if _, err := RecordAttemptCompletionWithTransition(database, jobID, attemptID, 0, 0, endTime); err != nil {
		t.Fatalf("RecordAttemptCompletionWithTransition: %v", err)
	}
	return jobID, attemptID, endTime
}

// A dispatch pass reads a job as queued, appends it to the remote queue, and
// only then marks it queued locally. When the job finishes inside that window
// the reconcile write would reset the terminal attempt in place — erasing the
// exit code and end time that were already recorded, and orphaning the
// attempt's job.terminal event. wj8888 sat queued for 19 hours that way.
func TestClearPendingRefusesResurrectingTerminalAttempt(t *testing.T) {
	database := setupTestDB(t)
	jobID, attemptID, endTime := completeOneAttempt(t, database)

	err := ClearPendingAndUpdateStatus(database, jobID, StatusQueued)
	if !errors.Is(err, ErrAttemptAlreadyTerminal) {
		t.Fatalf("reset of a completed attempt returned %v, want ErrAttemptAlreadyTerminal", err)
	}

	var gotStatus string
	var gotEnd, gotExit sql.NullInt64
	if err := database.QueryRow(
		`SELECT status, end_time, exit_code FROM job_attempts WHERE id = ?`, attemptID,
	).Scan(&gotStatus, &gotEnd, &gotExit); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if gotStatus != StatusCompleted {
		t.Fatalf("attempt status = %q, want %q", gotStatus, StatusCompleted)
	}
	if !gotEnd.Valid || gotEnd.Int64 != endTime {
		t.Fatalf("attempt end_time = %v, want %d", gotEnd, endTime)
	}
	if !gotExit.Valid || gotExit.Int64 != 0 {
		t.Fatalf("attempt exit_code = %v, want 0", gotExit)
	}
}

// Databases that already contain a resurrected attempt must still be able to
// settle it. The attempt's job.terminal event survived the reset, so a plain
// INSERT in the lifecycle trigger made every later completion write fail on
// idx_job_lifecycle_events_terminal_once — permanently, since nothing clears
// the event.
func TestCompletionRecordableAfterAttemptWasResurrected(t *testing.T) {
	database := setupTestDB(t)
	jobID, attemptID, endTime := completeOneAttempt(t, database)

	// Exactly what the pre-guard reconcile path wrote.
	if _, err := database.Exec(
		`UPDATE job_attempts
		    SET status = ?, last_synced_status = ?, start_time = NULL, end_time = NULL, exit_code = NULL
		  WHERE id = ?`,
		StatusQueued, StatusQueued, attemptID,
	); err != nil {
		t.Fatalf("resurrect attempt: %v", err)
	}

	if _, err := RecordAttemptCompletionWithTransition(database, jobID, attemptID, 0, 0, endTime); err != nil {
		t.Fatalf("settlement blocked after resurrection: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, StatusCompleted)
	}

	var terminalEvents int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_lifecycle_events
		  WHERE job_id = ? AND attempt_id = ? AND event_kind = 'job.terminal'`,
		jobID, attemptID,
	).Scan(&terminalEvents); err != nil {
		t.Fatalf("count terminal events: %v", err)
	}
	if terminalEvents != 1 {
		t.Fatalf("terminal events for attempt = %d, want 1", terminalEvents)
	}
}
