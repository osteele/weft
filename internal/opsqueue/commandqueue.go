package opsqueue

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
	ID        int64  `json:"id"`
	RunID     int64  `json:"run_id,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Cmd       string `json:"cmd"`
	Desc      string `json:"desc,omitempty"`
	SourceSHA string `json:"source_sha256,omitempty"`
	// SourceR2Key, when set, tells the runner to skip the per-job marker
	// check and instead download + extract the exact recorded v1 or v2 source
	// key from R2 into a per-job dir. Used by Layer D as a content-fidelity
	// fallback after a per-job marker failure.
	SourceR2Key  string   `json:"source_r2_key,omitempty"`
	Env          []string `json:"env,omitempty"`
	Deps         string   `json:"deps,omitempty"`
	CPU          *int     `json:"cpu,omitempty"`
	GPU          string   `json:"gpu,omitempty"`       // CUDA_VISIBLE_DEVICES value (e.g. "0" or "0,1")
	GPUClass     string   `json:"gpu_class,omitempty"` // GPU class name (e.g. "A100") — resolved to device at runtime
	GPUCount     int      `json:"gpu_count,omitempty"` // Exact GPU count requested on this host
	GPUMem       *int     `json:"gpu_mem,omitempty"`   // GPU memory reservation in GB per device
	Interconnect string   `json:"interconnect,omitempty"`
	CPUCores     int      `json:"cpu_cores,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	OutputDirs   []string `json:"output_dirs,omitempty"` // convention-based output directories from .weft.toml
	Outputs      []string `json:"outputs,omitempty"`     // declared output refs from PEP 723/CLI
	Produces     []string `json:"produces,omitempty"`    // artifact specs this job produces
	Needs        []string `json:"needs,omitempty"`       // artifact specs this job needs
}

// QueueCommand represents a command in the append-only command log.
// The CLI appends commands; the queue runner reads and processes them.
type QueueCommand struct {
	Timestamp string      `json:"ts"`
	Op        string      `json:"op"`
	JobID     int64       `json:"job_id,omitempty"` // For priority, cancel ops
	Job       *CommandJob `json:"job,omitempty"`    // For add op
	Env       []string    `json:"env,omitempty"`    // For restart ops
}

// CommandsFileName returns the path to the commands file for a queue.
func CommandsFileName() string {
	return queueName + ".commands"
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
			ID:           entry.JobID,
			RunID:        entry.RunID,
			Dir:          entry.WorkingDir,
			Cmd:          entry.Command,
			Desc:         entry.Description,
			SourceSHA:    entry.SourceSHA256,
			SourceR2Key:  entry.SourceR2Key,
			Env:          entry.EnvVars,
			Deps:         entry.DepSpec,
			CPU:          entry.CPUAllotment,
			GPU:          entry.GPU,
			GPUClass:     entry.GPUClass,
			GPUCount:     entry.GPUCount,
			GPUMem:       entry.GPUMemGB,
			Interconnect: entry.Interconnect,
			CPUCores:     entry.CPUCores,
			Tags:         entry.Tags,
			OutputDirs:   entry.OutputDirs,
			Outputs:      entry.Outputs,
			Produces:     entry.Produces,
			Needs:        entry.Needs,
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
func NewRestartCommand() QueueCommand {
	return NewRestartCommandWithEnv(nil)
}

// NewRestartCommandWithEnv creates a restart command with environment overrides.
func NewRestartCommandWithEnv(env []string) QueueCommand {
	return QueueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        OpRestart,
		Env:       env,
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
// to the commands file.
func buildAppendShellCommand(jsonLine, commandsFile string) string {
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

	if err := os.MkdirAll(filepath.Dir(commandsFile), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

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
type RunnerState struct {
	Cursor     string                         `json:"cursor"`
	CursorLine int                            `json:"cursor_line"`
	Pending    []int64                        `json:"pending"`
	Current    *int64                         `json:"current"`
	Running    map[string]RunnerJobState      `json:"running,omitempty"`
	Finished   map[string]RunnerFinishedState `json:"finished,omitempty"`
}

// RunnerFinishedState records the runner's terminal entry for a job. The
// laptop needs presence-only awareness so the forward-reconcile path in
// ensureQueuedJobsOnRemote does not re-dispatch jobs the runner has already
// completed (which, after the archive-on-add fix, is no longer a no-op and
// would re-run them).
type RunnerFinishedState struct {
	ExitCode   int   `json:"exit_code"`
	FinishedAt int64 `json:"finished_at"`
}

// RunnerJobState captures per-job runtime state for concurrent execution.
type RunnerJobState struct {
	RunID          int64    `json:"run_id,omitempty"`
	StartedAt      int64    `json:"started_at"`
	WarmupUntil    int64    `json:"warmup_until"`
	LocalAllotment int      `json:"local_allotment"`
	Samples        []int    `json:"samples,omitempty"`
	OverCount      int      `json:"over_count"`
	UnderCount     int      `json:"under_count"`
	GPUDevices     []string `json:"gpu_devices,omitempty"`
	GPUMemGB       int      `json:"gpu_mem_gb,omitempty"`
}

// StateFileName returns the filename for the runner state file.
func StateFileName() string {
	return queueName + ".state.json"
}

// StateFilePath returns the full remote path to the state file.
func StateFilePath() string {
	return fmt.Sprintf("%s/%s", QueueDir, StateFileName())
}
