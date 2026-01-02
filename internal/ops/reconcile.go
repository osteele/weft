// Package ops provides unified job operations shared between CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// ConflictPolicy determines how to handle conflicts when both local and remote
// have changed from the base state.
type ConflictPolicy int

const (
	// TerminalWins accepts terminal remote states (completed/dead/failed),
	// otherwise keeps trying to reach local (pending) target.
	TerminalWins ConflictPolicy = iota

	// LocalWins always tries to apply the pending status to remote.
	LocalWins

	// RemoteWins always accepts the remote state and clears pending.
	RemoteWins
)

// ReconcileResult describes what happened during reconciliation.
type ReconcileResult struct {
	Action     string // "none", "update_db", "update_remote", "conflict_resolved"
	OldStatus  string
	NewStatus  string
	Conflict   bool
	Resolution string
	Error      error
}

// ReconcileOptions configures reconciliation behavior.
type ReconcileOptions struct {
	Timeout time.Duration
	Policy  ConflictPolicy
}

// Reconcile performs three-way merge between base (last synced), local (pending),
// and remote (probed) states. It determines what action to take based on which
// states have diverged.
//
// The three states are:
//   - Base: job.LastSyncedStatus - what remote was at last successful sync
//   - Local: job.PendingStatus - what user wants (nil = no pending change)
//   - Remote: remoteStatus - what remote actually is now
//
// Returns a ReconcileResult describing what action was taken.
func Reconcile(database *sql.DB, job *db.Job, remoteStatus string, opts ReconcileOptions) (*ReconcileResult, error) {
	base := job.LastSyncedStatus
	local := job.PendingStatus
	remote := remoteStatus

	// If base is empty (legacy job or first sync), treat current status as base
	if base == "" {
		base = job.Status
	}

	// Case 1: No local intent and remote unchanged from base
	if local == nil && base == remote {
		return &ReconcileResult{Action: "none"}, nil
	}

	// Case 2: Remote changed, no local intent (pure sync)
	if local == nil && base != remote {
		if err := db.UpdateStatusAndLastSynced(database, job.ID, remote); err != nil {
			return nil, err
		}
		oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
			oplog.WithDetailf("sync: %s -> %s", base, remote))
		return &ReconcileResult{
			Action:    "update_db",
			OldStatus: base,
			NewStatus: remote,
		}, nil
	}

	// Case 3: Local intent, remote unchanged from base (pure ops)
	if local != nil && base == remote {
		result, err := applyPendingToRemote(database, job, *local, opts)
		if err != nil {
			// If connection error, leave pending in place for retry
			if ssh.IsConnectionError(err.Error()) {
				return &ReconcileResult{
					Action: "none",
					Error:  err,
				}, nil
			}
			return nil, err
		}
		return result, nil
	}

	// Case 4: Both changed to same state (convergence)
	if local != nil && *local == remote {
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, remote); err != nil {
			return nil, err
		}
		oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
			oplog.WithDetailf("converged: pending %s matches remote", remote))
		return &ReconcileResult{
			Action:    "update_db",
			OldStatus: base,
			NewStatus: remote,
		}, nil
	}

	// Case 5: Conflict - local wants one thing, remote became something else
	return resolveConflict(database, job, base, *local, remote, opts)
}

// resolveConflict handles the case where both local and remote have diverged from base.
func resolveConflict(database *sql.DB, job *db.Job, base, local, remote string, opts ReconcileOptions) (*ReconcileResult, error) {
	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("conflict: base=%s local=%s remote=%s", base, local, remote))

	switch opts.Policy {
	case TerminalWins:
		if db.IsTerminalStatus(remote) {
			// Remote reached a terminal state - accept it
			if err := db.ClearPendingAndUpdateStatus(database, job.ID, remote); err != nil {
				return nil, err
			}
			return &ReconcileResult{
				Action:     "conflict_resolved",
				OldStatus:  base,
				NewStatus:  remote,
				Conflict:   true,
				Resolution: fmt.Sprintf("accepted terminal remote state %s (pending was %s)", remote, local),
			}, nil
		}
		// Remote not terminal - keep trying for local intent
		fallthrough

	case LocalWins:
		result, err := applyPendingToRemote(database, job, local, opts)
		if err != nil {
			if ssh.IsConnectionError(err.Error()) {
				return &ReconcileResult{
					Action:     "none",
					Conflict:   true,
					Resolution: "deferred: connection error",
					Error:      err,
				}, nil
			}
			return nil, err
		}
		result.Conflict = true
		result.Resolution = fmt.Sprintf("applied local intent %s (remote was %s)", local, remote)
		return result, nil

	case RemoteWins:
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, remote); err != nil {
			return nil, err
		}
		return &ReconcileResult{
			Action:     "conflict_resolved",
			OldStatus:  base,
			NewStatus:  remote,
			Conflict:   true,
			Resolution: fmt.Sprintf("accepted remote state %s (pending was %s)", remote, local),
		}, nil
	}

	return nil, fmt.Errorf("unknown conflict policy")
}

