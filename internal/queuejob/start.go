package queuejob

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// StartNow removes a queued job from the remote queue and starts it immediately.
// Returns (true, nil) if the start was deferred due to a connection failure.
func StartNow(database *sql.DB, job *db.Job) (bool, error) {
	if job == nil {
		return false, fmt.Errorf("job not found")
	}

	oplog.LogJob(oplog.OpJobStart, job.ID, job.Host, oplog.WithDetail("starting queued job directly"))

	// Re-fetch job from DB to get current status (TUI may have stale data)
	freshJob, err := db.GetJobByID(database, job.ID)
	if err != nil {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("fetch job failed"))
		return false, fmt.Errorf("fetch job: %w", err)
	}
	if freshJob == nil {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr("job not found in database"))
		return false, fmt.Errorf("job %d not found", job.ID)
	}
	if freshJob.Status != db.StatusQueued {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithDetailf("job already %s", freshJob.Status))
		return false, fmt.Errorf("job %d is already %s", job.ID, freshJob.Status)
	}
	// Use fresh job data from here on
	job = freshJob

	queueName := job.QueueName
	if queueName == "" {
		queueName = queuefile.DefaultQueueName
	}

	entry, err := queuefile.FetchEntry(job.Host, queueName, job.ID)
	entryWasInQueue := err == nil
	if err != nil {
		if queuefile.IsConnectionError(err) {
			return markStartPending(database, job, queueName, false)
		}
		// Job not found in remote queue - check if queue runner is already running it
		if isQueueRunnerRunningJob(job.Host, queueName, job.ID) {
			oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithDetail("queue runner is already running this job"))
			return false, fmt.Errorf("job %d is already being run by the queue runner", job.ID)
		}
		// Job not in queue and not being run by queue runner - use database info to start directly
		entry = &queuefile.Entry{
			JobID:       job.ID,
			WorkingDir:  job.WorkingDir,
			Command:     job.Command,
			Description: job.Description,
		}
	}

	// Only try to remove from queue if it was actually there
	if entryWasInQueue {
		if err := queuefile.RemoveEntry(job.Host, queueName, job.ID); err != nil {
			if queuefile.IsConnectionError(err) {
				return markStartPending(database, job, queueName, false)
			}
			return false, err
		}
	}

	// Use the session name so sync knows this is a tmux-based job, not a queue runner job
	tmuxSession := session.TmuxSessionName(job.ID)
	if err := db.UpdateQueuedToRunningWithSession(database, job.ID, tmuxSession); err != nil {
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
	if _, stderr, err := ssh.Run(job.Host, mkdirAndArchive); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("mkdir failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("mkdir failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	// Save metadata (use EffectiveDescription to include AI-generated descriptions)
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.EffectiveDescription(), updated.StartTime)
	writeMetadata := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, stderr, err := ssh.Run(job.Host, writeMetadata); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("metadata write failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("metadata write failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	envVars := artifacts.MergeEnvVars(entry.EnvVars, job.ID)
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
	stdout, _, err := ssh.Run(job.Host, checkCmd)
	if err == nil && strings.TrimSpace(stdout) == "exists" {
		// Session already exists - job is running, just DB is out of sync
		return false, fmt.Errorf("job %d is already running (session %s exists)", job.ID, tmuxSession)
	}

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("tmux create failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("tmux create failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	oplog.LogJob(oplog.OpJobStarted, job.ID, job.Host, oplog.WithDetailf("started in session %s", tmuxSession))
	return false, nil
}

// startJobDirectly starts a job without interacting with the remote queue file.
// Used when job has a pending queue_job operation (not yet synced to remote).
func startJobDirectly(database *sql.DB, job *db.Job, queueName string, entry *queuefile.Entry) (bool, error) {
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
	if _, stderr, err := ssh.Run(job.Host, mkdirAndArchive); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("mkdir failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("mkdir failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	// Save metadata (use EffectiveDescription to include AI-generated descriptions)
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.EffectiveDescription(), updated.StartTime)
	writeMetadata := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, stderr, err := ssh.Run(job.Host, writeMetadata); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("metadata write failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("metadata write failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	var envVars []string
	if entry != nil {
		envVars = entry.EnvVars
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
	stdout, _, err := ssh.Run(job.Host, checkCmd)
	if err == nil && strings.TrimSpace(stdout) == "exists" {
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithDetailf("session %s already exists", tmuxSession))
		return false, fmt.Errorf("job %d is already running (session %s exists)", job.ID, tmuxSession)
	}

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			oplog.LogJob(oplog.OpDeferred, job.ID, job.Host, oplog.WithDetail("tmux create failed, deferring start"))
			return markStartPending(database, job, queueName, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		oplog.LogJob(oplog.OpJobStartFailed, job.ID, job.Host, oplog.WithErrorStr(errMsg), oplog.WithDetail("tmux create failed"))
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	oplog.LogJob(oplog.OpJobStarted, job.ID, job.Host, oplog.WithDetailf("started directly in session %s", tmuxSession))
	return false, nil
}

func markStartPending(database *sql.DB, job *db.Job, queueName string, revertToQueue bool) (bool, error) {
	if queueName == "" {
		queueName = queuefile.DefaultQueueName
	}
	if revertToQueue {
		if err := db.UpdateJobRunningToQueued(database, job.ID, queueName); err != nil {
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
func isQueueRunnerRunningJob(host, queueName string, jobID int64) bool {
	// Check if this job is the current job in the queue runner
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
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

	stdout, _, err := ssh.Run(host, checkCmd)
	if err != nil {
		return false // Can't determine, allow start to proceed
	}

	result := strings.TrimSpace(stdout)
	return result == "CURRENT" || result == "RUNNING"
}
