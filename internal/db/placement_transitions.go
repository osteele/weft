package db

import (
	"database/sql"
	"fmt"
	"time"
)

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
		SELECT DISTINCT COALESCE(l.machine_id, '')
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
		var machineID string
		if err := rows.Scan(&machineID); err != nil {
			return nil, err
		}
		if machineID != "" {
			out[machineID] = struct{}{}
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
		if _, err := tx.Exec(
			`UPDATE move_intents
			    SET state = ?, resolved_at = ?, resolution = ?
			  WHERE id = ?
			    AND state = ?`,
			string(MoveIntentStateCanceled), now, "move target failed before start; restored to source",
			intentID, string(MoveIntentStateOpen),
		); err != nil {
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
