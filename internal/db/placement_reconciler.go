package db

import (
	"database/sql"
	"fmt"
	"time"
)

const defaultPlacementReconcileProtectionWindow = 5 * time.Minute

// PlacementReconcileOptions controls conservative stale-row repair. Now is
// injectable for tests; ProtectionWindow shields fresh in-flight placements.
type PlacementReconcileOptions struct {
	ProtectionWindow time.Duration
	Now              time.Time
}

// PlacementReconcileResult summarizes DB placement repairs made by a pass.
type PlacementReconcileResult struct {
	Actions         []PlacementReconcileAction
	JobsUpdated     int
	IntentsResolved int
}

// PlacementReconcileAction is a single placement convergence action.
type PlacementReconcileAction struct {
	Kind     string
	JobID    int64
	LaunchID int64
	IntentID int64
	Detail   string
}

const (
	PlacementReconcileMoveConfirmed      = "move_intent_confirmed"
	PlacementReconcileMoveRestored       = "move_intent_restored_source"
	PlacementReconcileMoveCanceled       = "move_intent_canceled"
	PlacementReconcilePlacementConfirmed = "placement_intent_confirmed"
	PlacementReconcilePlacementCanceled  = "placement_intent_canceled"
	PlacementReconcileStaleCloudHost     = "stale_cloud_host_requeued"
)

// ReconcilePlacementRows converges stale placement rows into a valid shape.
// It is intentionally DB-only: it repairs intent/attempt state but never
// launches, destroys, or probes provider instances.
func ReconcilePlacementRows(database *sql.DB, opts PlacementReconcileOptions) (PlacementReconcileResult, error) {
	if database == nil {
		return PlacementReconcileResult{}, nil
	}
	nowTime := opts.Now
	if nowTime.IsZero() {
		nowTime = time.Now()
	}
	protectionWindow := opts.ProtectionWindow
	if protectionWindow <= 0 {
		protectionWindow = defaultPlacementReconcileProtectionWindow
	}

	tx, err := database.Begin()
	if err != nil {
		return PlacementReconcileResult{}, err
	}
	now := nowTime.Unix()
	cutoff := nowTime.Add(-protectionWindow).Unix()

	var result PlacementReconcileResult
	if err := reconcileOpenMoveIntentsTx(tx, now, cutoff, &result); err != nil {
		tx.Rollback()
		return PlacementReconcileResult{}, err
	}
	if err := reconcileOpenPlacementIntentsTx(tx, now, cutoff, &result); err != nil {
		tx.Rollback()
		return PlacementReconcileResult{}, err
	}
	if err := reconcileStaleCloudHostJobsTx(tx, now, &result); err != nil {
		tx.Rollback()
		return PlacementReconcileResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return PlacementReconcileResult{}, err
	}
	return result, nil
}

type openMoveIntentFact struct {
	ID             int64
	JobID          int64
	SourceLaunchID sql.NullInt64
	TargetKind     MoveTargetKind
	TargetLaunchID sql.NullInt64
	AttemptCount   int
	MaxAttempts    int
	CreatedAt      int64
}

