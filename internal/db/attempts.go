package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataplane"
)

const stalePendingPlacementNoLaunchMaxAgeSeconds = int64(120)

func sqlNormalizeGPUClassExpr(expr string) string {
	cleaned := fmt.Sprintf("lower(trim(coalesce(%s, '')))", expr)
	for _, token := range []string{
		"nvidia", "geforce", "tesla", "amd", "radeon", "instinct",
		" ", "-", "_", ".", "/", "(", ")", "[", "]",
	} {
		cleaned = fmt.Sprintf("replace(%s, '%s', '')", cleaned, token)
	}
	return fmt.Sprintf(`
		CASE
			WHEN NULLIF(%[1]s, '') IS NULL THEN NULL
			WHEN %[1]s LIKE '%%b200%%' THEN 'b200'
			WHEN %[1]s LIKE '%%h200%%' THEN 'h200'
			WHEN %[1]s LIKE '%%h100%%' THEN 'h100'
			WHEN %[1]s LIKE '%%a100%%' THEN 'a100'
			WHEN %[1]s LIKE '%%l40s%%' THEN 'l40s'
			WHEN %[1]s LIKE '%%l40%%' THEN 'l40'
			WHEN %[1]s LIKE '%%a40%%' THEN 'a40'
			WHEN %[1]s LIKE '%%a10g%%' THEN 'a10g'
			WHEN %[1]s LIKE '%%a10%%' THEN 'a10'
			WHEN %[1]s LIKE '%%m2max%%' THEN 'm2max'
			WHEN instr(%[1]s, 'rtx') > 0 THEN substr(%[1]s, instr(%[1]s, 'rtx'))
			WHEN %[1]s GLOB '[0-9][0-9][0-9][0-9]' OR %[1]s GLOB '[0-9][0-9][0-9][0-9]ti' THEN 'rtx' || %[1]s
			ELSE %[1]s
		END`, cleaned)
}

func sqlParseMemoryMiBExpr(expr string) string {
	cleaned := fmt.Sprintf("lower(replace(trim(coalesce(%s, '')), ' ', ''))", expr)
	return fmt.Sprintf(`
		CASE
			WHEN NULLIF(%[1]s, '') IS NULL THEN NULL
			WHEN %[1]s GLOB '*gib' THEN CAST(replace(%[1]s, 'gib', '') AS INTEGER) * 1024
			WHEN %[1]s GLOB '*gb' THEN CAST(replace(%[1]s, 'gb', '') AS INTEGER) * 1024
			WHEN %[1]s GLOB '*mib' THEN CAST(replace(%[1]s, 'mib', '') AS INTEGER)
			WHEN %[1]s GLOB '*mb' THEN CAST(replace(%[1]s, 'mb', '') AS INTEGER)
			ELSE CAST(%[1]s AS INTEGER)
		END`, cleaned)
}

// dbExecer is the common interface for *sql.DB and *sql.Tx.
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// closeAttemptsAndRequeue closes open attempts for a cloud job and sets
// requested_status='queued' so the view derives "queued" without a hostless
// attempt. Used by ResetLaunchJobs, RequeueByID, ResetJobToUnplaced, and
// cleanupStaleAttempts.
//
// Preserves requested_status='canceled': a user cancel is a durable intent
// to retract the job (see CancelSurvivesInstanceTermination in
// specs/job-lifecycle.allium). Overwriting it to 'queued' would let the
// dispatcher re-place the job on a fresh rental after its original instance
// terminates.
func closeAttemptsAndRequeue(db dbExecer, jobID int64, now int64) error {
	return closeAttemptsAndRequeueWithOutcome(db, jobID, now, "")
}

// closeAttemptsAndRequeueWithOutcome closes the open attempts for a job,
// requeues the spec, and (when cloudOutcome != "") records cloud_outcome on
// the just-closed attempts. Status and cloud_outcome are written in one
// UPDATE so a follow-up predicated on end_time can't race with an earlier
// close.
//
// Any open move intent is canceled alongside the closure: the requeue closes
// every attempt — including a hidden move-target attempt on another launch —
// so the intent no longer describes an executable move, and leaving it open
// would hide the requeued job from the autopilot forever. See rule
// RequeueAbandonsOpenMoveIntent in specs/job-move.allium.
func closeAttemptsAndRequeueWithOutcome(db dbExecer, jobID int64, now int64, cloudOutcome string) error {
	resolution := "job requeued; move abandoned"
	if cloudOutcome != "" {
		resolution = fmt.Sprintf("job requeued (%s); move abandoned", cloudOutcome)
	}
	if err := resolveOpenMoveIntentAbandonedTx(db, jobID, now, resolution); err != nil {
		return err
	}
	// No from-status guard by design: this is the requeue primitive shared by
	// ResetLaunchJobs, RequeueByID, ResetJobToUnplaced, and
	// cleanupStaleAttempts — its contract is "cancel whatever is open, then
	// requeue", so the end_time (open-attempt) constraint is the guard.
	if _, err := db.Exec(
		`UPDATE job_attempts
		    SET status = ?,
		        end_time = COALESCE(NULLIF(end_time, 0), ?),
		        cloud_outcome = COALESCE(cloud_outcome, NULLIF(?, ''))
		  WHERE job_id = ? AND (end_time IS NULL OR end_time = 0)`,
		StatusCanceled, now, cloudOutcome, jobID); err != nil {
		return err
	}
	// Convergence: when the requeue outcome detaches the job from its launch
	// ('orphaned'/'canceled'), also restamp the latest authoritative attempt
	// if it is already closed without a decisive record. The job_status view
	// derives 'orphaned' from such attempts whenever their launch is
	// failed/canceled, so a repair that only touches open attempts leaves the
	// job permanently non-terminal and re-selected by every subsequent repair
	// pass (jobs wj889/wj890 sat in that loop for weeks).
	if cloudOutcome == AttemptOutcomeOrphaned || cloudOutcome == AttemptOutcomeCancelled {
		if _, err := db.Exec(
			`UPDATE job_attempts
			    SET cloud_outcome = ?
			  WHERE id = `+latestAuthoritativeAttemptSubquery+`
			    AND `+attemptClosedWithoutDecisiveRecord,
			cloudOutcome, jobID,
			StatusCompleted, StatusFailed,
			AttemptOutcomeOrphaned, AttemptOutcomeCancelled); err != nil {
			return err
		}
	}
	_, err := db.Exec(
		`UPDATE jobs SET requested_status = ? WHERE id = ? AND COALESCE(requested_status, '') != ?`,
		StatusQueued, jobID, StatusCanceled)
	return err
}

