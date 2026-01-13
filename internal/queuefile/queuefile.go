package queuefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/osteele/remote-jobs/internal/ssh"
)

// DefaultQueueName is used when no explicit queue name is provided.
const DefaultQueueName = "default"

const queueDir = "~/.cache/remote-jobs/queue"

// Entry represents a job line stored in the remote queue file.
type Entry struct {
	JobID       int64
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	DepSpec     string
}

// ErrConnection indicates the queue file couldn't be accessed due to an SSH connection failure.
var ErrConnection = errors.New("queuefile connection error")

// IsConnectionError reports whether err was caused by an SSH connection failure.
func IsConnectionError(err error) bool {
	return errors.Is(err, ErrConnection)
}

func jobFilePath(jobID int64) string {
	return fmt.Sprintf("%s/jobs/%d.json", queueDir, jobID)
}

// jobData represents the JSON structure of a job file
type jobData struct {
	ID   int64    `json:"id"`
	Dir  string   `json:"dir"`
	Cmd  string   `json:"cmd"`
	Desc string   `json:"desc"`
	Env  []string `json:"env"`
	Deps string   `json:"deps"`
}

// FetchEntry retrieves the queue file entry for a job ID from the remote host.
func FetchEntry(host, queueName string, jobID int64) (*Entry, error) {
	jobFile := jobFilePath(jobID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null || true", jobFile)
	stdout, stderr, err := ssh.Run(host, cmd)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return nil, fmt.Errorf("%w: %s", ErrConnection, errMsg)
		}
		return nil, fmt.Errorf("read job file: %s", errMsg)
	}
	content := strings.TrimSpace(stdout)
	if content == "" {
		return nil, fmt.Errorf("job %d not found in queue %s on %s", jobID, queueName, host)
	}

	var data jobData
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return nil, fmt.Errorf("parse job file for job %d: %w", jobID, err)
	}

	return &Entry{
		JobID:       data.ID,
		WorkingDir:  data.Dir,
		Command:     data.Cmd,
		Description: data.Desc,
		EnvVars:     data.Env,
		DepSpec:     data.Deps,
	}, nil
}

func commandsFilePath(queueName string) string {
	if queueName == "" {
		queueName = DefaultQueueName
	}
	return fmt.Sprintf("%s/%s.commands", queueDir, queueName)
}

func stateFilePath(queueName string) string {
	if queueName == "" {
		queueName = DefaultQueueName
	}
	return fmt.Sprintf("%s/%s.state.json", queueDir, queueName)
}

// appendCommand appends a command to the commands file
func appendCommand(host, queueName, cmdJSON string) error {
	commandsFile := commandsFilePath(queueName)
	// Append command to the commands file with locking
	lockDir := commandsFile + ".lock.d"
	appendCmd := fmt.Sprintf(
		`mkdir -p %s && (
			while ! mkdir %s 2>/dev/null; do sleep 0.01; done
			trap 'rmdir %s 2>/dev/null' EXIT
			echo '%s' >> %s
			rmdir %s 2>/dev/null
		)`,
		queueDir,
		lockDir,
		lockDir,
		cmdJSON, commandsFile,
		lockDir)

	_, stderr, err := ssh.Run(host, appendCmd)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("%w: %s", ErrConnection, errMsg)
		}
		return fmt.Errorf("append command: %s", errMsg)
	}
	return nil
}

// RemoveEntry deletes a queued job entry from the remote queue.
func RemoveEntry(host, queueName string, jobID int64) error {
	cmdJSON := fmt.Sprintf(`{"op":"cancel","job_id":%d}`, jobID)
	return appendCommand(host, queueName, cmdJSON)
}

// MoveToFront moves a job to the front of the queue (next to run after current job).
// Returns true if the job was moved, false if it was already at the front or not found.
func MoveToFront(host, queueName string, jobID int64) (bool, error) {
	// Check if job is in the pending list
	stateFile := stateFilePath(queueName)
	checkCmd := fmt.Sprintf("jq -e '.pending | index(%d) != null' %s 2>/dev/null && echo YES || echo NO", jobID, stateFile)
	stdout, stderr, err := ssh.Run(host, checkCmd)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return false, fmt.Errorf("%w: %s", ErrConnection, errMsg)
		}
		return false, fmt.Errorf("check job %d in queue: %s", jobID, errMsg)
	}

	result := strings.TrimSpace(stdout)
	if result != "YES" {
		return false, fmt.Errorf("job %d not found in queue %s", jobID, queueName)
	}

	// Check if already at front
	frontCmd := fmt.Sprintf("jq -r '.pending[0] // \"\"' %s 2>/dev/null", stateFile)
	frontStdout, _, _ := ssh.Run(host, frontCmd)
	if strings.TrimSpace(frontStdout) == fmt.Sprintf("%d", jobID) {
		return false, nil // Already at front
	}

	// Send priority command
	cmdJSON := fmt.Sprintf(`{"op":"priority","job_id":%d}`, jobID)
	if err := appendCommand(host, queueName, cmdJSON); err != nil {
		return false, err
	}

	return true, nil
}
