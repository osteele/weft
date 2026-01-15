package remote

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

const (
	// QueueDir is the remote directory for queue state files
	QueueDir = "~/.cache/remote-jobs/queue"
)

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
func (h *SSHHost) IsJobInQueue(queueName string, jobID int64) (bool, error) {
	stateFile := fmt.Sprintf("%s/%s.state.json", QueueDir, queueName)
	// Redirect jq output to /dev/null to avoid output pollution
	cmd := fmt.Sprintf("jq -e '.pending | index(%d) != null' %s >/dev/null 2>&1 && echo YES || echo NO", jobID, stateFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) == "YES", nil
}

// IsJobCurrent checks if a job is the currently running job in the queue.
func (h *SSHHost) IsJobCurrent(queueName string, jobID int64) (bool, error) {
	currentFile := fmt.Sprintf("%s/%s.current", QueueDir, queueName)
	cmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err := ssh.RunWithTimeout(h.hostname, cmd, h.timeout)
	if err != nil {
		return false, err
	}
	currentID := strings.TrimSpace(stdout)
	return currentID == fmt.Sprintf("%d", jobID), nil
}

// AppendToQueue adds a job to the queue's command log.
func (h *SSHHost) AppendToQueue(queueName string, entry QueueEntry) error {
	cmd := queueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        "add",
		Job: &commandJob{
			ID:   entry.JobID,
			Dir:  entry.WorkingDir,
			Cmd:  entry.Command,
			Desc: entry.Description,
			Env:  entry.EnvVars,
			Deps: entry.DepSpec,
			CPU:  entry.CPUAllotment,
		},
	}

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	commandsFile := fmt.Sprintf("%s/%s.commands", QueueDir, queueName)
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' %q >> %s`,
		QueueDir,
		string(jsonBytes),
		commandsFile,
	)

	_, stderr, err := ssh.RunWithTimeout(h.hostname, appendCmd, h.timeout)
	if err != nil {
		return fmt.Errorf("append to queue: %s: %w", stderr, err)
	}
	return nil
}

// RemoveFromQueue removes a job from the queue by appending a cancel command.
func (h *SSHHost) RemoveFromQueue(queueName string, jobID int64) error {
	cmd := queueCommand{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Op:        "cancel",
		JobID:     jobID,
	}

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	commandsFile := fmt.Sprintf("%s/%s.commands", QueueDir, queueName)
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && printf '%%s\n' %q >> %s`,
		QueueDir,
		string(jsonBytes),
		commandsFile,
	)

	_, stderr, err := ssh.RunWithTimeout(h.hostname, appendCmd, h.timeout)
	if err != nil {
		return fmt.Errorf("remove from queue: %s: %w", stderr, err)
	}
	return nil
}

// GetJobCompletion checks if a job has completed and returns its exit info.
// Returns nil if the job has not completed.
func (h *SSHHost) GetJobCompletion(jobID int64) (*CompletionInfo, error) {
	statusPattern := session.StatusFilePattern(jobID)
	// Get both exit code content and file mtime in one command
	cmd := fmt.Sprintf(`f=$(ls %s 2>/dev/null | head -1); if [ -n "$f" ]; then echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)"; fi`, statusPattern)
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

	exitCode, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("parse exit code: %w", err)
	}

	var mtime int64
	if len(parts) >= 2 {
		mtime, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}

	return &CompletionInfo{ExitCode: exitCode, EndTime: mtime}, nil
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

// Internal types for JSON serialization

type queueCommand struct {
	Timestamp string      `json:"ts"`
	Op        string      `json:"op"`
	JobID     int64       `json:"job_id,omitempty"`
	Job       *commandJob `json:"job,omitempty"`
}

type commandJob struct {
	ID   int64    `json:"id"`
	Dir  string   `json:"dir,omitempty"`
	Cmd  string   `json:"cmd"`
	Desc string   `json:"desc,omitempty"`
	Env  []string `json:"env,omitempty"`
	Deps string   `json:"deps,omitempty"`
	CPU  *int     `json:"cpu,omitempty"`
}