// cleanupStaleAttempts fixes data corruption from earlier bugs:
//  1. Jobs with multiple open attempts — keeps only the latest, closes the rest.
//  2. Jobs with cloud_instance_id on their latest attempt pointing to a terminated
//     instance — closes that attempt and creates a fresh unplaced one.
func cleanupStaleAttempts(db *sql.DB) error {
	now := time.Now().Unix()

	// Step 1: Close duplicate open attempts. For each job with multiple open
	// attempts, keep only the one with the highest attempt_number. Cloud
	// attempts (launch_id IS NOT NULL) are stamped cloud_outcome='superseded'
	// so the view's cloud-job branch can recognise them as terminated by an
	// internal sweep, not by a user cancel — preventing the canceled-with-no-
	// cloud_outcome wedge that previously left jobs stuck as 'dead'.
	if _, err := db.Exec(`
		UPDATE job_attempts
		   SET end_time = ?,
		       status = 'canceled',
		       cloud_outcome = CASE
		           WHEN launch_id IS NOT NULL AND cloud_outcome IS NULL THEN ?
		           ELSE cloud_outcome
		       END
		WHERE end_time IS NULL
		  AND job_id NOT IN (
			SELECT job_id FROM move_intents WHERE state = 'open'
		  )
		  AND id NOT IN (
			SELECT MAX(id) FROM job_attempts
			WHERE end_time IS NULL
			GROUP BY job_id
		  )`, now, AttemptOutcomeSuperseded); err != nil {
		return fmt.Errorf("close duplicate open attempts: %w", err)
	}

	// Step 2: For jobs whose latest open attempt references a failed or
	// canceled cloud instance, close the attempt and create a fresh unplaced
	// one. Completed instances are intentionally skipped — their jobs should
	// be finalized by the R2 result sync, not reset to queued.
	rows, err := db.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		JOIN launches ci ON ci.id = ja.launch_id
		WHERE ja.end_time IS NULL
		  AND ci.status IN (?, ?)
		  AND NOT EXISTS (
			SELECT 1 FROM job_attempts ja2
			WHERE ja2.job_id = ja.job_id
			  AND ja2.id != ja.id
			  AND ja2.status IN (?, ?, ?, ?)
		  )`,
		LaunchStatusFailed, LaunchStatusCancelled,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
	if err != nil {
		return fmt.Errorf("find orphaned cloud attempts: %w", err)
	}
	var orphaned []struct{ jobID, attemptID int64 }
	for rows.Next() {
		var jobID, attemptID int64
		if err := rows.Scan(&jobID, &attemptID); err != nil {
			rows.Close()
			return err
		}
		orphaned = append(orphaned, struct{ jobID, attemptID int64 }{jobID, attemptID})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, o := range orphaned {
		if err := closeAttemptsAndRequeue(db, o.jobID, now); err != nil {
			return fmt.Errorf("requeue orphaned job %d: %w", o.jobID, err)
		}
	}

	return nil
}

// repairOrphanedCompletedAttempts fixes completed attempts that lost their
// launch association. This happens when cleanupStaleAttempts creates a blank
// replacement attempt (no host, no launch_id) and R2 sync later completes it
// without propagating the launch_id.
func repairOrphanedCompletedAttempts(database *sql.DB) error {
	rows, err := database.Query(`
		SELECT job_id, id FROM job_attempts
		WHERE status IN (?, ?)
		  AND (host = '' OR host IS NULL)
		  AND launch_id IS NULL
		  AND abandoned_at IS NULL
		  AND id = (SELECT MAX(ja3.id) FROM job_attempts ja3 WHERE ja3.job_id = job_attempts.job_id)`,
		StatusCompleted, StatusFailed,
	)
	if err != nil {
		return fmt.Errorf("find orphaned attempts: %w", err)
	}
	type orphan struct{ jobID, attemptID int64 }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.jobID, &o.attemptID); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()

	for _, o := range orphans {
		launchID, err := inferSiblingLaunch(database, o.jobID)
		if err != nil || launchID == 0 {
			continue
		}
		host := LaunchHost(launchID)
		database.Exec(
			`UPDATE job_attempts
			 SET launch_id = ?, host = ?
			 WHERE id = ? AND launch_id IS NULL AND abandoned_at IS NULL`,
			launchID, host, o.attemptID,
		)
	}

	return nil
}

// inferSiblingLaunch finds the replacement launch for an orphaned job by
// looking at the job's prior attempt that had a launch_id, then finding a
// sibling launch (same GPU class, completed status) that ran other jobs from
// that same original failed launch. Returns 0 if no match found.
func inferSiblingLaunch(db *sql.DB, jobID int64) (int64, error) {
	var launchID sql.NullInt64
	err := db.QueryRow(inferSiblingLaunchSQL, jobID).Scan(&launchID)
	if err == sql.ErrNoRows || !launchID.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return launchID.Int64, nil
}

// inferSiblingLaunchSQL is used both as a standalone query (via inferSiblingLaunch)
// and as a correlated subquery (in repairOrphanedCompletedAttempts, where ? is
// bound to job_attempts.job_id from the outer UPDATE).
const inferSiblingLaunchSQL = `
	SELECT DISTINCT ja_sibling.launch_id
	FROM job_attempts ja_prior
	JOIN job_attempts ja_sibling ON ja_sibling.launch_id != ja_prior.launch_id
	  AND ja_sibling.status IN ('completed', 'dead')
	  AND ja_sibling.job_id IN (
		SELECT ja_peer.job_id FROM job_attempts ja_peer
		WHERE ja_peer.launch_id = ja_prior.launch_id
	  )
	JOIN launches l_sibling ON l_sibling.id = ja_sibling.launch_id
	  AND l_sibling.status = 'completed'
	JOIN launches l_prior ON l_prior.id = ja_prior.launch_id
	  AND UPPER(l_sibling.gpu_class) = UPPER(l_prior.gpu_class)
	WHERE ja_prior.job_id = ?
	  AND ja_prior.launch_id IS NOT NULL
	  AND ja_prior.abandoned_at IS NULL
	ORDER BY ja_sibling.launch_id DESC LIMIT 1`

func hasJobPhaseTimingsTable(db *sql.DB) bool {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'job_phase_timings'`).Scan(&name)
	return err == nil
}

