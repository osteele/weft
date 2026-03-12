// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queuefile"
	"github.com/osteele/weft/internal/ssh"
)

const (
	// QueueDir is the remote directory where queue files are stored
	QueueDir = "~/.cache/weft/queue"
	// DefaultQueueName is the default queue name when none is specified
	DefaultQueueName = "default"
	// DefaultGPUMemGB is the default GPU memory reservation when a job uses a GPU
	DefaultGPUMemGB = 20
)

// QueueEntry represents a job entry to be added to a remote queue
type QueueEntry struct {
	JobID        int64
	WorkingDir   string
	Command      string
	Description  string
	EnvVars      []string
	DepSpec      string
	CPUAllotment *int
	GPU          string
	GPUClass     string
	GPUMemGB     *int
	Tags         []string
	OutputDirs   []string
	Produces     []string
	Needs        []string
}

// AppendQueueEntryOptions configures the queue append operation
type AppendQueueEntryOptions struct {
	Timeout time.Duration // SSH timeout (0 = no timeout)
}

// QueueAppendError represents an error during queue append operations
type QueueAppendError struct {
	Op     string // operation that failed
	Stderr string // stderr output from SSH command
	Err    error  // underlying error
}

func (e *QueueAppendError) Error() string {
	if e == nil {
		return ""
	}
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Sprintf("%s: %s", e.Op, msg)
}

func (e *QueueAppendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsConnectionError returns true if this error was due to SSH connection failure
func (e *QueueAppendError) IsConnectionError() bool {
	if e == nil {
		return false
	}
	if e.Stderr != "" && ssh.IsConnectionError(e.Stderr) {
		return true
	}
	if e.Err != nil && ssh.IsConnectionError(e.Err.Error()) {
		return true
	}
	return false
}

// AppendQueueEntry adds a job entry to the remote command log (queue runner input).
func AppendQueueEntry(host string, entry QueueEntry, opts AppendQueueEntryOptions) error {
	if entry.Command == "" {
		return fmt.Errorf("job %d missing command", entry.JobID)
	}
	addCmd := NewAddCommand(entry)
	return AppendCommand(host, addCmd, AppendCommandOptions{Timeout: opts.Timeout})
}

// AppendJobToQueue adds an existing job to the remote queue.
// This is the canonical function for appending existing jobs to the queue,
// ensuring artifact env vars are properly merged.
func AppendJobToQueue(job *db.Job, timeout time.Duration) error {
	entry := QueueEntry{
		JobID:        job.ID,
		WorkingDir:   job.WorkingDir,
		Command:      job.Command,
		Description:  job.Description,
		EnvVars:      artifacts.MergeEnvVars(job.EnvVars, job.ID),
		DepSpec:      job.DepSpec,
		CPUAllotment: job.CPUAllotment,
		GPU:          job.GPU,
		GPUClass:     job.GPUClass,
		GPUMemGB:     job.GPUMemGB,
		Tags:         job.Tags,
		OutputDirs:   job.OutputDirs,
		Produces:     job.Produces,
		Needs:        job.Needs,
	}
	addCmd := NewAddCommand(entry)
	opts := AppendCommandOptions{Timeout: timeout}
	return AppendCommand(job.Host, addCmd, opts)
}

// UpdateQueueEntryParams contains parameters for updating an existing queue entry
type UpdateQueueEntryParams struct {
	Host    string
	Job     *db.Job
	EnvVars []string
	DepSpec string
	Timeout time.Duration
}

// UpdateQueueEntry updates an existing job's entry in the remote queue file.
// This is used when editing a queued job's command, working directory, or other parameters.
// The existing entry is removed and a new one is appended with the updated values.
func UpdateQueueEntry(params UpdateQueueEntryParams) error {
	if params.Job == nil {
		return fmt.Errorf("job is nil")
	}

	entry := queueEntryForJob(params.Job, params.EnvVars, params.DepSpec)
	if err := writeQueueJobFile(params.Host, entry, params.Timeout); err != nil {
		if isQueueConnectionError(err) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("update queue entry: %w", err)
	}
	return nil
}

// UpdateQueuedJobEntry refreshes a queued job entry on the remote host, fetching
// missing fields from the queue file if necessary.
func UpdateQueuedJobEntry(job *db.Job, envVars []string, depSpec string) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}

	entryJob := &db.Job{
		ID:          job.ID,
		Host:        job.Host,
		WorkingDir:  job.WorkingDir,
		Command:     job.Command,
		Description: job.Description,
	}

	needEnv := len(envVars) == 0
	needDir := entryJob.WorkingDir == ""
	needCmd := entryJob.Command == ""

	if needEnv || needDir || needCmd {
		entry, err := queuefile.FetchEntry(job.Host, job.ID)
		if err != nil {
			return err
		}
		if needDir && entry.WorkingDir != "" {
			entryJob.WorkingDir = entry.WorkingDir
		}
		if needCmd && entry.Command != "" {
			entryJob.Command = entry.Command
		}
		if entryJob.Description == "" && entry.Description != "" {
			entryJob.Description = entry.Description
		}
		if needEnv {
			envVars = entry.EnvVars
		}
	}

	return UpdateQueueEntry(UpdateQueueEntryParams{
		Host:    job.Host,
		Job:     entryJob,
		EnvVars: envVars,
		DepSpec: depSpec,
	})
}

