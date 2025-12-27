package queuejob

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
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
	if job.Status != db.StatusQueued {
		return false, fmt.Errorf("job %d is not queued (status: %s)", job.ID, job.Status)
	}

	queueName := job.QueueName
	if queueName == "" {
		queueName = queuefile.DefaultQueueName
	}

	// Check if job has a pending queue_job operation (not yet synced to remote)
	hasPendingQueue, _ := db.HasPendingOperation(database, job.ID, db.OpQueueJob)
	if hasPendingQueue {
		// Job isn't in remote queue yet - get data from pending operation and start directly
		payload, _ := db.GetDeferredOperationPayload(database, job.ID, db.OpQueueJob)
		var entry *queuefile.Entry
		if payload != "" {
			var p struct {
				WorkingDir  string   `json:"working_dir"`
				Command     string   `json:"command"`
				Description string   `json:"description"`
				EnvVars     []string `json:"env_vars"`
				DepSpec     string   `json:"dep_spec"`
			}
			if err := json.Unmarshal([]byte(payload), &p); err == nil {
				entry = &queuefile.Entry{
					JobID:       job.ID,
					WorkingDir:  p.WorkingDir,
					Command:     p.Command,
					Description: p.Description,
					EnvVars:     p.EnvVars,
					DepSpec:     p.DepSpec,
				}
			}
		}
		// Delete the pending queue_job operation
		db.DeletePendingOperation(database, job.ID, db.OpQueueJob)
		// Start the job directly (no need to remove from remote queue)
		return startJobDirectly(database, job, queueName, entry)
	}

	entry, err := queuefile.FetchEntry(job.Host, queueName, job.ID)
	if err != nil {
		if queuefile.IsConnectionError(err) {
			return deferQueuedJobStart(database, job, queueName, nil, false)
		}
		return false, err
	}

	if err := queuefile.RemoveEntry(job.Host, queueName, job.ID); err != nil {
		if queuefile.IsConnectionError(err) {
			return deferQueuedJobStart(database, job, queueName, entry, false)
		}
		return false, err
	}
	entryRemoved := true

	if err := db.UpdateQueuedToRunning(database, job.ID); err != nil {
		return false, fmt.Errorf("update queued job: %w", err)
	}

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil || updated == nil {
		return false, fmt.Errorf("refresh job after start: %w", err)
	}

	// Prepare log/metadata paths
	logFile := session.LogFile(job.ID, updated.StartTime)
	statusFile := session.StatusFile(job.ID, updated.StartTime)
	metadataFile := session.MetadataFile(job.ID, updated.StartTime)
	pidFile := session.PidFile(job.ID, updated.StartTime)
	tmuxSession := session.TmuxSessionName(job.ID)

	// Ensure log directory exists
	mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
	if _, stderr, err := ssh.Run(job.Host, mkdirCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			return deferQueuedJobStart(database, job, queueName, entry, entryRemoved)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	// Save metadata
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.Description, updated.StartTime)
	writeMetadata := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, stderr, err := ssh.Run(job.Host, writeMetadata); err != nil {
		if isConnectionFailure(stderr, err) {
			return deferQueuedJobStart(database, job, queueName, entry, entryRemoved)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      job.ID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    entry.EnvVars,
	})

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			return deferQueuedJobStart(database, job, queueName, entry, entryRemoved)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	return false, nil
}

// startJobDirectly starts a job without interacting with the remote queue file.
// Used when job has a pending queue_job operation (not yet synced to remote).
func startJobDirectly(database *sql.DB, job *db.Job, queueName string, entry *queuefile.Entry) (bool, error) {
	if err := db.UpdateQueuedToRunning(database, job.ID); err != nil {
		return false, fmt.Errorf("update queued job: %w", err)
	}

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil || updated == nil {
		return false, fmt.Errorf("refresh job after start: %w", err)
	}

	// Prepare log/metadata paths
	logFile := session.LogFile(job.ID, updated.StartTime)
	statusFile := session.StatusFile(job.ID, updated.StartTime)
	metadataFile := session.MetadataFile(job.ID, updated.StartTime)
	pidFile := session.PidFile(job.ID, updated.StartTime)
	tmuxSession := session.TmuxSessionName(job.ID)

	// Ensure log directory exists
	mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
	if _, stderr, err := ssh.Run(job.Host, mkdirCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			// Pass entryRemoved=true: job was never in remote queue, so sync needs to
			// recreate it and revert status to queued
			return deferQueuedJobStart(database, job, queueName, entry, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	// Save metadata
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.Description, updated.StartTime)
	writeMetadata := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, stderr, err := ssh.Run(job.Host, writeMetadata); err != nil {
		if isConnectionFailure(stderr, err) {
			// Pass entryRemoved=true: job was never in remote queue, so sync needs to
			// recreate it and revert status to queued
			return deferQueuedJobStart(database, job, queueName, entry, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	var envVars []string
	if entry != nil {
		envVars = entry.EnvVars
	}

	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      job.ID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    envVars,
	})

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			// Pass entryRemoved=true: job was never in remote queue, so sync needs to
			// recreate it and revert status to queued
			return deferQueuedJobStart(database, job, queueName, entry, true)
		}
		errMsg := ssh.FriendlyError(job.Host, stderr, err)
		db.UpdateJobFailed(database, job.ID, errMsg)
		return false, fmt.Errorf("%s", errMsg)
	}

	return false, nil
}

type deferredStartPayload struct {
	QueueName    string   `json:"queue_name"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	Command      string   `json:"command,omitempty"`
	Description  string   `json:"description,omitempty"`
	EnvVars      []string `json:"env_vars,omitempty"`
	DepSpec      string   `json:"dep_spec,omitempty"`
	EntryMissing bool     `json:"entry_missing,omitempty"`
}

func deferQueuedJobStart(database *sql.DB, job *db.Job, queueName string, entry *queuefile.Entry, entryRemoved bool) (bool, error) {
	// Check if a start operation already exists for this job
	hasPending, err := db.HasPendingOperation(database, job.ID, db.OpStartQueuedJob)
	if err != nil {
		return false, fmt.Errorf("check pending operations: %w", err)
	}
	if hasPending {
		// Already has a pending start operation, don't add another
		return true, nil
	}

	payload := deferredStartPayload{
		QueueName:    queueName,
		EntryMissing: entryRemoved,
	}

	if entry != nil {
		payload.WorkingDir = entry.WorkingDir
		payload.Command = entry.Command
		payload.Description = entry.Description
		payload.EnvVars = entry.EnvVars
		payload.DepSpec = entry.DepSpec
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("encode deferred start payload: %w", err)
	}

	if entryRemoved {
		if err := db.UpdateJobRunningToQueued(database, job.ID, queueName); err != nil {
			return false, fmt.Errorf("mark job queued: %w", err)
		}
	}

	if err := db.AddDeferredOperation(database, job.Host, db.OpStartQueuedJob, job.ID, queueName, string(payloadJSON)); err != nil {
		return false, fmt.Errorf("add deferred operation: %w", err)
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
