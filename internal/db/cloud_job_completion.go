package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/status"
)

// RecordCloudJobCompletion updates the job attempt in the DB with the given
// exit code, times, and failure reason. Returns the cloud instance ID if the
// job was assigned to one.
func RecordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason string) (int64, error) {
	targetStatus := StatusCompleted
	outcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		targetStatus = StatusFailed
		outcome = AttemptOutcomeFailed
	}
	if err := checkTransition(database, jobID, targetStatus, true, status.SourceR2Completion); err != nil {
		return 0, err
	}

	// Treat 0 as "unknown" — fall back to current time so downstream code
	// (CloseLaunchAttempts, ComputeJobState) doesn't confuse 0 with NULL.
	if endTimeUnix == 0 {
		endTimeUnix = time.Now().Unix()
	}

	var cloudInstanceID sql.NullInt64
	if err := database.QueryRow(`SELECT launch_id FROM job_status WHERE id = ? AND tombstoned = 0`, jobID).Scan(&cloudInstanceID); err != nil {
		return 0, err
	}

	// Cloud completion is authoritative — update the latest attempt regardless
	// of whether it is open or closed. This handles the case where
	// cleanupStaleAttempts already closed the original attempt and created a
	// new one.
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
		     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
		     cloud_outcome = ?
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		targetStatus, exitCode, startTimeUnix, endTimeUnix, targetStatus, failureReason, outcome, jobID,
	); err != nil {
		return 0, err
	}

	// If the latest attempt has no launch_id (e.g., a blank replacement from
	// cleanupStaleAttempts), infer it by finding a sibling launch that ran
	// other jobs from the same original launch.
	if !cloudInstanceID.Valid {
		inferredID, err := inferSiblingLaunch(database, jobID)
		if err == nil && inferredID > 0 {
			host := LaunchHost(inferredID)
			if _, err := database.Exec(`
				UPDATE job_attempts
				SET launch_id = ?,
				    host = CASE WHEN (host = '' OR host IS NULL) THEN ? ELSE host END
				WHERE id = `+latestAttemptSubquery+`
				  AND launch_id IS NULL`,
				inferredID, host, jobID,
			); err != nil {
				return 0, fmt.Errorf("set inferred launch_id for job %d: %w", jobID, err)
			}
			return inferredID, nil
		}
	}

	if cloudInstanceID.Valid {
		return cloudInstanceID.Int64, nil
	}
	return 0, nil
}

// NeedsCloudCompletionBackfill reports whether the latest attempt for jobID is
// terminal but still missing authoritative cloud completion fields.
//
// This is used to recover from reconcile races where attempts were force-closed
// (for example at grace expiry) before R2 completion metadata was ingested.
func NeedsCloudCompletionBackfill(database *sql.DB, jobID int64) (bool, error) {
	var (
		status           sql.NullString
		exitCode         sql.NullInt64
		startTime        sql.NullInt64
		endTime          sql.NullInt64
		lastSyncedStatus sql.NullString
		launchID         sql.NullInt64
	)
	err := database.QueryRow(
		`SELECT status, exit_code, start_time, end_time, last_synced_status, launch_id
		 FROM job_attempts
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		jobID,
	).Scan(&status, &exitCode, &startTime, &endTime, &lastSyncedStatus, &launchID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !status.Valid || !IsTerminalStatus(status.String) {
		return false, nil
	}
	// Non-cloud attempts are out of scope for cloud completion backfill.
	if !launchID.Valid {
		return false, nil
	}

	switch status.String {
	case StatusCompleted, StatusFailed, StatusDead, StatusKilled:
		if !endTime.Valid || !startTime.Valid || !exitCode.Valid {
			return true, nil
		}
		// last_synced_status should reflect the terminal state once cloud
		// completion data was applied.
		if !lastSyncedStatus.Valid || lastSyncedStatus.String != status.String {
			return true, nil
		}
	}

	return false, nil
}

// StuckJob represents a job with non-terminal status on a completed launch.
type StuckJob struct {
	JobID     int64
	AttemptID int64
}

// FindStuckJobsOnCompletedLaunches returns jobs with non-terminal status whose
// launch has already completed. These jobs missed R2 result sync.
func FindStuckJobsOnCompletedLaunches(database *sql.DB) ([]StuckJob, error) {
	rows, err := database.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		JOIN launches l ON ja.launch_id = l.id
		WHERE l.status = ?
		AND ja.status NOT IN (?, ?, ?, ?, ?, ?)
		AND ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)`,
		LaunchStatusCompleted,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stuck []StuckJob
	for rows.Next() {
		var s StuckJob
		if err := rows.Scan(&s.JobID, &s.AttemptID); err != nil {
			return nil, err
		}
		stuck = append(stuck, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stuck, nil
}

// MarkStuckJobDead marks a stuck job attempt as dead. Called when R2 sync
// has been attempted and failed — no completion data is recoverable.
func MarkStuckJobDead(database *sql.DB, attemptID int64) error {
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, end_time = COALESCE(end_time, ?),
		     failure_reason = 'launch completed but job results were not synced from R2',
		     cloud_outcome = ?
		 WHERE id = ?`,
		StatusDead, time.Now().Unix(), AttemptOutcomeOrphaned, attemptID,
	)
	return err
}

// FinalizeStuckJobsOnCompletedLaunches finds jobs with non-terminal status
// whose launch has already completed and marks them as "dead". This is the
// DB-only fallback — callers with R2 access should use
// campaign.FinalizeStuckJobsWithR2Check instead.
func FinalizeStuckJobsOnCompletedLaunches(database *sql.DB) ([]int64, error) {
	stuck, err := FindStuckJobsOnCompletedLaunches(database)
	if err != nil {
		return nil, err
	}

	var finalized []int64
	for _, s := range stuck {
		if err := MarkStuckJobDead(database, s.AttemptID); err != nil {
			return finalized, err
		}
		finalized = append(finalized, s.JobID)
	}
	return finalized, nil
}
