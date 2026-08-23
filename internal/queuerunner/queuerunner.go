package queuerunner

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/agentenv"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/ssh"
)

const agentBinaryPath = "$HOME/.cache/weft/bin/weft-agent"

// RunnerCommand builds the command to start the Go queue runner.
func RunnerCommand(envPrefix, r2Bucket string, setupTimeout time.Duration) string {
	return RunnerCommandForHost(envPrefix, r2Bucket, "", setupTimeout)
}

// RunnerCommandForHost builds a queue-runner command and enables the
// host-addressed R2 inbox when r2QueueHost is non-empty.
func RunnerCommandForHost(envPrefix, r2Bucket, r2QueueHost string, setupTimeout time.Duration) string {
	cmd := fmt.Sprintf("%s%s %s run-queue", envPrefix, agentenv.ShellPathAssignment(), agentBinaryPath)
	if r2Bucket != "" {
		cmd += fmt.Sprintf(" --r2-bucket=%s", r2Bucket)
	}
	if r2QueueHost != "" {
		cmd += fmt.Sprintf(" --r2-queue-host=%s", r2QueueHost)
	}
	if setupTimeout > 0 {
		cmd += fmt.Sprintf(" --setup-timeout=%s", setupTimeout)
	}
	return cmd
}

// RunnerSessionName returns the tmux session name for the queue runner.
func RunnerSessionName() string {
	return opsqueue.TmuxSessionName()
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

	tmuxCmd := ssh.TmuxCommand(fmt.Sprintf("new-session -d -s '%s' bash -c '%s'", session, ssh.EscapeForSingleQuotes(runnerCmd)))
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" {
			return false, fmt.Errorf("start queue runner: %s", msg)
		}
		return false, fmt.Errorf("start queue runner: %w", err)
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

// Runner models the queue runner on a specific host.
type Runner struct {
	host string
}

// NewRunner creates a runner manager for the given host.
func NewRunner(host string) *Runner {
	return &Runner{host: host}
}

// Host returns the runner host.
func (r *Runner) Host() string { return r.host }

// SessionName returns the tmux session associated with this runner.
func (r *Runner) SessionName() string { return RunnerSessionName() }

// EnsureStarted ensures the runner is active, starting tmux if needed.
func (r *Runner) EnsureStarted(envPrefix, r2Bucket string, setupTimeout time.Duration) (bool, error) {
	return r.EnsureStartedForHost(envPrefix, r2Bucket, "", setupTimeout)

}

// EnsureStartedForHost starts the runner with an optional R2 queue address.
func (r *Runner) EnsureStartedForHost(envPrefix, r2Bucket, r2QueueHost string, setupTimeout time.Duration) (bool, error) {
	runnerCmd := RunnerCommandForHost(envPrefix, r2Bucket, r2QueueHost, setupTimeout)
	return EnsureRunnerStarted(r.host, runnerCmd)
}

// SendStopSignal signals the runner to stop after the current job.
func (r *Runner) SendStopSignal() error {
	cmd := opsqueue.NewStopCommand()
	return opsqueue.AppendCommand(r.host, cmd, opsqueue.AppendCommandOptions{})
}

// SendRestartSignal signals the runner to re-exec with the current binary.
func (r *Runner) SendRestartSignal() error {
	return r.SendRestartSignalWithEnv(nil)
}

// SendRestartSignalWithEnv signals the runner to re-exec with environment overrides.
func (r *Runner) SendRestartSignalWithEnv(env []string) error {
	cmd := opsqueue.NewRestartCommandWithEnv(env)
	return opsqueue.AppendCommand(r.host, cmd, opsqueue.AppendCommandOptions{})
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
