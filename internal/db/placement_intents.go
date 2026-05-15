package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PlacementIntent records that a placement operation (initial launch,
// orphan relaunch, bulk move-to-new) is in flight for a given job. While
// open, the autopilot ignores the job — the operation is responsible for
// either confirming the intent (placement succeeded; job is on a launch)
// or canceling it (placement failed; job returns to its prior state).
//
// PlacementIntent and MoveIntent share the same role from the autopilot's
// perspective ("don't touch this job") but represent different operations:
//   - MoveIntent: the user's deliberate move action (CLI / TUI 'm' key).
//   - PlacementIntent: a programmatic placement attempt by the orchestrator
//     itself (LaunchCampaign-driven, RelaunchOrphanedJobs, bulk move).
//
// See specs/job-move.allium § AutopilotIgnoresMovingJobs (intent inclusive).
type PlacementIntent struct {
	ID         int64
	JobID      int64
	Operation  string
	State      PlacementIntentState
	CreatedAt  int64
	ResolvedAt *int64
	Resolution string
}

type PlacementIntentState string

const (
	PlacementIntentStateOpen      PlacementIntentState = "open"
	PlacementIntentStateConfirmed PlacementIntentState = "confirmed"
	PlacementIntentStateCanceled  PlacementIntentState = "canceled"
)

// ErrPlacementIntentAlreadyOpen indicates a second placement was requested
// on a job that already has one in flight.
var ErrPlacementIntentAlreadyOpen = errors.New("job already has an open placement intent")

// CreatePlacementIntent inserts a new open intent. Atomicity of the
// "at most one open intent per job" property is enforced by the UNIQUE
// partial index `idx_placement_intents_open`.
func CreatePlacementIntent(database *sql.DB, jobID int64, operation string) (*PlacementIntent, error) {
	if jobID <= 0 {
		return nil, fmt.Errorf("invalid job id")
	}
	if operation == "" {
		return nil, fmt.Errorf("operation required")
	}
	now := time.Now().Unix()
	res, err := database.Exec(
		`INSERT INTO placement_intents (job_id, operation, state, created_at) VALUES (?, ?, 'open', ?)`,
		jobID, operation, now,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, fmt.Errorf("%w", ErrPlacementIntentAlreadyOpen)
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return GetPlacementIntent(database, id)
}

func GetPlacementIntent(database *sql.DB, id int64) (*PlacementIntent, error) {
	row := database.QueryRow(placementIntentSelect+` WHERE id = ?`, id)
	return scanPlacementIntent(row)
}

// GetOpenPlacementIntent returns the open placement intent for a job, or nil
// if none exists.
func GetOpenPlacementIntent(database *sql.DB, jobID int64) (*PlacementIntent, error) {
	row := database.QueryRow(placementIntentSelect+` WHERE job_id = ? AND state = 'open' LIMIT 1`, jobID)
	return scanPlacementIntent(row)
}

// JobIDsWithOpenPlacementIntents returns the set of job ids that currently
// have an open placement intent.
func JobIDsWithOpenPlacementIntents(database *sql.DB) (map[int64]struct{}, error) {
	rows, err := database.Query(`SELECT DISTINCT job_id FROM placement_intents WHERE state = 'open'`)
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

// JobIDsWithOpenMoveOrPlacementIntents returns the union of job ids with an
// open MoveIntent or PlacementIntent — the "autopilot, hands off" set. See
// specs/job-move.allium § AutopilotIgnoresMovingJobs.
func JobIDsWithOpenMoveOrPlacementIntents(database *sql.DB) (map[int64]struct{}, error) {
	rows, err := database.Query(`
		SELECT job_id FROM move_intents WHERE state = 'open'
		UNION
		SELECT job_id FROM placement_intents WHERE state = 'open'`)
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

// OpenMoveOrPlacementIntentCreatedAt returns the newest open intent timestamp
// per job across MoveIntent and PlacementIntent. The UI uses this as the
// display age for Placing rows so retries do not inherit attempt-chain times.
func OpenMoveOrPlacementIntentCreatedAt(database *sql.DB, jobIDs []int64) (map[int64]int64, error) {
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
	query := `
		WITH open_intents AS (
			SELECT mi.job_id,
			       CASE
			       	WHEN mi.target_launch_id IS NOT NULL
			       	 AND l.created_at IS NOT NULL
			       	 AND l.created_at > mi.created_at
			       	THEN l.created_at
			       	ELSE mi.created_at
			       END AS created_at
			  FROM move_intents mi
			  LEFT JOIN launches l ON l.id = mi.target_launch_id
			 WHERE mi.state = 'open'
			UNION ALL
			SELECT job_id, created_at FROM placement_intents WHERE state = 'open'
		)
		SELECT job_id, MAX(created_at)
		  FROM open_intents
		 WHERE job_id IN (` + sqlPlaceholders(len(args)) + `)
		 GROUP BY job_id`
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]int64)
	for rows.Next() {
		var jobID, createdAt int64
		if err := rows.Scan(&jobID, &createdAt); err != nil {
			return nil, err
		}
		if createdAt > 0 {
			out[jobID] = createdAt
		}
	}
	return out, rows.Err()
}

// ResolvePlacementIntent transitions an open intent to a terminal state.
// No-op if the intent is already resolved.
func ResolvePlacementIntent(database *sql.DB, intentID int64, state PlacementIntentState, resolution string) error {
	if state == PlacementIntentStateOpen {
		return fmt.Errorf("cannot resolve to open state")
	}
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE placement_intents SET state = ?, resolved_at = ?, resolution = ? WHERE id = ? AND state = 'open'`,
		string(state), now, resolution, intentID,
	)
	return err
}

// CancelStalePlacementIntents resolves any open placement intent whose
// created_at is older than `olderThan`. Returns the number of intents
// canceled. Stale intents are usually a sign that a launch / relaunch
// path crashed without resolving its intent — the autopilot's
// pending_placement rescue branch existed for this case before intents
// were modeled.
func CancelStalePlacementIntents(database *sql.DB, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := database.Exec(
		`UPDATE placement_intents SET state = 'canceled', resolved_at = ?, resolution = 'stale (auto-canceled)'
		 WHERE state = 'open' AND created_at < ?`,
		time.Now().Unix(), cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PrunedIntent describes a placement intent that PrunePlacementIntents
// just resolved, suitable for logging or emitting lifecycle events.
type PrunedIntent struct {
	ID        int64
	JobID     int64
	Operation string
	CreatedAt int64
}

// PlacementIntentResolutionStale is the resolution string written to
// intents canceled by PrunePlacementIntents.
const PlacementIntentResolutionStale = "stale: no non-terminal launch (auto-pruned)"

// PrunePlacementIntents cancels open placement intents that have outlived
// their orchestration window. An intent is pruned when both:
//
//  1. it is older than `protectionWindow` (so a freshly-opened intent in
//     the offer-search → instance-create gap is not canceled out from
//     under a healthy in-flight relaunch); and
//  2. the job has no non-terminal cloud launch backing it — its most
//     recent job_attempts row either has no launch_id, or points at a
//     launch whose status is failed, canceled, or completed.
//
// Implemented as a single UPDATE … RETURNING so the read and write are
// atomic, eliminating the SELECT/UPDATE race window.
func PrunePlacementIntents(database *sql.DB, protectionWindow time.Duration) ([]PrunedIntent, error) {
	if database == nil {
		return nil, nil
	}
	now := time.Now().Unix()
	cutoff := time.Now().Add(-protectionWindow).Unix()
	const query = `
		UPDATE placement_intents
		   SET state = 'canceled', resolved_at = ?, resolution = ?
		 WHERE state = 'open'
		   AND created_at < ?
		   AND NOT EXISTS (
		         SELECT 1
		           FROM job_attempts ja
		           JOIN launches l ON l.id = ja.launch_id
		          WHERE ja.job_id = placement_intents.job_id
		            AND ja.id = (SELECT MAX(id) FROM job_attempts WHERE job_id = placement_intents.job_id)
		            AND COALESCE(l.status, '') NOT IN ('failed','canceled','completed')
		       )
		RETURNING id, job_id, operation, created_at`
	rows, err := database.Query(query, now, PlacementIntentResolutionStale, cutoff)
	if err != nil {
		return nil, fmt.Errorf("prune stale placement intents: %w", err)
	}
	defer rows.Close()
	var pruned []PrunedIntent
	for rows.Next() {
		var pi PrunedIntent
		if err := rows.Scan(&pi.ID, &pi.JobID, &pi.Operation, &pi.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pruned placement intent: %w", err)
		}
		pruned = append(pruned, pi)
	}
	return pruned, rows.Err()
}

const placementIntentSelect = `SELECT id, job_id, operation, state, created_at, resolved_at, resolution FROM placement_intents`

func scanPlacementIntent(s moveIntentScanner) (*PlacementIntent, error) {
	var (
		pi         PlacementIntent
		resolvedAt sql.NullInt64
		resolution sql.NullString
		stateStr   string
	)
	err := s.Scan(&pi.ID, &pi.JobID, &pi.Operation, &stateStr, &pi.CreatedAt, &resolvedAt, &resolution)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resolvedAt.Valid {
		pi.ResolvedAt = &resolvedAt.Int64
	}
	pi.State = PlacementIntentState(stateStr)
	pi.Resolution = resolution.String
	return &pi, nil
}
