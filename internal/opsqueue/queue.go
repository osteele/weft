// Package opsqueue provides queue command protocol types and queue entry operations.
package opsqueue

import (
	"fmt"
	"strings"
	"time"

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
	SourceSHA256 string
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