// --- Attempt helper functions ---
//
// SQLite doesn't support UPDATE ... ORDER BY ... LIMIT without SQLITE_ENABLE_UPDATE_DELETE_LIMIT.
// Selecting "the attempt to write" is a per-call-site decision with exactly
// three sanctioned answers, in order of increasing visibility. Every
// attempt-mutating helper WHERE id = (subquery ... LIMIT 1) must use one of
// these named constants rather than inlining its own selector — the
// TestNoInlineAttemptSelector guard enforces this so a future edit cannot
// quietly add a fourth, undocumented rule. The choice:
//
//   - latestOpenAttemptSubquery: the currently-open, structurally-tracked
//     attempt. The default for mutations that only make sense on a live
//     attempt (status/session/cost/telemetry). Hides open-move-target
//     attempts until their MoveIntent confirms.
//   - latestAuthoritativeAttemptSubquery: the latest non-abandoned attempt,
//     open or terminal. For writes that own the job's outcome and must reach
//     a closed attempt without ever touching an abandoned move-loser.
//   - latestAttemptSubquery: the physical latest, including abandoned and
//     hidden (open-move-target) attempts. Prefer the open or authoritative
//     selectors; reach for this only where a call site must not apply their
//     filters, and say why at the call site.

// latestOpenAttemptSubquery returns a SQL subquery that selects the
// authoritative structurally tracked open attempt for the given job_id
// placeholder. Open move target attempts are deliberately hidden until their
// MoveIntent is confirmed.
const latestOpenAttemptSubquery = `(SELECT joa.attempt_id
FROM job_open_attempts joa
LEFT JOIN job_attempts ja ON ja.id = joa.attempt_id
LEFT JOIN move_intents mi ON mi.id = ja.move_intent_id AND mi.state = 'open'
WHERE joa.job_id = ? AND mi.id IS NULL
ORDER BY joa.attempt_id DESC
LIMIT 1)`

// latestAttemptSubquery returns a SQL subquery for the physical latest attempt,
// including abandoned and hidden (open-move-target) attempts. Prefer
// latestOpenAttemptSubquery or latestAuthoritativeAttemptSubquery; use this
// only when a write/read must reach an attempt those two exclude.
const latestAttemptSubquery = `(SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`

// latestAuthoritativeAttemptSubquery returns the latest attempt that still
// owns the logical job. Abandoned attempts are retained for forensics but do
// not drive job_status, info, artifacts, or telemetry by default.
const latestAuthoritativeAttemptSubquery = `(SELECT id FROM authoritative_job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`

// latestAuthoritativeAttemptCorrelatedSubquery is the correlated form of
// latestAuthoritativeAttemptSubquery for set-based UPDATEs on job_attempts,
// where the job id comes from the row under update rather than a bound
// parameter.
const latestAuthoritativeAttemptCorrelatedSubquery = `(SELECT aja.id FROM authoritative_job_attempts aja
WHERE aja.job_id = job_attempts.job_id
ORDER BY aja.attempt_number DESC
LIMIT 1)`

// attemptClosedWithoutDecisiveRecord matches attempts that are closed
// (end_time set) yet record no decision: no exit code, non-terminal status,
// and no detaching cloud_outcome. The job_status view derives 'orphaned' from
// such attempts while their launch is failed/canceled, so convergence repairs
// stamp a disposition onto exactly this set. Bind args: StatusCompleted,
// StatusFailed, AttemptOutcomeOrphaned, AttemptOutcomeCancelled.
const attemptClosedWithoutDecisiveRecord = `(end_time IS NOT NULL AND end_time != 0
AND exit_code IS NULL
AND status NOT IN (?, ?)
AND COALESCE(cloud_outcome, '') NOT IN (?, ?))`

const (
	AttemptAbandonedMoveSourceWon               = "move_source_won"
	AttemptAbandonedMoveTargetWon               = "move_target_won"
	AttemptAbandonedMoveTargetAccepted          = "move_target_accepted"
	AttemptAbandonedMoveDestinationRejected     = "move_destination_rejected"
	AttemptAbandonedMoveDestinationInfraFailed  = "move_destination_infra_failed"
	AttemptAbandonedDuplicateSuperseded         = "duplicate_superseded"
	AttemptAbandonedDuplicateSameLaunchTerminal = "duplicate_same_launch_terminal"
)

// closeOpenAttempts closes all open attempts for a job.
func closeOpenAttempts(execer dbExecer, jobID int64, now int64) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET end_time = COALESCE(end_time, ?), pending_status = NULL
		WHERE job_id = ? AND end_time IS NULL`,
		now, jobID,
	)
	return err
}

// AbandonAttempt removes an attempt from logical job authority while retaining
// its row for audit and explicit attempt-level queries.
func AbandonAttempt(execer dbExecer, attemptID int64, reason string, intentID *int64) error {
	if attemptID <= 0 {
		return fmt.Errorf("invalid attempt id")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("abandon attempt reason is required")
	}
	now := time.Now().Unix()
	_, err := execer.Exec(
		`UPDATE job_attempts
		    SET abandoned_at = COALESCE(abandoned_at, ?),
		        abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), ?),
		        abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, ?)
		  WHERE id = ?`,
		now, reason, nullableInt64(intentID), attemptID,
	)
	return err
}

func AbandonMoveLoser(database *sql.DB, intentID int64, attemptID int64, reason string) error {
	if intentID <= 0 {
		return fmt.Errorf("invalid move intent id")
	}
	return AbandonAttempt(database, attemptID, reason, &intentID)
}

// CreateAttempt inserts a new attempt for a job and returns its ID.
func CreateAttempt(db *sql.DB, jobID int64, host string, cloudInstanceID *int64, status string) (int64, error) {
	return createAttemptTx(db, jobID, host, cloudInstanceID, status)
}

func createAttemptTx(execer dbExecer, jobID int64, host string, cloudInstanceID *int64, status string) (int64, error) {
	now := time.Now().Unix()
	if err := closeOpenAttempts(execer, jobID, now); err != nil {
		return 0, err
	}
	var endTime any
	if IsTerminalStatus(status) {
		endTime = now
	}
	targetID, err := ensureExecutionTargetForAttempt(execer, host, cloudInstanceID)
	if err != nil {
		return 0, fmt.Errorf("ensure execution target for job %d: %w", jobID, err)
	}
	result, err := execer.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, target_id, status, queued_at, end_time)
		VALUES (?,
			COALESCE((SELECT MAX(attempt_number) FROM job_attempts WHERE job_id = ?), 0) + 1,
			?, ?, ?, ?, ?, ?)`,
		jobID, jobID, host, cloudInstanceID, targetID, status, now, endTime,
	)
	if err != nil {
		return 0, fmt.Errorf("create attempt for job %d: %w", jobID, err)
	}
	attemptID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := carryForwardPersistentMetadata(execer, jobID, attemptID); err != nil {
		return 0, fmt.Errorf("carry forward persistent metadata for job %d: %w", jobID, err)
	}
	return attemptID, nil
}

