package queuerunner

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/ssh"
)

// StatusInfo captures queue runner state and queue depth.
type StatusInfo struct {
	RunnerActive   bool
	QueuedJobCount int
	CurrentJob     string
	StopPending    bool
}

// StatusCommand returns the SSH command that gathers queue runner status.
func StatusCommand() string {
	queueName := opsqueue.DefaultQueueName
	return fmt.Sprintf(
		`tmux has-session -t 'weft-queue-%s' 2>/dev/null && echo "RUNNER:yes" || echo "RUNNER:no"; `+
			`PATH="$HOME/.local/bin:$PATH" jq -r '.current // ""' ~/.cache/weft/queue/%s.state.json 2>/dev/null | sed 's/^/CURRENT:/' || echo "CURRENT:"; `+
			`PATH="$HOME/.local/bin:$PATH" jq -r '.pending | length // 0' ~/.cache/weft/queue/%s.state.json 2>/dev/null | sed 's/^/DEPTH:/' || echo "DEPTH:0"; `+
			`test -f ~/.cache/weft/queue/%s.stop && echo "STOP:yes" || echo "STOP:no"`,
		queueName, queueName, queueName, queueName)
}

// ParseStatus parses the output of StatusCommand into a StatusInfo struct.
func ParseStatus(output string) *StatusInfo {
	info := &StatusInfo{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			key := line[:idx]
			value := strings.TrimSpace(line[idx+1:])
			switch key {
			case "RUNNER":
				info.RunnerActive = value == "yes"
			case "CURRENT":
				info.CurrentJob = value
			case "DEPTH":
				if value == "" {
					value = "0"
				}
				var depth int
				fmt.Sscanf(value, "%d", &depth)
				info.QueuedJobCount = depth
			case "STOP":
				info.StopPending = value == "yes"
			}
		}
	}
	return info
}

// FetchStatus queries the remote host for queue status information.
func FetchStatus(host string, timeout time.Duration) (*StatusInfo, error) {
	stdout, _, err := ssh.RunWithTimeout(host, StatusCommand(), timeout)
	if err != nil {
		return nil, err
	}
	return ParseStatus(stdout), nil
}

// Status returns the runner status using the provided timeout.
func (r *Runner) Status(timeout time.Duration) (*StatusInfo, error) {
	return FetchStatus(r.host, timeout)
}
