package queuerunner

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// StatusInfo captures queue runner state and queue depth.
type StatusInfo struct {
	RunnerActive   bool
	QueuedJobCount int
	CurrentJob     string
	StopPending    bool
	JqMissing      bool // jq is required but not installed
}

func statusCommand() string {
	queueName := ops.DefaultQueueName
	return fmt.Sprintf(
		// Check for jq availability first (check ~/.local/bin/jq as well)
		`(command -v jq >/dev/null 2>&1 || test -x ~/.local/bin/jq) && echo "JQ:yes" || echo "JQ:no"; `+
			`tmux has-session -t 'rj-queue-%s' 2>/dev/null && echo "RUNNER:yes" || echo "RUNNER:no"; `+
			`PATH="$HOME/.local/bin:$PATH" jq -r '.current // ""' ~/.cache/remote-jobs/queue/%s.state.json 2>/dev/null | sed 's/^/CURRENT:/' || echo "CURRENT:"; `+
			`PATH="$HOME/.local/bin:$PATH" jq -r '.pending | length // 0' ~/.cache/remote-jobs/queue/%s.state.json 2>/dev/null | sed 's/^/DEPTH:/' || echo "DEPTH:0"; `+
			`test -f ~/.cache/remote-jobs/queue/%s.stop && echo "STOP:yes" || echo "STOP:no"`,
		queueName, queueName, queueName, queueName)
}

func parseStatus(output string) *StatusInfo {
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
			case "JQ":
				info.JqMissing = value != "yes"
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
	stdout, _, err := ssh.RunWithTimeout(host, statusCommand(), timeout)
	if err != nil {
		return nil, err
	}
	return parseStatus(stdout), nil
}

// Status returns the runner status using the provided timeout.
func (r *Runner) Status(timeout time.Duration) (*StatusInfo, error) {
	return FetchStatus(r.host, timeout)
}
