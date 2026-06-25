package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AutopilotState is the singleton row in the autopilot_state table. It records
// whether autopilot is paused (sticky across processes) and which process, if
// any, is currently driving an autopilot pass.
//
// While a pass is in flight, the runner updates LastHeartbeat every few
// seconds; if it ages past a stale threshold, callers may reclaim the slot.
type AutopilotState struct {
	Paused             bool
	PausedAt           time.Time
	PausedBy           string
	PausedReason       string
	ActiveRunnerPID    int
	ActiveRunnerLabel  string
	ActiveRunnerHost   string
	ActiveBinary       BinaryIdentity
	PassStartedAt      time.Time
	LastHeartbeat      time.Time
	LastPassFinishedAt time.Time
	LastPassDurationMS int64
	LastPassSummary    string
	LastPassError      string
}

type BinaryIdentity struct {
	Path        string
	Size        int64
	ModTimeUnix int64
	Dev         uint64
	Ino         uint64
}

func (s *AutopilotState) IsActive(now time.Time, staleAfter time.Duration) bool {
	if s == nil || s.PassStartedAt.IsZero() || s.LastHeartbeat.IsZero() {
		return false
	}
	return now.Sub(s.LastHeartbeat) < staleAfter
}

func (s *AutopilotState) IsStale(now time.Time, staleAfter time.Duration) bool {
	if s == nil || s.PassStartedAt.IsZero() {
		return false
	}
	if s.LastHeartbeat.IsZero() {
		return now.Sub(s.PassStartedAt) >= staleAfter
	}
	return now.Sub(s.LastHeartbeat) >= staleAfter
}

// LoadAutopilotState reads the singleton row.
func LoadAutopilotState(database *sql.DB) (*AutopilotState, error) {
	return scanAutopilotState(database)
}

// IsAutopilotPaused performs a single-column read to answer "is autopilot
// paused?" cheaply. Used by callers (e.g. watch_tui) on per-tick hot paths.
func IsAutopilotPaused(database *sql.DB) (bool, error) {
	var paused int64
	err := database.QueryRow(`SELECT paused FROM autopilot_state WHERE id = 1`).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return paused != 0, nil
}

func scanAutopilotState(database *sql.DB) (*AutopilotState, error) {
	row := database.QueryRow(`
		SELECT paused, paused_at, paused_by, paused_reason,
		       active_runner_pid, active_runner_label, active_runner_host,
		       active_binary_path, active_binary_size, active_binary_mtime,
		       active_binary_dev, active_binary_ino,
		       pass_started_at, last_heartbeat,
		       last_pass_finished_at, last_pass_duration_ms,
		       last_pass_summary, last_pass_error
		FROM autopilot_state WHERE id = 1
	`)
	var (
		paused             int64
		pausedAt           sql.NullInt64
		pausedBy           sql.NullString
		pausedReason       sql.NullString
		runnerPID          sql.NullInt64
		runnerLabel        sql.NullString
		runnerHost         sql.NullString
		binaryPath         sql.NullString
		binarySize         sql.NullInt64
		binaryMtime        sql.NullInt64
		binaryDev          sql.NullInt64
		binaryIno          sql.NullInt64
		passStartedAt      sql.NullInt64
		lastHeartbeat      sql.NullInt64
		lastPassFinishedAt sql.NullInt64
		lastPassDurationMS sql.NullInt64
		lastPassSummary    sql.NullString
		lastPassError      sql.NullString
	)
	if err := row.Scan(
		&paused, &pausedAt, &pausedBy, &pausedReason,
		&runnerPID, &runnerLabel, &runnerHost,
		&binaryPath, &binarySize, &binaryMtime, &binaryDev, &binaryIno,
		&passStartedAt, &lastHeartbeat,
		&lastPassFinishedAt, &lastPassDurationMS,
		&lastPassSummary, &lastPassError,
	); err != nil {
		return nil, err
	}
	state := &AutopilotState{
		Paused:            paused != 0,
		PausedBy:          pausedBy.String,
		PausedReason:      pausedReason.String,
		ActiveRunnerPID:   int(runnerPID.Int64),
		ActiveRunnerLabel: runnerLabel.String,
		ActiveRunnerHost:  runnerHost.String,
		ActiveBinary: BinaryIdentity{
			Path:        binaryPath.String,
			Size:        binarySize.Int64,
			ModTimeUnix: binaryMtime.Int64,
			Dev:         uint64(binaryDev.Int64),
			Ino:         uint64(binaryIno.Int64),
		},
		LastPassDurationMS: lastPassDurationMS.Int64,
		LastPassSummary:    lastPassSummary.String,
		LastPassError:      lastPassError.String,
	}
	if pausedAt.Valid && pausedAt.Int64 != 0 {
		state.PausedAt = time.Unix(pausedAt.Int64, 0)
	}
	if passStartedAt.Valid && passStartedAt.Int64 != 0 {
		state.PassStartedAt = time.Unix(passStartedAt.Int64, 0)
	}
	if lastHeartbeat.Valid && lastHeartbeat.Int64 != 0 {
		state.LastHeartbeat = time.Unix(lastHeartbeat.Int64, 0)
	}
	if lastPassFinishedAt.Valid && lastPassFinishedAt.Int64 != 0 {
		state.LastPassFinishedAt = time.Unix(lastPassFinishedAt.Int64, 0)
	}
	return state, nil
}