// CreateMoveTargetAttempt opens a destination attempt for an open MoveIntent
// without closing the source attempt. The attempt is hidden from job_status
// while the intent remains open; confirming the intent makes it authoritative.
func CreateMoveTargetAttempt(database *sql.DB, intentID, jobID int64, host string, cloudInstanceID *int64, status string) (int64, error) {
	if intentID <= 0 {
		return 0, fmt.Errorf("invalid move intent id")
	}
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRow(`SELECT state FROM move_intents WHERE id = ? AND job_id = ?`, intentID, jobID).Scan(&state); err != nil {
		return 0, fmt.Errorf("find move intent: %w", err)
	}
	if MoveIntentState(state) != MoveIntentStateOpen {
		return 0, fmt.Errorf("move intent %d is %s, want open", intentID, state)
	}
	// A move-target attempt binds the job to its destination without passing
	// setJobLaunchIDOnce, so it carries the same pin backstop; see
	// assertMachineAffinitySatisfied. Any non-launch destination — an on-prem
	// host, or no identity at all — can never satisfy a pin, so it fails
	// closed for pinned jobs.
	if cloudInstanceID != nil {
		if err := assertMachineAffinitySatisfied(tx, jobID, *cloudInstanceID); err != nil {
			return 0, err
		}
	} else if err := assertNoMachineAffinityForHost(tx, jobID, host); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	var endTime any
	if IsTerminalStatus(status) {
		endTime = now
	}
	targetID, err := ensureExecutionTargetForAttempt(tx, host, cloudInstanceID)
	if err != nil {
		return 0, fmt.Errorf("ensure execution target for job %d: %w", jobID, err)
	}
	result, err := tx.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, target_id, status, queued_at, end_time, move_intent_id)
		VALUES (?,
			COALESCE((SELECT MAX(attempt_number) FROM job_attempts WHERE job_id = ?), 0) + 1,
			?, ?, ?, ?, ?, ?, ?)`,
		jobID, jobID, host, cloudInstanceID, targetID, status, now, endTime, intentID,
	)
	if err != nil {
		return 0, fmt.Errorf("create move target attempt for job %d: %w", jobID, err)
	}
	attemptID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := carryForwardPersistentMetadata(tx, jobID, attemptID); err != nil {
		return 0, fmt.Errorf("carry forward persistent metadata for job %d: %w", jobID, err)
	}
	if _, err := tx.Exec(
		`UPDATE move_intents
		    SET target_attempt_id = ?,
		        target_launch_id = COALESCE(target_launch_id, ?),
		        target_host = COALESCE(target_host, NULLIF(?, ''))
		  WHERE id = ? AND state = 'open'`,
		attemptID, nullableInt64(cloudInstanceID), host, intentID,
	); err != nil {
		return 0, fmt.Errorf("record move target attempt: %w", err)
	}
	if _, err := tx.Exec(`UPDATE jobs SET placement_reasons = NULL WHERE id = ?`, jobID); err != nil {
		return 0, fmt.Errorf("clear placement reasons for move target: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// ConfirmMoveTargetAccepted makes the target attempt authoritative after the
// destination has accepted the job and abandons the source attempt so late
// source updates cannot retake the logical job.
func ConfirmMoveTargetAccepted(database *sql.DB, intentID int64, resolution string) error {
	if intentID <= 0 {
		return fmt.Errorf("invalid move intent id")
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = "target accepted job"
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sourceAttempt sql.NullInt64
	var targetAttempt sql.NullInt64
	var state string
	if err := tx.QueryRow(
		`SELECT source_attempt_id, target_attempt_id, state FROM move_intents WHERE id = ?`,
		intentID,
	).Scan(&sourceAttempt, &targetAttempt, &state); err != nil {
		return fmt.Errorf("find move intent: %w", err)
	}
	if MoveIntentState(state) != MoveIntentStateOpen {
		return nil
	}
	if !targetAttempt.Valid {
		return fmt.Errorf("move intent %d has no target attempt", intentID)
	}
	now := time.Now().Unix()
	if sourceAttempt.Valid {
		if _, err := tx.Exec(
			`UPDATE job_attempts
			    SET abandoned_at = COALESCE(abandoned_at, ?),
			        abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), ?),
			        abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, ?)
			  WHERE id = ?`,
			now, AttemptAbandonedMoveTargetAccepted, intentID, sourceAttempt.Int64,
		); err != nil {
			return fmt.Errorf("abandon source attempt: %w", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE move_intents
		    SET state = ?,
		        resolved_at = ?,
		        resolution = ?
		  WHERE id = ? AND state = ?`,
		string(MoveIntentStateConfirmed), now, resolution, intentID, string(MoveIntentStateOpen),
	); err != nil {
		return fmt.Errorf("confirm move intent: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE jobs
		    SET placement_reasons = NULL
		  WHERE id = (SELECT job_id FROM move_intents WHERE id = ?)`,
		intentID,
	); err != nil {
		return fmt.Errorf("clear placement reasons for accepted move target: %w", err)
	}
	return tx.Commit()
}

type placementDisplayAttempt struct {
	id            int64
	jobID         int64
	launchID      sql.NullInt64
	status        string
	queuedAt      sql.NullInt64
	startTime     sql.NullInt64
	endTime       sql.NullInt64
	cloudOutcome  sql.NullString
	predecessorID sql.NullInt64
}

// PlacementDisplayQueuedAt returns display-only placement timestamps for queued
// cloud jobs. If a failed no-start move briefly created an attempt on another
// launch and the job was restored to the same source launch, this preserves the
// original source queued_at so the UI does not show the restore as a fresh
// placement.
func PlacementDisplayQueuedAt(database *sql.DB, jobIDs []int64) (map[int64]int64, error) {
	if database == nil || len(jobIDs) == 0 {
		return map[int64]int64{}, nil
	}
	args := make([]any, 0, len(jobIDs))
	for _, id := range jobIDs {
		if id > 0 {
			args = append(args, id)
		}
	}
	if len(args) == 0 {
		return map[int64]int64{}, nil
	}
	query := `SELECT id, job_id, launch_id, status, queued_at, start_time, end_time, cloud_outcome, predecessor_attempt_id
		FROM job_attempts
		WHERE job_id IN (` + sqlPlaceholders(len(args)) + `)
		ORDER BY job_id ASC, attempt_number ASC`
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[int64]placementDisplayAttempt)
	latestByJob := make(map[int64]placementDisplayAttempt)
	for rows.Next() {
		var a placementDisplayAttempt
		if err := rows.Scan(&a.id, &a.jobID, &a.launchID, &a.status, &a.queuedAt, &a.startTime, &a.endTime, &a.cloudOutcome, &a.predecessorID); err != nil {
			return nil, err
		}
		byID[a.id] = a
		latestByJob[a.jobID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[int64]int64, len(latestByJob))
	for jobID, latest := range latestByJob {
		if !latest.launchID.Valid || !latest.queuedAt.Valid || latest.queuedAt.Int64 <= 0 {
			continue
		}
		displayAt := latest.queuedAt.Int64
		launchID := latest.launchID.Int64
		for cur := latest; cur.predecessorID.Valid; {
			pred, ok := byID[cur.predecessorID.Int64]
			if !ok {
				break
			}
			if pred.launchID.Valid && pred.launchID.Int64 == launchID {
				if pred.startTime.Valid && pred.startTime.Int64 > 0 {
					break
				}
				if pred.queuedAt.Valid && pred.queuedAt.Int64 > 0 {
					displayAt = pred.queuedAt.Int64
				}
				cur = pred
				continue
			}
			if !isNoStartCloudDetour(pred) {
				break
			}
			cur = pred
		}
		out[jobID] = displayAt
	}
	return out, nil
}

func isNoStartCloudDetour(a placementDisplayAttempt) bool {
	if !a.launchID.Valid || a.launchID.Int64 <= 0 {
		return false
	}
	if a.startTime.Valid && a.startTime.Int64 > 0 {
		return false
	}
	if !a.endTime.Valid || a.endTime.Int64 <= 0 {
		return false
	}
	switch a.cloudOutcome.String {
	case AttemptOutcomeOrphaned, AttemptOutcomeFailed, AttemptOutcomeCancelled:
		return true
	default:
		return a.status == StatusCanceled || a.status == StatusFailed || a.status == StatusDead
	}
}

// carryForwardPersistentMetadata copies persistent submission metadata to a
// new attempt. Source execution evidence is deliberately cleared because it
// belongs to the attempt that observed it.
func carryForwardPersistentMetadata(execer dbExecer, jobID, newAttemptID int64) error {
	var previousRaw, submissionRaw sql.NullString
	err := execer.QueryRow(
		`SELECT
			(SELECT job_metadata FROM job_attempts WHERE job_id = j.id AND id != ? ORDER BY attempt_number DESC LIMIT 1),
			j.job_metadata
		 FROM jobs j WHERE j.id = ?`,
		newAttemptID,
		jobID,
	).Scan(&previousRaw, &submissionRaw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	previous := decodeJobMetadata(previousRaw)
	submission := decodeJobMetadata(submissionRaw)
	meta := &JobMetadata{}
	if previous != nil && previous.Dependencies != nil {
		deps := &JobDependencyMetadata{}
		deps.CloudAfter = append([]JobDependencyRef(nil), previous.Dependencies.CloudAfter...)
		deps.CloudNeeds = append([]string(nil), previous.Dependencies.CloudNeeds...)
		if len(deps.CloudAfter) > 0 || len(deps.CloudNeeds) > 0 {
			meta.Dependencies = deps
		}
	}
	source := (*JobSourceMetadata)(nil)
	if previous != nil {
		source = previous.Source
	}
	if source == nil && submission != nil {
		source = submission.Source
	}
	meta.Source = clonePersistentJobSource(source)
	if meta.Dependencies == nil && meta.Source == nil {
		return nil
	}

	encoded, err := encodeJobMetadata(meta)
	if err != nil {
		return err
	}
	_, err = execer.Exec(`UPDATE job_attempts SET job_metadata = ? WHERE id = ?`, encoded, newAttemptID)
	return err
}

func clonePersistentJobSource(source *JobSourceMetadata) *JobSourceMetadata {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Execution = nil
	clone.Roots = append([]JobSourceRootMetadata(nil), source.Roots...)
	clone.Warnings = append([]string(nil), source.Warnings...)
	if source.Pin != nil {
		pin := *source.Pin
		pin.Roots = append([]JobSourcePinRootMetadata(nil), source.Pin.Roots...)
		for i := range pin.Roots {
			pin.Roots[i].Blobs = append([]dataplane.SourceBlob(nil), pin.Roots[i].Blobs...)
		}
		clone.Pin = &pin
	}
	return &clone
}

// CloseAttempt marks the current open attempt for a job as ended.
// Scoping to the open attempt is the guard; the caller owns the target
// status (racing close paths deliberately write terminal statuses).
func CloseAttempt(db *sql.DB, jobID int64, status string, exitCode *int, endTime int64) error {
	_, err := db.Exec(`
		UPDATE job_attempts
		SET status = ?, exit_code = ?, end_time = ?, pending_status = NULL, session_name = NULL
		WHERE id = `+latestOpenAttemptSubquery,
		status, exitCode, endTime, jobID,
	)
	return err
}

// GetLatestAttemptID returns the ID of the latest attempt for a job, or 0 if none.
func GetLatestAttemptID(db *sql.DB, jobID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`
		SELECT id FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// GetAuthoritativeAttemptID returns the latest non-abandoned attempt for a
// job, or 0 if none exists.
func GetAuthoritativeAttemptID(db *sql.DB, jobID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`
		SELECT id FROM authoritative_job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// UpdateAttemptRunning marks the latest open attempt as running with a start time.
// Open-attempt scoping is the guard; transition validation happens in the
// Mark* wrappers (see MarkQueuedJobRunning).
//
// On-prem callers invoke this optimistically before the remote start has
// actually succeeded, and the failure path relies on pending_status being
// preserved to drive retry — so this function deliberately does NOT touch
// pending_status. See MarkQueuedJobRunning for the cloud-observed path that
// clears satisfied intents.
func UpdateAttemptRunning(execer dbExecer, jobID int64) error {
	now := time.Now().Unix()
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?, start_time = COALESCE(start_time, ?)
		WHERE id = `+latestOpenAttemptSubquery,
		StatusRunning, StatusRunning, now, jobID,
	)
	return err
}

// UpdateAttemptStarting marks the latest open attempt as starting with a start
// time. This is the cloud-observed setup counterpart to UpdateAttemptRunning;
// like it, open-attempt scoping is the guard and MarkQueuedJobStarting owns
// the transition validation.
func UpdateAttemptStarting(execer dbExecer, jobID int64) error {
	now := time.Now().Unix()
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?, start_time = COALESCE(start_time, ?)
		WHERE id = `+latestOpenAttemptSubquery,
		StatusStarting, StatusStarting, now, jobID,
	)
	return err
}

// SetAttemptLaunch associates the latest open attempt for a job with a launch.
// This re-links an orphaned job (whose attempt was reset without a launch_id)
// back to the launch that is actually running it, as observed from R2 phase.
func SetAttemptLaunch(database *sql.DB, jobID int64, launchID int64) error {
	return AttachOpenAttemptToLaunch(database, jobID, launchID)
}

// NormalizePendingPlacementForLaunch converts stale pending_placement markers
// to queued for jobs scoped to a launch once that launch has progressed beyond
// initial placement and the latest attempt is not a completed non-orphaned run.
func NormalizePendingPlacementForLaunch(database *sql.DB, launchID int64) (int64, error) {
	return RetryOnDatabaseLockedValue(context.Background(), "normalize pending placement for launch", func() (int64, error) {
		result, err := database.Exec(`
			UPDATE job_attempts
			SET pending_status = ?, pending_at = COALESCE(pending_at, strftime('%s','now'))
			WHERE id IN (
				SELECT ja.id
				FROM job_attempts ja
				JOIN (
					SELECT job_id, MAX(attempt_number) AS max_attempt
					FROM job_attempts
					GROUP BY job_id
				) latest
				  ON latest.job_id = ja.job_id AND latest.max_attempt = ja.attempt_number
				JOIN launches l ON l.id = ja.launch_id
				WHERE ja.launch_id = ?
				  AND COALESCE(ja.pending_status, '') = ?
				  AND l.status IN (?, ?, ?)
				  AND NOT (
				    COALESCE(ja.status, '') = ?
				    AND COALESCE(ja.cloud_outcome, '') NOT IN (?, ?)
				  )
			)`,
			StatusQueued, launchID, StatusPendingPlacement,
			LaunchStatusRunning, LaunchStatusGrace, LaunchStatusCompleted,
			StatusCompleted, AttemptOutcomeOrphaned, AttemptOutcomeCancelled,
		)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
}

// NormalizeStalePendingPlacementNoLaunch converts stale pending_placement
// markers to queued when a job has no launch association. This protects the UI
// from showing long-lived "launching" jobs that are no longer being placed.
func NormalizeStalePendingPlacementNoLaunch(database *sql.DB) (int64, error) {
	result, err := database.Exec(`
		UPDATE job_attempts
		SET pending_status = ?, pending_at = COALESCE(pending_at, strftime('%s','now'))
		WHERE id IN (
			SELECT ja.id
			FROM job_attempts ja
			JOIN (
				SELECT job_id, MAX(attempt_number) AS max_attempt
				FROM job_attempts
				GROUP BY job_id
			) latest
			  ON latest.job_id = ja.job_id AND latest.max_attempt = ja.attempt_number
			WHERE COALESCE(ja.pending_status, '') = ?
			  AND COALESCE(ja.launch_id, 0) = 0
			  AND COALESCE(ja.host, '') = ''
			  AND COALESCE(ja.status, '') IN (?, ?)
			  AND (
			       ja.pending_at IS NULL
			       OR ja.pending_at <= strftime('%s','now') - ?
			  )
		)`,
		StatusQueued, StatusPendingPlacement, StatusQueued, StatusPendingPlacement, stalePendingPlacementNoLaunchMaxAgeSeconds,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UpdateAttemptCompletion marks the latest attempt as completed.
// Uses latestAttemptSubquery (not just open attempts) because completion is
// authoritative — it can override a previous "failed" status from a race
// condition (e.g., job was marked dead locally but status file shows completion).
// Guard is caller-side: RecordCompletionByIDWithTransition runs checkTransition
// before this write; the authoritative-override edges (failed/dead/killed/
// canceled -> completed) are in internal/status.
func UpdateAttemptCompletion(execer dbExecer, jobID int64, exitCode int, endTime int64) error {
	// Stamp cloud_outcome in the same UPDATE for cloud rows
	// (launch_id NOT NULL). Mirrors the derivation the
	// cloud_attempts_auto_derive_outcome_* trigger would do in 00006;
	// keeping the writer explicit here keeps the intent visible at the
	// call site and makes the trigger fire only for genuine leaks. The
	// CASE preserves any explicit cloud_outcome that was already set
	// (e.g., 'preempted', 'orphaned') by an earlier path.
	cloudOutcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		cloudOutcome = AttemptOutcomeFailed
	}
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, exit_code = ?, end_time = ?,
		    last_synced_status = ?, pending_status = NULL, session_name = NULL,
		    cloud_outcome = CASE
		        WHEN launch_id IS NULL THEN cloud_outcome
		        WHEN cloud_outcome IS NULL THEN ?
		        ELSE cloud_outcome
		    END
		WHERE id = `+latestAttemptSubquery,
		StatusCompleted, exitCode, endTime, StatusCompleted, cloudOutcome, jobID,
	)
	return err
}

