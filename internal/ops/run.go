package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
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

// RunJob creates a job record and queues it for execution.
// The job is recorded locally first, then the operation is queued.
// If the host is reachable, the job starts immediately.
// If not, the job remains in "starting" status until the host is available.
func RunJob(database *sql.DB, params RunJobParams, opts ExecuteOptions) (Result, error) {
	oplog.Log(oplog.OpJobStart, oplog.WithHost(params.Host), oplog.WithDetail("creating new job"))

	// 1. Create job record locally first (so we have an ID)
	jobID, err := db.RecordQueuedWithGPU(database, params.Host, params.WorkingDir, params.Command, params.Description, "default", "")
	if err != nil {
		oplog.Log(oplog.OpJobStartFailed, oplog.WithHost(params.Host), oplog.WithError(err), oplog.WithDetail("create job record failed"))
		return Result{}, fmt.Errorf("create job record: %w", err)
	}
	oplog.LogJob(oplog.OpJobStart, jobID, params.Host, oplog.WithDetail("job record created"))

	// 2. Set pending status to queued
	if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
		return Result{}, fmt.Errorf("set pending status: %w", err)
	}

	backend, err := ResolveBackend(params.Host, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Host %s unreachable, job %d will start on next sync", params.Host, jobID),
			}, nil
		}
		return Result{}, err
	}
	if err := db.SetJobBackend(database, jobID, backend); err != nil {
		return Result{}, fmt.Errorf("set job backend: %w", err)
	}

	// 3. Retrieve job with pending status set (must be after SetPendingStatus)
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return Result{}, fmt.Errorf("get job: %w", err)
	}

	// 4. Trigger reconciliation
	_, err = Reconcile(database, job, "", ReconcileOptions{Timeout: opts.Timeout})
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Host %s unreachable, job %d will start on next sync", params.Host, jobID),
			}, nil
		}
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   jobID,
		Message: fmt.Sprintf("Job %d started", jobID),
	}, nil
}
