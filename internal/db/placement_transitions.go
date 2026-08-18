package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ClaimJobForLaunch creates a fresh open placement attempt for jobID on
// launchID. It rejects jobs already owned by another active launch.
func ClaimJobForLaunch(database *sql.DB, jobID, launchID int64) error {
	return RetryOnDatabaseLocked(context.Background(), "claim job for launch", func() error {
		return setJobLaunchIDOnce(database, jobID, launchID, false)
	})
}

// TransferJobToLaunch creates a fresh open placement attempt for jobID on
// launchID while deliberately superseding any prior active launch owner.
func TransferJobToLaunch(database *sql.DB, jobID, launchID int64) error {
	return RetryOnDatabaseLocked(context.Background(), "transfer job to launch", func() error {
		return setJobLaunchIDOnce(database, jobID, launchID, true)
	})
}

// AttachOpenAttemptToLaunch links the latest open attempt to a launch observed
// from cloud state. This is narrower than ClaimJobForLaunch: it preserves the
// existing attempt and only fills the launch target fields.
func AttachOpenAttemptToLaunch(database *sql.DB, jobID int64, launchID int64) error {
	if err := EnsureRentalExecutionTarget(database, launchID); err != nil {
		return err
	}
	_, err := database.Exec(`
		UPDATE job_attempts
		SET launch_id = ?, host = ?, target_id = (
		        SELECT id FROM execution_targets
		         WHERE kind = 'rental_instance' AND launch_id = ?
		    ),
		    pending_status = CASE
		        WHEN pending_status = ? THEN ?
		        ELSE pending_status
		    END
		WHERE id = `+latestOpenAttemptSubquery,
		launchID, LaunchHost(launchID), launchID, StatusPendingPlacement, StatusQueued, jobID,
	)
	return err
}