// PauseAutopilot sets the sticky paused flag. While paused, all autopilot
// runners must skip their passes. Returns the new state.
func PauseAutopilot(database *sql.DB, by, reason string) (*AutopilotState, error) {
	now := time.Now().Unix()
	if _, err := database.Exec(`
		UPDATE autopilot_state
		   SET paused = 1, paused_at = ?, paused_by = ?, paused_reason = ?
		 WHERE id = 1
	`, now, by, reason); err != nil {
		return nil, err
	}
	return scanAutopilotState(database)
}

// ResumeAutopilot clears the paused flag.
func ResumeAutopilot(database *sql.DB) (*AutopilotState, error) {
	if _, err := database.Exec(`
		UPDATE autopilot_state
		   SET paused = 0, paused_at = NULL, paused_by = NULL, paused_reason = NULL
		 WHERE id = 1
	`); err != nil {
		return nil, err
	}
	return scanAutopilotState(database)
}

// TryClaimAutopilotPass attempts to take the singleton pass slot. Returns
// claimed=true on success. Reasons claimed=false:
//   - paused=true: autopilot is globally paused
//   - existing!=nil: another runner holds a fresh claim
//
// A claim aged past staleAfter is reclaimed automatically.
func TryClaimAutopilotPass(database *sql.DB, pid int, label, host string, staleAfter time.Duration, binary BinaryIdentity) (claimed, paused bool, existing *AutopilotState, err error) {
	return TryClaimAutopilotPassWithOptions(database, pid, label, host, staleAfter, binary, AutopilotClaimOptions{})
}

type AutopilotClaimOptions struct {
	IgnorePaused bool
}

func TryClaimAutopilotPassWithOptions(database *sql.DB, pid int, label, host string, staleAfter time.Duration, binary BinaryIdentity, opts AutopilotClaimOptions) (claimed, paused bool, existing *AutopilotState, err error) {
	result, err := RetryOnDatabaseLockedValue(context.Background(), "claim autopilot pass", func() (autopilotClaimResult, error) {
		claimed, paused, existing, err := tryClaimAutopilotPassOnce(database, pid, label, host, staleAfter, binary, opts)
		return autopilotClaimResult{claimed: claimed, paused: paused, existing: existing}, err
	})
	if err != nil {
		return false, false, nil, err
	}
	return result.claimed, result.paused, result.existing, nil
}

type autopilotClaimResult struct {
	claimed  bool
	paused   bool
	existing *AutopilotState
}

