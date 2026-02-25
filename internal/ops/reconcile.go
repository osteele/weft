// Package ops provides unified job operations shared between CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hooks"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
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

	// Log reconcile inputs for debugging
	pendingStr := "nil"
	if local != nil {
		pendingStr = *local
	}
	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("reconcile-input: base=%s pending=%s remote=%s", base, pendingStr, remote))

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

	// Case 5: Job is queued locally but does not exist remotely
	if job.Status == db.StatusQueued && remoteStatus == "" {
		if err := applyQueueToRemote(database, job, opts.Timeout); err != nil {
			return nil, err
		}
		// Set queued_at when job is successfully added to the remote queue
		if err := db.SetQueuedAtNow(database, job.ID); err != nil {
			return nil, err
		}
		return &ReconcileResult{
			Action:    "update_remote",
			OldStatus: job.Status,
			NewStatus: db.StatusQueued,
		}, nil
	}

	// Case 6: Conflict - local wants one thing, remote became something else
	return resolveConflict(database, job, base, *local, remote, opts)
}

// resolveConflict handles the case where both local and remote have diverged from base.
func resolveConflict(database *sql.DB, job *db.Job, base, local, remote string, opts ReconcileOptions) (*ReconcileResult, error) {
	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("conflict: base=%s local=%s remote=%s", base, local, remote))

	if db.IsTerminalStatus(remote) {
		// If local intent is to requeue, the terminal state is from the previous run.
		// Apply the requeue rather than accepting stale completion.
		if local == db.StatusQueued {
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
			result.Resolution = fmt.Sprintf("requeued over stale terminal state %s", remote)
			return result, nil
		}

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
}

// applyPendingToRemote attempts to make the remote state match the pending status.
func applyPendingToRemote(database *sql.DB, job *db.Job, targetStatus string, opts ReconcileOptions) (*ReconcileResult, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	var err error
	resolvedStatus := targetStatus
	switch targetStatus {
	case db.StatusKilled:
		err = applyKillToRemote(job, timeout)
	case db.StatusCanceled:
		err = applyCancelToRemote(job, timeout)
	case db.StatusDead:
		// Legacy: treat pending dead as a kill request.
		err = applyKillToRemote(job, timeout)
		resolvedStatus = db.StatusKilled
	case db.StatusRunning:
		if job.UsesSlurm() {
			err = applyStartToRemote(database, job, timeout)
			resolvedStatus = db.StatusQueued
		} else if job.Status == db.StatusPaused {
			err = applyResumeToRemote(job, timeout)
		} else {
			err = applyStartToRemote(database, job, timeout)
		}
	case db.StatusQueued:
		err = applyQueueToRemote(database, job, timeout)
	case db.StatusPaused:
		err = applyPauseToRemote(job, timeout)
	case db.StatusDraft:
		err = applyDraftToRemote(job, timeout)
	default:
		return nil, fmt.Errorf("unsupported pending status: %s", targetStatus)
	}

	if err != nil {
		return nil, err
	}

	// Success - update status and clear pending
	if err := db.ClearPendingAndUpdateStatus(database, job.ID, resolvedStatus); err != nil {
		return nil, err
	}

	// Set queued_at when job is successfully added to the remote queue
	if resolvedStatus == db.StatusQueued {
		if err := db.SetQueuedAtNow(database, job.ID); err != nil {
			return nil, err
		}
	}

	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
		oplog.WithDetailf("applied pending: %s -> %s", job.Status, targetStatus))

	return &ReconcileResult{
		Action:    "update_remote",
		OldStatus: job.Status,
		NewStatus: resolvedStatus,
	}, nil
}

