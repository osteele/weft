package remote

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/secrets"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
)

// QueueDir is the remote directory for queue state files.
const QueueDir = opsqueue.QueueDir

// SSHHost implements the Host interface using SSH.
type SSHHost struct {
	hostname string
	timeout  time.Duration
}

// SSHHostFactory creates SSHHost instances.
type SSHHostFactory struct{}

// ForHost returns a Host for the given hostname.
func (f SSHHostFactory) ForHost(hostname string, timeout time.Duration) Host {
	return &SSHHost{hostname: hostname, timeout: timeout}
}

// NewSSHHost creates a new SSHHost for the given hostname.
func NewSSHHost(hostname string, timeout time.Duration) *SSHHost {
	return &SSHHost{hostname: hostname, timeout: timeout}
}

// IsJobInQueue checks if a job is in the queue's pending list.
func (h *SSHHost) IsJobInQueue(jobID int64) (bool, error) {
	stateFile := opsqueue.StateFilePath()
	// Redirect jq output to /dev/null to avoid output pollution
	cmd := fmt.Sprintf("jq -e '.pending | index(%d) != null' %s >/dev/null 2>&1 && echo YES || echo NO", jobID, stateFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) == "YES", nil
}

// IsJobCurrent checks if a job is the currently running job in the queue.
func (h *SSHHost) IsJobCurrent(jobID int64) (bool, error) {
	currentFile := opsqueue.CurrentFilePath()
	cmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	currentID := strings.TrimSpace(stdout)
	return currentID == fmt.Sprintf("%d", jobID), nil
}

// AppendToQueue adds a job to the queue's command log.
func (h *SSHHost) AppendToQueue(entry QueueEntry) error {
	resolvedEnv, err := secrets.ResolveEnvVars(entry.EnvVars)
	if err != nil {
		return err
	}
	cmd := queueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        "add",
		Job: &commandJob{
			ID:         entry.JobID,
			Dir:        entry.WorkingDir,
			Cmd:        entry.Command,
			Desc:       entry.Description,
			SourceSHA:  entry.SourceSHA256,
			Env:        resolvedEnv,
			Deps:       entry.DepSpec,
			CPU:        entry.CPUAllotment,
			GPU:        entry.GPU,
			GPUClass:   entry.GPUClass,
			GPUMem:     entry.GPUMemGB,
			Tags:       entry.Tags,
			OutputDirs: entry.OutputDirs,
			Outputs:    entry.Outputs,
			Produces:   entry.Produces,
			Needs:      entry.Needs,
		},
	}

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	commandsFile := opsqueue.CommandsFilePath()
	// Use single quotes to prevent shell expansion of $(), backticks, etc.
	escaped := strings.ReplaceAll(string(jsonBytes), "'", `'\''`)
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' '%s' >> %s`,
		QueueDir,
		escaped,
		commandsFile,
	)

	_, stderr, err := ssh.RunWithTimeout(h.hostname, appendCmd, h.timeout)
	if err != nil {
		return fmt.Errorf("append to queue: %s: %w", stderr, err)
	}
	return nil
}

// RemoveFromQueue removes a job from the queue by appending a cancel command.
func (h *SSHHost) RemoveFromQueue(jobID int64) error {
	cmd := queueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        "cancel",
		JobID:     jobID,
	}

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	commandsFile := opsqueue.CommandsFilePath()
	escaped := strings.ReplaceAll(string(jsonBytes), "'", `'\''`)
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' '%s' >> %s`,
		QueueDir,
		escaped,
		commandsFile,
	)

	_, stderr, err := ssh.RunWithTimeout(h.hostname, appendCmd, h.timeout)
	if err != nil {
		return fmt.Errorf("remove from queue: %s: %w", stderr, err)
	}
	return nil
}

// IsJobInCommandsFile checks if a job ID appears in the default commands file (for testing).
func (h *SSHHost) IsJobInCommandsFile(jobID int64) (bool, error) {
	commandsFile := opsqueue.CommandsFilePath()
	cmd := fmt.Sprintf(`grep -q '"id":%d' %s 2>/dev/null && echo YES || echo NO`, jobID, commandsFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) == "YES", nil
}

