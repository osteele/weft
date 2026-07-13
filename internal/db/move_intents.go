package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/retrypolicy"
)

// MoveIntent records a speculative attempt to move a queued job from its
// current placement (the source) to a target placement. The source remains
// authoritative — its launch association is unchanged — until the intent is
// confirmed. Until then, the autopilot ignores the job (a placed job is not
// an autopilot candidate). On confirmation, the source attempt is canceled
// and the job is associated with the target. On cancellation, the source is
// untouched.
//
// See specs/job-move.allium for the formal model.
type MoveIntent struct {
	ID                  int64
	JobID               int64
	SourceAttemptID     *int64
	SourceLaunchID      *int64
	TargetKind          MoveTargetKind
	TargetLaunchID      *int64 // set immediately for 'existing'; populated after cloud launch returns for 'new'
	TargetAttemptID     *int64
	TargetHost          string
	TargetOfferProvider string
	TargetOfferID       string
	TargetGPUName       string
	TargetRequestID     string
	TargetRequestKind   string
	TargetRequestAt     *int64
	State               MoveIntentState
	AttemptCount        int
	MaxAttempts         int
	CreatedAt           int64
	ResolvedAt          *int64
	Resolution          string
}

type MoveTargetKind string

const (
	MoveTargetExisting MoveTargetKind = "existing"
	MoveTargetNew      MoveTargetKind = "new"
)

type MoveIntentState string

const (
	MoveIntentStateOpen      MoveIntentState = "open"
	MoveIntentStateConfirmed MoveIntentState = "confirmed"
	MoveIntentStateCanceled  MoveIntentState = "canceled"
	MoveIntentStateObsoleted MoveIntentState = "obsoleted"
)

// ErrMoveIntentAlreadyOpen indicates a second move was requested on a job
// that already has an open intent.
var ErrMoveIntentAlreadyOpen = errors.New("job already has an open move intent")

// CreateMoveIntentParams carries the fields needed to open an intent.
type CreateMoveIntentParams struct {
	JobID               int64
	TargetKind          MoveTargetKind
	TargetLaunchID      *int64 // pass nil for MoveTargetNew until launch resolves
	TargetAttemptID     *int64
	TargetHost          string
	TargetOfferProvider string
	TargetOfferID       string
	TargetGPUName       string
	TargetRequestID     string
	TargetRequestKind   string
	TargetRequestAt     *int64
	AttemptCount        int
	MaxAttempts         int
}

