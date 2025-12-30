// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
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
	JobID       int64
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	DepSpec     string
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
	lockFile := queueFile + ".lock"

	// Base64 encode env vars (newline-separated) for safe shell transport
	envVarsB64 := ""
	if len(entry.EnvVars) > 0 {
		envVarsB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join(entry.EnvVars, "\n")))
	}

	// Format the queue entry line (with trailing newline)
	jobLine := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s\n",
		entry.JobID, entry.WorkingDir, entry.Command, entry.Description, envVarsB64, entry.DepSpec)

	// Base64 encode the entire line for safe shell transport
	jobLineB64 := base64.StdEncoding.EncodeToString([]byte(jobLine))

	// Build the command:
	// 1. Create queue directory if needed
	// 2. Use flock for atomic operation
	// 3. Remove any existing entry for this job ID
	// 4. Append new entry
	appendCmd := fmt.Sprintf(
		"mkdir -p %s && flock %s bash -c \"sed -i '/^%d\\t/d' %s 2>/dev/null || true; echo '%s' | base64 -d >> %s\"",
		QueueDir, lockFile, entry.JobID, queueFile, jobLineB64, queueFile)

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

// QueueJobParams contains parameters for queueing a new job
type QueueJobParams struct {
	Host        string
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	QueueName   string
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

	// Build queue entry
	entry := QueueEntry{
		JobID:       jobID,
		WorkingDir:  params.WorkingDir,
		Command:     params.Command,
		Description: params.Description,
		EnvVars:     params.EnvVars,
	}

	// Append to remote queue
	appendOpts := AppendQueueEntryOptions{Timeout: opts.Timeout}
	if err := AppendQueueEntry(params.Host, queueName, entry, appendOpts); err != nil {
		// Check if this is a connection error - if so, defer the operation
		var qaErr *QueueAppendError
		if isConnectionErr := false; err != nil {
			if e, ok := err.(*QueueAppendError); ok {
				qaErr = e
				isConnectionErr = qaErr.IsConnectionError()
			} else {
				isConnectionErr = ssh.IsConnectionError(err.Error())
			}
			if isConnectionErr {
				// Defer the queue append for when host comes online
				if deferErr := db.AddDeferredOperation(database, params.Host, db.OpQueueJob, jobID, queueName, ""); deferErr != nil {
					db.DeleteJob(database, jobID)
					return Result{}, fmt.Errorf("defer queue append: %w", deferErr)
				}
				return Result{
					Success:  true,
					Deferred: true,
					JobID:    jobID,
					Message:  fmt.Sprintf("Job %d queued (will append to queue when host is online)", jobID),
				}, nil
			}
		}
		// Non-connection error - delete the job and return error
		db.DeleteJob(database, jobID)
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   jobID,
		Message: fmt.Sprintf("Job %d added to queue '%s'", jobID, queueName),
	}, nil
}
