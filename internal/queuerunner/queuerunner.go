package queuerunner

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/scripts"
	"github.com/osteele/remote-jobs/internal/ssh"
)

const (
	queueDir        = "~/.cache/remote-jobs/queue"
	scriptsDir      = "~/.cache/remote-jobs/scripts"
	queueRunnerPath = "~/.cache/remote-jobs/scripts/queue-runner.sh"
)

// QueueDir returns the remote directory used for queue files.
func QueueDir() string {
	return queueDir
}

var (
	localBuildHeader = secondLine(scripts.QueueRunnerScript)
	localBuildNumber = parseBuildHeader(localBuildHeader)
)

func secondLine(data []byte) string {
	buf := bytes.NewBuffer(data)
	// Skip first line (shebang)
	if _, err := buf.ReadString('\n'); err != nil {
		return ""
	}
	// Read second line (BUILD header)
	line, err := buf.ReadString('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	return strings.TrimSpace(line)
}

func parseBuildHeader(line string) int {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "# BUILD:") {
		return -1
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "# BUILD:"))
	n, err := strconv.Atoi(rest)
	if err != nil {
		return -1
	}
	return n
}

// LocalBuildNumber returns the embedded queue runner build number.
func LocalBuildNumber() int {
	return localBuildNumber
}

// RemoteBuildNumber returns the build number from the remote script (or -1 if missing).
func RemoteBuildNumber(host string) (int, error) {
	header, err := readRemoteHeader(host)
	if err != nil {
		return -1, err
	}
	if header == "" {
		return -1, nil
	}
	return parseBuildHeader(header), nil
}

func readRemoteHeader(host string) (string, error) {
	// Read line 2 (BUILD header is after shebang)
	cmd := fmt.Sprintf("sed -n '2p' %s 2>/dev/null || true", queueRunnerPath)
	stdout, stderr, err := ssh.Run(host, cmd)
	if err != nil {
		return "", fmt.Errorf("read remote version: %s", strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

func remoteFileSize(host string) (int, error) {
	cmd := fmt.Sprintf("wc -c < %s 2>/dev/null || echo 0", queueRunnerPath)
	stdout, _, err := ssh.Run(host, cmd)
	if err != nil {
		return 0, err
	}
	size, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return 0, nil // Treat parse errors as size 0
	}
	return size, nil
}

// EnsureScriptUpToDate deploys the queue runner script if the remote build is older
// or if the file sizes differ (failsafe for when BUILD number wasn't bumped).
// Returns true if a new script was deployed.
func EnsureScriptUpToDate(host string) (bool, error) {
	remoteBuild, err := RemoteBuildNumber(host)
	if err != nil {
		return false, err
	}

	needsDeploy := false

	// Deploy when remote is missing or older
	if remoteBuild < localBuildNumber || remoteBuild == -1 {
		needsDeploy = true
	}

	// Failsafe: also deploy if BUILD numbers match but file sizes differ
	// (catches forgotten BUILD bumps without causing ping-pong between different versions)
	if !needsDeploy && remoteBuild == localBuildNumber {
		remoteSize, err := remoteFileSize(host)
		if err == nil && remoteSize != len(scripts.QueueRunnerScript) {
			needsDeploy = true
		}
	}

	if !needsDeploy {
		return false, nil
	}

	if err := ensureDirectories(host); err != nil {
		return false, err
	}
	if err := writeScript(host); err != nil {
		return false, err
	}
	return true, nil
}

func ensureDirectories(host string) error {
	cmd := fmt.Sprintf("mkdir -p %s %s", queueDir, scriptsDir)
	if _, stderr, err := ssh.Run(host, cmd); err != nil {
		return fmt.Errorf("create directories: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func writeScript(host string) error {
	// Script content already ends with newline, don't add another
	writeCmd := fmt.Sprintf("cat > %s << 'SCRIPT_EOF'\n%sSCRIPT_EOF", queueRunnerPath, string(scripts.QueueRunnerScript))
	if _, stderr, err := ssh.Run(host, writeCmd); err != nil {
		return fmt.Errorf("write queue runner: %s", strings.TrimSpace(stderr))
	}
	chmodCmd := fmt.Sprintf("chmod +x %s", queueRunnerPath)
	if _, stderr, err := ssh.Run(host, chmodCmd); err != nil {
		return fmt.Errorf("chmod queue runner: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// RunnerCommand builds the command to start the queue runner (optionally with env prefix).
func RunnerCommand(queueName, envPrefix string) string {
	queueName = ops.DefaultQueueName
	return fmt.Sprintf("%sbash $HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", envPrefix, queueName)
}

// EnsureRunnerStarted checks whether the runner tmux session exists and starts it if missing.
// Also upgrades the queue runner script if needed, restarting the runner to pick up changes.
// Returns true when a new runner was started (or restarted due to upgrade).
func EnsureRunnerStarted(host, queueName, runnerCmd string) (bool, error) {
	queueName = ops.DefaultQueueName
	session := fmt.Sprintf("rj-queue-%s", queueName)

	// Always check if script needs upgrade, even if runner is already running
	upgraded, err := EnsureScriptUpToDate(host)
	if err != nil {
		return false, err
	}

	exists, err := ssh.TmuxSessionExists(host, session)
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}

	// If runner exists and script was upgraded, restart to pick up new version
	if exists && upgraded {
		// Signal runner to stop gracefully after current job using command queue
		stopCmd := ops.NewStopCommand()
		_ = ops.AppendCommand(host, queueName, stopCmd, ops.AppendCommandOptions{}) // Best effort - ignore errors

		// Wait briefly for runner to stop (it will exit after current job)
		// Don't block too long - let it finish naturally
		for i := 0; i < 3; i++ {
			time.Sleep(500 * time.Millisecond)
			stillExists, _ := ssh.TmuxSessionExists(host, session)
			if !stillExists {
				exists = false
				break
			}
		}
	}

	if exists {
		return false, nil
	}

	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", session, ssh.EscapeForSingleQuotes(runnerCmd))
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return false, fmt.Errorf("start queue runner: %s", strings.TrimSpace(stderr))
	}
	return true, nil
}

// Runner models a queue runner on a specific host/queue combination.
type Runner struct {
	host  string
	queue string
}

// NewRunner creates a runner manager for the given host and queue.
func NewRunner(host, queue string) *Runner {
	return &Runner{host: host, queue: queue}
}

// Host returns the runner host.
func (r *Runner) Host() string { return r.host }

// Queue returns the queue name.
func (r *Runner) Queue() string { return r.queue }

// SessionName returns the tmux session associated with this runner.
func (r *Runner) SessionName() string { return fmt.Sprintf("rj-queue-%s", r.queue) }

// EnsureStarted ensures the runner is active, deploying scripts and starting tmux if needed.
func (r *Runner) EnsureStarted(envPrefix string) (bool, error) {
	runnerCmd := RunnerCommand(r.queue, envPrefix)
	return EnsureRunnerStarted(r.host, r.queue, runnerCmd)
}

// SendStopSignal signals the runner to stop after the current job.
func (r *Runner) SendStopSignal() error {
	cmd := ops.NewStopCommand()
	return ops.AppendCommand(r.host, r.queue, cmd, ops.AppendCommandOptions{})
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