// CreateMoveIntent inserts a new open intent. Atomicity of the
// "at most one open intent per job" property is enforced by the UNIQUE
// partial index `idx_move_intents_open`; a uniqueness violation is
// translated to ErrMoveIntentAlreadyOpen.
func CreateMoveIntent(database *sql.DB, p CreateMoveIntentParams) (*MoveIntent, error) {
	if p.JobID <= 0 {
		return nil, fmt.Errorf("invalid job id")
	}
	if p.TargetKind != MoveTargetExisting && p.TargetKind != MoveTargetNew {
		return nil, fmt.Errorf("invalid target kind %q", p.TargetKind)
	}
	targetHost := strings.TrimSpace(p.TargetHost)
	if p.TargetKind == MoveTargetExisting && targetHost == "" && (p.TargetLaunchID == nil || *p.TargetLaunchID <= 0) {
		return nil, fmt.Errorf("existing target requires target_launch_id or target_host")
	}
	now := time.Now().Unix()
	attemptCount := p.AttemptCount
	if attemptCount <= 0 {
		attemptCount = 1
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = retrypolicy.MaxPlacementAttempts()
	}

	tx, err := database.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sourceAttemptID sql.NullInt64
	var sourceLaunchID sql.NullInt64
	if err := tx.QueryRow(`
		SELECT ja.id, ja.launch_id
		  FROM job_attempts ja
		 WHERE ja.job_id = ?
		   AND ja.end_time IS NULL
		   AND ja.abandoned_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1
		         FROM move_intents mi
		        WHERE mi.state = 'open'
		          AND mi.id = ja.move_intent_id
		   )
		 ORDER BY ja.attempt_number DESC, ja.id DESC
		 LIMIT 1`,
		p.JobID,
	).Scan(&sourceAttemptID, &sourceLaunchID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("snapshot move source attempt: %w", err)
	}

	res, err := tx.Exec(
		`INSERT INTO move_intents (
			job_id, source_attempt_id, source_launch_id,
			target_kind, target_launch_id, target_attempt_id, target_host, target_offer_provider, target_offer_id, target_gpu_name,
			target_request_id, target_request_kind, target_request_created_at,
			state, attempt_count, max_attempts, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		p.JobID, nullableSQLInt64(sourceAttemptID), nullableSQLInt64(sourceLaunchID),
		string(p.TargetKind), nullableInt64(p.TargetLaunchID), nullableInt64(p.TargetAttemptID), emptyToNull(targetHost),
		emptyToNull(p.TargetOfferProvider), emptyToNull(p.TargetOfferID), emptyToNull(p.TargetGPUName),
		emptyToNull(p.TargetRequestID), emptyToNull(p.TargetRequestKind), nullableInt64(p.TargetRequestAt),
		attemptCount, maxAttempts, now,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, fmt.Errorf("%w", ErrMoveIntentAlreadyOpen)
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetMoveIntent(database, id)
}

func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed")
}

// GetMoveIntent returns the intent by id, or nil if not found.
func GetMoveIntent(database *sql.DB, id int64) (*MoveIntent, error) {
	row := database.QueryRow(moveIntentSelect+` WHERE id = ?`, id)
	return scanMoveIntent(row)
}

// GetOpenMoveIntent returns the open intent for a job, or nil if none.
func GetOpenMoveIntent(database *sql.DB, jobID int64) (*MoveIntent, error) {
	row := database.QueryRow(moveIntentSelect+` WHERE job_id = ? AND state = 'open' LIMIT 1`, jobID)
	return scanMoveIntent(row)
}

// GetRecentMoveIntent returns the newest open intent for a job, or the newest
// resolved intent at or after sinceUnix. Open intents take precedence because
// they describe the active transition.
func GetRecentMoveIntent(database *sql.DB, jobID, sinceUnix int64) (*MoveIntent, error) {
	row := database.QueryRow(moveIntentSelect+`
		WHERE job_id = ?
		  AND (state = 'open' OR (resolved_at IS NOT NULL AND resolved_at >= ?))
		ORDER BY CASE WHEN state = 'open' THEN 0 ELSE 1 END,
		         COALESCE(resolved_at, created_at) DESC,
		         id DESC
		LIMIT 1`, jobID, sinceUnix)
	return scanMoveIntent(row)
}

// ListOpenMoveIntents returns all currently open intents.
func ListOpenMoveIntents(database *sql.DB) ([]*MoveIntent, error) {
	rows, err := database.Query(moveIntentSelect + ` WHERE state = 'open' ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MoveIntent
	for rows.Next() {
		intent, err := scanMoveIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, intent)
	}
	return out, rows.Err()
}

// ListOpenNewMoveIntents returns open move-to-new intents for durable
// fulfillment by the autopilot.
func ListOpenNewMoveIntents(database *sql.DB) ([]*MoveIntent, error) {
	rows, err := database.Query(moveIntentSelect+` WHERE state = 'open' AND target_kind = ? ORDER BY created_at ASC`, string(MoveTargetNew))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MoveIntent
	for rows.Next() {
		intent, err := scanMoveIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, intent)
	}
	return out, rows.Err()
}

