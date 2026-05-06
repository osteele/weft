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
//
// markerLastModified is the LastModified timestamp of the R2 .complete marker
// (zero time if unavailable). It is used as a fallback for end_time when the
// completion JSON is missing — the marker is uploaded by the agent immediately
// after the job exits, so its LastModified is within ~1s of the true end time.
//
// When neither endTimeUnix nor markerLastModified yields a real timestamp, the
// row is left with NULL end_time and last_synced_status is cleared so that
// NeedsCloudCompletionBackfill keeps returning true and a later sync that
// finds the JSON will overwrite the placeholder values. This avoids silently
// substituting wall-clock sync time as a completion timestamp.
func RecordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason string, markerLastModified time.Time) (int64, error) {
	targetStatus := StatusCompleted
	outcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		targetStatus = StatusFailed
		outcome = AttemptOutcomeFailed
	}
	if err := checkTransition(database, jobID, targetStatus, true, status.SourceR2Completion); err != nil {
		return 0, err
	}

	// endTimeUnix == 0 means the completion JSON wasn't ingested. Prefer the
	// marker's LastModified (close to true end time); leave NULL otherwise so
	// the row remains backfill-eligible.
	var endTimeArg any
	authoritativeTimes := endTimeUnix != 0
	switch {
	case endTimeUnix != 0:
		endTimeArg = endTimeUnix
	case !markerLastModified.IsZero():
		endTimeArg = markerLastModified.Unix()
	default:
		endTimeArg = nil
	}

	var startTimeArg any
	if startTimeUnix != 0 {
		startTimeArg = startTimeUnix
	} else {
		startTimeArg = nil
	}

	// last_synced_status: only mark fully synced when we have authoritative
	// times from the completion JSON. On the marker-only path, leave it NULL
	// so backfill stays armed for a later sync that finds the JSON.
	var lastSyncedStatusArg any
	if authoritativeTimes {
		lastSyncedStatusArg = targetStatus
	} else {
		lastSyncedStatusArg = nil
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
		targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome, jobID,
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
		// Treat 0 as "unknown" symmetrically with NULL: an authoritative
		// completion JSON always carries non-zero start_time, so a stored 0
		// indicates the marker-only fallback path and remains eligible for
		// backfill.
		if startTime.Int64 == 0 || endTime.Int64 == 0 {
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

// PoisonedCloudAttempt identifies a terminal cloud attempt whose start_time
// is 0 — the signature of an earlier marker-only sync that fabricated end_time
// = wall-clock time instead of using the agent's authoritative completion JSON.
type PoisonedCloudAttempt struct {
	JobID     int64
	AttemptID int64
}

// FindPoisonedCloudCompletions returns the latest attempt for every terminal
// cloud job whose start_time is 0 and end_time is set. These rows need
// last_synced_status cleared so a later sync can rewrite the timestamps from
// R2 (NeedsCloudCompletionBackfill already treats start_time=0 as needing
// backfill, but the cleared last_synced_status documents the intent).
func FindPoisonedCloudCompletions(database *sql.DB) ([]PoisonedCloudAttempt, error) {
	rows, err := database.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		WHERE ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)
		  AND ja.launch_id IS NOT NULL
		  AND ja.status IN (?, ?, ?, ?)
		  AND ja.start_time = 0
		  AND ja.end_time IS NOT NULL`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
	if err != nil {
		return nil, fmt.Errorf("query poisoned cloud completions: %w", err)
	}
	defer rows.Close()

	var out []PoisonedCloudAttempt
	for rows.Next() {
		var p PoisonedCloudAttempt
		if err := rows.Scan(&p.JobID, &p.AttemptID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RearmPoisonedCloudCompletions clears last_synced_status on every row matched
// by FindPoisonedCloudCompletions in a single statement. Returns rows affected.
func RearmPoisonedCloudCompletions(database *sql.DB) (int64, error) {
	res, err := database.Exec(`
		UPDATE job_attempts
		SET last_synced_status = NULL
		WHERE id IN (
			SELECT ja.id FROM job_attempts ja
			WHERE ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)
			  AND ja.launch_id IS NOT NULL
			  AND ja.status IN (?, ?, ?, ?)
			  AND ja.start_time = 0
			  AND ja.end_time IS NOT NULL
		)`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
	if err != nil {
		return 0, fmt.Errorf("rearm poisoned cloud completions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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
