package db

import (
	"database/sql"
	"fmt"
	"time"
)

// DuplicateTerminalAttemptGroup describes repeated terminal attempts that carry
// identical execution facts for the same job on the same launch.
type DuplicateTerminalAttemptGroup struct {
	JobID              int64
	LaunchID           int64
	Status             string
	StartTime          *int64
	EndTime            *int64
	ExitCode           *int
	FailureReason      string
	CloudOutcome       string
	Count              int
	KeepAttemptID      int64
	FirstAttemptNumber int
	LastAttemptNumber  int
}

// FindDuplicateTerminalCloudAttemptGroups returns exact duplicate terminal
// same-launch attempt groups. The earliest attempt in each group is the row to
// keep authoritative; later rows can be abandoned as bookkeeping duplicates.
func FindDuplicateTerminalCloudAttemptGroups(database *sql.DB) ([]DuplicateTerminalAttemptGroup, error) {
	rows, err := database.Query(`
		SELECT job_id, launch_id, status, start_time, end_time, exit_code,
		       COALESCE(failure_reason, ''), COALESCE(cloud_outcome, ''),
		       COUNT(*) AS n,
		       MIN(id) AS keep_attempt_id,
		       MIN(attempt_number) AS first_attempt_number,
		       MAX(attempt_number) AS last_attempt_number
		  FROM job_attempts
		 WHERE launch_id IS NOT NULL
		   AND abandoned_at IS NULL
		   AND end_time IS NOT NULL
		   AND end_time != 0
		   AND status IN (?, ?, ?, ?, ?)
		 GROUP BY job_id, launch_id, status, start_time, end_time, exit_code,
		          COALESCE(failure_reason, ''), COALESCE(cloud_outcome, '')
		HAVING COUNT(*) > 1
		 ORDER BY n DESC, job_id ASC, launch_id ASC`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
	)
	if err != nil {
		return nil, fmt.Errorf("find duplicate terminal cloud attempts: %w", err)
	}
	defer rows.Close()

	var groups []DuplicateTerminalAttemptGroup
	for rows.Next() {
		var g DuplicateTerminalAttemptGroup
		var startTime, endTime, exitCode sql.NullInt64
		if err := rows.Scan(
			&g.JobID, &g.LaunchID, &g.Status, &startTime, &endTime, &exitCode,
			&g.FailureReason, &g.CloudOutcome, &g.Count, &g.KeepAttemptID,
			&g.FirstAttemptNumber, &g.LastAttemptNumber,
		); err != nil {
			return nil, err
		}
		if startTime.Valid {
			v := startTime.Int64
			g.StartTime = &v
		}
		if endTime.Valid {
			v := endTime.Int64
			g.EndTime = &v
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			g.ExitCode = &v
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// AbandonDuplicateTerminalCloudAttempts marks exact duplicate terminal
// same-launch attempts as non-authoritative, preserving the earliest row in
// each group for audit and user-visible job state.
func AbandonDuplicateTerminalCloudAttempts(database *sql.DB) (int64, error) {
	now := time.Now().Unix()
	res, err := database.Exec(`
		WITH ranked AS (
			SELECT id,
			       ROW_NUMBER() OVER (
			           PARTITION BY job_id, launch_id, status, start_time, end_time, exit_code,
			                        COALESCE(failure_reason, ''), COALESCE(cloud_outcome, '')
			           ORDER BY attempt_number ASC, id ASC
			       ) AS rn
			  FROM job_attempts
			 WHERE launch_id IS NOT NULL
			   AND abandoned_at IS NULL
			   AND end_time IS NOT NULL
			   AND end_time != 0
			   AND status IN (?, ?, ?, ?, ?)
		)
		UPDATE job_attempts
		   SET abandoned_at = COALESCE(abandoned_at, ?),
		       abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), ?)
		 WHERE id IN (SELECT id FROM ranked WHERE rn > 1)`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
		now, AttemptAbandonedDuplicateSameLaunchTerminal,
	)
	if err != nil {
		return 0, fmt.Errorf("abandon duplicate terminal cloud attempts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
