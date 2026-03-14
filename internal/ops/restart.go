package ops

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/workdir"
)

// RestartJobParams contains parameters for restarting a job
type RestartJobParams struct {
	OriginalJob  *db.Job  // The job to restart
	WorkingDir   string   // Override working directory (empty = use original)
	Command      string   // Override command (empty = use original)
	Description  string   // Override description (empty = use original)
	EnvVars      []string // Environment variables
	Tags         []string // Job tags
	DepSpec      string   // Dependency specification
	CPUAllotment *int     // CPU allotment
	Project      string   // Project name
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
	newJobID, err := db.RecordQueuedWithGPU(database, orig.Host, workingDir, command, description, orig.GPU)
	if err != nil {
		return Result{}, fmt.Errorf("create job record: %w", err)
	}
	if orig.GPUClass != "" {
		if err := db.SetJobGPUClass(database, newJobID, orig.GPUClass); err != nil {
			return Result{}, fmt.Errorf("set GPU class: %w", err)
		}
	}
	if orig.GPUMemGB != nil {
		if err := db.SetJobGPUMemGB(database, newJobID, orig.GPUMemGB); err != nil {
			return Result{}, fmt.Errorf("set GPU mem: %w", err)
		}
	}
	if len(params.EnvVars) > 0 {
		if err := db.SetJobEnvVars(database, newJobID, params.EnvVars); err != nil {
			return Result{}, fmt.Errorf("set env vars: %w", err)
		}
	}
	if len(params.Tags) > 0 {
		if err := db.SetJobTags(database, newJobID, params.Tags); err != nil {
			return Result{}, fmt.Errorf("set tags: %w", err)
		}
	}
	if params.DepSpec != "" {
		if err := db.SetJobDepSpec(database, newJobID, params.DepSpec); err != nil {
			return Result{}, fmt.Errorf("set dep spec: %w", err)
		}
	}
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(database, newJobID, params.CPUAllotment); err != nil {
			return Result{}, fmt.Errorf("set CPU allotment: %w", err)
		}
	}
	project := strings.TrimSpace(params.Project)
	if project == "" {
		var err error
		project, err = workdir.ResolveProjectName("", workingDir)
		if err != nil {
			return Result{}, fmt.Errorf("resolve project: %w", err)
		}
	}
	if project == "" {
		project = db.DeriveProject(workingDir, command)
	}
	if project != "" {
		if err := db.SetJobProject(database, newJobID, project); err != nil {
			return Result{}, fmt.Errorf("set project: %w", err)
		}
	}
	if err := RefreshProjectDerivedMetadata(database, newJobID, workingDir, command, orig.Inputs); err != nil {
		return Result{}, err
	}
	backend := orig.Backend
	if backend == "" {
		var resolveErr error
		backend, resolveErr = ResolveBackend(orig.Host, opts.Timeout)
		if resolveErr != nil {
			if ssh.IsConnectionError(resolveErr.Error()) {
				return Result{
					Success:  true,
					Deferred: true,
					JobID:    newJobID,
					Message:  fmt.Sprintf("Host %s unreachable, job %d will restart on next sync", orig.Host, newJobID),
				}, nil
			}
			return Result{}, resolveErr
		}
	}
	if err := db.SetJobBackend(database, newJobID, backend); err != nil {
		return Result{}, fmt.Errorf("set job backend: %w", err)
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