// UpdateAttemptCompletionIfNonTerminal is the notification-safe counterpart to
// UpdateAttemptCompletion. It performs the terminal write only while the
// selected attempt is still non-terminal, so concurrent sync processes cannot
// both claim the same terminal transition. The WHERE status NOT IN (terminal)
// clause is the hard from-status guard.
func UpdateAttemptCompletionIfNonTerminal(execer dbExecer, jobID int64, exitCode int, endTime int64) (bool, error) {
	cloudOutcome := AttemptOutcomeCompleted
	if exitCode != 0 {
		cloudOutcome = AttemptOutcomeFailed
	}
	res, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, exit_code = ?, end_time = ?,
		    last_synced_status = ?, pending_status = NULL, session_name = NULL,
		    cloud_outcome = CASE
		        WHEN launch_id IS NULL THEN cloud_outcome
		        WHEN cloud_outcome IS NULL THEN ?
		        ELSE cloud_outcome
		    END
		WHERE id = `+latestAttemptSubquery+`
		  AND status NOT IN (?, ?, ?, ?, ?, ?)`,
		StatusCompleted, exitCode, endTime, StatusCompleted, cloudOutcome, jobID,
		StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, StatusDraft,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateAttemptDead marks the latest open attempt as failed (unexpected termination).
// Open-attempt scoping is the guard; failed is a table-valid target from every
// open execution status (queued/starting/running/paused).
func UpdateAttemptDead(execer dbExecer, jobID int64) error {
	endTime := time.Now().Unix()
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, end_time = ?,
		    last_synced_status = ?, pending_status = NULL, session_name = NULL,
		    cloud_outcome = CASE
		        WHEN launch_id IS NULL THEN cloud_outcome
		        WHEN cloud_outcome IS NULL THEN ?
		        ELSE cloud_outcome
		    END
		WHERE id = `+latestOpenAttemptSubquery,
		StatusFailed, endTime, StatusFailed, AttemptOutcomeFailed, jobID,
	)
	return err
}

