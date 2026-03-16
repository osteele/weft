// Package ops provides unified job operation functions used by both CLI and TUI.
package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// Command log operations
const (
	OpAdd      = "add"      // Add a job to the queue
	OpPriority = "priority" // Move a job to the front of the queue
	OpCancel   = "cancel"   // Remove a job from the queue
	OpStop     = "stop"     // Graceful shutdown after current job
	OpRestart  = "restart"  // Re-exec to pick up new script version
)

// CommandJob contains job data for an add command.
type CommandJob struct {
	ID         int64    `json:"id"`
	Dir        string   `json:"dir,omitempty"`
	Cmd        string   `json:"cmd"`
	Desc       string   `json:"desc,omitempty"`
	Env        []string `json:"env,omitempty"`
	Deps       string   `json:"deps,omitempty"`
	CPU        *int     `json:"cpu,omitempty"`
	GPU        string   `json:"gpu,omitempty"`       // CUDA_VISIBLE_DEVICES value (e.g. "0" or "0,1")
	GPUClass   string   `json:"gpu_class,omitempty"` // GPU class name (e.g. "A100") — resolved to device at runtime
	GPUMem     *int     `json:"gpu_mem,omitempty"`   // GPU memory reservation in GB per device
	Tags       []string `json:"tags,omitempty"`
	OutputDirs []string `json:"output_dirs,omitempty"` // convention-based output directories from .weft.toml
	Produces   []string `json:"produces,omitempty"`    // artifact specs this job produces
	Needs      []string `json:"needs,omitempty"`       // artifact specs this job needs
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
func CommandsFileName() string {
	return fmt.Sprintf("%s.commands", DefaultQueueName)
}

// CommandsFilePath returns the full remote path to the commands file.
func CommandsFilePath() string {
	return fmt.Sprintf("%s/%s", QueueDir, CommandsFileName())
}

// NewAddCommand creates a command to add a job to the queue.
func NewAddCommand(entry QueueEntry) QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpAdd,
		Job: &CommandJob{
			ID:         entry.JobID,
			Dir:        entry.WorkingDir,
			Cmd:        entry.Command,
			Desc:       entry.Description,
			Env:        entry.EnvVars,
			Deps:       entry.DepSpec,
			CPU:        entry.CPUAllotment,
			GPU:        entry.GPU,
			GPUClass:   entry.GPUClass,
			GPUMem:     entry.GPUMemGB,
			Tags:       entry.Tags,
			OutputDirs: entry.OutputDirs,
			Produces:   entry.Produces,
			Needs:      entry.Needs,
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

// NewRestartCommand creates a command for the queue runner to re-exec itself.
// This is used when the script has been upgraded and the runner needs to pick up the new version.
func NewRestartCommand() QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpRestart,
	}
}

// AppendCommandOptions configures the command append operation.
type AppendCommandOptions struct {
	Timeout time.Duration
}

// AppendCommand appends a command to the remote queue's command log.
// This is append-only - no locking required.
func AppendCommand(host string, cmd QueueCommand, opts AppendCommandOptions) error {
	// Serialize command to JSON (single line)
	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	jsonLine := string(jsonBytes)

	commandsFile := CommandsFilePath()
	appendCmd := buildAppendShellCommand(jsonLine, commandsFile)

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

// buildAppendShellCommand constructs a shell command that appends a JSON line
// to the commands file. Uses single quotes to prevent shell expansion of $(),
// backticks, and other shell metacharacters in the JSON content.
func buildAppendShellCommand(jsonLine, commandsFile string) string {
	// Escape single quotes: replace ' with '\'' (end quote, escaped quote, start quote)
	escaped := strings.ReplaceAll(jsonLine, "'", `'\''`)
	return fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' '%s' >> %s`,
		QueueDir,
		escaped,
		commandsFile,
	)
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
	Cursor     string                    `json:"cursor"`            // Timestamp of last processed command
	CursorLine int                       `json:"cursor_line"`       // Line number of last processed command
	Pending    []int64                   `json:"pending"`           // Job IDs waiting to run (in order)
	Current    *int64                    `json:"current"`           // Currently running job ID (nil if none)
	Running    map[string]RunnerJobState `json:"running,omitempty"` // Active jobs keyed by string ID (matches runner JSON format)
}

// RunnerJobState captures per-job runtime state for concurrent execution.
type RunnerJobState struct {
	StartedAt      int64    `json:"started_at"`
	WarmupUntil    int64    `json:"warmup_until"`
	LocalAllotment int      `json:"local_allotment"`
	Samples        []int    `json:"samples,omitempty"`
	OverCount      int      `json:"over_count"`
	UnderCount     int      `json:"under_count"`
	GPUDevices     []string `json:"gpu_devices,omitempty"` // Which GPU indices this job uses
	GPUMemGB       int      `json:"gpu_mem_gb,omitempty"`  // Reserved GB per device
}

// StateFileName returns the filename for the runner state file.
func StateFileName() string {
	return fmt.Sprintf("%s.state.json", DefaultQueueName)
}

// StateFilePath returns the full remote path to the state file.
func StateFilePath() string {
	return fmt.Sprintf("%s/%s", QueueDir, StateFileName())
}
