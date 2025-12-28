package ops

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// RestartJobParams contains parameters for restarting a job
type RestartJobParams struct {
	OriginalJob *db.Job  // The job to restart
	WorkingDir  string   // Override working directory (empty = use original)
	Command     string   // Override command (empty = use original)
	Description string   // Override description (empty = use original)
	EnvVars     []string // Environment variables
}

// restartJobPayload is the JSON payload for deferred restart operations
type restartJobPayload struct {
	OriginalJobID int64    `json:"original_job_id"`
	WorkingDir    string   `json:"working_dir"`
	Command       string   `json:"command"`
	Description   string   `json:"description"`
	EnvVars       []string `json:"env_vars,omitempty"`
}

// RestartJob creates a new job record based on an existing job and queues it for execution.
// Returns the new job ID on success.
func RestartJob(database *sql.DB, params RestartJobParams, opts ExecuteOptions) (Result, error) {
	if params.OriginalJob == nil {
		return Result{}, fmt.Errorf("original job is nil")
	}

	orig := params.OriginalJob

	// Use original values if not overridden
	workingDir := params.WorkingDir
	if workingDir == "" {
		workingDir = orig.WorkingDir
	}
	command := params.Command
	if command == "" {
		command = orig.Command
	}
	description := params.Description
	if description == "" {
		description = orig.Description
	}

	// 1. Create new job record locally
	newJobID, err := db.RecordJobStarting(database, orig.Host, workingDir, command, description)
	if err != nil {
		return Result{}, fmt.Errorf("create job record: %w", err)
	}

	// 2. Build payload for deferred operation
	payload := restartJobPayload{
		OriginalJobID: orig.ID,
		WorkingDir:    workingDir,
		Command:       command,
		Description:   description,
		EnvVars:       params.EnvVars,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		db.UpdateJobFailed(database, newJobID, "failed to encode operation payload")
		return Result{}, fmt.Errorf("encode payload: %w", err)
	}

	// 3. Queue and execute the restart operation
	result, err := QueueAndExecute(database, orig.Host, db.OpRestartJob, newJobID, "", string(payloadJSON), opts)
	if err != nil {
		db.UpdateJobFailed(database, newJobID, err.Error())
		return Result{}, err
	}

	result.JobID = newJobID
	return result, nil
}

// executeRestart executes a restart job operation (called during queue drain)
func executeRestart(database *sql.DB, host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	// Parse payload
	var payload restartJobPayload
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		return Result{}, fmt.Errorf("parse payload: %w", err)
	}

	// Get new job to access start time
	job, err := db.GetJobByID(database, op.JobID)
	if err != nil || job == nil {
		return Result{}, fmt.Errorf("get job %d: %w", op.JobID, err)
	}

	// Skip if job is no longer in starting state
	if job.Status != db.StatusStarting {
		return Result{
			Success: true,
			JobID:   op.JobID,
			Message: fmt.Sprintf("Job %d has status '%s', skipping restart", op.JobID, job.Status),
		}, nil
	}

	// Generate file paths
	tmuxSession := session.TmuxSessionName(op.JobID)
	logFile := session.LogFile(op.JobID, job.StartTime)
	statusFile := session.StatusFile(op.JobID, job.StartTime)
	metadataFile := session.MetadataFile(op.JobID, job.StartTime)
	pidFile := session.PidFile(op.JobID, job.StartTime)

	// Create log directory on remote
	mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
	if _, stderr, err := ssh.RunWithTimeout(host, mkdirCmd, opts.Timeout); err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, op.JobID, errMsg)
		return Result{}, fmt.Errorf("create log directory: %s", errMsg)
	}

	// Save metadata
	metadata := session.FormatMetadata(op.JobID, payload.WorkingDir, payload.Command, host, payload.Description, job.StartTime)
	metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	ssh.RunWithTimeout(host, metadataCmd, opts.Timeout) // Best effort

	// Build wrapped command
	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      op.JobID,
		WorkingDir: payload.WorkingDir,
		Command:    payload.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    payload.EnvVars,
	})

	// Start tmux session
	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.RunWithTimeout(host, tmuxCmd, opts.Timeout); err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, op.JobID, errMsg)
		return Result{}, fmt.Errorf("start tmux: %s", errMsg)
	}

	// Mark job as running
	if err := db.UpdateJobRunning(database, op.JobID); err != nil {
		return Result{}, fmt.Errorf("update job status: %w", err)
	}

	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d started (restart of %d)", op.JobID, payload.OriginalJobID),
	}, nil
}
