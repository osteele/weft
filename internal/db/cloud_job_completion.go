package db

import (
	"database/sql"
	"fmt"
	"time"
)

// RecordCloudJobCompletion updates the job attempt in the DB with the given
// exit code, times, and failure reason. Returns the cloud instance ID if the
// job was assigned to one.
func RecordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason string) (int64, error) {
	status := StatusCompleted
	outcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		status = StatusFailed
		outcome = AttemptOutcomeFailed
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
		status, exitCode, startTimeUnix, endTimeUnix, status, failureReason, outcome, jobID,
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

// FinalizeStuckJobsOnCompletedLaunches finds jobs with non-terminal status
// whose launch has already completed. These jobs missed R2 result sync
// (markers missing or unprocessable). Marks them as "dead" since we can't
// recover the exit code. Returns the IDs of finalized jobs.
func FinalizeStuckJobsOnCompletedLaunches(database *sql.DB) ([]int64, error) {
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

	type stuckJob struct {
		jobID     int64
		attemptID int64
	}
	var stuck []stuckJob
	for rows.Next() {
		var s stuckJob
		if err := rows.Scan(&s.jobID, &s.attemptID); err != nil {
			return nil, err
		}
		stuck = append(stuck, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	var finalized []int64
	for _, s := range stuck {
		if _, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, end_time = COALESCE(end_time, ?),
			     failure_reason = 'launch completed but job results were not synced from R2',
			     cloud_outcome = ?
			 WHERE id = ?`,
			StatusDead, now, AttemptOutcomeOrphaned, s.attemptID,
		); err != nil {
			return finalized, err
		}
		finalized = append(finalized, s.jobID)
	}
	return finalized, nil
}
