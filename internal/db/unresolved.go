package db

import (
	"database/sql"
	"fmt"
	"time"

	jobstatus "github.com/osteele/weft/internal/status"
)

// ObserveQueueWorkerAbsent records one reconciliation observation for the
// exact current attempt. It changes the attempt to unresolved only after the
// evidence has remained unresolved for the configured bound. The attempt
// stays open and keeps its last positively observed remote status because no
// execution outcome has been established.
func ObserveQueueWorkerAbsent(database *sql.DB, jobID, attemptID int64, now time.Time, bound time.Duration, reason string) (bool, error) {
	if bound <= 0 {
		return false, fmt.Errorf("queue unknown bound must be positive")
	}
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var fromStatus string
	var rawMetadata sql.NullString
	err = tx.QueryRow(`
		SELECT status, job_metadata
		  FROM authoritative_job_attempts
		 WHERE id = ? AND job_id = ? AND end_time IS NULL`,
		attemptID, jobID,
	).Scan(&fromStatus, &rawMetadata)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	meta := decodeJobMetadata(rawMetadata)
	if meta == nil {
		meta = &JobMetadata{}
	}
	if meta.Reconciliation == nil {
		meta.Reconciliation = &JobReconciliationMetadata{}
	}
	observation := meta.Reconciliation
	if observation.StatusUnknownSince == 0 {
		observation.StatusUnknownSince = now.Unix()
	}

	becameUnresolved := false
	if fromStatus != StatusUnresolved && !now.Before(time.Unix(observation.StatusUnknownSince, 0).Add(bound)) {
		if _, err := jobstatus.ValidateTransition(fromStatus, StatusUnresolved, false); err != nil {
			return false, err
		}
		observation.UnresolvedAt = now.Unix()
		observation.UnresolvedReason = reason
		becameUnresolved = true
	}
	encoded, err := encodeJobMetadata(meta)
	if err != nil {
		return false, err
	}
	targetStatus := fromStatus
	if becameUnresolved {
		targetStatus = StatusUnresolved
	}
	result, err := tx.Exec(`
		UPDATE job_attempts
		   SET status = ?, job_metadata = ?, session_name = CASE WHEN ? THEN NULL ELSE session_name END
		 WHERE id = ? AND job_id = ? AND end_time IS NULL AND status = ?`,
		targetStatus, encoded, becameUnresolved, attemptID, jobID, fromStatus,
	)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if updated == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return becameUnresolved, nil
}

// ClearQueueWorkerAbsentObservation clears the age anchor after positive
// evidence shows that the attempt is still live or queued. Historical
// unresolved details remain on the attempt for auditability.
func ClearQueueWorkerAbsentObservation(database *sql.DB, jobID, attemptID int64) error {
	_, err := database.Exec(`
		UPDATE job_attempts
		   SET job_metadata = json_remove(COALESCE(job_metadata, '{}'), '$.reconciliation.status_unknown_since')
		 WHERE id = ? AND job_id = ? AND end_time IS NULL`, attemptID, jobID)
	return err
}

// MarkRunningFromUnresolved restores the same open attempt when the runner
// later supplies positive process evidence.
func MarkRunningFromUnresolved(database *sql.DB, jobID int64) error {
	if err := checkOpenTransition(database, jobID, StatusRunning, false, jobstatus.SourceSSHSync); err != nil {
		return err
	}
	_, err := database.Exec(`
		UPDATE job_attempts
		   SET status = ?, last_synced_status = ?
		 WHERE id = `+latestOpenAttemptSubquery+` AND status = ?`,
		StatusRunning, StatusRunning, jobID, StatusUnresolved)
	return err
}

// MarkPausedFromUnresolved restores the same open attempt when positive
// process evidence shows that it is paused.
func MarkPausedFromUnresolved(database *sql.DB, jobID int64) error {
	if err := checkOpenTransition(database, jobID, StatusPaused, false, jobstatus.SourceSSHSync); err != nil {
		return err
	}
	_, err := database.Exec(`
		UPDATE job_attempts
		   SET status = ?, last_synced_status = ?
		 WHERE id = `+latestOpenAttemptSubquery+` AND status = ?`,
		StatusPaused, StatusPaused, jobID, StatusUnresolved)
	return err
}