// QueueJobParams contains parameters for queueing a new job
type QueueJobParams struct {
	Host         string
	WorkingDir   string
	Command      string
	Description  string
	EnvVars      []string
	Tags         []string
	GPU          string // Explicit GPU setting; if empty, extracted from EnvVars
	GPUClass     string // GPU class name (e.g., "A100") — resolved to device at runtime
	GPUMemGB     *int   // GPU memory reservation in GB per device
	DepSpec      string
	CPUAllotment *int
	OutputDirs   []string // convention-based output directories from .weft.toml
	Inputs       []string // Data asset refs the job reads (e.g., "hf:meta-llama/Llama-3-8B")
	Outputs      []string // Data asset refs the job produces (e.g., "checkpoint:llama-ft-v1")
	Produces     []string // Artifact specs this job produces (e.g., "output/model.pt" or "output/model.pt:100")
	Needs        []string // Artifact specs this job needs (e.g., "output/model.pt:100")
}

// RecordQueuedJob records a job in the local database with "queued" status.
// This is DB-only — no SSH or remote operations are performed.
// The job will be pushed to the remote host by SyncHost on the next sync cycle.
func RecordQueuedJob(database *sql.DB, params QueueJobParams) (int64, error) {
	// Extract GPU from env vars if not explicitly set
	gpu := params.GPU
	if gpu == "" {
		for _, ev := range params.EnvVars {
			if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
				gpu = strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
				break
			}
		}
	}

	// Apply default GPU memory reservation when GPU is involved but no explicit reservation.
	gpuMemGB := params.GPUMemGB
	if gpuMemGB == nil && (gpu != "" || params.GPUClass != "") {
		defaultMem := DefaultGPUMemGB
		gpuMemGB = &defaultMem
	}

	// Record job with queued status
	jobID, err := db.RecordQueuedWithGPU(database, params.Host, params.WorkingDir, params.Command, params.Description, gpu)
	if err != nil {
		return 0, fmt.Errorf("record job: %w", err)
	}
	if err := db.SetJobEnvVars(database, jobID, params.EnvVars); err != nil {
		db.DeleteJob(database, jobID)
		return 0, fmt.Errorf("record env vars: %w", err)
	}
	if len(params.Tags) > 0 {
		if err := db.SetJobTags(database, jobID, params.Tags); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record tags: %w", err)
		}
	}
	if err := db.SetJobDepSpec(database, jobID, params.DepSpec); err != nil {
		return 0, fmt.Errorf("record dependencies: %w", err)
	}
	if project := db.DeriveProject(params.WorkingDir, params.Command); project != "" {
		if err := db.SetJobProject(database, jobID, project); err != nil {
			return 0, fmt.Errorf("record project: %w", err)
		}
	}
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(database, jobID, params.CPUAllotment); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record CPU allotment: %w", err)
		}
	}
	if gpuMemGB != nil {
		if err := db.SetJobGPUMemGB(database, jobID, gpuMemGB); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record GPU memory: %w", err)
		}
	}
	if params.GPUClass != "" {
		if err := db.SetJobGPUClass(database, jobID, params.GPUClass); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record GPU class: %w", err)
		}
	}
	if len(params.Inputs) > 0 {
		if err := db.SetJobInputs(database, jobID, params.Inputs); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record inputs: %w", err)
		}
	}
	if len(params.Outputs) > 0 {
		if err := db.SetJobOutputs(database, jobID, params.Outputs); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record outputs: %w", err)
		}
	}
	if len(params.OutputDirs) > 0 {
		if err := db.SetJobOutputDirs(database, jobID, params.OutputDirs); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record output dirs: %w", err)
		}
	}
	if len(params.Produces) > 0 {
		if err := db.SetJobProduces(database, jobID, params.Produces); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record produces: %w", err)
		}
	}
	if len(params.Needs) > 0 {
		if err := db.SetJobNeeds(database, jobID, params.Needs); err != nil {
			db.DeleteJob(database, jobID)
			return 0, fmt.Errorf("record needs: %w", err)
		}
	}

	return jobID, nil
}

// QueueJob creates a job record and adds it to the remote queue.
// The job is recorded locally with "queued" status, then appended to the queue file.
// If the host is unreachable, the operation is deferred until the host comes online.
func QueueJob(database *sql.DB, params QueueJobParams, opts ExecuteOptions) (Result, error) {
	jobID, err := RecordQueuedJob(database, params)
	if err != nil {
		return Result{}, err
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

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return Result{}, fmt.Errorf("get job: %w", err)
	}

	if backend == db.BackendSlurm {
		outcome, err := requestJobStatus(database, job, db.StatusQueued, opts)
		if err != nil {
			return Result{}, err
		}
		if !outcome.hostAvailable {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Host %s unreachable, job %d will start on next sync", params.Host, jobID),
			}, nil
		}
		if !outcome.resolved {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Host %s unreachable, job %d will start on next sync", params.Host, jobID),
			}, nil
		}
		return Result{
			Success: true,
			JobID:   jobID,
			Message: fmt.Sprintf("Job %d submitted", jobID),
		}, nil
	}

	if err := AppendJobToQueue(job, opts.Timeout); err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Job %d queued locally (will append to queue when host is online)", jobID),
			}, nil
		}
		return Result{}, err
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   jobID,
		Message: fmt.Sprintf("Job %d added to queue", jobID),
	}, nil
}