func tryClaimAutopilotPassOnce(database *sql.DB, pid int, label, host string, staleAfter time.Duration, binary BinaryIdentity, opts AutopilotClaimOptions) (claimed, paused bool, existing *AutopilotState, err error) {
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		return false, false, nil, err
	}
	defer tx.Rollback()

	var (
		pausedInt          int64
		pausedAt           sql.NullInt64
		pausedBy           sql.NullString
		pausedReason       sql.NullString
		runnerPID          sql.NullInt64
		runnerLabel        sql.NullString
		runnerHost         sql.NullString
		binaryPath         sql.NullString
		binarySize         sql.NullInt64
		binaryMtime        sql.NullInt64
		binaryDev          sql.NullInt64
		binaryIno          sql.NullInt64
		passStartedAt      sql.NullInt64
		lastHeartbeat      sql.NullInt64
		lastPassFinishedAt sql.NullInt64
		lastPassDurationMS sql.NullInt64
		lastPassSummary    sql.NullString
		lastPassError      sql.NullString
	)
	if err := tx.QueryRow(`
		SELECT paused, paused_at, paused_by, paused_reason,
		       active_runner_pid, active_runner_label, active_runner_host,
		       active_binary_path, active_binary_size, active_binary_mtime,
		       active_binary_dev, active_binary_ino,
		       pass_started_at, last_heartbeat,
		       last_pass_finished_at, last_pass_duration_ms,
		       last_pass_summary, last_pass_error
		FROM autopilot_state WHERE id = 1
	`).Scan(
		&pausedInt, &pausedAt, &pausedBy, &pausedReason,
		&runnerPID, &runnerLabel, &runnerHost,
		&binaryPath, &binarySize, &binaryMtime, &binaryDev, &binaryIno,
		&passStartedAt, &lastHeartbeat,
		&lastPassFinishedAt, &lastPassDurationMS,
		&lastPassSummary, &lastPassError,
	); err != nil {
		return false, false, nil, err
	}

	buildExisting := func() *AutopilotState {
		s := &AutopilotState{
			Paused:            pausedInt != 0,
			PausedBy:          pausedBy.String,
			PausedReason:      pausedReason.String,
			ActiveRunnerPID:   int(runnerPID.Int64),
			ActiveRunnerLabel: runnerLabel.String,
			ActiveRunnerHost:  runnerHost.String,
			ActiveBinary: BinaryIdentity{
				Path:        binaryPath.String,
				Size:        binarySize.Int64,
				ModTimeUnix: binaryMtime.Int64,
				Dev:         uint64(binaryDev.Int64),
				Ino:         uint64(binaryIno.Int64),
			},
			LastPassDurationMS: lastPassDurationMS.Int64,
			LastPassSummary:    lastPassSummary.String,
			LastPassError:      lastPassError.String,
		}
		if pausedAt.Valid && pausedAt.Int64 != 0 {
			s.PausedAt = time.Unix(pausedAt.Int64, 0)
		}
		if passStartedAt.Valid && passStartedAt.Int64 != 0 {
			s.PassStartedAt = time.Unix(passStartedAt.Int64, 0)
		}
		if lastHeartbeat.Valid && lastHeartbeat.Int64 != 0 {
			s.LastHeartbeat = time.Unix(lastHeartbeat.Int64, 0)
		}
		if lastPassFinishedAt.Valid && lastPassFinishedAt.Int64 != 0 {
			s.LastPassFinishedAt = time.Unix(lastPassFinishedAt.Int64, 0)
		}
		return s
	}

	now := time.Now()
	if pausedInt != 0 && !opts.IgnorePaused {
		return false, true, buildExisting(), nil
	}
	if passStartedAt.Int64 != 0 {
		freshRef := lastHeartbeat.Int64
		if freshRef == 0 {
			freshRef = passStartedAt.Int64
		}
		if now.Sub(time.Unix(freshRef, 0)) < staleAfter {
			return false, false, buildExisting(), nil
		}
	}

	if _, err := tx.Exec(`
		UPDATE autopilot_state
		   SET active_runner_pid = ?, active_runner_label = ?, active_runner_host = ?,
		       active_binary_path = ?, active_binary_size = ?, active_binary_mtime = ?,
		       active_binary_dev = ?, active_binary_ino = ?,
		       pass_started_at = ?, last_heartbeat = ?,
		       last_pass_error = NULL
		 WHERE id = 1
	`, pid, label, host,
		emptyToNull(binary.Path), binary.Size, binary.ModTimeUnix, int64(binary.Dev), int64(binary.Ino),
		now.Unix(), now.Unix()); err != nil {
		return false, false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, nil, err
	}
	return true, false, nil, nil
}

// HeartbeatAutopilotPass updates the heartbeat timestamp if pid still owns the
// slot. Returns ErrAutopilotPassLost if another runner has taken over.
func HeartbeatAutopilotPass(database *sql.DB, pid int) error {
	return RetryOnDatabaseLocked(context.Background(), "heartbeat autopilot pass", func() error {
		res, err := database.Exec(`
			UPDATE autopilot_state
			   SET last_heartbeat = ?
			 WHERE id = 1 AND active_runner_pid = ?
		`, time.Now().Unix(), pid)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrAutopilotPassLost
		}
		return nil
	})
}

// ReleaseAutopilotPass clears the active runner slot if pid owns it, recording
// the pass summary and any error. Idempotent if the slot is no longer owned.
func ReleaseAutopilotPass(database *sql.DB, pid int, duration time.Duration, summary string, passErr error) error {
	errText := ""
	if passErr != nil {
		errText = passErr.Error()
	}
	return RetryOnDatabaseLocked(context.Background(), "release autopilot pass", func() error {
		_, err := database.Exec(`
			UPDATE autopilot_state
			   SET active_runner_pid = NULL, active_runner_label = NULL, active_runner_host = NULL,
			       active_binary_path = NULL, active_binary_size = NULL, active_binary_mtime = NULL,
			       active_binary_dev = NULL, active_binary_ino = NULL,
			       pass_started_at = NULL, last_heartbeat = NULL,
			       last_pass_finished_at = ?, last_pass_duration_ms = ?,
			       last_pass_summary = ?, last_pass_error = ?
			 WHERE id = 1 AND active_runner_pid = ?
		`, time.Now().Unix(), duration.Milliseconds(), summary, errText, pid)
		return err
	})
}

// ErrAutopilotPassLost is returned by HeartbeatAutopilotPass when another
// runner has claimed the pass slot (e.g. after a stale takeover).
var ErrAutopilotPassLost = errors.New("autopilot pass slot was taken by another runner")