// AssignQueuedJobToInventoryHost moves the open non-rental attempt for jobID
// to an inventory host. It rejects jobs already claimed by a rental launch.
func AssignQueuedJobToInventoryHost(database *sql.DB, jobID int64, host string) error {
	targetID, err := ensureExecutionTargetForAttempt(database, host, nil)
	if err != nil {
		return err
	}
	result, err := database.Exec(`
		UPDATE job_attempts
		   SET host = ?, target_id = ?
		 WHERE job_id = ?
		   AND end_time IS NULL
		   AND launch_id IS NULL`,
		host, targetID, jobID,
	)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows > 0 {
		return nil
	}
	var launchID sql.NullInt64
	err = database.QueryRow(`
 		SELECT launch_id FROM job_attempts
 		WHERE job_id = ? AND end_time IS NULL
 		ORDER BY attempt_number DESC LIMIT 1`, jobID).Scan(&launchID)
	if err == nil && launchID.Valid {
		return fmt.Errorf("job %d is already assigned to launch %d: %w", jobID, launchID.Int64, ErrJobAlreadyClaimed)
	}
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

// MoveTargetFailureResult reports how a failed move-to-new target was consumed.
type MoveTargetFailureResult struct {
	Handled        bool
	Retryable      bool
	Exhausted      bool
	SourceLaunchID int64
}

// AttachMoveIntentTargetLaunch records a new target launch for a move-to-new
// intent. Initial attachments fill target_launch_id; replacements advance the
// durable attempt counter.
func AttachMoveIntentTargetLaunch(database *sql.DB, intent *MoveIntent, launchID int64) error {
	if intent == nil || intent.ID <= 0 || launchID <= 0 {
		return nil
	}
	if intent.TargetLaunchID != nil && *intent.TargetLaunchID > 0 && *intent.TargetLaunchID != launchID {
		return AdvanceMoveIntentTargetLaunch(database, intent.ID, launchID)
	}
	return UpdateMoveIntentTargetLaunch(database, intent.ID, launchID)
}

// HandleMoveTargetFailedBeforeStart consumes a failed move-to-new target if
// the job never started on that target. Source placement is restored while the
// intent remains open until its durable launch-attempt budget is exhausted.
func HandleMoveTargetFailedBeforeStart(database *sql.DB, jobID, targetLaunchID int64, outcome string) (MoveTargetFailureResult, error) {
	tx, err := database.Begin()
	if err != nil {
		return MoveTargetFailureResult{}, err
	}
	now := time.Now().Unix()
	result, err := handleMoveTargetFailedBeforeStartTx(tx, jobID, targetLaunchID, now, outcome)
	if err != nil {
		tx.Rollback()
		return MoveTargetFailureResult{}, err
	}
	if result.Handled {
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice([]string{fmt.Sprintf("move target instance %d failed before start; job restored to source queue", targetLaunchID)}),
			jobID,
		); err != nil {
			tx.Rollback()
			return MoveTargetFailureResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return MoveTargetFailureResult{}, err
	}
	return result, nil
}

// FailedMoveTargetMachineIDs returns provider machine IDs that have already
// failed before starting this move-to-new intent's job. These machines should
// be skipped when selecting a replacement target for the same move.
func FailedMoveTargetMachineIDs(database *sql.DB, intent *MoveIntent) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if database == nil || intent == nil || intent.JobID <= 0 || intent.TargetKind != MoveTargetNew {
		return out, nil
	}
	sourceLaunchID := int64(0)
	if intent.SourceLaunchID != nil {
		sourceLaunchID = *intent.SourceLaunchID
	}
	rows, err := database.Query(`
		SELECT DISTINCT COALESCE(l.provider, ''), COALESCE(l.machine_id, '')
		  FROM job_attempts ja
		  JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.job_id = ?
		   AND ja.launch_id IS NOT NULL
		   AND ja.launch_id != ?
		   AND COALESCE(l.machine_id, '') != ''
		   AND (ja.start_time IS NULL OR ja.start_time = 0)
		   AND (
		        ja.cloud_outcome IN (?, ?)
		        OR l.termination_reason IN (?, ?)
		   )`,
		intent.JobID,
		sourceLaunchID,
		AttemptOutcomeOrphaned,
		AttemptOutcomeFailed,
		TerminationReasonInfraFailure,
		TerminationReasonProviderFailure,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider string
		var machineID string
		if err := rows.Scan(&provider, &machineID); err != nil {
			return nil, err
		}
		if key := ProviderMachineKey(provider, machineID); key != "" {
			out[key] = struct{}{}
		}
	}
	return out, rows.Err()
}

