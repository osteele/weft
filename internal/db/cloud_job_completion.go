package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/status"
)

// recordCloudCompletionLockHook, when non-nil, is consulted at the start of each
// completion write attempt. Tests set it to simulate transient SQLITE_BUSY so the
// retry wrapper can be exercised end-to-end; it is nil in production.
var recordCloudCompletionLockHook func() error

var (
	// ErrCloudCompletionAttemptNotFound marks a completion for a run that no longer has a DB attempt row.
	ErrCloudCompletionAttemptNotFound = errors.New("cloud completion attempt not found")
	// ErrCloudCompletionAttemptAbandoned marks a completion for a run that has been removed from job authority.
	ErrCloudCompletionAttemptAbandoned = errors.New("cloud completion attempt abandoned")
)

// CloudJobCompletionResult describes the DB effects of a cloud completion.
type CloudJobCompletionResult struct {
	LaunchID     int64
	Transitioned bool
}

// IsPermanentCloudCompletionNoop reports whether a completion can never be recorded for its run.
func IsPermanentCloudCompletionNoop(err error) bool {
	return errors.Is(err, ErrCloudCompletionAttemptNotFound) || errors.Is(err, ErrCloudCompletionAttemptAbandoned)
}

// RecordCloudJobCompletion updates the job attempt in the DB with the given
// exit code, times, and failure reason. Returns the cloud instance ID if the
// job was assigned to one.
//
// markerLastModified is the LastModified timestamp of the R2 .complete marker
// (zero time if unavailable). It is used as a fallback for end_time when the
// completion JSON is missing — the marker is uploaded by the agent immediately
// after the job exits, so its LastModified is within ~1s of the true end time.
//
// When neither endTimeUnix nor markerLastModified yields a real timestamp, the
// row is left with NULL end_time and last_synced_status is cleared so that
// NeedsCloudCompletionBackfill keeps returning true and a later sync that
// finds the JSON will overwrite the placeholder values. This avoids silently
// substituting wall-clock sync time as a completion timestamp.
//
// killReason is the kill_reason from the completion record ("" when absent).
// KillReasonUserKill means the non-zero exit was produced by weft's own kill
// signal: the marker confirms the user's stop, so the attempt keeps its
// user-intended killed/canceled status (with metadata backfilled) rather than
// being recorded as failed.
//
// The write is retried on transient SQLITE_BUSY. Under multi-process contention
// (concurrent agents, TUIs, and sync/reconcile passes all writing jobs.db) a
// lock burst must not drop the completion — dropping it leaves a finished job
// stuck in "running" and corrupts the DB, the system's source of truth. The
// body is idempotent on re-entry: a partially-applied completion re-runs as a
// same-status no-op (see checkTransition and the target-state UPDATEs below).
func RecordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason, killReason string, markerLastModified time.Time, runID int64) (int64, error) {
	result, err := RecordCloudJobCompletionWithTransition(database, jobID, exitCode, startTimeUnix, endTimeUnix, failureReason, killReason, markerLastModified, runID)
	return result.LaunchID, err
}

// RecordCloudJobCompletionWithTransition is like RecordCloudJobCompletion, and
// also reports whether this call moved a non-terminal attempt into terminal
// state. Notification senders must use this result instead of a separate
// preflight status read, because concurrent sync/reconcile processes can both
// observe the old state before either writes.
func RecordCloudJobCompletionWithTransition(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason, killReason string, markerLastModified time.Time, runID int64) (CloudJobCompletionResult, error) {
	return RetryOnDatabaseLockedValue(context.Background(), "record cloud job completion", func() (CloudJobCompletionResult, error) {
		if recordCloudCompletionLockHook != nil {
			if err := recordCloudCompletionLockHook(); err != nil {
				return CloudJobCompletionResult{}, err
			}
		}
		return recordCloudJobCompletion(database, jobID, exitCode, startTimeUnix, endTimeUnix, failureReason, killReason, markerLastModified, runID)
	})
}

func recordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason, killReason string, markerLastModified time.Time, runID int64) (CloudJobCompletionResult, error) {
	targetStatus := StatusCompleted
	outcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		targetStatus = StatusFailed
		outcome = AttemptOutcomeFailed
		if killReason == KillReasonUserKill {
			outcome = AttemptOutcomeCancelled
			if cur := getAttemptStatus(database, jobID, false); IsTerminalStatus(cur) {
				targetStatus = cur
			} else {
				targetStatus = StatusKilled
			}
		}
	}

	var cloudInstanceID sql.NullInt64
	if runID > 0 {
		var abandonedAt sql.NullInt64
		err := database.QueryRow(
			`SELECT launch_id, abandoned_at FROM job_attempts WHERE id = ? AND job_id = ?`,
			runID, jobID,
		).Scan(&cloudInstanceID, &abandonedAt)
		if err == sql.ErrNoRows {
			return CloudJobCompletionResult{}, fmt.Errorf("%w: job %d run %d", ErrCloudCompletionAttemptNotFound, jobID, runID)
		}
		if err != nil {
			return CloudJobCompletionResult{}, err
		}
		if abandonedAt.Valid {
			return CloudJobCompletionResult{}, fmt.Errorf("%w: job %d run %d", ErrCloudCompletionAttemptAbandoned, jobID, runID)
		}
	}
	if err := checkTransition(database, jobID, targetStatus, true, status.SourceR2Completion); err != nil {
		return CloudJobCompletionResult{}, err
	}

	// endTimeUnix == 0 means the completion JSON wasn't ingested. Prefer the
	// marker's LastModified (close to true end time); leave NULL otherwise so
	// the row remains backfill-eligible.
	var endTimeArg any
	authoritativeTimes := endTimeUnix != 0
	switch {
	case endTimeUnix != 0:
		endTimeArg = endTimeUnix
	case !markerLastModified.IsZero():
		endTimeArg = markerLastModified.Unix()
	default:
		endTimeArg = nil
	}

	var startTimeArg any
	if startTimeUnix != 0 {
		startTimeArg = startTimeUnix
	} else {
		startTimeArg = nil
	}

	// last_synced_status: only mark fully synced when we have authoritative
	// times from the completion JSON. On the marker-only path, leave it NULL
	// so backfill stays armed for a later sync that finds the JSON.
	var lastSyncedStatusArg any
	if authoritativeTimes {
		lastSyncedStatusArg = targetStatus
	} else {
		lastSyncedStatusArg = nil
	}

	if runID == 0 {
		if err := database.QueryRow(`SELECT launch_id FROM job_status WHERE id = ? AND tombstoned = 0`, jobID).Scan(&cloudInstanceID); err != nil {
			return CloudJobCompletionResult{}, err
		}
	}

	if runID == 0 && !cloudInstanceID.Valid {
		if err := database.QueryRow(
			`SELECT launch_id FROM authoritative_job_attempts
			 WHERE job_id = ?
			 ORDER BY attempt_number DESC
			 LIMIT 1`,
			jobID,
		).Scan(&cloudInstanceID); err != nil && err != sql.ErrNoRows {
			return CloudJobCompletionResult{}, err
		}
	}
	transitioned := false
	// Cloud completion is authoritative for the live owner attempt. Abandoned
	// move attempts are audit rows and must not be revived by late source or
	// destination completion metadata.
	if runID > 0 {
		res, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
			     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
			     cloud_outcome = ?
			 WHERE id = ? AND job_id = ? AND abandoned_at IS NULL
			   AND status NOT IN (?, ?, ?, ?, ?, ?)`,
			targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome, runID, jobID,
			StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, StatusDraft,
		)
		if err != nil {
			return CloudJobCompletionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return CloudJobCompletionResult{}, err
		}
		transitioned = n > 0
		if !transitioned {
			if err := checkTransition(database, jobID, targetStatus, true, status.SourceR2Completion); err != nil {
				return CloudJobCompletionResult{}, err
			}
			if _, err := database.Exec(
				`UPDATE job_attempts
				 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
				     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
				     cloud_outcome = ?
				 WHERE id = ? AND job_id = ? AND abandoned_at IS NULL`,
				targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome, runID, jobID,
			); err != nil {
				return CloudJobCompletionResult{}, err
			}
		}
	} else {
		res, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
			     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
			     cloud_outcome = ?
			 WHERE id = (SELECT id FROM authoritative_job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)
			   AND status NOT IN (?, ?, ?, ?, ?, ?)`,
			targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome, jobID,
			StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, StatusDraft,
		)
		if err != nil {
			return CloudJobCompletionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return CloudJobCompletionResult{}, err
		}
		transitioned = n > 0
		if !transitioned {
			if err := checkTransition(database, jobID, targetStatus, true, status.SourceR2Completion); err != nil {
				return CloudJobCompletionResult{}, err
			}
			if _, err := database.Exec(
				`UPDATE job_attempts
				 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
				     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
				     cloud_outcome = ?
				 WHERE id = (SELECT id FROM authoritative_job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
				targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome, jobID,
			); err != nil {
				return CloudJobCompletionResult{}, err
			}
		}
	}

	if runID > 0 && cloudInstanceID.Valid {
		// From-status guard is the WHERE clause: end_time IS NULL plus
		// status IN (queued, starting, running) — a bulk terminal close of
		// later open attempts on the same launch, never a rewrite of an
		// already-terminal row.
		if _, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, exit_code = ?, start_time = COALESCE(start_time, ?),
			     end_time = ?, last_synced_status = ?,
			     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
			     cloud_outcome = ?
			 WHERE job_id = ?
			   AND launch_id = ?
			   AND attempt_number > (
			       SELECT attempt_number FROM job_attempts WHERE id = ? AND job_id = ?
			   )
			   AND end_time IS NULL
			   AND abandoned_at IS NULL
			   AND status IN (?, ?, ?)`,
			targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, outcome,
			jobID, cloudInstanceID.Int64, runID, jobID,
			StatusQueued, StatusStarting, StatusRunning,
		); err != nil {
			return CloudJobCompletionResult{}, fmt.Errorf("finalize later same-launch attempts for job %d: %w", jobID, err)
		}
	}
	if runID > 0 && exitCode == 0 {
		// Same from-status guard shape as the finalize write above: only
		// later open attempts in an execution status are superseded.
		if _, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, exit_code = ?, start_time = COALESCE(start_time, ?),
			     end_time = ?, last_synced_status = ?,
			     failure_reason = COALESCE(NULLIF(?, ''), failure_reason),
			     cloud_outcome = ?
			 WHERE job_id = ?
			   AND launch_id IS NOT NULL
			   AND id != ?
			   AND attempt_number > (
			       SELECT attempt_number FROM job_attempts WHERE id = ? AND job_id = ?
			   )
			   AND end_time IS NULL
			   AND abandoned_at IS NULL
			   AND status IN (?, ?, ?)`,
			targetStatus, exitCode, startTimeArg, endTimeArg, lastSyncedStatusArg, failureReason, AttemptOutcomeSuperseded,
			jobID, runID, runID, jobID,
			StatusQueued, StatusStarting, StatusRunning,
		); err != nil {
			return CloudJobCompletionResult{}, fmt.Errorf("supersede later attempts after completed run for job %d: %w", jobID, err)
		}
	}

	// If the latest attempt has no launch_id (e.g., a blank replacement from
	// cleanupStaleAttempts), infer it by finding a sibling launch that ran
	// other jobs from the same original launch.
	if !cloudInstanceID.Valid {
		inferredID, err := inferSiblingLaunch(database, jobID)
		if err == nil && inferredID > 0 {
			host := LaunchHost(inferredID)
			if runID > 0 {
				if _, err := database.Exec(`
					UPDATE job_attempts
					SET launch_id = ?,
					    host = CASE WHEN (host = '' OR host IS NULL) THEN ? ELSE host END
					WHERE id = ? AND job_id = ? AND launch_id IS NULL AND abandoned_at IS NULL`,
					inferredID, host, runID, jobID,
				); err != nil {
					return CloudJobCompletionResult{}, fmt.Errorf("set inferred launch_id for job %d: %w", jobID, err)
				}
			} else {
				if _, err := database.Exec(`
					UPDATE job_attempts
					SET launch_id = ?,
					    host = CASE WHEN (host = '' OR host IS NULL) THEN ? ELSE host END
					WHERE id = `+latestAuthoritativeAttemptSubquery+`
					  AND launch_id IS NULL
					  AND abandoned_at IS NULL`,
					inferredID, host, jobID,
				); err != nil {
					return CloudJobCompletionResult{}, fmt.Errorf("set inferred launch_id for job %d: %w", jobID, err)
				}
			}
			return CloudJobCompletionResult{LaunchID: inferredID, Transitioned: transitioned}, nil
		}
	}

	if cloudInstanceID.Valid {
		return CloudJobCompletionResult{LaunchID: cloudInstanceID.Int64, Transitioned: transitioned}, nil
	}
	return CloudJobCompletionResult{Transitioned: transitioned}, nil
}

// NeedsCloudCompletionBackfill reports whether the latest attempt for jobID is
// terminal but still missing authoritative cloud completion fields.
//
// This is used to recover from reconcile races where attempts were force-closed
// (for example at grace expiry) before R2 completion metadata was ingested.
func NeedsCloudCompletionBackfill(database *sql.DB, jobID int64) (bool, error) {
	var (
		status           sql.NullString
		exitCode         sql.NullInt64
		startTime        sql.NullInt64
		endTime          sql.NullInt64
		lastSyncedStatus sql.NullString
		launchID         sql.NullInt64
	)
	// Reads the physical latest attempt (backfill recovery inspects whichever
	// attempt sync last force-closed, including a hidden move-target one).
	err := database.QueryRow(
		`SELECT status, exit_code, start_time, end_time, last_synced_status, launch_id
		 FROM job_attempts
		 WHERE id = `+latestAttemptSubquery,
		jobID,
	).Scan(&status, &exitCode, &startTime, &endTime, &lastSyncedStatus, &launchID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !status.Valid || !IsTerminalStatus(status.String) {
		return false, nil
	}
	// Non-cloud attempts are out of scope for cloud completion backfill.
	if !launchID.Valid {
		return false, nil
	}

	switch status.String {
	case StatusCompleted, StatusFailed, StatusDead, StatusKilled:
		if !endTime.Valid || !startTime.Valid || !exitCode.Valid {
			return true, nil
		}
		// Treat 0 as "unknown" symmetrically with NULL: an authoritative
		// completion JSON always carries non-zero start_time, so a stored 0
		// indicates the marker-only fallback path and remains eligible for
		// backfill.
		if startTime.Int64 == 0 || endTime.Int64 == 0 {
			return true, nil
		}
		// last_synced_status should reflect the terminal state once cloud
		// completion data was applied.
		if !lastSyncedStatus.Valid || lastSyncedStatus.String != status.String {
			return true, nil
		}
	}

	return false, nil
}

// PoisonedCloudAttempt identifies a terminal cloud attempt whose start_time
// is 0 — the signature of an earlier marker-only sync that fabricated end_time
// = wall-clock time instead of using the agent's authoritative completion JSON.
type PoisonedCloudAttempt struct {
	JobID     int64
	AttemptID int64
}

// FindPoisonedCloudCompletions returns the latest attempt for every terminal
// cloud job whose start_time is 0 and end_time is set. These rows need
// last_synced_status cleared so a later sync can rewrite the timestamps from
// R2 (NeedsCloudCompletionBackfill already treats start_time=0 as needing
// backfill, but the cleared last_synced_status documents the intent).
func FindPoisonedCloudCompletions(database *sql.DB) ([]PoisonedCloudAttempt, error) {
	rows, err := database.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		WHERE ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)
		  AND ja.launch_id IS NOT NULL
		  AND ja.status IN (?, ?, ?, ?)
		  AND ja.start_time = 0
		  AND ja.end_time IS NOT NULL`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
	if err != nil {
		return nil, fmt.Errorf("query poisoned cloud completions: %w", err)
	}
	defer rows.Close()

	var out []PoisonedCloudAttempt
	for rows.Next() {
		var p PoisonedCloudAttempt
		if err := rows.Scan(&p.JobID, &p.AttemptID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RearmPoisonedCloudCompletions clears last_synced_status on every row matched
// by FindPoisonedCloudCompletions in a single statement. Returns rows affected.
func RearmPoisonedCloudCompletions(database *sql.DB) (int64, error) {
	res, err := database.Exec(`
		UPDATE job_attempts
		SET last_synced_status = NULL
		WHERE id IN (
			SELECT ja.id FROM job_attempts ja
			WHERE ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)
			  AND ja.launch_id IS NOT NULL
			  AND ja.status IN (?, ?, ?, ?)
			  AND ja.start_time = 0
			  AND ja.end_time IS NOT NULL
		)`,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
	if err != nil {
		return 0, fmt.Errorf("rearm poisoned cloud completions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// StuckJob represents an active job attempt on a completed launch.
type StuckJob struct {
	JobID     int64
	AttemptID int64
}

// FindStuckJobsOnCompletedLaunches returns jobs with active execution status
// whose launch has already completed. These jobs missed R2 result sync.
func FindStuckJobsOnCompletedLaunches(database *sql.DB) ([]StuckJob, error) {
	rows, err := database.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		JOIN launches l ON ja.launch_id = l.id
		WHERE l.status = ?
		AND ja.status IN (?, ?, ?)
		AND ja.id = (SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id)`,
		LaunchStatusCompleted,
		StatusStarting, StatusRunning, StatusPaused,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stuck []StuckJob
	for rows.Next() {
		var s StuckJob
		if err := rows.Scan(&s.JobID, &s.AttemptID); err != nil {
			return nil, err
		}
		stuck = append(stuck, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stuck, nil
}

// MarkStuckJobDead marks a stuck job attempt as dead. Called when R2 sync
// has been attempted and failed — no completion data is recoverable.
// The status guard mirrors FindStuckJobsOnCompletedLaunches: dead may only
// land on an attempt still in an execution status, so a completion that
// arrived between find and mark is never stomped (completed -> dead has no
// transition edge).
func MarkStuckJobDead(database *sql.DB, attemptID int64) error {
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, end_time = COALESCE(end_time, ?),
		     failure_reason = 'launch completed but job results were not synced from R2',
		     cloud_outcome = ?
		 WHERE id = ?
		   AND status IN (?, ?, ?)`,
		StatusDead, time.Now().Unix(), AttemptOutcomeOrphaned, attemptID,
		StatusStarting, StatusRunning, StatusPaused,
	)
	return err
}

// FinalizeStuckJobsOnCompletedLaunches finds jobs with non-terminal status
// whose launch has already completed and marks them as "dead". This is the
// DB-only fallback — callers with R2 access should use
// campaign.FinalizeStuckJobsWithR2Check instead.
func FinalizeStuckJobsOnCompletedLaunches(database *sql.DB) ([]int64, error) {
	stuck, err := FindStuckJobsOnCompletedLaunches(database)
	if err != nil {
		return nil, err
	}

	var finalized []int64
	for _, s := range stuck {
		if err := MarkStuckJobDead(database, s.AttemptID); err != nil {
			return finalized, err
		}
		finalized = append(finalized, s.JobID)
	}
	return finalized, nil
}
