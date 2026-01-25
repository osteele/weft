// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/ssh"
)

const (
	// QueueDir is the remote directory where queue files are stored
	QueueDir = "~/.cache/remote-jobs/queue"
	// DefaultQueueName is the default queue name when none is specified
	DefaultQueueName = "default"
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
	Tags         []string
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
func AppendQueueEntry(host, queueName string, entry QueueEntry, opts AppendQueueEntryOptions) error {
	if entry.Command == "" {
		return fmt.Errorf("job %d missing command", entry.JobID)
	}
	queueName = DefaultQueueName
	addCmd := NewAddCommand(entry)
	appendOpts := AppendCommandOptions{Timeout: opts.Timeout}
	if err := AppendCommand(host, queueName, addCmd, appendOpts); err != nil {
		return err
	}
	return nil
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
		Tags:         job.Tags,
	}
	addCmd := NewAddCommand(entry)
	opts := AppendCommandOptions{Timeout: timeout}
	return AppendCommand(job.Host, DefaultQueueName, addCmd, opts)
}

// UpdateQueueEntryParams contains parameters for updating an existing queue entry
type UpdateQueueEntryParams struct {
	Host      string
	QueueName string
	Job       *db.Job
	EnvVars   []string
	DepSpec   string
	Timeout   time.Duration
}

// UpdateQueueEntry updates an existing job's entry in the remote queue file.
// This is used when editing a queued job's command, working directory, or other parameters.
// The existing entry is removed and a new one is appended with the updated values.
func UpdateQueueEntry(params UpdateQueueEntryParams) error {
	if params.Job == nil {
		return fmt.Errorf("job is nil")
	}

	queueName := DefaultQueueName

	entry := QueueEntry{
		JobID:        params.Job.ID,
		WorkingDir:   params.Job.WorkingDir,
		Command:      params.Job.Command,
		Description:  params.Job.Description,
		EnvVars:      params.EnvVars,
		DepSpec:      params.DepSpec,
		CPUAllotment: params.Job.CPUAllotment,
		Tags:         params.Job.Tags,
	}

	// Use new command queue format
	addCmd := NewAddCommand(entry)
	opts := AppendCommandOptions{Timeout: params.Timeout}
	if err := AppendCommand(params.Host, queueName, addCmd, opts); err != nil {
		var qaErr *QueueAppendError
		if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("update queue entry: %w", err)
	}

	return nil
}

// UpdateQueuedJobEntry refreshes a queued job entry on the remote host, fetching
// missing fields from the queue file if necessary.
func UpdateQueuedJobEntry(job *db.Job, queueName string, envVars []string, depSpec string) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	queueName = queuefile.DefaultQueueName

	entryJob := &db.Job{
		ID:          job.ID,
		Host:        job.Host,
		WorkingDir:  job.WorkingDir,
		Command:     job.Command,
		Description: job.Description,
		QueueName:   queueName,
	}

	needEnv := len(envVars) == 0
	needDir := entryJob.WorkingDir == ""
	needCmd := entryJob.Command == ""

	if needEnv || needDir || needCmd {
		entry, err := queuefile.FetchEntry(job.Host, queueName, job.ID)
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
		Host:      job.Host,
		QueueName: queueName,
		Job:       entryJob,
		EnvVars:   envVars,
		DepSpec:   depSpec,
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
	QueueName    string
	DepSpec      string
	CPUAllotment *int
}

// QueueJob creates a job record and adds it to the remote queue.
// The job is recorded locally with "queued" status, then appended to the queue file.
// If the host is unreachable, the operation is deferred until the host comes online.
func QueueJob(database *sql.DB, params QueueJobParams, opts ExecuteOptions) (Result, error) {
	queueName := DefaultQueueName

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

	// Record job with queued status
	jobID, err := db.RecordQueuedWithGPU(database, params.Host, params.WorkingDir, params.Command, params.Description, queueName, gpu)
	if err != nil {
		return Result{}, fmt.Errorf("record job: %w", err)
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
	if err := db.SetJobEnvVars(database, jobID, params.EnvVars); err != nil {
		db.DeleteJob(database, jobID)
		return Result{}, fmt.Errorf("record env vars: %w", err)
	}
	if len(params.Tags) > 0 {
		if err := db.SetJobTags(database, jobID, params.Tags); err != nil {
			db.DeleteJob(database, jobID)
			return Result{}, fmt.Errorf("record tags: %w", err)
		}
	}
	if err := db.SetJobDepSpec(database, jobID, params.DepSpec); err != nil {
		return Result{}, fmt.Errorf("record dependencies: %w", err)
	}
	if params.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(database, jobID, params.CPUAllotment); err != nil {
			db.DeleteJob(database, jobID)
			return Result{}, fmt.Errorf("record CPU allotment: %w", err)
		}
	}

	if backend == db.BackendSlurm {
		if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
			return Result{}, fmt.Errorf("set pending status: %w", err)
		}
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return Result{}, fmt.Errorf("get job: %w", err)
		}
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
			Message: fmt.Sprintf("Job %d submitted", jobID),
		}, nil
	}

	// Build queue entry with artifact env vars merged in
	entry := QueueEntry{
		JobID:        jobID,
		WorkingDir:   params.WorkingDir,
		Command:      params.Command,
		Description:  params.Description,
		EnvVars:      artifacts.MergeEnvVars(params.EnvVars, jobID),
		DepSpec:      params.DepSpec,
		CPUAllotment: params.CPUAllotment,
		Tags:         params.Tags,
	}

	// Append to remote queue
	addCmd := NewAddCommand(entry)
	appendOpts := AppendCommandOptions{Timeout: opts.Timeout}
	if err := AppendCommand(params.Host, queueName, addCmd, appendOpts); err != nil {
		var qaErr *QueueAppendError
		if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Job %d queued locally (will append to queue when host is online)", jobID),
			}, nil
		}
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Job %d queued locally (will append to queue when host is online)", jobID),
			}, nil
		}
		db.DeleteJob(database, jobID)
		return Result{}, err
	}

	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		return Result{}, fmt.Errorf("update sync state: %w", err)
	}

	return Result{
		Success: true,
		JobID:   jobID,
		Message: fmt.Sprintf("Job %d added to queue", jobID),
	}, nil
}
