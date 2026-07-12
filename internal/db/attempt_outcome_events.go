package db

import (
	"database/sql"
	"fmt"
)

type AttemptOutcomeEvent struct {
	JobID   int64
	Outcome string
	AtUnix  int64
}

// LatestAttemptOutcomeEvents returns the latest cloud outcome event per job.
// It includes both explicitly stamped attempt outcomes and the same derived
// orphan evidence used by job_status: an authoritative attempt on a failed or
// canceled launch before the repair pass has stamped cloud_outcome.
func LatestAttemptOutcomeEvents(database *sql.DB, jobIDs []int64) (map[int64]AttemptOutcomeEvent, error) {
	out := make(map[int64]AttemptOutcomeEvent, len(jobIDs))
	if database == nil || len(jobIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(jobIDs))
	args := make([]any, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		if jobID <= 0 {
			continue
		}
		if _, ok := seen[jobID]; ok {
			continue
		}
		seen[jobID] = struct{}{}
		args = append(args, jobID)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := database.Query(`
		WITH candidates AS (
			SELECT job_id,
			       cloud_outcome AS outcome,
			       COALESCE(
			           CASE
			             WHEN cloud_outcome IN ('orphaned', 'canceled') THEN l.ended_at
			             ELSE NULL
			           END,
			           ja.end_time,
			           ja.queued_at,
			           ja.start_time,
			           0
			       ) AS event_at,
			       ja.attempt_number,
			       ja.id
			  FROM job_attempts ja
			  LEFT JOIN launches l ON l.id = ja.launch_id
			 WHERE ja.job_id IN (`+sqlPlaceholders(len(args))+`)
			   AND COALESCE(ja.cloud_outcome, '') != ''
			UNION ALL
			SELECT js.id AS job_id,
			       CASE WHEN l.status = 'canceled' THEN 'canceled' ELSE 'orphaned' END AS outcome,
			       COALESCE(l.ended_at, ja.end_time, ja.queued_at, ja.start_time, 0) AS event_at,
			       ja.attempt_number,
			       ja.id
			  FROM job_status js
			  JOIN authoritative_job_attempts ja ON ja.id = js.latest_run_id
			  JOIN launches l ON l.id = ja.launch_id
			 WHERE js.id IN (`+sqlPlaceholders(len(args))+`)
			   AND js.status = 'orphaned'
			   AND l.status IN ('failed', 'canceled')
			   AND COALESCE(ja.cloud_outcome, '') = ''
		),
		ranked AS (
			SELECT job_id,
			       outcome,
			       event_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY job_id
			           ORDER BY event_at DESC, attempt_number DESC, id DESC
			       ) AS rn
			  FROM candidates
		)
		SELECT job_id, outcome, event_at
		  FROM ranked
		 WHERE rn = 1`,
		append(args, args...)...,
	)
	if err != nil {
		return nil, fmt.Errorf("query latest attempt outcome events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event AttemptOutcomeEvent
		if err := rows.Scan(&event.JobID, &event.Outcome, &event.AtUnix); err != nil {
			return nil, fmt.Errorf("scan latest attempt outcome event: %w", err)
		}
		out[event.JobID] = event
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest attempt outcome events: %w", err)
	}
	return out, nil
}