// applyKillToRemote kills the job on the remote host.
func applyKillToRemote(job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return cancelSlurmJob(job, timeout)
	}
	// Queue-runner jobs (no session name) are killed via PID
	if job.UsesQueueRunner() {
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

func applyPauseToRemote(job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return fmt.Errorf("pause not supported for slurm jobs")
	}
	// Create .paused marker file so queue runner knows this is intentional
	pausedFile := session.SimplePausedFile(job.ID)
	touchCmd := fmt.Sprintf("touch %s", pausedFile)
	if _, stderr, err := ssh.RunWithTimeout(job.Host, touchCmd, timeout); err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		return fmt.Errorf("create paused marker: %w", err)
	}
	return signalJobProcess(job, "STOP", timeout)
}

func applyResumeToRemote(job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return fmt.Errorf("resume not supported for slurm jobs")
	}
	if err := signalJobProcess(job, "CONT", timeout); err != nil {
		return err
	}
	// Remove .paused marker file
	pausedFile := session.SimplePausedFile(job.ID)
	rmCmd := fmt.Sprintf("rm -f %s", pausedFile)
	_, _, _ = ssh.RunWithTimeout(job.Host, rmCmd, timeout) // Best effort
	return nil
}

func signalJobProcess(job *db.Job, signal string, timeout time.Duration) error {
	pgidFile := session.SimplePgidFile(job.ID)
	pidPattern := session.PidFilePattern(job.ID)
	// Try PGID file first (for queue-runner jobs that use setsid).
	// Then fall back to PID file, but signal the entire process group to reach child processes.
	// Using ps to get the PGID ensures we signal all processes spawned by the job.
	cmd := fmt.Sprintf(`pgid=$(cat %s 2>/dev/null | head -1); `+
		`if [ -n "$pgid" ] && ps -p $pgid > /dev/null 2>&1; then `+
		`kill -%s -$pgid; exit 0; fi; `+
		`pid=$(cat %s 2>/dev/null | head -1); `+
		`if [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1; then `+
		`pgid=$(ps -o pgid= -p $pid 2>/dev/null | tr -d " "); `+
		`if [ -n "$pgid" ]; then kill -%s -$pgid; else kill -%s $pid; fi; exit 0; fi; `+
		`echo "no running process found" >&2; exit 3`,
		pgidFile, signal, pidPattern, signal, signal)
	_, stderr, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		if strings.TrimSpace(stderr) != "" {
			return fmt.Errorf("%s signal failed: %s", signal, strings.TrimSpace(stderr))
		}
		return fmt.Errorf("%s signal failed", signal)
	}
	return nil
}

// applyCancelToRemote removes a queued job from the remote queue.
// If the job is already running (queue runner started it before cancel),
// this also kills the running process.
func applyCancelToRemote(job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return cancelSlurmJob(job, timeout)
	}

	// Remove from queue file (in case it's still queued)
	if err := removeFromQueueFile(job.Host, job.ID, timeout); err != nil {
		// Only fail on connection errors, non-connection errors are OK
		if ssh.IsConnectionError(err.Error()) {
			return err
		}
	}

	// Also kill the process if it's running (queue runner may have started it)
	return killQueueRunnerJob(job, timeout)
}

// killQueueRunnerJob kills a queue-runner managed job via its PID and process group.
func killQueueRunnerJob(job *db.Job, timeout time.Duration) error {
	pidFile := session.JobPidFile(job.ID, job.StartTime)
	pgidFile := session.SimplePgidFile(job.ID)

	// Kill both the wrapper process (pid file) and the command's process group (pgid file).
	// The pgid file contains the setsid process PID, which is also the process group leader.
	// Using kill -TERM -pgid sends SIGTERM to all processes in the group.
	// Note: pidFile contains ~ which must NOT be single-quoted (prevents expansion)
	killCmd := fmt.Sprintf(`
		# Kill wrapper process
		if [ -f %s ]; then kill $(cat %s) 2>/dev/null || true; fi
		# Kill process group (setsid tree) - the minus sign targets the whole group
		if [ -f %s ]; then
			pgid=$(cat %s 2>/dev/null)
			if [ -n "$pgid" ]; then
				kill -TERM -$pgid 2>/dev/null || true
				sleep 0.5
				kill -KILL -$pgid 2>/dev/null || true
			fi
			rm -f %s
		fi
	`, pidFile, pidFile, pgidFile, pgidFile, pgidFile)

	_, stderr, err := ssh.RunWithTimeout(job.Host, killCmd, timeout)
	if err != nil && ssh.IsConnectionError(stderr) {
		return fmt.Errorf("connection error: %s", stderr)
	}
	return nil
}

