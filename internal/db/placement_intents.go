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