// FailedTorchPreflightMachineIDs returns provider machine IDs where this job
// already failed the weft-owned torch CUDA preflight. Replacement placement
// should avoid those machines when the provider exposes machine identity.
func FailedTorchPreflightMachineIDs(database *sql.DB, jobID int64) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if database == nil || jobID <= 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT DISTINCT COALESCE(l.provider, ''), COALESCE(l.machine_id, '')
		  FROM job_attempts ja
		  JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.job_id = ?
		   AND ja.launch_id IS NOT NULL
		   AND COALESCE(l.machine_id, '') != ''
		   AND COALESCE(ja.failure_reason, '') = ?`,
		jobID,
		FailureReasonInfraTorchPreflightFailed,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider string
		var machineID string
		if err := rows.Scan(&provider, &machineID); err != nil {
			return nil, err
		}
		if key := ProviderMachineKey(provider, machineID); key != "" {
			out[key] = struct{}{}
		}
	}
	return out, rows.Err()
}

func handleMoveTargetFailedBeforeStartTx(tx *sql.Tx, jobID, targetLaunchID, now int64, outcome string) (MoveTargetFailureResult, error) {
	var (
		intentID        int64
		sourceLaunchID  int64
		sourceStatus    string
		targetAttemptID int64
		targetStarted   sql.NullInt64
		targetEnded     sql.NullInt64
		attemptCount    int
		maxAttempts     int
	)
	err := tx.QueryRow(`
		SELECT mi.id, mi.source_launch_id, COALESCE(src.status, ''), target.id, target.start_time, target.end_time,
		       mi.attempt_count, mi.max_attempts
		  FROM move_intents mi
		  JOIN launches src ON src.id = mi.source_launch_id
		  JOIN job_attempts target
		    ON target.job_id = mi.job_id
		   AND target.launch_id = mi.target_launch_id
		 WHERE mi.job_id = ?
		   AND mi.target_launch_id = ?
		   AND mi.target_kind = ?
		   AND mi.state = ?
		 ORDER BY target.attempt_number DESC, mi.created_at DESC
		 LIMIT 1`,
		jobID, targetLaunchID, string(MoveTargetNew), string(MoveIntentStateOpen),
	).Scan(&intentID, &sourceLaunchID, &sourceStatus, &targetAttemptID, &targetStarted, &targetEnded, &attemptCount, &maxAttempts)
	if err == sql.ErrNoRows {
		return MoveTargetFailureResult{}, nil
	}
	if err != nil {
		return MoveTargetFailureResult{}, err
	}
	if sourceLaunchID <= 0 || !IsLiveLaunchStatus(sourceStatus) {
		return MoveTargetFailureResult{}, nil
	}
	if targetStarted.Valid && targetStarted.Int64 > 0 {
		return MoveTargetFailureResult{}, nil
	}

	if !targetEnded.Valid || targetEnded.Int64 <= 0 {
		// From-status guard: the target attempt was proven not-yet-started
		// (target.start_time invalid, checked above) and the end_time clause
		// scopes to open attempts — an unstarted move target is
		// queued/pending_placement, from which canceled is table-valid.
		if _, err := tx.Exec(
			`UPDATE job_attempts
			    SET status = ?,
			        end_time = COALESCE(NULLIF(end_time, 0), ?),
			        cloud_outcome = COALESCE(cloud_outcome, NULLIF(?, ''))
			  WHERE id = ?
			    AND (end_time IS NULL OR end_time = 0)`,
			StatusCanceled, now, outcome, targetAttemptID); err != nil {
			return MoveTargetFailureResult{}, err
		}
	}

	if err := ensureMoveSourceAttemptTx(tx, jobID, sourceLaunchID); err != nil {
		return MoveTargetFailureResult{}, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = NULL WHERE id = ?`, jobID); err != nil {
		return MoveTargetFailureResult{}, err
	}

	exhausted := attemptCount >= maxAttempts
	if exhausted {
		if _, err := resolveMoveIntentRestoredToSourceTx(tx, now, intentID); err != nil {
			return MoveTargetFailureResult{}, err
		}
	}
	return MoveTargetFailureResult{
		Handled:        true,
		Retryable:      !exhausted,
		Exhausted:      exhausted,
		SourceLaunchID: sourceLaunchID,
	}, nil
}

func resolveMoveIntentRestoredToSourceTx(tx *sql.Tx, now, intentID int64) (bool, error) {
	res, err := tx.Exec(
		`UPDATE move_intents
		    SET state = ?, resolved_at = ?, resolution = ?
		  WHERE id = ?
		    AND state = ?`,
		string(MoveIntentStateCanceled), now, "move target failed before start; restored to source",
		intentID, string(MoveIntentStateOpen),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func ensureMoveSourceAttemptTx(tx *sql.Tx, jobID, sourceLaunchID int64) error {
	var sourceOpenCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*)
		   FROM job_attempts
		  WHERE job_id = ?
		    AND launch_id = ?
		    AND end_time IS NULL`,
		jobID, sourceLaunchID,
	).Scan(&sourceOpenCount); err != nil {
		return err
	}
	if sourceOpenCount > 0 {
		return nil
	}

	var predecessorID sql.NullInt64
	_ = tx.QueryRow(
		`SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&predecessorID)

	newAttemptID, err := createAttemptTx(tx, jobID, "", &sourceLaunchID, StatusQueued)
	if err != nil {
		return err
	}
	if predecessorID.Valid {
		if _, err := tx.Exec(
			`UPDATE job_attempts SET predecessor_attempt_id = ? WHERE id = ?`,
			predecessorID.Int64, newAttemptID,
		); err != nil {
			return fmt.Errorf("link predecessor attempt %d -> %d: %w", predecessorID.Int64, newAttemptID, err)
		}
	}
	return nil
}
