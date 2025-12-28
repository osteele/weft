package ops

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// RunJobParams contains parameters for running a new job
type RunJobParams struct {
	Host        string
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
}

// runJobPayload is the JSON payload for deferred run operations
type runJobPayload struct {
	WorkingDir  string   `json:"working_dir"`
	Command     string   `json:"command"`
	Description string   `json:"description"`
	EnvVars     []string `json:"env_vars,omitempty"`
}

// RunJob creates a job record and queues it for execution.
// The job is recorded locally first, then the operation is queued.
// If the host is reachable, the job starts immediately.
// If not, the job remains in "starting" status until the host is available.
func RunJob(database *sql.DB, params RunJobParams, opts ExecuteOptions) (Result, error) {
	// 1. Create job record locally first (so we have an ID)
	jobID, err := db.RecordJobStarting(database, params.Host, params.WorkingDir, params.Command, params.Description)
	if err != nil {
		return Result{}, fmt.Errorf("create job record: %w", err)
	}

	// 2. Build payload for deferred operation
	payload := runJobPayload{
		WorkingDir:  params.WorkingDir,
		Command:     params.Command,
		Description: params.Description,
		EnvVars:     params.EnvVars,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		db.UpdateJobFailed(database, jobID, "failed to encode operation payload")
		return Result{}, fmt.Errorf("encode payload: %w", err)
	}

	// 3. Queue and execute the run operation
	result, err := QueueAndExecute(database, params.Host, db.OpRunJob, jobID, "", string(payloadJSON), opts)
	if err != nil {
		db.UpdateJobFailed(database, jobID, err.Error())
		return Result{}, err
	}

	result.JobID = jobID
	return result, nil
}

// executeRun executes a run job operation (called during queue drain)
func executeRun(database *sql.DB, host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	// Parse payload
	var payload runJobPayload
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		return Result{}, fmt.Errorf("parse payload: %w", err)
	}

	// Get job to access start time
	job, err := db.GetJobByID(database, op.JobID)
	if err != nil || job == nil {
		return Result{}, fmt.Errorf("get job %d: %w", op.JobID, err)
	}

	// Skip if job is no longer in starting state
	if job.Status != db.StatusStarting {
		return Result{
			Success: true,
			JobID:   op.JobID,
			Message: fmt.Sprintf("Job %d has status '%s', skipping run", op.JobID, job.Status),
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
		Message: fmt.Sprintf("Job %d started", op.JobID),
	}, nil
}