// removeFromQueueFile removes a job from the remote queue by issuing a cancel command.
func removeFromQueueFile(host string, jobID int64, timeout time.Duration) error {
	cancelCmd := NewCancelCommand(jobID)
	opts := AppendCommandOptions{Timeout: timeout}
	if err := AppendCommand(host, cancelCmd, opts); err != nil {
		var qaErr *QueueAppendError
		if e, ok := err.(*QueueAppendError); ok {
			qaErr = e
			if qaErr.IsConnectionError() {
				return fmt.Errorf("connection error: %s", qaErr.Stderr)
			}
		} else if ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("connection error: %s", err.Error())
		}
		// Non-connection errors are OK (job might not be in queue)
	}
	return nil
}

// applyDraftToRemote ensures no remote execution state exists for a draft job.
// This removes the job from any queue and kills any running process.
func applyDraftToRemote(job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return cancelSlurmJob(job, timeout)
	}

	// Remove from queue if present
	if err := removeFromQueueFile(job.Host, job.ID, timeout); err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return err
		}
		// Non-connection errors are OK (not in queue)
	}

	// Kill if running
	if err := applyKillToRemote(job, timeout); err != nil {
		return err
	}

	return nil
}

// applyQueueToRemote adds a job to the remote queue file.
func applyQueueToRemote(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		_, _, err := submitSlurmJob(database, job, timeout)
		return err
	}
	return AppendJobToQueue(job, timeout)
}

// applyStartToRemote starts a queued or draft job on the remote host.
func applyStartToRemote(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if job.UsesSlurm() {
		return applyQueueToRemote(database, job, timeout)
	}
	// For draft jobs, first add to queue
	if job.Status == db.StatusDraft {
		if err := applyQueueToRemote(database, job, timeout); err != nil {
			return fmt.Errorf("queue draft job: %w", err)
		}
		// Set queued_at since job is now in queue
		if err := db.SetQueuedAtNow(database, job.ID); err != nil {
			return fmt.Errorf("set queued_at: %w", err)
		}
		// Update job status to queued for startQueuedJobNow.
		// Don't clear pending_status yet - startQueuedJobNow will clear it on success,
		// and we need it preserved for retry if startQueuedJobNow fails.
		if err := db.UpdateStatusAndLastSynced(database, job.ID, db.StatusQueued); err != nil {
			return fmt.Errorf("update status to queued: %w", err)
		}
	} else if job.Status != db.StatusQueued {
		return fmt.Errorf("can only start queued or draft jobs, got status %s", job.Status)
	}

	// Signal the queue runner to start this job immediately
	return startQueuedJobNow(database, job, timeout)
}