// applyPendingToRemote attempts to make the remote state match the pending status.
func applyPendingToRemote(database *sql.DB, job *db.Job, targetStatus string, opts ReconcileOptions) (*ReconcileResult, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	var err error
	switch targetStatus {
	case db.StatusDead:
		err = applyKillToRemote(job, timeout)
	case db.StatusRunning:
		err = applyStartToRemote(database, job, timeout)
	default:
		return nil, fmt.Errorf("unsupported pending status: %s", targetStatus)
	}

	if err != nil {
		return nil, err
	}

	// Success - update status and clear pending
	if err := db.ClearPendingAndUpdateStatus(database, job.ID, targetStatus); err != nil {
		return nil, err
	}

	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("applied pending: %s -> %s", job.Status, targetStatus))

	return &ReconcileResult{
		Action:    "update_remote",
		OldStatus: job.Status,
		NewStatus: targetStatus,
	}, nil
}

// applyKillToRemote kills the job on the remote host.
func applyKillToRemote(job *db.Job, timeout time.Duration) error {
	// Queue-runner jobs (no session name) are killed via PID
	if job.SessionName == "" {
		return killQueueRunnerJob(job, timeout)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	killCmd := fmt.Sprintf("tmux kill-session -t '%s'", tmuxSession)
	_, stderr, err := ssh.RunWithTimeout(job.Host, killCmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("connection error: %s", stderr)
		}
		// Session might already be gone - that's OK
	}
	return nil
}

// killQueueRunnerJob kills a queue-runner managed job via its PID.
func killQueueRunnerJob(job *db.Job, timeout time.Duration) error {
	pidFile := session.JobPidFile(job.ID, job.StartTime)
	killCmd := fmt.Sprintf("if [ -f '%s' ]; then kill $(cat '%s') 2>/dev/null || true; fi", pidFile, pidFile)
	_, stderr, err := ssh.RunWithTimeout(job.Host, killCmd, timeout)
	if err != nil && ssh.IsConnectionError(stderr) {
		return fmt.Errorf("connection error: %s", stderr)
	}
	return nil
}

// removeFromQueueFile removes a job from the remote queue file.
func removeFromQueueFile(host, queueName string, jobID int64, timeout time.Duration) error {
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", jobID, queueFile)
	_, stderr, err := ssh.RunWithTimeout(host, removeCmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("connection error: %s", stderr)
		}
		// Non-connection errors are OK (file might not exist, etc.)
	}
	return nil
}

// applyStartToRemote starts a queued job on the remote host.
func applyStartToRemote(database *sql.DB, job *db.Job, timeout time.Duration) error {
	// This would trigger the queue runner to start the job
	// For now, we only support this for queued jobs
	if job.Status != db.StatusQueued {
		return fmt.Errorf("can only start queued jobs, got status %s", job.Status)
	}

	// Signal the queue runner to start this job immediately
	// This is a placeholder - the actual implementation would depend on
	// how the queue runner accepts immediate start signals
	return fmt.Errorf("immediate start not yet implemented")
}

// ProbeRemoteStatus determines the current status of a job on the remote host.
// Returns the detected status or an error if probing failed.
func ProbeRemoteStatus(job *db.Job, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	// Queue-runner jobs use pattern-based file lookup
	if job.SessionName == "" && job.QueueName != "" {
		return probeQueueRunnerJobStatus(job, timeout)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return "", err
	}

	if exists {
		return db.StatusRunning, nil
	}

	// Session doesn't exist - check for status file
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	result, err := ReadStatusFile(job.Host, statusFile, timeout)
	if err != nil {
		return "", err
	}

	if result != nil {
		return db.StatusCompleted, nil
	}

	// No session, no status file - could be dead or uncertain
	// Return the current status as "unchanged" since we can't confirm
	return job.Status, nil
}

// probeQueueRunnerJobStatus checks status for queue-runner managed jobs.
func probeQueueRunnerJobStatus(job *db.Job, timeout time.Duration) (string, error) {
	// Check if job is completed (has status file)
	exitCode, _, found := queueRemoteClient.StatusFile(job.Host, job.ID, timeout)
	if found.IsSome() && found.Unwrap() {
		_ = exitCode // Exit code available if needed
		return db.StatusCompleted, nil
	}

	// Check if job is current in queue runner
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}
	current := queueRemoteClient.CurrentJob(job.Host, queueName, job.ID, timeout)
	if current.IsSome() && current.Unwrap() {
		return db.StatusRunning, nil
	}

	// Check if job is still in queue file
	queued := queueRemoteClient.InQueue(job.Host, queueName, job.ID, timeout)
	if queued.IsSome() && queued.Unwrap() {
		return db.StatusQueued, nil
	}

	// Check if process is running via PID
	running := queueRemoteClient.ProcessRunning(job.Host, job.ID, timeout)
	if running.IsSome() && running.Unwrap() {
		return db.StatusRunning, nil
	}

	// Unable to determine - return current status
	return job.Status, nil
}

// SyncAndReconcile probes remote state and reconciles with local state.
// This is the unified entry point that replaces separate sync and deferred ops execution.
func SyncAndReconcile(database *sql.DB, job *db.Job, opts ReconcileOptions) (*ReconcileResult, error) {
	// Probe remote state
	remoteStatus, err := ProbeRemoteStatus(job, opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("probe remote: %w", err)
	}

	// Reconcile the three states
	return Reconcile(database, job, remoteStatus, opts)
}
