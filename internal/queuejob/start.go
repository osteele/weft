package queuejob

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

const (
	// sshTimeout is the timeout for SSH operations during job start.
	// This prevents the CLI from hanging indefinitely on offline hosts.
	sshTimeout = 30 * time.Second
)

// StartNow requests a queued job to start immediately via the sync path.
// Returns (true, nil) if the start was deferred because the host was unreachable.
func StartNow(database *sql.DB, job *db.Job) (bool, error) {
	if job == nil {
		return false, fmt.Errorf("job not found")
	}

	// Re-fetch job from DB to get current status (TUI may have stale data)
	freshJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return false, fmt.Errorf("fetch job: %w", err)
	}
	if freshJob == nil {
		return false, fmt.Errorf("job %d not found", job.ID)
	}
	if freshJob.EffectiveStatus() != db.StatusQueued {
		return false, fmt.Errorf("job %d is %s, not queued", job.ID, freshJob.EffectiveStatus())
	}

	oplog.LogJob(oplog.OpJobStart, job.ID, job.Host, oplog.WithDetail("starting job immediately"))

	cancelCmd := ops.NewCancelCommand(job.ID)
	if err := ops.AppendCommand(job.Host, cancelCmd, ops.AppendCommandOptions{Timeout: sshTimeout}); err != nil {
		var qaErr *ops.QueueAppendError
		if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("cancel failed, deferring start"))
			return markStartPending(database, freshJob, false)
		}
		if ssh.IsConnectionError(err.Error()) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("cancel failed, deferring start"))
			return markStartPending(database, freshJob, false)
		}
		return false, err
	}

	return startJobDirectly(database, freshJob, nil)
}

// startJobDirectly starts a job without interacting with the remote queue file.
// Used when job has a pending queue_job operation (not yet synced to remote).
func startJobDirectly(database *sql.DB, job *db.Job, entry *queuefile.Entry) (bool, error) {
	oplog.LogJob(oplog.OpJobStart, job.ID, job.Host, oplog.WithDetail("starting job directly (pending queue op)"))

	// Use the session name so sync knows this is a tmux-based job, not a queue runner job
	tmuxSession := session.TmuxSessionName(job.ID)
	if err := db.UpdateQueuedToRunningWithSession(database, job.ID, tmuxSession); err != nil {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("db update failed"))
		return false, fmt.Errorf("update queued job: %w", err)
	}

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil || updated == nil {
		return false, fmt.Errorf("refresh job after start: %w", err)
	}

	// Simple file paths (no timestamp in primary files)
	logFile := session.SimpleLogFile(job.ID)
	statusFile := session.SimpleStatusFile(job.ID)
	metadataFile := session.SimpleMetadataFile(job.ID)
	pidFile := session.SimplePidFile(job.ID)

	// Ensure log directory exists and archive any old files
	mkdirAndArchive := fmt.Sprintf("mkdir -p %s %s; %s", session.LogDir, artifacts.RemoteArtifactsDir, session.ArchiveCommand(job.ID))
	if _, stderr, err := ssh.RunWithTimeout(job.Host, mkdirAndArchive, sshTimeout); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("mkdir failed, deferring start"))
			return markStartPending(database, job, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("mkdir failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	// Save metadata (use EffectiveDescription to include AI-generated descriptions)
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.EffectiveDescription(), updated.StartTime)
	writeMetadata := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, stderr, err := ssh.RunWithTimeout(job.Host, writeMetadata, sshTimeout); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("metadata write failed, deferring start"))
			return markStartPending(database, job, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("metadata write failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	var envVars []string
	if entry != nil {
		envVars = entry.EnvVars
	} else {
		envVars = job.EnvVars
	}

	envVars = artifacts.MergeEnvVars(envVars, job.ID)
	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      job.ID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    envVars,
	})

	// Check if tmux session already exists (job may already be running but DB out of sync)
	checkCmd := fmt.Sprintf("tmux has-session -t '%s' 2>/dev/null && echo exists || echo missing", tmuxSession)
	stdout, _, err := ssh.RunWithTimeout(job.Host, checkCmd, sshTimeout)
	if err == nil && strings.TrimSpace(stdout) == "exists" {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithDetailf("session %s already exists", tmuxSession))
		return false, fmt.Errorf("job %d is already running (session %s exists)", job.ID, tmuxSession)
	}

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.RunWithTimeout(job.Host, tmuxCmd, sshTimeout); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("tmux create failed, deferring start"))
			return markStartPending(database, job, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("tmux create failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	oplog.LogJob(oplog.OpJobStarted, job.ID, job.Host, oplog.WithDetailf("started directly in session %s", tmuxSession))
	return false, nil
}

func markStartPending(database *sql.DB, job *db.Job, revertToQueue bool) (bool, error) {
	if revertToQueue {
		if err := db.UpdateJobRunningToQueued(database, job.ID); err != nil {
			return false, fmt.Errorf("mark job queued: %w", err)
		}
	}
	if err := db.SetPendingStatus(database, job.ID, db.StatusRunning); err != nil {
		return false, fmt.Errorf("set pending status: %w", err)
	}
	return true, nil
}

func isConnectionFailure(stderr string, err error) bool {
	if stderr != "" && ssh.IsConnectionError(stderr) {
		return true
	}
	if err != nil && ssh.IsConnectionError(err.Error()) {
		return true
	}
	return false
}

// isQueueRunnerRunningJob checks if the queue runner is currently running this job.
// Returns true if either:
// 1. The queue's .current file contains this job ID
// 2. There's a PID file for this job with a running process
func isQueueRunnerRunningJob(host string, jobID int64) bool {
	// Check if this job is the current job in the queue runner
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queuefile.DefaultQueueName)
	pidPattern := session.PidFilePattern(jobID)

	// Combined check: is this job current OR has a running process?
	checkCmd := fmt.Sprintf(`
		current=$(cat %s 2>/dev/null)
		if [ "$current" = "%d" ]; then
			echo "CURRENT"
			exit 0
		fi
		pid_file=$(ls %s 2>/dev/null | head -1)
		if [ -n "$pid_file" ]; then
			pid=$(cat "$pid_file" 2>/dev/null | head -1)
			if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
				echo "RUNNING"
				exit 0
			fi
		fi
		echo "NO"
	`, currentFile, jobID, pidPattern)

	stdout, _, err := ssh.RunWithTimeout(host, checkCmd, sshTimeout)
	if err != nil {
		return false // Can't determine, allow start to proceed
	}

	result := strings.TrimSpace(stdout)
	return result == "CURRENT" || result == "RUNNING"
}