func reconcileOpenMoveIntentsTx(tx *sql.Tx, now, cutoff int64, result *PlacementReconcileResult) error {
	rows, err := tx.Query(`
		SELECT id, job_id, source_launch_id, target_kind, target_launch_id, attempt_count, max_attempts, created_at
		  FROM move_intents
		 WHERE state = ?
		 ORDER BY created_at ASC`,
		string(MoveIntentStateOpen),
	)
	if err != nil {
		return err
	}
	var intents []openMoveIntentFact
	for rows.Next() {
		var fact openMoveIntentFact
		var targetKind string
		if err := rows.Scan(&fact.ID, &fact.JobID, &fact.SourceLaunchID, &targetKind, &fact.TargetLaunchID, &fact.AttemptCount, &fact.MaxAttempts, &fact.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		fact.TargetKind = MoveTargetKind(targetKind)
		intents = append(intents, fact)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, intent := range intents {
		if intent.TargetKind != MoveTargetNew {
			continue
		}
		if err := reconcileOpenMoveIntentTx(tx, now, cutoff, intent, result); err != nil {
			return err
		}
	}
	return nil
}

func reconcileOpenMoveIntentTx(tx *sql.Tx, now, cutoff int64, intent openMoveIntentFact, result *PlacementReconcileResult) error {
	if !intent.TargetLaunchID.Valid || intent.TargetLaunchID.Int64 <= 0 {
		if intent.CreatedAt < cutoff && intent.AttemptCount >= intent.MaxAttempts {
			return resolveMoveIntentTx(tx, now, intent.ID, MoveIntentStateCanceled, "stale: no target launch before retry budget exhausted", result, PlacementReconcileMoveCanceled, intent.JobID, 0)
		}
		return nil
	}

	launchID := intent.TargetLaunchID.Int64
	var (
		launchStatus string
		agentReady   sql.NullInt64
	)
	err := tx.QueryRow(`SELECT status, agent_ready_at_unix FROM launches WHERE id = ?`, launchID).Scan(&launchStatus, &agentReady)
	if err == sql.ErrNoRows {
		if intent.CreatedAt < cutoff && intent.AttemptCount >= intent.MaxAttempts {
			return resolveMoveIntentTx(tx, now, intent.ID, MoveIntentStateCanceled, "stale: target launch missing before retry budget exhausted", result, PlacementReconcileMoveCanceled, intent.JobID, launchID)
		}
		return nil
	}
	if err != nil {
		return err
	}

	started, err := moveIntentTargetStartedJobTx(tx, intent.JobID, launchID)
	if err != nil {
		return err
	}
	if (agentReady.Valid && agentReady.Int64 > 0) || started {
		return resolveMoveIntentTx(tx, now, intent.ID, MoveIntentStateConfirmed, "target accepted job before terminal", result, PlacementReconcileMoveConfirmed, intent.JobID, launchID)
	}
	if IsLiveLaunchStatus(launchStatus) {
		return nil
	}
	latestOpenLaunch, hasOpenAttempt, err := latestOpenAttemptLaunchTx(tx, intent.JobID)
	if err != nil {
		return err
	}
	if hasOpenAttempt && latestOpenLaunch != launchID {
		if intent.SourceLaunchID.Valid && latestOpenLaunch == intent.SourceLaunchID.Int64 {
			if intent.AttemptCount < intent.MaxAttempts {
				return nil
			}
			resolved, err := resolveMoveIntentRestoredToSourceTx(tx, now, intent.ID)
			if err != nil {
				return err
			}
			if resolved {
				result.IntentsResolved++
				result.Actions = append(result.Actions, PlacementReconcileAction{
					Kind:     PlacementReconcileMoveCanceled,
					JobID:    intent.JobID,
					LaunchID: latestOpenLaunch,
					IntentID: intent.ID,
					Detail:   "move target failed before start; restored to source",
				})
			}
			return nil
		}
		return nil
	}

	transition, err := handleMoveTargetFailedBeforeStartTx(tx, intent.JobID, launchID, now, AttemptOutcomeOrphaned)
	if err != nil {
		return err
	}
	if transition.Handled {
		result.JobsUpdated++
		result.Actions = append(result.Actions, PlacementReconcileAction{
			Kind:     PlacementReconcileMoveRestored,
			JobID:    intent.JobID,
			LaunchID: transition.SourceLaunchID,
			IntentID: intent.ID,
			Detail:   fmt.Sprintf("move target launch %d failed before start; restored source launch %d", launchID, transition.SourceLaunchID),
		})
		if transition.Exhausted {
			result.IntentsResolved++
		}
		return nil
	}
	if intent.AttemptCount < intent.MaxAttempts {
		return nil
	}

	if err := closeAttemptsAndRequeueWithOutcome(tx, intent.JobID, now, AttemptOutcomeOrphaned); err != nil {
		return err
	}
	if err := ensureUnplacedQueuedAttemptTx(tx, intent.JobID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
		encodeStringSlice([]string{fmt.Sprintf("move target instance %d failed before start; source launch unavailable", launchID)}),
		intent.JobID,
	); err != nil {
		return err
	}
	if err := resolveMoveIntentTx(tx, now, intent.ID, MoveIntentStateCanceled, "move target failed before start; source unavailable", result, PlacementReconcileMoveCanceled, intent.JobID, launchID); err != nil {
		return err
	}
	result.JobsUpdated++
	return nil
}

func latestOpenAttemptLaunchTx(tx *sql.Tx, jobID int64) (int64, bool, error) {
	var launchID sql.NullInt64
	err := tx.QueryRow(
		`SELECT launch_id
		   FROM job_attempts
		  WHERE job_id = ?
		    AND end_time IS NULL
		  ORDER BY attempt_number DESC
		  LIMIT 1`,
		jobID,
	).Scan(&launchID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !launchID.Valid {
		return 0, true, nil
	}
	return launchID.Int64, true, nil
}

func moveIntentTargetStartedJobTx(tx *sql.Tx, jobID, launchID int64) (bool, error) {
	var count int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND launch_id = ? AND start_time IS NOT NULL AND start_time > 0`,
		jobID, launchID,
	).Scan(&count)
	return count > 0, err
}

func resolveMoveIntentTx(tx *sql.Tx, now, intentID int64, state MoveIntentState, resolution string, result *PlacementReconcileResult, actionKind string, jobID, launchID int64) error {
	res, err := tx.Exec(
		`UPDATE move_intents
		    SET state = ?, resolved_at = ?, resolution = ?
		  WHERE id = ?
		    AND state = ?`,
		string(state), now, resolution, intentID, string(MoveIntentStateOpen),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil
	}
	result.IntentsResolved++
	result.Actions = append(result.Actions, PlacementReconcileAction{
		Kind:     actionKind,
		JobID:    jobID,
		LaunchID: launchID,
		IntentID: intentID,
		Detail:   resolution,
	})
	return nil
}

func reconcileOpenPlacementIntentsTx(tx *sql.Tx, now, cutoff int64, result *PlacementReconcileResult) error {
	rows, err := tx.Query(`
		SELECT pi.id, pi.job_id, pi.created_at,
		       COALESCE(l.status, '')
		  FROM placement_intents pi
		  LEFT JOIN job_attempts ja
		    ON ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = pi.job_id)
		  LEFT JOIN launches l ON l.id = ja.launch_id
		 WHERE pi.state = ?
		 ORDER BY pi.created_at ASC`,
		string(PlacementIntentStateOpen),
	)
	if err != nil {
		return err
	}
	type fact struct {
		id           int64
		jobID        int64
		createdAt    int64
		launchStatus string
	}
	var facts []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.id, &f.jobID, &f.createdAt, &f.launchStatus); err != nil {
			rows.Close()
			return err
		}
		facts = append(facts, f)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, f := range facts {
		switch {
		case IsLiveLaunchStatus(f.launchStatus):
			if err := resolvePlacementIntentTx(tx, now, f.id, PlacementIntentStateConfirmed, "placement has live launch", result, PlacementReconcilePlacementConfirmed, f.jobID); err != nil {
				return err
			}
		case f.createdAt < cutoff:
			if err := resolvePlacementIntentTx(tx, now, f.id, PlacementIntentStateCanceled, PlacementIntentResolutionStale, result, PlacementReconcilePlacementCanceled, f.jobID); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolvePlacementIntentTx(tx *sql.Tx, now, intentID int64, state PlacementIntentState, resolution string, result *PlacementReconcileResult, actionKind string, jobID int64) error {
	res, err := tx.Exec(
		`UPDATE placement_intents
		    SET state = ?, resolved_at = ?, resolution = ?
		  WHERE id = ?
		    AND state = ?`,
		string(state), now, resolution, intentID, string(PlacementIntentStateOpen),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil
	}
	result.IntentsResolved++
	result.Actions = append(result.Actions, PlacementReconcileAction{
		Kind:     actionKind,
		JobID:    jobID,
		IntentID: intentID,
		Detail:   resolution,
	})
	return nil
}

func reconcileStaleCloudHostJobsTx(tx *sql.Tx, now int64, result *PlacementReconcileResult) error {
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
		result.Actions = append(result.Actions, PlacementReconcileAction{
			Kind:   PlacementReconcileStaleCloudHost,
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
