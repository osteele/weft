package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
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
	newJobID, err := db.RecordQueuedWithGPU(database, orig.Host, workingDir, command, description, DefaultQueueName, orig.GPU)
	if err != nil {
		return Result{}, fmt.Errorf("create job record: %w", err)
	}

	// 2. Set pending status to queued
	if err := db.SetPendingStatus(database, newJobID, db.StatusQueued); err != nil {
		return Result{}, fmt.Errorf("set pending status: %w", err)
	}

	// 3. Retrieve job with pending status set (must be after SetPendingStatus)
	newJob, err := db.GetJobByID(database, newJobID)
	if err != nil {
		return Result{}, fmt.Errorf("get new job: %w", err)
	}

	// 4. Trigger reconciliation
	_, err = Reconcile(database, newJob, "", ReconcileOptions{Timeout: opts.Timeout})
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    newJobID,
				Message:  fmt.Sprintf("Host %s unreachable, job %d will restart on next sync", newJob.Host, newJobID),
			}, nil
		}
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   newJobID,
		Message: fmt.Sprintf("Job %d queued for restart", newJobID),
	}, nil
}
