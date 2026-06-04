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
	// queueName is the only queue name used. It's retained as a literal in remote
	// filenames for backward compatibility with in-flight jobs and agent wrappers.
	queueName = "default"
	// DefaultGPUMemGB is the default GPU memory reservation when a job uses a GPU
	DefaultGPUMemGB = 20
)

// CurrentFileName returns the filename for the "current job" marker file.
func CurrentFileName() string { return queueName + ".current" }

// CurrentFilePath returns the full remote path to the current-job marker file.
func CurrentFilePath() string { return QueueDir + "/" + CurrentFileName() }

// PidFileName returns the filename for the runner's PID file.
func PidFileName() string { return queueName + ".runner.pid" }

// StopFilePath returns the full remote path to the runner stop file.
func StopFilePath() string { return QueueDir + "/" + queueName + ".stop" }

// RunnerLogName returns the filename for the runner's log file.
func RunnerLogName() string { return "runner-" + queueName + ".log" }

// TmuxSessionName returns the name of the tmux session hosting the queue runner.
func TmuxSessionName() string { return "weft-queue-" + queueName }

// AgentLegacyQueueArg is the queue name accepted by the agent CLI for backward
// compatibility with existing wrappers. No other value is accepted.
const AgentLegacyQueueArg = queueName

// QueueEntry represents a job entry to be added to a remote queue
type QueueEntry struct {
	JobID        int64
	RunID        int64
	WorkingDir   string
	Command      string
	Description  string
	SourceSHA256 string
	// SourceR2Key is the content-addressed R2 key for the source tarball
	// when this job is queued in R2-isolated mode (Layer D fallback after a
	// per-job marker failure). Empty for jobs that run against the shared
	// working dir.
	SourceR2Key  string
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
