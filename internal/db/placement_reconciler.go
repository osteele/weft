package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
	now := nowTime.Unix()

	// Retry the whole transaction on transient SQLITE_BUSY. A rolled-back tx
	// re-reads the current stale set and re-applies from scratch, so retry is
	// idempotent; dropping this pass leaves jobs stranded on dead cloud-host
	// references instead of returning them to the unplaced queue.
	return RetryOnDatabaseLockedValue(context.Background(), "reconcile stale cloud-host jobs", func() (ReconcileStaleCloudHostJobsResult, error) {
		tx, err := database.Begin()
		if err != nil {
			return ReconcileStaleCloudHostJobsResult{}, err
		}

		var result ReconcileStaleCloudHostJobsResult
		if err := reconcileStaleCloudHostJobsTx(tx, now, &result); err != nil {
			tx.Rollback()
			return ReconcileStaleCloudHostJobsResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return ReconcileStaleCloudHostJobsResult{}, err
		}
		return result, nil
	})
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

// rebalancePlacementReasonPrefix marks placement reasons written by a successful
// queue rebalance move (orchestration.rebalancePlacementReason, formatted as
// "rebalanced from <src> → <dst> (cost ratio <r>)"). Such a reason describes a
// completed instance→instance move and is only meaningful while the job remains
// placed on the destination launch. It is recognized here as a persisted data
// contract so stale copies can be superseded once the job falls back to the
// unplaced queue.
const rebalancePlacementReasonPrefix = "rebalanced from "

// queuedAwaitingPlacementReason is the neutral live cause recorded for a
// queued+unplaced job whose only stored placement reason was stale rebalance
// history. The next placement pass replaces it with the real block reason.
const queuedAwaitingPlacementReason = "queued; awaiting placement"

// IsRebalancePlacementReason reports whether reason was written by a queue
// rebalance move.
func IsRebalancePlacementReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), rebalancePlacementReasonPrefix)
}

// RefreshStalePlacementReasonsResult summarizes a refresh pass.
type RefreshStalePlacementReasonsResult struct {
	JobsUpdated int
}

// RefreshStalePlacementReasons supersedes placement reasons that describe a
// completed placement action (a queue rebalance move) on jobs that have since
// fallen back to the unplaced queue. A "rebalanced from X → Y" reason is only
// meaningful while the job is placed on Y; once the job is queued with no host
// and no live launch attempt, that reason is stale history that misleads
// `weft diagnose`/status (e.g. it shows a failed rebalance while the job is
// actually free to place). Such reasons are removed; if that empties the list,
// the job's live cause ("queued; awaiting placement") is recorded so the next
// placement pass repopulates the real block reason. Live block reasons
// (waiting-for-producer, no-offers, SSH timeout, manual move) are left
// untouched — they describe why the job is currently unplaced.
//
// This complements ReconcileStaleCloudHostJobs, which only refreshes reasons on
// jobs still referencing a dead `vastai:<id>` host; a job that already fell back
// to an empty host keeps its stale rebalance reason without this pass.
func RefreshStalePlacementReasons(database *sql.DB) (RefreshStalePlacementReasonsResult, error) {
	if database == nil {
		return RefreshStalePlacementReasonsResult{}, nil
	}
	return RetryOnDatabaseLockedValue(context.Background(), "refresh stale placement reasons", func() (RefreshStalePlacementReasonsResult, error) {
		tx, err := database.Begin()
		if err != nil {
			return RefreshStalePlacementReasonsResult{}, err
		}
		var result RefreshStalePlacementReasonsResult
		if err := refreshStalePlacementReasonsTx(tx, &result); err != nil {
			tx.Rollback()
			return RefreshStalePlacementReasonsResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return RefreshStalePlacementReasonsResult{}, err
		}
		return result, nil
	})
}

func refreshStalePlacementReasonsTx(tx *sql.Tx, result *RefreshStalePlacementReasonsResult) error {
	rows, err := tx.Query(`
		SELECT id, placement_reasons FROM job_status
		WHERE status = ?
		  AND COALESCE(host, '') = ''
		  AND COALESCE(launch_id, 0) = 0
		  AND tombstoned = 0
		  AND placement_reasons LIKE ?`,
		StatusQueued, "%"+rebalancePlacementReasonPrefix+"%",
	)
	if err != nil {
		return err
	}
	type staleJob struct {
		id      int64
		reasons sql.NullString
	}
	var jobs []staleJob
	for rows.Next() {
		var j staleJob
		if err := rows.Scan(&j.id, &j.reasons); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, j := range jobs {
		reasons := decodeStringSlice(j.reasons)
		kept := make([]string, 0, len(reasons))
		removed := false
		for _, r := range reasons {
			if IsRebalancePlacementReason(r) {
				removed = true
				continue
			}
			kept = append(kept, r)
		}
		if !removed {
			continue
		}
		if len(kept) == 0 {
			kept = []string{queuedAwaitingPlacementReason}
		}
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice(kept), j.id,
		); err != nil {
			return err
		}
		result.JobsUpdated++
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
