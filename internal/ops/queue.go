// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

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

// escapeForQueueFile escapes a string for safe inclusion in queue file entries.
// The queue runner uses printf '%b' to interpret escape sequences, so we need to
// escape backslashes to prevent unintended interpretation (e.g., \n becoming newline).
// This also converts actual newlines/tabs to their escape sequences.
func escapeForQueueFile(s string) string {
	// First escape existing backslashes (\ -> \\)
	s = strings.ReplaceAll(s, `\`, `\\`)
	// Then convert actual newlines and tabs to escape sequences
	// (these would break the tab-separated, one-line-per-entry format)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// QueueEntry represents a job entry to be added to a remote queue
type QueueEntry struct {
	JobID        int64
	WorkingDir   string
	Command      string
	Description  string
	EnvVars      []string
	DepSpec      string
	CPUAllotment *int
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

// AppendQueueEntry adds a job entry to a remote queue file.
// It uses base64 encoding to safely pass data through shell contexts
// and flock to prevent race conditions with the queue runner.
// If an entry for the same job ID exists, it is removed first.
func AppendQueueEntry(host, queueName string, entry QueueEntry, opts AppendQueueEntryOptions) error {
	if entry.Command == "" {
		return fmt.Errorf("job %d missing command", entry.JobID)
	}
	if queueName == "" {
		queueName = DefaultQueueName
	}

	queueFile := fmt.Sprintf("%s/%s.queue", QueueDir, queueName)

	// Base64 encode env vars (newline-separated) for safe shell transport
	envVarsB64 := ""
	if len(entry.EnvVars) > 0 {
		envVarsB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join(entry.EnvVars, "\n")))
	}

	// Escape backslashes in fields that might contain them (command, description)
	// This prevents printf '%b' in queue-runner.sh from interpreting \n as newlines
	// After escaping: \n -> \\n, \t -> \\t, \\ -> \\\\
	// printf '%b' will then convert them back to the original characters
	escapedCommand := escapeForQueueFile(entry.Command)
	escapedDescription := escapeForQueueFile(entry.Description)

	// Format the queue entry line (with trailing newline)
	jobLine := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s\n",
		entry.JobID, entry.WorkingDir, escapedCommand, escapedDescription, envVarsB64, entry.DepSpec)

	// Base64 encode the entire line for safe shell transport
	jobLineB64 := base64.StdEncoding.EncodeToString([]byte(jobLine))

	// Build the command:
	// 1. Create queue directory if needed
	// 2. Use mkdir-based lock (atomic on all POSIX systems, works on Linux and macOS)
	// 3. Remove any existing entry for this job ID
	// 4. Append new entry
	// Note: We use grep + temp file instead of sed -i because sed -i syntax differs between Linux and macOS
	lockDir := queueFile + ".lock.d"
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && (
			while ! mkdir %s 2>/dev/null; do sleep 0.01; done
			trap 'rmdir %s 2>/dev/null' EXIT
			grep -v '^%d	' %s > %s.tmp 2>/dev/null || true
			mv %s.tmp %s 2>/dev/null || true
			echo '%s' | base64 -d >> %s
			rmdir %s 2>/dev/null
		)`,
		QueueDir,
		lockDir,
		lockDir,
		entry.JobID, queueFile, queueFile,
		queueFile, queueFile,
		jobLineB64, queueFile,
		lockDir)

	var stdout, stderr string
	var err error
	if opts.Timeout > 0 {
		stdout, stderr, err = ssh.RunWithTimeout(host, appendCmd, opts.Timeout)
	} else {
		stdout, stderr, err = ssh.Run(host, appendCmd)
	}
	_ = stdout // unused

	if err != nil {
		return &QueueAppendError{Op: "append queue entry", Stderr: stderr, Err: err}
	}

	return nil
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

	queueName := params.QueueName
	if queueName == "" {
		queueName = params.Job.QueueName
	}
	if queueName == "" {
		queueName = DefaultQueueName
	}

	entry := QueueEntry{
		JobID:        params.Job.ID,
		WorkingDir:   params.Job.WorkingDir,
		Command:      params.Job.Command,
		Description:  params.Job.Description,
		EnvVars:      params.EnvVars,
		DepSpec:      params.DepSpec,
		CPUAllotment: params.Job.CPUAllotment,
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
	if queueName == "" {
		queueName = job.QueueName
	}
	if queueName == "" {
		queueName = queuefile.DefaultQueueName
	}

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
	QueueName    string
	DepSpec      string
	CPUAllotment *int
}

// QueueJob creates a job record and adds it to the remote queue.
// The job is recorded locally with "queued" status, then appended to the queue file.
// If the host is unreachable, the operation is deferred until the host comes online.
func QueueJob(database *sql.DB, params QueueJobParams, opts ExecuteOptions) (Result, error) {
	queueName := params.QueueName
	if queueName == "" {
		queueName = DefaultQueueName
	}

	// Extract GPU from env vars if present
	gpu := ""
	for _, ev := range params.EnvVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			gpu = strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
			break
		}
	}

	// Record job with queued status
	jobID, err := db.RecordQueuedWithGPU(database, params.Host, params.WorkingDir, params.Command, params.Description, queueName, gpu)
	if err != nil {
		return Result{}, fmt.Errorf("record job: %w", err)
	}
	if err := db.SetJobEnvVars(database, jobID, params.EnvVars); err != nil {
		db.DeleteJob(database, jobID)
		return Result{}, fmt.Errorf("record env vars: %w", err)
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

	// Build queue entry
	entry := QueueEntry{
		JobID:        jobID,
		WorkingDir:   params.WorkingDir,
		Command:      params.Command,
		Description:  params.Description,
		EnvVars:      params.EnvVars,
		DepSpec:      params.DepSpec,
		CPUAllotment: params.CPUAllotment,
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
		Message: fmt.Sprintf("Job %d added to queue '%s'", jobID, queueName),
	}, nil
}
