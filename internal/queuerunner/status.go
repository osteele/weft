package queuerunner

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// StatusInfo captures queue runner state and queue depth.
type StatusInfo struct {
	RunnerActive   bool
	QueuedJobCount int
	CurrentJob     string
	StopPending    bool
	BlockedReasons map[int64]string
}

// StatusCommand returns the SSH command that gathers queue runner status.
func StatusCommand() string {
	return fmt.Sprintf(
		`tmux has-session -t 'weft-queue-default' 2>/dev/null && echo "RUNNER:yes" || echo "RUNNER:no"; ` +
			`PATH="$HOME/.local/bin:$PATH" jq -r '.current // ""' ~/.cache/weft/queue/default.state.json 2>/dev/null | sed 's/^/CURRENT:/' || echo "CURRENT:"; ` +
			`PATH="$HOME/.local/bin:$PATH" jq -r '.pending | length // 0' ~/.cache/weft/queue/default.state.json 2>/dev/null | sed 's/^/DEPTH:/' || echo "DEPTH:0"; ` +
			`PATH="$HOME/.local/bin:$PATH" jq -r '(.pending_reasons // {}) as $reasons | (.pending // [])[]? as $job | ($reasons[($job|tostring)] // empty) | select(length > 0) | "BLOCKED:\($job):\(.)"' ~/.cache/weft/queue/default.state.json 2>/dev/null || true; ` +
			`test -f ~/.cache/weft/queue/default.stop && echo "STOP:yes" || echo "STOP:no"`)
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
			case "BLOCKED":
				parts := strings.SplitN(value, ":", 2)
				if len(parts) != 2 {
					continue
				}
				jobID, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
				if err != nil {
					continue
				}
				reason := strings.TrimSpace(parts[1])
				if reason == "" {
					continue
				}
				if info.BlockedReasons == nil {
					info.BlockedReasons = make(map[int64]string)
				}
				info.BlockedReasons[jobID] = reason
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
