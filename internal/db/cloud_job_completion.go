package db

import "database/sql"

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
	if cloudInstanceID.Valid {
		return cloudInstanceID.Int64, nil
	}
	return 0, nil
}