// JobIDsWithOpenMoveIntents returns the set of job ids that currently have
// an open intent. Used by the autopilot to exclude moving jobs from its
// candidate set.
func JobIDsWithOpenMoveIntents(database *sql.DB) (map[int64]struct{}, error) {
	rows, err := database.Query(`SELECT DISTINCT job_id FROM move_intents WHERE state = 'open'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// JobIDsWithRecentFailedMoveIntents returns jobs with at least minFailures
// recent canceled move intents. Benign cancellations that should not suppress
// automatic rebalancing are excluded explicitly; every other cancellation is
// treated as failure evidence so new rejection paths do not bypass convergence.
func JobIDsWithRecentFailedMoveIntents(database *sql.DB, since time.Time, minFailures int) (map[int64]struct{}, error) {
	if database == nil || minFailures <= 0 {
		return map[int64]struct{}{}, nil
	}
	rows, err := database.Query(`
		SELECT job_id
		  FROM move_intents
		 WHERE state = ?
		   AND resolved_at IS NOT NULL
		   AND resolved_at >= ?
		   AND COALESCE(resolution, '') NOT IN (?, ?)
		 GROUP BY job_id
		HAVING COUNT(*) >= ?`,
		string(MoveIntentStateCanceled),
		since.Unix(),
		MoveIntentResolutionStale,
		MoveIntentResolutionSuperseded,
		minFailures,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// MoveIntentSourceStop describes the source side effects to perform after a
// target launch accepts a move-to-new job.
type MoveIntentSourceStop struct {
	IntentID        int64
	JobID           int64
	SourceAttemptID int64
	SourceLaunchID  *int64
	SourceHost      string
	SourceStartTime int64
}

type MoveIntentPendingTargetRequest struct {
	IntentID         int64
	JobID            int64
	TargetLaunchID   int64
	TargetAttemptID  int64
	RequestID        string
	RequestKind      string
	RequestCreatedAt int64
	SourceAttemptID  int64
	SourceLaunchID   *int64
	SourceHost       string
	SourceStartTime  int64
}

// ListMoveIntentSourceStopsForTargetLaunch returns the source attempts that
// should be stopped after the target launch is confirmed. Call before
// ConfirmOpenMoveIntentsForTargetLaunch, while the intents are still open.
// Intents with target_request_id are excluded because running existing rental
// targets must be confirmed by the matching grace ack, not by generic
// agent-ready convergence.
func ListMoveIntentSourceStopsForTargetLaunch(database *sql.DB, launchID int64) ([]MoveIntentSourceStop, error) {
	if launchID <= 0 {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT mi.id, mi.job_id, mi.source_attempt_id,
		       mi.source_launch_id, ja.host, COALESCE(ja.start_time, 0)
		  FROM move_intents mi
		  JOIN job_attempts ja ON ja.id = mi.source_attempt_id
		 WHERE mi.target_launch_id = ?
	   AND mi.state = ?
	   AND mi.source_attempt_id IS NOT NULL
	   AND mi.target_request_id IS NULL
	 ORDER BY mi.id`,
		launchID, string(MoveIntentStateOpen),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MoveIntentSourceStop
	for rows.Next() {
		var stop MoveIntentSourceStop
		var sourceLaunch sql.NullInt64
		var host sql.NullString
		if err := rows.Scan(&stop.IntentID, &stop.JobID, &stop.SourceAttemptID, &sourceLaunch, &host, &stop.SourceStartTime); err != nil {
			return nil, err
		}
		if sourceLaunch.Valid {
			v := sourceLaunch.Int64
			stop.SourceLaunchID = &v
		}
		if host.Valid {
			stop.SourceHost = strings.TrimSpace(host.String)
		}
		out = append(out, stop)
	}
	return out, rows.Err()
}

func SetMoveIntentTargetRequest(database *sql.DB, intentID int64, requestKind, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	requestKind = strings.TrimSpace(requestKind)
	if intentID <= 0 {
		return fmt.Errorf("invalid move intent id")
	}
	if requestID == "" {
		return fmt.Errorf("target request id is required")
	}
	if requestKind == "" {
		requestKind = "jobs"
	}
	_, err := database.Exec(
		`UPDATE move_intents
		    SET target_request_id = ?,
		        target_request_kind = ?,
		        target_request_created_at = COALESCE(target_request_created_at, ?)
		  WHERE id = ? AND state = 'open'`,
		requestID, requestKind, time.Now().Unix(), intentID,
	)
	return err
}

func ListOpenMoveIntentPendingTargetRequests(database *sql.DB, launchID int64) ([]MoveIntentPendingTargetRequest, error) {
	if launchID <= 0 {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT mi.id, mi.job_id, mi.target_launch_id, mi.target_attempt_id,
		       mi.target_request_id, COALESCE(mi.target_request_kind, ''),
		       COALESCE(mi.target_request_created_at, 0),
		       mi.source_attempt_id, mi.source_launch_id, ja.host, COALESCE(ja.start_time, 0)
		  FROM move_intents mi
		  LEFT JOIN job_attempts ja ON ja.id = mi.source_attempt_id
		 WHERE mi.target_launch_id = ?
		   AND mi.state = 'open'
		   AND mi.target_attempt_id IS NOT NULL
		   AND mi.target_request_id IS NOT NULL
		 ORDER BY mi.id`,
		launchID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MoveIntentPendingTargetRequest
	for rows.Next() {
		var req MoveIntentPendingTargetRequest
		var sourceAttempt, sourceLaunch sql.NullInt64
		var host sql.NullString
		if err := rows.Scan(
			&req.IntentID, &req.JobID, &req.TargetLaunchID, &req.TargetAttemptID,
			&req.RequestID, &req.RequestKind, &req.RequestCreatedAt,
			&sourceAttempt, &sourceLaunch, &host, &req.SourceStartTime,
		); err != nil {
			return nil, err
		}
		if sourceAttempt.Valid {
			req.SourceAttemptID = sourceAttempt.Int64
		}
		if sourceLaunch.Valid {
			v := sourceLaunch.Int64
			req.SourceLaunchID = &v
		}
		if host.Valid {
			req.SourceHost = strings.TrimSpace(host.String)
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// UpdateMoveIntentTargetLaunch fills in target_launch_id once a 'new' move
// has had its instance created. No-op if the intent is no longer open.
func UpdateMoveIntentTargetLaunch(database *sql.DB, intentID, launchID int64) error {
	_, err := database.Exec(
		`UPDATE move_intents SET target_launch_id = ? WHERE id = ? AND state = 'open'`,
		launchID, intentID,
	)
	return err
}

// UpdateMoveIntentTargetAttempt records the destination attempt opened for an
// in-flight move. No-op if the intent is no longer open.
func UpdateMoveIntentTargetAttempt(database *sql.DB, intentID, attemptID int64) error {
	_, err := database.Exec(
		`UPDATE move_intents SET target_attempt_id = ? WHERE id = ? AND state = 'open'`,
		attemptID, intentID,
	)
	return err
}

// AdvanceMoveIntentTargetLaunch records a replacement launch for an open
// move-to-new intent and increments the durable attempt counter.
func AdvanceMoveIntentTargetLaunch(database *sql.DB, intentID, launchID int64) error {
	_, err := database.Exec(
		`UPDATE move_intents
		    SET target_launch_id = ?,
		        attempt_count = attempt_count + 1
		  WHERE id = ?
		    AND state = 'open'
		    AND target_kind = ?`,
		launchID, intentID, string(MoveTargetNew),
	)
	return err
}

// ResolveMoveIntent transitions an open intent to a terminal state. No-op if
// the intent is already resolved.
func ResolveMoveIntent(database *sql.DB, intentID int64, state MoveIntentState, resolution string) error {
	if state == MoveIntentStateOpen {
		return fmt.Errorf("cannot resolve to open state")
	}
	if intentID <= 0 {
		return fmt.Errorf("invalid move intent id")
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = string(state)
	}
	var currentState string
	var targetAttempt sql.NullInt64
	if err := database.QueryRow(
		`SELECT state, target_attempt_id FROM move_intents WHERE id = ?`,
		intentID,
	).Scan(&currentState, &targetAttempt); err != nil {
		return err
	}
	if MoveIntentState(currentState) != MoveIntentStateOpen {
		return nil
	}
	if state == MoveIntentStateConfirmed && targetAttempt.Valid {
		return ConfirmMoveTargetAccepted(database, intentID, resolution)
	}

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if targetAttempt.Valid {
		reason := AttemptAbandonedMoveDestinationRejected
		if state == MoveIntentStateObsoleted {
			reason = AttemptAbandonedMoveSourceWon
		}
		if _, err := tx.Exec(
			`UPDATE job_attempts
			    SET abandoned_at = COALESCE(abandoned_at, ?),
			        abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), ?),
			        abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, ?)
			  WHERE id = ?`,
			now, reason, intentID, targetAttempt.Int64,
		); err != nil {
			return fmt.Errorf("abandon target attempt: %w", err)
		}
	}
	_, err = tx.Exec(
		`UPDATE move_intents SET state = ?, resolved_at = ?, resolution = ? WHERE id = ? AND state = 'open'`,
		string(state), now, resolution, intentID,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmOpenMoveIntentsForTargetLaunch confirms open move intents that target
// a launch once that launch has accepted its initial job queue. Intents with a
// pending target_request_id are excluded; those require the exact grace ack.
func ConfirmOpenMoveIntentsForTargetLaunch(database *sql.DB, launchID int64, resolution string) error {
	if launchID <= 0 {
		return nil
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = fmt.Sprintf("target launch %d accepted jobs", launchID)
	}
	rows, err := database.Query(
		`SELECT id, target_attempt_id FROM move_intents
		  WHERE target_launch_id = ?
		    AND state = ?
		    AND target_request_id IS NULL
		  ORDER BY id`,
		launchID, string(MoveIntentStateOpen),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type targetIntent struct {
		id               int64
		hasTargetAttempt bool
	}
	var intents []targetIntent
	for rows.Next() {
		var id int64
		var targetAttempt sql.NullInt64
		if err := rows.Scan(&id, &targetAttempt); err != nil {
			return err
		}
		intents = append(intents, targetIntent{id: id, hasTargetAttempt: targetAttempt.Valid})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, intent := range intents {
		if intent.hasTargetAttempt {
			if err := ConfirmMoveTargetAccepted(database, intent.id, resolution); err != nil {
				return err
			}
			continue
		}
		if _, err := database.Exec(
			`UPDATE move_intents
			    SET state = ?, resolved_at = ?, resolution = ?
			  WHERE id = ? AND state = ?`,
			string(MoveIntentStateConfirmed), now, resolution, intent.id, string(MoveIntentStateOpen),
		); err != nil {
			return err
		}
	}
	return nil
}

// PrunedMoveIntent describes a move intent that PruneMoveIntents just
// resolved, suitable for logging or UI narration. State and Resolution
// carry the sweep's verdict: canceled (stale target), obsoleted (source
// won), or confirmed (target attempt finished first).
type PrunedMoveIntent struct {
	ID         int64
	JobID      int64
	CreatedAt  int64
	State      MoveIntentState
	Resolution string
}

// MoveIntentResolutionStale is the resolution string written to stale move
// intents whose target launch is no longer live.
const MoveIntentResolutionStale = "stale: no non-terminal target launch (auto-pruned)"

// MoveIntentResolutionSuperseded is the resolution string written when an
// explicit user-initiated move supersedes an open intent. Both benign
// resolutions are excluded from JobIDsWithRecentFailedMoveIntents; keep the
// write sites and that query on these constants.
const MoveIntentResolutionSuperseded = "superseded by explicit move"

// PruneMoveIntents cancels open move intents that no longer represent an
// active move. A fresh intent is protected for protectionWindow so the
// offer-search -> provider-create gap is not pruned prematurely.
func PruneMoveIntents(database *sql.DB, protectionWindow time.Duration) ([]PrunedMoveIntent, error) {
	if database == nil {
		return nil, nil
	}
	cutoff := time.Now().Add(-protectionWindow).Unix()
	const query = `
		SELECT id, job_id, created_at
		  FROM move_intents
		 WHERE state = 'open'
		   AND created_at < ?
		   AND (
		         target_launch_id IS NULL
		         OR NOT EXISTS (
		              SELECT 1
		                FROM launches l
		               WHERE l.id = move_intents.target_launch_id
		                 AND l.status NOT IN ('failed','canceled','completed')
		            )
		       )
		 ORDER BY id`
	repairsByID := map[int64]PrunedMoveIntent{}
	rows, err := database.Query(query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("prune stale move intents: %w", err)
	}

	for rows.Next() {
		mi := PrunedMoveIntent{
			State:      MoveIntentStateCanceled,
			Resolution: MoveIntentResolutionStale,
		}
		if err := rows.Scan(&mi.ID, &mi.JobID, &mi.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pruned move intent: %w", err)
		}
		repairsByID[mi.ID] = mi
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	outcomeRepairs, err := listOpenMoveIntentOutcomeRepairs(database)
	if err != nil {
		return nil, err
	}
	for _, repair := range outcomeRepairs {
		repairsByID[repair.ID] = repair
	}

	pruned := make([]PrunedMoveIntent, 0, len(repairsByID))
	for _, repair := range repairsByID {
		if err := ResolveMoveIntent(database, repair.ID, repair.State, repair.Resolution); err != nil {
			return nil, fmt.Errorf("resolve stale move intent %d: %w", repair.ID, err)
		}
		pruned = append(pruned, repair)
	}
	return pruned, nil
}

// moveOutcomeTerminalStatuses is the attempt-status set that decides a move's
// outcome. It must match the move_intents auto-resolution triggers
// (move_intents_auto_obsolete_on_source_attempt_* and
// move_intents_auto_confirm_on_target_attempt_end).
const moveOutcomeTerminalStatuses = `('completed','failed')`

func moveSourceWonResolution(status string) string {
	return "source attempt finished before target won (status=" + status + ")"
}

func listOpenMoveIntentOutcomeRepairs(database *sql.DB) ([]PrunedMoveIntent, error) {
	const targetQuery = `
		SELECT mi.id, mi.job_id, mi.created_at, ta.status
		  FROM move_intents mi
		  JOIN job_attempts ta ON ta.id = mi.target_attempt_id
		 WHERE mi.state = 'open'
		   AND ta.end_time IS NOT NULL
		   AND ta.status IN ` + moveOutcomeTerminalStatuses + `
		 ORDER BY mi.id`
	repairs, err := scanMoveIntentOutcomeRepairs(database, targetQuery, MoveIntentStateConfirmed,
		func(status string) string { return "target attempt finished first (status=" + status + ")" })
	if err != nil {
		return nil, err
	}

	const sourceQuery = `
		SELECT mi.id, mi.job_id, mi.created_at, sa.status
		  FROM move_intents mi
		  JOIN job_attempts sa ON sa.id = mi.source_attempt_id
		 WHERE mi.state = 'open'
		   AND sa.end_time IS NOT NULL
		   AND sa.status IN ` + moveOutcomeTerminalStatuses + `
		 ORDER BY mi.id`
	sourceRepairs, err := scanMoveIntentOutcomeRepairs(database, sourceQuery, MoveIntentStateObsoleted, moveSourceWonResolution)
	if err != nil {
		return nil, err
	}
	repairs = append(repairs, sourceRepairs...)

	const missingSourceQuery = `
		SELECT mi.id, mi.job_id, mi.created_at, sa.status
		  FROM move_intents mi
		  JOIN job_attempts sa ON sa.id = (
		       SELECT ja.id
		         FROM job_attempts ja
		        WHERE ja.job_id = mi.job_id
		          AND ja.launch_id = mi.source_launch_id
		          AND ja.end_time IS NOT NULL
		          AND ja.status IN ` + moveOutcomeTerminalStatuses + `
		          AND (mi.target_attempt_id IS NULL OR ja.id != mi.target_attempt_id)
		        ORDER BY ja.attempt_number DESC, ja.id DESC
		        LIMIT 1
		   )
		 WHERE mi.state = 'open'
		   AND mi.source_attempt_id IS NULL
		   AND mi.source_launch_id IS NOT NULL
		 ORDER BY mi.id`
	missingSourceRepairs, err := scanMoveIntentOutcomeRepairs(database, missingSourceQuery, MoveIntentStateObsoleted, moveSourceWonResolution)
	if err != nil {
		return nil, err
	}
	repairs = append(repairs, missingSourceRepairs...)
	return repairs, nil
}

func scanMoveIntentOutcomeRepairs(database *sql.DB, query string, state MoveIntentState, resolution func(string) string) ([]PrunedMoveIntent, error) {
	rows, err := database.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repairs []PrunedMoveIntent
	for rows.Next() {
		var repair PrunedMoveIntent
		var status string
		if err := rows.Scan(&repair.ID, &repair.JobID, &repair.CreatedAt, &status); err != nil {
			return nil, err
		}
		repair.State = state
		repair.Resolution = resolution(status)
		repairs = append(repairs, repair)
	}
	return repairs, rows.Err()
}

const moveIntentSelect = `SELECT
	id, job_id, source_attempt_id, source_launch_id,
	target_kind, target_launch_id, target_attempt_id, target_host, target_offer_provider, target_offer_id, target_gpu_name,
	target_request_id, target_request_kind, target_request_created_at,
	state, attempt_count, max_attempts, created_at, resolved_at, resolution
