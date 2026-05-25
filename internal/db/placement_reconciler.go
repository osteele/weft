package db

import (
	"database/sql"
	"fmt"
	"time"
)

// ReconcileStaleCloudHostJobsOptions is reserved for future tuning; it has no
// fields today.
type ReconcileStaleCloudHostJobsOptions struct {
	Now time.Time
}

// ReconcileStaleCloudHostJobsResult summarizes the DB repairs made by a pass.
type ReconcileStaleCloudHostJobsResult struct {
	Actions     []StaleCloudHostAction
	JobsUpdated int
}

// StaleCloudHostAction records a single requeue from a dead cloud-instance
// host reference back onto the unplaced queue.
type StaleCloudHostAction struct {
	JobID  int64
	Detail string
}

// ReconcileStaleCloudHostJobs requeues jobs that reference cloud instances
// (`vastai:<id>`) whose backing launch is no longer in a live state. It runs
// from autopilot/reconcile passes; it never launches, destroys, or probes
// provider instances.
//
// Intent reconciliation (open MoveIntent / PlacementIntent rows that drifted
// past their actual placement) used to live alongside this function; it now
// happens atomically via triggers added in
// 00004_intent_auto_resolution.sql + 00005_intent_auto_resolution_launch_status.sql,
// and is intentionally not re-implemented here. The leak class that recurred
// (autopilot opens intent → sibling code path lands the placement → intent
// never closed) is now impossible by construction: any job_attempts insert/
// update onto a live launch resolves the intent in the same transaction.
func ReconcileStaleCloudHostJobs(database *sql.DB, opts ReconcileStaleCloudHostJobsOptions) (ReconcileStaleCloudHostJobsResult, error) {
	if database == nil {
		return ReconcileStaleCloudHostJobsResult{}, nil
	}
	nowTime := opts.Now
	if nowTime.IsZero() {
		nowTime = time.Now()
	}
	tx, err := database.Begin()
	if err != nil {
		return ReconcileStaleCloudHostJobsResult{}, err
	}
	now := nowTime.Unix()

	var result ReconcileStaleCloudHostJobsResult
	if err := reconcileStaleCloudHostJobsTx(tx, now, &result); err != nil {
		tx.Rollback()
		return ReconcileStaleCloudHostJobsResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReconcileStaleCloudHostJobsResult{}, err
	}
	return result, nil
}

func reconcileStaleCloudHostJobsTx(tx *sql.Tx, now int64, result *ReconcileStaleCloudHostJobsResult) error {
	rows, err := tx.Query(`
		SELECT id, host FROM job_status js
		WHERE status IN (?, ?) AND host LIKE 'vastai:%' AND tombstoned = 0
		AND NOT EXISTS (
			SELECT 1 FROM launches ci
			WHERE ci.id = CAST(SUBSTR(js.host, 8) AS INTEGER)
			AND ci.status IN (?, ?, ?, ?, ?)
		)`,
		StatusQueued, StatusRunning,
		LaunchStatusRunning, LaunchStatusLaunching, LaunchStatusPaused, LaunchStatusGrace, LaunchStatusCompleted,
	)
	if err != nil {
		return err
	}
	type staleCloudJob struct {
		id   int64
		host string
	}
	var jobs []staleCloudJob
	for rows.Next() {
		var job staleCloudJob
		if err := rows.Scan(&job.id, &job.host); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, job := range jobs {
		if err := closeAttemptsAndRequeueWithOutcome(tx, job.id, now, AttemptOutcomeOrphaned); err != nil {
			return err
		}
		if err := ensureUnplacedQueuedAttemptTx(tx, job.id); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice(orphanedCloudPlacementReasons(job.host)), job.id,
		); err != nil {
			return err
		}
		result.JobsUpdated++
		result.Actions = append(result.Actions, StaleCloudHostAction{
			JobID:  job.id,
			Detail: fmt.Sprintf("stale cloud host %s returned to unplaced queue", job.host),
		})
	}
	return nil
}

func ensureUnplacedQueuedAttemptTx(tx *sql.Tx, jobID int64) error {
	var openCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND end_time IS NULL`,
		jobID,
	).Scan(&openCount); err != nil {
		return err
	}
	if openCount > 0 {
		return nil
	}
	_, err := createAttemptTx(tx, jobID, "", nil, StatusQueued)
	return err
}
