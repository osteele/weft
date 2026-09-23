package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FailCloudDependency records a confirmed dependency failure only while the
// observed consumer still owns the same unstarted placement. A move destination
// is not authoritative and must never fail the source job.
func FailCloudDependency(database *sql.DB, observed *Job, detail string) error {
	return RetryOnDatabaseLocked(context.Background(), "fail cloud dependency", func() error {
		tx, err := database.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := failCloudDependencyTx(tx, observed, detail); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func failCloudDependencyTx(tx *sql.Tx, observed *Job, detail string) error {
	if observed == nil {
		return nil
	}
	job, err := scanJob(tx.QueryRow(`SELECT `+jobSelectColumns+` FROM job_status WHERE id = ?`, observed.ID))
	if err != nil {
		return err
	}
	if job == nil || job.Tombstoned || job.HasInventoryHost() {
		return nil
	}
	switch job.Status {
	case StatusQueued, StatusPendingPlacement, "orphaned":
	default:
		return nil
	}
	if (job.StartTime > 0 && job.EndTime == nil) || int64OrZero(job.LatestRunID) != int64OrZero(observed.LatestRunID) || int64OrZero(job.LaunchID) != int64OrZero(observed.LaunchID) {
		return nil
	}
	var moving bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM move_intents WHERE job_id = ? AND state = 'open')`, job.ID).Scan(&moving); err != nil {
		return err
	}
	if moving {
		return nil
	}
	attemptID := int64OrZero(job.LatestRunID)
	var ended sql.NullInt64
	if attemptID > 0 {
		if err := tx.QueryRow(`SELECT end_time FROM job_attempts WHERE id = ?`, attemptID).Scan(&ended); err != nil {
			return err
		}
	}
	if attemptID == 0 || ended.Valid {
		// An old retryable attempt is historical evidence, not this admission
		// failure. A terminal local attempt records the new diagnosis without
		// allocating a rental or altering its producer's attempt.
		attemptID, err = createAttemptTx(tx, job.ID, "", nil, StatusFailed)
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE job_attempts
		SET status = ?, end_time = ?, failure_reason = ?, error_message = ?,
		    pending_status = NULL, cloud_outcome = CASE WHEN launch_id IS NOT NULL THEN ? ELSE cloud_outcome END
		WHERE id = ?`, StatusFailed, time.Now().Unix(), FailureReasonDependencyFailed, detail, AttemptOutcomeFailed, attemptID); err != nil {
		return err
	}
	// requested_status=queued would hide an unstarted terminal attempt in
	// job_status and make the consumer eligible again on the next pass.
	_, err = tx.Exec(`UPDATE jobs SET requested_status = NULL, placement_reasons = NULL WHERE id = ?`, job.ID)
	return err
}

// CancelLaunchForCloudDependency closes a pre-provider launch and releases its
// healthy siblings atomically. failedJob is nil for pending or unknown evidence.
// Rejected move targets are abandoned without disturbing their source attempts.
func CancelLaunchForCloudDependency(database *sql.DB, launchID int64, failedJob *Job, detail string) error {
	return RetryOnDatabaseLocked(context.Background(), "cancel dependency launch", func() error {
		tx, err := database.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		launch, err := scanLaunchFrom(tx.QueryRow(`SELECT `+launchSelectColumns+` FROM launches WHERE id = ?`, launchID))
		if err != nil {
			return err
		}
		if IsTerminalLaunchStatus(launch.Status) {
			return tx.Commit()
		}
		if launch.EffectiveProviderID() != "" {
			return fmt.Errorf("launch %d already has a provider instance", launchID)
		}
		if err := failCloudDependencyTx(tx, failedJob, detail); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT job_id FROM move_intents WHERE target_launch_id = ? AND state = 'open'`, launchID)
		if err != nil {
			return err
		}
		var movingJobs []int64
		for rows.Next() {
			var jobID int64
			if err := rows.Scan(&jobID); err != nil {
				rows.Close()
				return err
			}
			movingJobs = append(movingJobs, jobID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, jobID := range movingJobs {
			if err := resolveOpenMoveIntentAbandonedTx(tx, jobID, now, detail); err != nil {
				return err
			}
		}
		status, reason := LaunchStatusCancelled, TerminationReasonCancelled
		if failedJob != nil {
			status, reason = LaunchStatusFailed, TerminationReasonInvalidRequest
		}
		if err := updateLaunchStatusWithExecer(tx, launchID, status, now, reason, detail); err != nil {
			return err
		}
		if _, err := resetLaunchJobsTx(tx, launchID, AttemptOutcomeCancelled); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// RejectReuseForCloudDependency releases only this submission's unstarted
// claims. The dependency failure and sibling release share one transaction;
// concurrent user cancellation, moves, or newer placements remain authoritative.
func RejectReuseForCloudDependency(database *sql.DB, claimedJobs []*Job, failedJob *Job, detail string) error {
	return RetryOnDatabaseLocked(context.Background(), "reject reuse dependency", func() error {
		tx, err := database.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := failCloudDependencyTx(tx, failedJob, detail); err != nil {
			return err
		}
		for _, observed := range claimedJobs {
			job, err := scanJob(tx.QueryRow(`SELECT `+jobSelectColumns+` FROM job_status WHERE id = ?`, observed.ID))
			if err != nil {
				return err
			}
			if job == nil || job.Tombstoned || job.StartTime > 0 ||
				(job.Status != StatusQueued && job.Status != StatusPendingPlacement) ||
				int64OrZero(job.LatestRunID) != int64OrZero(observed.LatestRunID) ||
				int64OrZero(job.LaunchID) != int64OrZero(observed.LaunchID) {
				continue
			}
			var moving bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM move_intents WHERE job_id = ? AND state = 'open')`, job.ID).Scan(&moving); err != nil {
				return err
			}
			if moving {
				continue
			}
			if err := closeAttemptsAndRequeueWithOutcome(tx, job.ID, time.Now().Unix(), AttemptOutcomeCancelled); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func int64OrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
