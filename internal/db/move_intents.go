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
	TargetOfferProvider string
	TargetOfferID       string
	TargetGPUName       string
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
	SourceAttemptID     *int64
	SourceLaunchID      *int64
	TargetKind          MoveTargetKind
	TargetLaunchID      *int64 // pass nil for MoveTargetNew until launch resolves
	TargetOfferProvider string
	TargetOfferID       string
	TargetGPUName       string
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
	if p.TargetKind == MoveTargetExisting && (p.TargetLaunchID == nil || *p.TargetLaunchID <= 0) {
		return nil, fmt.Errorf("existing target requires target_launch_id")
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

	res, err := database.Exec(
		`INSERT INTO move_intents (
			job_id, source_attempt_id, source_launch_id,
			target_kind, target_launch_id, target_offer_provider, target_offer_id, target_gpu_name,
			state, attempt_count, max_attempts, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		p.JobID, nullableInt64(p.SourceAttemptID), nullableInt64(p.SourceLaunchID),
		string(p.TargetKind), nullableInt64(p.TargetLaunchID),
		emptyToNull(p.TargetOfferProvider), emptyToNull(p.TargetOfferID), emptyToNull(p.TargetGPUName),
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

// UpdateMoveIntentTargetLaunch fills in target_launch_id once a 'new' move
// has had its instance created. No-op if the intent is no longer open.
func UpdateMoveIntentTargetLaunch(database *sql.DB, intentID, launchID int64) error {
	_, err := database.Exec(
		`UPDATE move_intents SET target_launch_id = ? WHERE id = ? AND state = 'open'`,
		launchID, intentID,
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
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE move_intents SET state = ?, resolved_at = ?, resolution = ? WHERE id = ? AND state = 'open'`,
		string(state), now, resolution, intentID,
	)
	return err
}

// ConfirmOpenMoveIntentsForTargetLaunch confirms any open move intents that
// target a launch once that launch has accepted its initial job queue.
func ConfirmOpenMoveIntentsForTargetLaunch(database *sql.DB, launchID int64, resolution string) error {
	if launchID <= 0 {
		return nil
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = fmt.Sprintf("target launch %d accepted jobs", launchID)
	}
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE move_intents
		    SET state = ?, resolved_at = ?, resolution = ?
		  WHERE target_launch_id = ?
		    AND state = ?`,
		string(MoveIntentStateConfirmed), now, resolution, launchID, string(MoveIntentStateOpen),
	)
	return err
}

// PrunedMoveIntent describes a move intent that PruneMoveIntents just
// resolved, suitable for logging or UI narration.
type PrunedMoveIntent struct {
	ID        int64
	JobID     int64
	CreatedAt int64
}

// MoveIntentResolutionStale is the resolution string written to stale move
// intents whose target launch is no longer live.
const MoveIntentResolutionStale = "stale: no non-terminal target launch (auto-pruned)"

// PruneMoveIntents cancels open move intents that no longer represent an
// active move. A fresh intent is protected for protectionWindow so the
// offer-search -> provider-create gap is not pruned prematurely.
func PruneMoveIntents(database *sql.DB, protectionWindow time.Duration) ([]PrunedMoveIntent, error) {
	if database == nil {
		return nil, nil
	}
	now := time.Now().Unix()
	cutoff := time.Now().Add(-protectionWindow).Unix()
	const query = `
 		UPDATE move_intents
 		   SET state = 'canceled', resolved_at = ?, resolution = ?
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
		   AND attempt_count >= max_attempts
		RETURNING id, job_id, created_at`
	rows, err := database.Query(query, now, MoveIntentResolutionStale, cutoff)
	if err != nil {
		return nil, fmt.Errorf("prune stale move intents: %w", err)
	}
	defer rows.Close()

	var pruned []PrunedMoveIntent
	for rows.Next() {
		var mi PrunedMoveIntent
		if err := rows.Scan(&mi.ID, &mi.JobID, &mi.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pruned move intent: %w", err)
		}
		pruned = append(pruned, mi)
	}
	return pruned, rows.Err()
}

const moveIntentSelect = `SELECT
	id, job_id, source_attempt_id, source_launch_id,
	target_kind, target_launch_id, target_offer_provider, target_offer_id, target_gpu_name,
	state, attempt_count, max_attempts, created_at, resolved_at, resolution
FROM move_intents`

type moveIntentScanner interface {
	Scan(dest ...any) error
}

func scanMoveIntent(s moveIntentScanner) (*MoveIntent, error) {
	var (
		mi                                                                        MoveIntent
		sourceAttempt, sourceLaunch, targetLaunch, resolvedAt                     sql.NullInt64
		targetOfferProvider, targetOfferID, targetGPUName, resolution, targetKind sql.NullString
		stateStr                                                                  string
	)
	err := s.Scan(
		&mi.ID, &mi.JobID, &sourceAttempt, &sourceLaunch,
		&targetKind, &targetLaunch, &targetOfferProvider, &targetOfferID, &targetGPUName,
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
	if resolvedAt.Valid {
		mi.ResolvedAt = &resolvedAt.Int64
	}
	mi.TargetKind = MoveTargetKind(targetKind.String)
	mi.TargetOfferProvider = targetOfferProvider.String
	mi.TargetOfferID = targetOfferID.String
	mi.TargetGPUName = targetGPUName.String
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

func emptyToNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
