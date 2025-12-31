package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// KillJob queues a kill operation and attempts to execute it.
// The job is marked as dead in the database regardless of whether the
// remote kill succeeds (for consistency with user intent).
// Any pending run/restart operations for this job are removed since
// they are now incompatible with the kill intent.
func KillJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	oplog.LogJob(oplog.OpJobKill, job.ID, job.Host, oplog.WithDetail("killing job"))

	// Mark job as dead locally first (user intent is clear)
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		oplog.LogJob(oplog.OpJobKill, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("mark dead failed"))
		return Result{}, fmt.Errorf("mark job dead: %w", err)
	}

	// Remove any pending run/restart operations for this job
	// (they are incompatible with kill intent)
	db.DeletePendingOperationsForJob(database, job.ID, db.OpRunJob, db.OpRestartJob)

	// Queue the kill operation
	return QueueAndExecute(database, job.Host, db.OpKillJob, job.ID, "", "", opts)
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

	// Queue-runner jobs don't have individual tmux sessions
	if job != nil && job.SessionName == "" {
		return executeKillQueueRunnerJob(host, job, opts)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.TmuxSessionName(op.JobID)
	if job != nil && job.SessionName != "" {
		tmuxSession = session.JobTmuxSession(op.JobID, job.SessionName)
	}

	err = ssh.TmuxKillSession(host, tmuxSession)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			oplog.LogJob(oplog.OpDeferred, op.JobID, host, oplog.WithDetail("kill failed, connection error"))
			return Result{}, fmt.Errorf("connection error: %w", err)
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
// The job is marked as dead and removed from the remote queue file.
// Any pending queue_job or start_queued_job operations for this job are removed.
func CancelQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	if job.Status != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d is not queued (status: %s)", job.ID, job.Status)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceling queued job"))

	// Mark job as dead locally first (user intent is clear)
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("mark dead failed"))
		return Result{}, fmt.Errorf("mark job dead: %w", err)
	}

	// Remove any pending queue/start operations for this job
	// (they are incompatible with cancel intent)
	db.DeletePendingOperationsForJob(database, job.ID, db.OpQueueJob, db.OpStartQueuedJob)

	// Queue the remove operation to clean up remote queue file
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}
	return QueueAndExecute(database, job.Host, db.OpRemoveQueued, job.ID, queueName, "", opts)
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