// SetAttemptPendingStatus sets pending_status on the latest attempt (open or closed).
// Pending status can be set on a terminal attempt (e.g., retry intent on a failed job).
func SetAttemptPendingStatus(db *sql.DB, jobID int64, status string) error {
	return RetryOnDatabaseLocked(context.Background(), "set attempt pending status", func() error {
		now := time.Now().Unix()
		_, err := db.Exec(`
			UPDATE job_attempts SET pending_status = ?, pending_at = ?
			WHERE id = `+latestAttemptSubquery,
			status, now, jobID,
		)
		return err
	})
}

// ClearAttemptPendingStatus clears pending_status on the latest attempt.
// Unfiltered because it pairs with SetAttemptPendingStatus, which can stamp a
// terminal attempt (retry intent on a failed job).
func ClearAttemptPendingStatus(db *sql.DB, jobID int64) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET pending_status = NULL, pending_at = NULL
		WHERE id = `+latestAttemptSubquery,
		jobID,
	)
	return err
}

// SetAttemptVastaiInstance sets the backend to 'vastai' on the latest open attempt.
func SetAttemptVastaiInstance(db *sql.DB, jobID int64, _ int) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET backend = ?
		WHERE id = `+latestOpenAttemptSubquery,
		BackendVastai, jobID,
	)
	return err
}

