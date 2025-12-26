package queuefile

import (
	"encoding/base64"
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

func queueFilePath(queueName string) string {
	if queueName == "" {
		queueName = DefaultQueueName
	}
	return fmt.Sprintf("%s/%s.queue", queueDir, queueName)
}

// FetchEntry retrieves the queue file entry for a job ID from the remote host.
func FetchEntry(host, queueName string, jobID int64) (*Entry, error) {
	queueFile := queueFilePath(queueName)
	cmd := fmt.Sprintf("grep -m1 '^%d\\\\t' %s 2>/dev/null || true", jobID, queueFile)
	stdout, stderr, err := ssh.Run(host, cmd)
	if err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return nil, fmt.Errorf("%w: %s", ErrConnection, errMsg)
		}
		return nil, fmt.Errorf("read queue file: %s", errMsg)
	}
	line := strings.TrimSpace(stdout)
	if line == "" {
		return nil, fmt.Errorf("job %d not found in queue %s on %s", jobID, queueName, host)
	}
	parts := strings.Split(line, "\t")
	if len(parts) < 3 {
		return nil, fmt.Errorf("malformed queue entry for job %d: %q", jobID, line)
	}

	entry := &Entry{
		JobID:       jobID,
		WorkingDir:  parts[1],
		Command:     parts[2],
		Description: "",
		EnvVars:     nil,
		DepSpec:     "",
	}

	if len(parts) >= 4 {
		entry.Description = parts[3]
	}
	if len(parts) >= 5 && parts[4] != "" {
		decoded, err := base64.StdEncoding.DecodeString(parts[4])
		if err == nil {
			for _, line := range strings.Split(string(decoded), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					entry.EnvVars = append(entry.EnvVars, line)
				}
			}
		}
	}
	if len(parts) >= 6 {
		entry.DepSpec = parts[5]
	}

	return entry, nil
}

// RemoveEntry deletes a queued job entry from the remote queue file.
func RemoveEntry(host, queueName string, jobID int64) error {
	queueFile := queueFilePath(queueName)
	removeCmd := fmt.Sprintf("grep -v '^%d\\t' %s > %s.tmp 2>/dev/null && mv %s.tmp %s || true",
		jobID, queueFile, queueFile, queueFile, queueFile)
	if _, stderr, err := ssh.Run(host, removeCmd); err != nil {
		errMsg := strings.TrimSpace(stderr)
		if errMsg == "" {
			errMsg = err.Error()
		}
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("%w: %s", ErrConnection, errMsg)
		}
		return fmt.Errorf("remove queued job %d: %s", jobID, errMsg)
	}
	return nil
}
