package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
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

	// Mark job as dead locally first (user intent is clear)
	if err := db.MarkDeadByID(database, job.ID); err != nil {
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
	// Get job to check if it's a queue-runner job
	job, err := db.GetJobByID(database, op.JobID)
	if err != nil {
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
			return Result{}, fmt.Errorf("connection error: %w", err)
		}
		// Session might already be gone - that's OK
		if opts.Verbose {
			fmt.Printf("Note: kill session returned: %v\n", err)
		}
	}

	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d killed", op.JobID),
	}, nil
}

// executeKillQueueRunnerJob kills a job running under the queue runner
func executeKillQueueRunnerJob(host string, job *db.Job, opts ExecuteOptions) (Result, error) {
	pidPattern := session.PidFilePattern(job.ID)

	killCmd := fmt.Sprintf(`
		pid=$(cat %s 2>/dev/null | head -1)
		if [ -n "$pid" ] && kill -0 $pid 2>/dev/null; then
			kill $pid 2>/dev/null && echo "killed" || echo "failed"
		else
			echo "not_running"
		fi
	`, pidPattern)

	stdout, stderr, err := ssh.RunWithTimeout(host, killCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		return Result{}, fmt.Errorf("kill process: %s", stderr)
	}

	result := stdout
	if result == "not_running" {
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d was not running", job.ID),
		}, nil
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d killed", job.ID),
	}, nil
}
