package queuerunner

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
)

const (
	queueDir = "~/.cache/weft/queue"
)

// QueueDir returns the remote directory used for queue files.
func QueueDir() string {
	return queueDir
}

const agentBinaryPath = "$HOME/.cache/weft/bin/weft-agent"

// RunnerCommand builds the command to start the Go queue runner.
func RunnerCommand(envPrefix string) string {
	return fmt.Sprintf("%s%s run-queue %s", envPrefix, agentBinaryPath, ops.DefaultQueueName)
}

// RunnerSessionName returns the tmux session name for the queue runner.
func RunnerSessionName() string {
	return fmt.Sprintf("weft-queue-%s", ops.DefaultQueueName)
}

// EnsureRunnerStarted checks whether the runner tmux session exists and starts it if missing.
// Returns true when a new runner was started.
func EnsureRunnerStarted(host, runnerCmd string) (bool, error) {
	session := RunnerSessionName()

	exists, err := ssh.TmuxSessionExists(host, session)
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}

	if exists {
		return false, nil
	}

	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", session, ssh.EscapeForSingleQuotes(runnerCmd))
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return false, fmt.Errorf("start queue runner: %s", strings.TrimSpace(stderr))
	}

	// Health check: wait briefly and verify the session is still alive.
	time.Sleep(2 * time.Second)
	stillExists, err := ssh.TmuxSessionExists(host, session)
	if err != nil {
		return true, nil
	}
	if !stillExists {
		return false, fmt.Errorf("queue runner on %s crashed immediately after starting - check runner log: ssh %s 'tail -20 ~/.cache/weft/queue/runner-default.log'", host, host)
	}

	return true, nil
}

// Runner models a queue runner on a specific host/queue combination.
type Runner struct {
	host string
}

// NewRunner creates a runner manager for the given host and queue.
func NewRunner(host string) *Runner {
	return &Runner{host: host}
}

// Host returns the runner host.
func (r *Runner) Host() string { return r.host }

// Queue returns the queue name.
func (r *Runner) Queue() string { return ops.DefaultQueueName }

// SessionName returns the tmux session associated with this runner.
func (r *Runner) SessionName() string { return RunnerSessionName() }

// EnsureStarted ensures the runner is active, starting tmux if needed.
func (r *Runner) EnsureStarted(envPrefix string) (bool, error) {
	runnerCmd := RunnerCommand(envPrefix)
	return EnsureRunnerStarted(r.host, runnerCmd)
}

// SendStopSignal signals the runner to stop after the current job.
func (r *Runner) SendStopSignal() error {
	cmd := ops.NewStopCommand()
	return ops.AppendCommand(r.host, cmd, ops.AppendCommandOptions{})
}

// WaitForStop waits until the runner's tmux session exits or timeout elapses.
func (r *Runner) WaitForStop(timeout time.Duration) error {
	session := r.SessionName()
	deadline := time.Now().Add(timeout)
	for {
		exists, err := ssh.TmuxSessionExists(r.host, session)
		if err != nil {
			return fmt.Errorf("check queue runner: %w", err)
		}
		if !exists {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("queue runner did not stop within %s", timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// IsRunning reports whether the runner tmux session exists.
func (r *Runner) IsRunning() (bool, error) {
	return ssh.TmuxSessionExists(r.host, r.SessionName())
}