// SetAttemptCost updates the cost on the latest open attempt.
func SetAttemptCost(db *sql.DB, jobID int64, cost float64) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET cost = ?
		WHERE id = `+latestOpenAttemptSubquery,
		cost, jobID,
	)
	return err
}

// SetAttemptPlacementMeta stores placement telemetry on the latest open attempt.
func SetAttemptPlacementMeta(db *sql.DB, jobID int64, meta *PlacementMeta) error {
	encoded, err := encodePlacementMeta(meta)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		UPDATE job_attempts SET placement_meta = ?
		WHERE id = `+latestOpenAttemptSubquery,
		encoded, jobID,
	)
	return err
}

// SetAttemptSessionName updates the session_name on the latest open attempt.
func SetAttemptSessionName(execer dbExecer, jobID int64, sessionName string) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET session_name = ?
		WHERE id = `+latestOpenAttemptSubquery,
		sessionName, jobID,
	)
	return err
}

// SetAttemptErrorDiagnosis updates error diagnosis on the latest attempt.
// Unfiltered because a diagnosis is usually recorded after the attempt has
// already closed terminally.
func SetAttemptErrorDiagnosis(db *sql.DB, jobID int64, diagnosis string) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET error_diagnosis = ?
		WHERE id = `+latestAttemptSubquery,
		diagnosis, jobID,
	)
	return err
}

// UpdateAttemptLastSyncedStatus updates last_synced_status on the latest open attempt.
func UpdateAttemptLastSyncedStatus(db *sql.DB, jobID int64, status string) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET last_synced_status = ?
		WHERE id = `+latestOpenAttemptSubquery,
		status, jobID,
	)
	return err
}

// UpdateAttemptStatusAndLastSynced updates status and last_synced_status on
// the latest open attempt. Terminal transitions also stamp end_time if
// missing, preserving the TerminalJobsHaveEndTime invariant — see
// StampAttemptStatus in specs/status-sync.allium. Guard is caller-side:
// UpdateStatusAndLastSynced runs checkOpenTransition before this write.
func UpdateAttemptStatusAndLastSynced(execer dbExecer, jobID int64, status string) error {
	if IsTerminalStatus(status) {
		now := time.Now().Unix()
		_, err := execer.Exec(`
			UPDATE job_attempts
			SET status = ?, last_synced_status = ?,
			    end_time = CASE WHEN end_time IS NULL OR end_time = 0 THEN ? ELSE end_time END
			WHERE id = `+latestOpenAttemptSubquery,
			status, status, now, jobID,
		)
		return err
	}
	_, err := execer.Exec(`
		UPDATE job_attempts SET status = ?, last_synced_status = ?
		WHERE id = `+latestOpenAttemptSubquery,
		status, status, jobID,
	)
	return err
}

// ClearAttemptPendingAndUpdateStatus reconciles pending_status by clearing it
// and updating status + last_synced_status on the latest open attempt.
// Guard is caller-side: ClearPendingAndUpdateStatus runs checkTransition
// before this write; the queued/draft branch deliberately resets a closed
// attempt (requeue semantics), which is why it uses latestAttemptSubquery.
func ClearAttemptPendingAndUpdateStatus(db *sql.DB, jobID int64, status string) error {
	// Use latestAttemptSubquery because this can reset a closed (terminal)
	// attempt back to queued/draft status.
	if status == StatusQueued || status == StatusDraft {
		// When going back to queued/draft, clear all execution fields
		_, err := db.Exec(`
			UPDATE job_attempts
			SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL,
			    start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL,
			    session_name = NULL, failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
			WHERE id = `+latestAttemptSubquery,
			status, status, jobID,
		)
		return err
	}
	if IsTerminalStatus(status) {
		// For terminal states, set end_time if missing and clear session
		now := time.Now().Unix()
		_, err := db.Exec(`
			UPDATE job_attempts
			SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL,
			    session_name = NULL,
			    end_time = CASE WHEN end_time IS NULL OR end_time = 0 THEN ? ELSE end_time END
			WHERE id = `+latestAttemptSubquery,
			status, status, now, jobID,
		)
		return err
	}
	_, err := db.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL
		WHERE id = `+latestAttemptSubquery,
		status, status, jobID,
	)
	return err
}

// JobAttempt is a row from job_attempts for display purposes.
type JobAttempt struct {
	ID            int64
	JobID         int64
	AttemptNumber int
	Host          string
	LaunchID      *int64
	Status        string
	QueuedAt      *int64
	StartTime     *int64
	EndTime       *int64
	ExitCode      *int
	ErrorMessage  string
	FailureReason string
	CloudOutcome  string
	Backend       string
	Metadata      *JobMetadata
}

