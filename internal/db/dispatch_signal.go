package db

import (
	"database/sql"
	"fmt"
	"time"
)

// RecordDispatchNotProgressing atomically records the audit event and its
// notification. The outbox primary key fences concurrent detectors and survives
// restarts. Unlike freshness dedupe, it never expires within an unchanged run.
// FirstEventID identifies the consecutive detail run above the queueing floor;
// a changed detail or an OK gives the next run a different first event.
func RecordDispatchNotProgressing(database *sql.DB, job *Job, run DispatchRun, now time.Time) (bool, error) {
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	key := fmt.Sprintf("dispatch-not-progressing/%d/%d/%d", job.ID, DispatchRunFloor(job), run.FirstEventID)
	result, err := tx.Exec(`INSERT INTO job_lifecycle_events (
		event_id, job_id, event_sequence, attempt_id, attempt_number,
		event_kind, status, occurred_at, project, project_root, submitter_session)
		SELECT ?, j.id,
		 (SELECT COALESCE(MAX(event_sequence), 0) + 1 FROM job_lifecycle_events WHERE job_id = j.id),
		 a.id, COALESCE(a.attempt_number, 0), ?, 'queued', ?,
		 COALESCE(j.project, ''), COALESCE(j.project_root, ''), COALESCE(j.submitter_session, '')
		FROM jobs j LEFT JOIN authoritative_job_attempts a
		 ON a.id = (SELECT a2.id FROM authoritative_job_attempts a2 WHERE a2.job_id = j.id ORDER BY a2.attempt_number DESC LIMIT 1)
		WHERE j.id = ?
		ON CONFLICT(event_id) DO NOTHING`, key, EventQueueDispatchNotProgressing, now.Unix(), job.ID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO lifecycle_events (occurred_at, event_kind, job_id, detail)
		VALUES (?, ?, ?, ?)`, now.Unix(), EventQueueDispatchNotProgressing, job.ID, run.Detail); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
