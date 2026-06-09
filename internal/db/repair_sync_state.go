package db

import (
	"database/sql"
	"fmt"
)

// SyncStateRepairCandidate is a terminal inventory-host attempt whose pending
// status can no longer represent live remote work.
type SyncStateRepairCandidate struct {
	JobID            int64
	AttemptID        int64
	Host             string
	Status           string
	PendingStatus    string
	LastSyncedStatus string
	EndTime          int64
}

// FindStaleTerminalPendingStatuses returns local DB rows that keep old
// terminal inventory jobs in the pending-reconciliation path.
func FindStaleTerminalPendingStatuses(database *sql.DB, host string) ([]SyncStateRepairCandidate, error) {
	query := `
		SELECT js.id, js.latest_run_id, js.host, js.status, js.pending_status,
		       COALESCE(js.last_synced_status, ''), js.end_time
		FROM job_status js
		WHERE js.effective_target_kind = ?
		  AND js.tombstoned = 0
		  AND js.pending_status IS NOT NULL
		  AND js.end_time IS NOT NULL
		  AND js.end_time > 0
		  AND js.status IN (?, ?, ?, ?, ?)`
	args := []any{
		string(JobTargetInventoryHost),
		StatusCompleted,
		StatusFailed,
		StatusDead,
		StatusKilled,
		StatusCanceled,
	}
	if host != "" {
		query += ` AND js.host = ?`
		args = append(args, host)
	}
	query += ` ORDER BY js.id ASC`

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("find stale terminal pending statuses: %w", err)
	}
	defer rows.Close()

	var out []SyncStateRepairCandidate
	for rows.Next() {
		var c SyncStateRepairCandidate
		if err := rows.Scan(
			&c.JobID,
			&c.AttemptID,
			&c.Host,
			&c.Status,
			&c.PendingStatus,
			&c.LastSyncedStatus,
			&c.EndTime,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClearStaleTerminalPendingStatuses clears pending_status/pending_at for the
// rows matched by FindStaleTerminalPendingStatuses. It does not touch
// last_synced_status, because completed remote attempts with nonzero exit codes
// legitimately derive a user-visible failed status.
func ClearStaleTerminalPendingStatuses(database *sql.DB, host string) (int64, error) {
	query := `
		UPDATE job_attempts
		SET pending_status = NULL, pending_at = NULL
		WHERE id IN (
			SELECT js.latest_run_id
			FROM job_status js
			WHERE js.effective_target_kind = ?
			  AND js.tombstoned = 0
			  AND js.pending_status IS NOT NULL
			  AND js.end_time IS NOT NULL
			  AND js.end_time > 0
			  AND js.status IN (?, ?, ?, ?, ?)`
	args := []any{
		string(JobTargetInventoryHost),
		StatusCompleted,
		StatusFailed,
		StatusDead,
		StatusKilled,
		StatusCanceled,
	}
	if host != "" {
		query += ` AND js.host = ?`
		args = append(args, host)
	}
	query += `
		)`

	res, err := database.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("clear stale terminal pending statuses: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