// ListAttempts returns non-abandoned attempts for a job, newest first.
func ListAttempts(database *sql.DB, jobID int64) ([]JobAttempt, error) {
	rows, err := database.Query(`
		SELECT id, job_id, attempt_number, host, launch_id, status,
		       queued_at, start_time, end_time, exit_code,
		       COALESCE(error_message, ''), COALESCE(failure_reason, ''),
		       COALESCE(cloud_outcome, ''), COALESCE(backend, ''), job_metadata
		FROM job_attempts
		WHERE job_id = ? AND abandoned_at IS NULL
		ORDER BY attempt_number DESC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list attempts for job %d: %w", jobID, err)
	}
	defer rows.Close()

	var out []JobAttempt
	for rows.Next() {
		var a JobAttempt
		var launchID, queuedAt, startTime, endTime, exitCode sql.NullInt64
		var metadata sql.NullString
		if err := rows.Scan(
			&a.ID, &a.JobID, &a.AttemptNumber, &a.Host, &launchID, &a.Status,
			&queuedAt, &startTime, &endTime, &exitCode,
			&a.ErrorMessage, &a.FailureReason, &a.CloudOutcome, &a.Backend, &metadata,
		); err != nil {
			return nil, err
		}
		if launchID.Valid {
			v := launchID.Int64
			a.LaunchID = &v
		}
		a.Metadata = decodeJobMetadata(metadata)
		if queuedAt.Valid {
			v := queuedAt.Int64
			a.QueuedAt = &v
		}
		if startTime.Valid {
			v := startTime.Int64
			a.StartTime = &v
		}
		if endTime.Valid {
			v := endTime.Int64
			a.EndTime = &v
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			a.ExitCode = &v
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkAttemptQueuedByID closes any open attempt and creates a fresh queued
// attempt, preserving the historical record of the previous attempt.
// Carries over host, pending_status, and pending_at from the old attempt so
// that placement and user intent survive the reset. Sets last_synced_status
// because this is called from the sync path when the remote reports the job
// is still queued.
func MarkAttemptQueuedByID(database *sql.DB, jobID int64) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}

	// Prefer placement and intent fields from the current open attempt before
	// closing. If sync requeues a closed on-prem attempt, preserve the previous
	// host for legacy queue-runner behavior. Closed cloud attempts are
	// historical facts; reusing their launch_id here fabricates duplicate
	// same-launch attempts when a stale sync path reports "queued" after
	// completion backfill has already landed.
	var host string
	var pendingStatus sql.NullString
	var pendingAt sql.NullInt64
	var launchID sql.NullInt64
	err = tx.QueryRow(`
		SELECT host, pending_status, pending_at, launch_id FROM job_attempts
		WHERE job_id = ? AND end_time IS NULL
		ORDER BY attempt_number DESC LIMIT 1`, jobID,
	).Scan(&host, &pendingStatus, &pendingAt, &launchID)
	if err == sql.ErrNoRows {
		err = tx.QueryRow(`
			SELECT host, pending_status, pending_at, launch_id FROM job_attempts
			WHERE job_id = ?
			ORDER BY attempt_number DESC LIMIT 1`, jobID,
		).Scan(&host, &pendingStatus, &pendingAt, &launchID)
		if err == sql.ErrNoRows {
			// Preserve historical first-queue behavior for jobs with no
			// attempt rows at all.
		} else if err != nil {
			tx.Rollback()
			return err
		} else if launchID.Valid {
			return tx.Commit()
		}
	} else if err != nil {
		tx.Rollback()
		return err
	}

	now := time.Now().Unix()
	if err := closeOpenAttempts(tx, jobID, now); err != nil {
		tx.Rollback()
		return err
	}

	var cloudInstanceID *int64
	if launchID.Valid {
		cloudInstanceID = &launchID.Int64
	}
	attemptID, err := createAttemptTx(tx, jobID, host, cloudInstanceID, StatusQueued)
	if err != nil {
		tx.Rollback()
		return err
	}
	if pendingStatus.Valid || pendingAt.Valid {
		if _, err := tx.Exec(`UPDATE job_attempts SET pending_status = ?, pending_at = ? WHERE id = ?`,
			pendingStatus, pendingAt, attemptID); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE job_attempts SET last_synced_status = ? WHERE id = ?`,
		StatusQueued, attemptID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CancelledJobStart records an attempt that began after its job's cancel was
// requested — work the user asked not to happen, running anyway.
type CancelledJobStart struct {
	JobID             int64
	AttemptID         int64
	LaunchID          *int64
	StartedAt         time.Time
	CancelRequestedAt time.Time
}

// JobsStartedAfterCancel returns attempts that started strictly after their
// job's recorded cancel time and are still open.
//
// Every clause is positive evidence, because the caller terminates on this.
// A job with cancel intent but no recorded cancel time is excluded: the time
// is unknown, not zero, and treating unknown as "before every attempt" would
// kill work on no evidence. An attempt whose start_time is null is likewise
// excluded — not yet observed to have started is not the same as started
// before the cancel. Equal timestamps do not qualify; only a start strictly
// after the cancel shows the dispatch outlived the user's decision.
func JobsStartedAfterCancel(database *sql.DB) ([]CancelledJobStart, error) {
	rows, err := database.Query(`
		SELECT j.id, ja.id, ja.launch_id, ja.start_time, j.cancel_requested_at
		FROM jobs j
		JOIN job_attempts ja ON ja.job_id = j.id
		WHERE j.requested_status = ?
		  AND j.cancel_requested_at IS NOT NULL
		  AND j.cancel_requested_at > 0
		  AND ja.start_time IS NOT NULL
		  AND ja.start_time > j.cancel_requested_at
		  AND ja.status IN (?, ?, ?)
		ORDER BY j.id, ja.id`,
		StatusCanceled, StatusRunning, StatusStarting, StatusQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CancelledJobStart
	for rows.Next() {
		var (
			rec       CancelledJobStart
			launchID  sql.NullInt64
			startedAt int64
			cancelAt  int64
		)
		if err := rows.Scan(&rec.JobID, &rec.AttemptID, &launchID, &startedAt, &cancelAt); err != nil {
			return nil, err
		}
		if launchID.Valid {
			id := launchID.Int64
			rec.LaunchID = &id
		}
		rec.StartedAt = time.Unix(startedAt, 0)
		rec.CancelRequestedAt = time.Unix(cancelAt, 0)
		out = append(out, rec)
	}
	return out, rows.Err()
}
