package queuerunner

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/scripts"
	"github.com/osteele/remote-jobs/internal/ssh"
)

const (
	queueDir        = "~/.cache/remote-jobs/queue"
	scriptsDir      = "~/.cache/remote-jobs/scripts"
	queueRunnerPath = "~/.cache/remote-jobs/scripts/queue-runner.sh"
)

var (
	localBuildHeader = firstLine(scripts.QueueRunnerScript)
	localBuildNumber = parseBuildHeader(localBuildHeader)
)

func firstLine(data []byte) string {
	buf := bytes.NewBuffer(data)
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
	cmd := fmt.Sprintf("head -n 1 %s 2>/dev/null || true", queueRunnerPath)
	stdout, stderr, err := ssh.Run(host, cmd)
	if err != nil {
		return "", fmt.Errorf("read remote version: %s", strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

// EnsureScriptUpToDate deploys the queue runner script if the remote build is older.
// Returns true if a new script was deployed.
func EnsureScriptUpToDate(host string) (bool, error) {
	remoteBuild, err := RemoteBuildNumber(host)
	if err != nil {
		return false, err
	}

	// Only deploy when remote is missing or older. Never downgrade newer scripts.
	if remoteBuild >= localBuildNumber && remoteBuild != -1 {
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
	writeCmd := fmt.Sprintf("cat > %s << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", queueRunnerPath, string(scripts.QueueRunnerScript))
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
	return fmt.Sprintf("%sbash $HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", envPrefix, queueName)
}

// EnsureRunnerStarted checks whether the runner tmux session exists and starts it if missing.
// Returns true when a new runner was started.
func EnsureRunnerStarted(host, queueName, runnerCmd string) (bool, error) {
	session := fmt.Sprintf("rj-queue-%s", queueName)
	exists, err := ssh.TmuxSessionExists(host, session)
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}
	if exists {
		return false, nil
	}

	if _, err := EnsureScriptUpToDate(host); err != nil {
		return false, err
	}

	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", session, ssh.EscapeForSingleQuotes(runnerCmd))
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return false, fmt.Errorf("start queue runner: %s", strings.TrimSpace(stderr))
	}
	return true, nil
}