// ProbeRemoteStatus determines the current status of a job on the remote host.
// Returns the detected status or an error if probing failed.
func ProbeRemoteStatus(job *db.Job, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	if job.UsesSlurm() {
		info, err := probeSlurmInfo(job, timeout)
		if err != nil {
			return "", err
		}
		if info == nil {
			if job.RemoteID == "" {
				return "", nil
			}
			return job.Status, nil
		}
		return slurmStateToLocal(job, info), nil
	}

	// Queue-runner jobs use pattern-based file lookup
	if job.UsesQueueRunner() {
		return probeQueueRunnerJobStatus(job, timeout)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return "", err
	}

	if exists {
		paused := queueRemoteClient.ProcessPaused(job.Host, job.ID, timeout)
		if paused.IsSome() && paused.Unwrap() {
			return db.StatusPaused, nil
		}
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

	// No session, no status file.
	// For running/paused jobs, this means the job vanished — report as failed.
	// For other states, this could be a race during startup.
	if job.Status == db.StatusRunning || job.Status == db.StatusPaused {
		return db.StatusFailed, nil
	}
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

	paused := queueRemoteClient.ProcessPaused(job.Host, job.ID, timeout)
	if paused.IsSome() && paused.Unwrap() {
		return db.StatusPaused, nil
	}

	// Check if job is current in queue runner
	current := queueRemoteClient.CurrentJob(job.Host, job.ID, timeout)
	if current.IsSome() && current.Unwrap() {
		return db.StatusRunning, nil
	}

	// Check if job is still in queue file
	queued := queueRemoteClient.InQueue(job.Host, job.ID, timeout)
	if queued.IsSome() && queued.Unwrap() {
		return db.StatusQueued, nil
	}

	// Check if process is running via PID
	running := queueRemoteClient.ProcessRunning(job.Host, job.ID, timeout)
	if running.IsSome() && running.Unwrap() {
		return db.StatusRunning, nil
	}

	// Job is locally queued but not found in remote queue, not running, no status file
	// This means it's orphaned (was removed from queue or never made it there)
	if job.Status == db.StatusQueued && queued.IsSome() && !queued.Unwrap() {
		// If job has pending cancel/kill/draft status, return that instead of dead
		// This allows clean cancellation or draft transition when the queue runner isn't running
		if job.PendingStatus != nil {
			switch *job.PendingStatus {
			case db.StatusCanceled:
				return db.StatusCanceled, nil
			case db.StatusKilled, db.StatusDead:
				return db.StatusKilled, nil
			case db.StatusDraft:
				return db.StatusDraft, nil
			case db.StatusRunning:
				return db.StatusQueued, nil
			}
		}
		return db.StatusDead, nil
	}

	// Unable to determine - return current status
	return job.Status, nil
}

// SyncAndReconcile probes remote state and reconciles with local state.
// This is the unified entry point that replaces separate sync and deferred ops execution.
func SyncAndReconcile(database *sql.DB, job *db.Job, opts ReconcileOptions) (*ReconcileResult, error) {
	oldStatus := job.Status
	// Probe remote state
	remoteStatus := ""
	var err error
	if job.UsesSlurm() {
		info, probeErr := probeSlurmInfo(job, opts.Timeout)
		if probeErr != nil {
			err = probeErr
		} else if info != nil {
			if err := db.SetJobRemoteState(database, job.ID, info.State, info.FailureReason); err != nil {
				return nil, err
			}
			remoteStatus = slurmStateToLocal(job, info)
		} else {
			if job.RemoteID == "" {
				remoteStatus = ""
			} else {
				remoteStatus = job.Status
			}
		}
	} else {
		remoteStatus, err = ProbeRemoteStatus(job, opts.Timeout)
	}
	if err != nil {
		oplog.LogJob(oplog.OpJobProbe, job.ID, job.Host,
			oplog.WithDetailf("error current=%s", job.Status),
			oplog.WithError(err))
		return nil, fmt.Errorf("probe remote: %w", err)
	}

	// Log probe result
	oplog.LogJob(oplog.OpJobProbe, job.ID, job.Host,
		oplog.WithDetailf("result=%s current=%s", remoteStatus, job.Status))

	// Reconcile the three states
	result, reconcileErr := Reconcile(database, job, remoteStatus, opts)
	if reconcileErr != nil {
		return nil, reconcileErr
	}

	// Fire hook if job transitioned from non-terminal to terminal
	if result != nil && result.NewStatus != "" && hooks.ShouldFireHook(oldStatus, result.NewStatus) {
		updated, fetchErr := db.GetJobByID(database, job.ID)
		if fetchErr == nil && updated != nil {
			hooks.RunOnJobComplete(updated)
		}
	}

	return result, nil
}