FROM move_intents`

type moveIntentScanner interface {
	Scan(dest ...any) error
}

func scanMoveIntent(s moveIntentScanner) (*MoveIntent, error) {
	var (
		mi                                                                                                                        MoveIntent
		sourceAttempt, sourceLaunch, targetLaunch, targetAttempt, targetRequestCreatedAt, resolvedAt                              sql.NullInt64
		targetHost, targetOfferProvider, targetOfferID, targetGPUName, targetRequestID, targetRequestKind, resolution, targetKind sql.NullString
		stateStr                                                                                                                  string
	)
	err := s.Scan(
		&mi.ID, &mi.JobID, &sourceAttempt, &sourceLaunch,
		&targetKind, &targetLaunch, &targetAttempt, &targetHost, &targetOfferProvider, &targetOfferID, &targetGPUName,
		&targetRequestID, &targetRequestKind, &targetRequestCreatedAt,
		&stateStr, &mi.AttemptCount, &mi.MaxAttempts, &mi.CreatedAt, &resolvedAt, &resolution,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sourceAttempt.Valid {
		mi.SourceAttemptID = &sourceAttempt.Int64
	}
	if sourceLaunch.Valid {
		mi.SourceLaunchID = &sourceLaunch.Int64
	}
	if targetLaunch.Valid {
		mi.TargetLaunchID = &targetLaunch.Int64
	}
	if targetAttempt.Valid {
		mi.TargetAttemptID = &targetAttempt.Int64
	}
	if resolvedAt.Valid {
		mi.ResolvedAt = &resolvedAt.Int64
	}
	if targetRequestCreatedAt.Valid {
		mi.TargetRequestAt = &targetRequestCreatedAt.Int64
	}
	mi.TargetKind = MoveTargetKind(targetKind.String)
	mi.TargetHost = targetHost.String
	mi.TargetOfferProvider = targetOfferProvider.String
	mi.TargetOfferID = targetOfferID.String
	mi.TargetGPUName = targetGPUName.String
	mi.TargetRequestID = targetRequestID.String
	mi.TargetRequestKind = targetRequestKind.String
	mi.State = MoveIntentState(stateStr)
	mi.Resolution = resolution.String
	return &mi, nil
}

func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableSQLInt64(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func emptyToNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