// GetLastCommandForJob returns the last command entry for a job ID from the default commands file.
func (h *SSHHost) GetLastCommandForJob(jobID int64) (string, error) {
	commandsFile := opsqueue.CommandsFilePath()
	cmd := fmt.Sprintf(`grep '"id":%d' %s 2>/dev/null | tail -1`, jobID, commandsFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// GetJobCompletion checks if a job has completed and returns its exit info.
// Returns nil if the job has not completed.
func (h *SSHHost) GetJobCompletion(jobID int64) (*CompletionInfo, error) {
	statusPattern := session.StatusFilePattern(jobID)
	statusFile := session.SimpleStatusFile(jobID)
	// Get both exit code content and file mtime in one command
	cmd := fmt.Sprintf(`if [ -f %s ]; then f=%s; else f=$(ls %s 2>/dev/null | head -1); fi; if [ -n "$f" ]; then c="${f%%.status}.completion.json"; rid=""; if [ -f "$c" ]; then rid=$(sed -n 's/.*"run_id"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$c" | head -1); fi; echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)|${rid}"; fi`, statusFile, statusFile, statusPattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return nil, err
	}

	output := strings.TrimSpace(stdout)
	if output == "" {
		return nil, nil // Not completed
	}

	parts := strings.Split(output, "|")
	if len(parts) < 1 {
		return nil, fmt.Errorf("invalid status file format: %q", output)
	}

	var exitCode int
	if _, err := fmt.Sscanf(parts[0], "%d", &exitCode); err != nil {
		return nil, fmt.Errorf("parse exit code from %q: %w", parts[0], err)
	}

	var mtime int64
	if len(parts) >= 2 {
		mtime, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	var runID int64
	if len(parts) >= 3 {
		runID, _ = strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	}

	return &CompletionInfo{ExitCode: exitCode, EndTime: mtime, RunID: runID}, nil
}

// IsProcessRunning checks if the job's process is still running via PID file.
func (h *SSHHost) IsProcessRunning(jobID int64) (bool, error) {
	pidPattern := session.PidFilePattern(jobID)
	cmd := fmt.Sprintf(`pid=$(cat %s 2>/dev/null | head -1); [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1 && echo YES || echo NO`, pidPattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) == "YES", nil
}

// IsProcessPaused checks if the job's process is paused (stopped) via PGID file.
// The PGID file contains the process group leader (the setsid process), which is
// what we signal for pause/resume. Falls back to PID file if PGID file doesn't exist.
//
// Note on pause detection: When a job is paused via SIGSTOP to the process group,
// the PGID (process group leader, e.g., "uv" or "python") will have state "T" (stopped),
// but the parent bash shell (PID) may still show state "S+" (running/sleeping).
// This is expected because we signal the process GROUP (-pgid), not the wrapper shell.
// We detect pause by checking the PGID's state, not the shell's state.
func (h *SSHHost) IsProcessPaused(jobID int64) (bool, error) {
	pgidFile := session.SimplePgidFile(jobID)
	pidPattern := session.PidFilePattern(jobID)
	// Try PGID file first (preferred), then fall back to PID file
	cmd := fmt.Sprintf(`pgid=$(cat %s 2>/dev/null | head -1); if [ -z "$pgid" ]; then pgid=$(cat %s 2>/dev/null | head -1); fi; if [ -n "$pgid" ]; then state=$(ps -o stat= -p $pgid 2>/dev/null | tr -d ' '); case "$state" in *T*) echo YES ;; *) echo NO ;; esac; else echo NO; fi`, pgidFile, pidPattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) == "YES", nil
}

// GetJobMetadata reads and parses the job's metadata file.
func (h *SSHHost) GetJobMetadata(jobID int64) (map[string]string, error) {
	metadataPattern := session.MetadataFilePattern(jobID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", metadataPattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	return session.ParseMetadata(stdout), nil
}

// GetJobSamples reads the job's CPU samples file.
func (h *SSHHost) GetJobSamples(jobID int64) (string, error) {
	samplesPattern := session.SamplesFilePattern(jobID)
	cmd := fmt.Sprintf(`f=$(ls -t %s 2>/dev/null | head -1); if [ -n "$f" ]; then cat "$f"; fi`, samplesPattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

// GetJobRusage reads the job's resource usage file.
func (h *SSHHost) GetJobRusage(jobID int64) (string, error) {
	rusagePattern := session.RusageFilePattern(jobID)
	cmd := fmt.Sprintf(`f=$(ls -t %s 2>/dev/null | head -1); if [ -n "$f" ]; then cat "$f"; fi`, rusagePattern)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

// TmuxSessionExists checks if a tmux session exists.
func (h *SSHHost) TmuxSessionExists(sessionName string) (bool, error) {
	return ssh.TmuxSessionExistsQuickTimeout(h.hostname, sessionName, h.timeout)
}

// StartTmuxSession starts a new tmux session with the given command.
func (h *SSHHost) StartTmuxSession(sessionName, command string) error {
	escapedCommand := ssh.EscapeForSingleQuotes(command)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", sessionName, escapedCommand)
	_, stderr, err := ssh.RunWithTimeout(h.hostname, tmuxCmd, h.timeout)
	if err != nil {
		return fmt.Errorf("start tmux: %s: %w", stderr, err)
	}
	return nil
}

// KillTmuxSession kills an existing tmux session.
func (h *SSHHost) KillTmuxSession(sessionName string) error {
	return ssh.TmuxKillSession(h.hostname, sessionName)
}

// GetRecentlyModifiedJobIDs returns job IDs that have had files modified since the given time.
// This is used to optimize restart detection by only checking jobs that might have restarted.
func (h *SSHHost) GetRecentlyModifiedJobIDs(since time.Time) ([]int64, error) {
	if since.IsZero() {
		return nil, nil // Return empty if no previous check
	}

	logsDir := session.LogDir

	// Calculate minutes ago (add 1 minute buffer for clock skew)
	minutesAgo := int(time.Since(since).Minutes()) + 1
	if minutesAgo < 1 {
		minutesAgo = 1
	}

	// Use -mmin which is relative to remote's current time (avoids timezone issues)
	cmd := fmt.Sprintf(
		`find %s -mmin -%d \( -name "*.pid" -o -name "*.status" \) 2>/dev/null | sed 's|.*/||; s/-.*//; s/\..*$//' | sort -un`,
		logsDir, minutesAgo,
	)

	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return nil, err
	}

	var jobIDs []int64
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue // Skip invalid entries
		}
		jobIDs = append(jobIDs, id)
	}

	return jobIDs, nil
}

// Internal types for JSON serialization

type queueCommand struct {
	Timestamp string      `json:"ts"`
	Op        string      `json:"op"`
	JobID     int64       `json:"job_id,omitempty"`
	Job       *commandJob `json:"job,omitempty"`
}

type commandJob struct {
	ID         int64    `json:"id"`
	Dir        string   `json:"dir,omitempty"`
	Cmd        string   `json:"cmd"`
	Desc       string   `json:"desc,omitempty"`
	SourceSHA  string   `json:"source_sha256,omitempty"`
	Env        []string `json:"env,omitempty"`
	Deps       string   `json:"deps,omitempty"`
	CPU        *int     `json:"cpu,omitempty"`
	GPU        string   `json:"gpu,omitempty"`
	GPUClass   string   `json:"gpu_class,omitempty"`
	GPUMem     *int     `json:"gpu_mem,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	OutputDirs []string `json:"output_dirs,omitempty"`
	Outputs    []string `json:"outputs,omitempty"`
	Produces   []string `json:"produces,omitempty"`
	Needs      []string `json:"needs,omitempty"`
}
