package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// KillJob sets the pending status to dead and attempts to reconcile immediately.
// Uses the three-way merge model: sets pending_status as user intent, then
// tries to apply to remote. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func KillJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	oplog.LogJob(oplog.OpJobKill, job.ID, job.Host, oplog.WithDetail("killing job"))

	// Set pending status to record user intent
	if err := db.SetPendingStatus(database, job.ID, db.StatusDead); err != nil {
		oplog.LogJob(oplog.OpJobKill, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("set pending status failed"))
		return Result{}, fmt.Errorf("set pending status: %w", err)
	}

	// Remove any pending run/restart deferred operations for this job
	// (they are incompatible with kill intent)
	db.DeletePendingOperationsForJob(database, job.ID, db.OpRunJob, db.OpRestartJob)

	// Reload job to get updated pending status
	job, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return Result{}, fmt.Errorf("reload job: %w", err)
	}

	// Try to reconcile immediately (apply kill to remote)
	reconcileOpts := ReconcileOptions{
		Timeout: opts.Timeout,
		Policy:  TerminalWins,
	}

	// Try to apply the pending status to remote
	result, err := applyPendingToRemote(database, job, db.StatusDead, reconcileOpts)
	if err != nil {
		// Connection error - leave pending status for later reconciliation
		if ssh.IsConnectionError(err.Error()) {
			oplog.LogJob(oplog.OpJobKill, job.ID, job.Host,
				oplog.WithDetail("kill deferred: connection error"))
			return Result{
				Success: true,
				JobID:   job.ID,
				Message: fmt.Sprintf("Job %d kill pending (host unreachable)", job.ID),
			}, nil
		}
		return Result{}, err
	}

	oplog.LogJob(oplog.OpJobKilled, job.ID, job.Host,
		oplog.WithDetailf("killed: %s -> %s", result.OldStatus, result.NewStatus))

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d killed", job.ID),
	}, nil
}

// executeKill executes a kill operation (called during queue drain)
func executeKill(database *sql.DB, host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	oplog.LogJob(oplog.OpDeferredExec, op.JobID, host, oplog.WithDetail("executing deferred kill"))

	// Get job to check if it's a queue-runner job
	job, err := db.GetJobByID(database, op.JobID)
	if err != nil {
		oplog.LogJob(oplog.OpJobKill, op.JobID, host, oplog.WithError(err), oplog.WithDetail("get job failed"))
		return Result{}, fmt.Errorf("get job: %w", err)
	}

	// Queue-runner jobs (no session name) are killed via PID, not tmux session.
	// Jobs with a SessionName have their own tmux session to kill.
	if job != nil && job.SessionName == "" {
		return executeKillQueueRunnerJob(host, job, opts)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.TmuxSessionName(op.JobID)
	if job != nil && job.SessionName != "" {
		tmuxSession = session.JobTmuxSession(op.JobID, job.SessionName)
	}

	killCmd := fmt.Sprintf("tmux kill-session -t '%s'", tmuxSession)
	_, stderr, err := ssh.RunWithTimeout(host, killCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			oplog.LogJob(oplog.OpDeferred, op.JobID, host, oplog.WithDetail("kill failed, connection error"))
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		// Session might already be gone - that's OK, continue silently
	}

	oplog.LogJob(oplog.OpJobKilled, op.JobID, host, oplog.WithDetailf("killed session %s", tmuxSession))
	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d killed", op.JobID),
	}, nil
}

// CancelQueuedJob cancels a queued job so it won't run when the queue drains to it.
// Uses the three-way merge model: sets pending_status to dead, then tries to
// remove from remote queue. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func CancelQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	if job.Status != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d is not queued (status: %s)", job.ID, job.Status)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceling queued job"))

	// Set pending status to record user intent
	if err := db.SetPendingStatus(database, job.ID, db.StatusDead); err != nil {
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("set pending status failed"))
		return Result{}, fmt.Errorf("set pending status: %w", err)
	}

	// Remove any pending queue/start deferred operations for this job
	// (they are incompatible with cancel intent)
	db.DeletePendingOperationsForJob(database, job.ID, db.OpQueueJob, db.OpStartQueuedJob)

	// Try to remove from remote queue file immediately
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	err := removeFromQueueFile(job.Host, queueName, job.ID, opts.Timeout)
	if err != nil {
		// Connection error - leave pending status for later reconciliation
		if ssh.IsConnectionError(err.Error()) {
			oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host,
				oplog.WithDetail("cancel deferred: connection error"))
			return Result{
				Success: true,
				JobID:   job.ID,
				Message: fmt.Sprintf("Job %d cancel pending (host unreachable)", job.ID),
			}, nil
		}
		return Result{}, err
	}

	// Success - update status and clear pending
	if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDead); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceled"))

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d canceled", job.ID),
	}, nil
}

// executeKillQueueRunnerJob kills a job running under the queue runner
func executeKillQueueRunnerJob(host string, job *db.Job, opts ExecuteOptions) (Result, error) {
	oplog.LogJob(oplog.OpJobKill, job.ID, host, oplog.WithDetail("killing queue runner job"))

	pidPattern := session.PidFilePattern(job.ID)

	// Kill the entire process tree, not just the main process
	// This handles jobs that spawn worker processes (e.g., Python multiprocessing)
	killCmd := fmt.Sprintf(`
		pid=$(cat %s 2>/dev/null | head -1)
		if [ -n "$pid" ] && kill -0 $pid 2>/dev/null; then
			# First, recursively kill all children
			pkill -TERM -P $pid 2>/dev/null
			# Then kill the main process
			kill -TERM $pid 2>/dev/null
			# Give processes a moment to terminate gracefully
			sleep 0.5
			# Force kill any remaining processes
			pkill -KILL -P $pid 2>/dev/null
			kill -KILL $pid 2>/dev/null
			echo "killed"
		else
			echo "not_running"
		fi
	`, pidPattern)

	stdout, stderr, err := ssh.RunWithTimeout(host, killCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			oplog.LogJob(oplog.OpDeferred, job.ID, host, oplog.WithDetail("kill failed, connection error"))
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		oplog.LogJob(oplog.OpJobKill, job.ID, host, oplog.WithErrorStr(stderr), oplog.WithDetail("kill process failed"))
		return Result{}, fmt.Errorf("kill process: %s", stderr)
	}

	result := stdout
	if result == "not_running" {
		oplog.LogJob(oplog.OpJobKilled, job.ID, host, oplog.WithDetail("job was not running"))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d was not running", job.ID),
		}, nil
	}

	oplog.LogJob(oplog.OpJobKilled, job.ID, host, oplog.WithDetail("killed process tree"))
	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d killed", job.ID),
	}, nil
}
