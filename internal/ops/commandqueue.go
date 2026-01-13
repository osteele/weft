// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/osteele/remote-jobs/internal/ssh"
)

// Command log operations
const (
	OpAdd      = "add"      // Add a job to the queue
	OpPriority = "priority" // Move a job to the front of the queue
	OpCancel   = "cancel"   // Remove a job from the queue
	OpStop     = "stop"     // Graceful shutdown after current job
)

// CommandJob contains job data for an add command.
type CommandJob struct {
	ID   int64    `json:"id"`
	Dir  string   `json:"dir,omitempty"`
	Cmd  string   `json:"cmd"`
	Desc string   `json:"desc,omitempty"`
	Env  []string `json:"env,omitempty"`
	Deps string   `json:"deps,omitempty"`
	CPU  *int     `json:"cpu,omitempty"`
}

// QueueCommand represents a command in the append-only command log.
// The CLI appends commands; the queue runner reads and processes them.
type QueueCommand struct {
	Timestamp string      `json:"ts"`
	Op        string      `json:"op"`
	JobID     int64       `json:"job_id,omitempty"` // For priority, cancel ops
	Job       *CommandJob `json:"job,omitempty"`    // For add op
}

// CommandsFileName returns the path to the commands file for a queue.
func CommandsFileName(queueName string) string {
	return fmt.Sprintf("%s.commands", queueName)
}

// CommandsFilePath returns the full remote path to the commands file.
func CommandsFilePath(queueName string) string {
	return fmt.Sprintf("%s/%s", QueueDir, CommandsFileName(queueName))
}

// NewAddCommand creates a command to add a job to the queue.
func NewAddCommand(entry QueueEntry) QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpAdd,
		Job: &CommandJob{
			ID:   entry.JobID,
			Dir:  entry.WorkingDir,
			Cmd:  entry.Command,
			Desc: entry.Description,
			Env:  entry.EnvVars,
			Deps: entry.DepSpec,
			CPU:  entry.CPUAllotment,
		},
	}
}

// NewPriorityCommand creates a command to move a job to the front of the queue.
func NewPriorityCommand(jobID int64) QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpPriority,
		JobID:     jobID,
	}
}

// NewCancelCommand creates a command to remove a job from the queue.
func NewCancelCommand(jobID int64) QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpCancel,
		JobID:     jobID,
	}
}

// NewStopCommand creates a command for graceful queue runner shutdown.
func NewStopCommand() QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpStop,
	}
}

// AppendCommandOptions configures the command append operation.
type AppendCommandOptions struct {
	Timeout time.Duration
}

// AppendCommand appends a command to the remote queue's command log.
// This is append-only - no locking required.
func AppendCommand(host, queueName string, cmd QueueCommand, opts AppendCommandOptions) error {
	if queueName == "" {
		queueName = DefaultQueueName
	}

	// Serialize command to JSON (single line)
	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	jsonLine := string(jsonBytes)

	commandsFile := CommandsFilePath(queueName)

	// Simple append - no locking needed for append-only log
	// Use printf to avoid echo interpretation issues
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' %q >> %s`,
		QueueDir,
		jsonLine,
		commandsFile,
	)

	var stdout, stderr string
	if opts.Timeout > 0 {
		stdout, stderr, err = ssh.RunWithTimeout(host, appendCmd, opts.Timeout)
	} else {
		stdout, stderr, err = ssh.Run(host, appendCmd)
	}
	_ = stdout

	if err != nil {
		return &QueueAppendError{Op: "append command", Stderr: stderr, Err: err}
	}

	return nil
}

// AppendCommandLocal appends a command to a local commands file (for testing).
func AppendCommandLocal(commandsFile string, cmd QueueCommand) error {
	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(commandsFile), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Append to file
	f, err := os.OpenFile(commandsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open commands file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(jsonBytes, '\n')); err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	return nil
}

// RunnerState represents the queue runner's internal state.
// Only the queue runner writes to this file.
type RunnerState struct {
	Cursor     string                   `json:"cursor"`            // Timestamp of last processed command
	CursorLine int                      `json:"cursor_line"`       // Line number of last processed command
	Pending    []int64                  `json:"pending"`           // Job IDs waiting to run (in order)
	Current    *int64                   `json:"current"`           // Currently running job ID (nil if none)
	Running    map[int64]RunnerJobState `json:"running,omitempty"` // Active jobs keyed by ID
}

// RunnerJobState captures per-job runtime state for concurrent execution.
type RunnerJobState struct {
	StartedAt      int64 `json:"started_at"`
	WarmupUntil    int64 `json:"warmup_until"`
	LocalAllotment int   `json:"local_allotment"`
	Samples        []int `json:"samples,omitempty"`
	OverCount      int   `json:"over_count"`
	UnderCount     int   `json:"under_count"`
}

// StateFileName returns the filename for the runner state file.
func StateFileName(queueName string) string {
	return fmt.Sprintf("%s.state.json", queueName)
}

// StateFilePath returns the full remote path to the state file.
func StateFilePath(queueName string) string {
	return fmt.Sprintf("%s/%s", QueueDir, StateFileName(queueName))
}
