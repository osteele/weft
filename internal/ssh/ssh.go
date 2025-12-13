package ssh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// execCommand is the function used to create exec.Cmd objects.
// It can be replaced in tests to capture command arguments.
var execCommand = exec.Command

const (
	// MaxRetries is the number of connection retry attempts
	MaxRetries = 5
	// RetryDelay is the delay between retries
	RetryDelay = 30 * time.Second
)

// connectionErrorPattern matches SSH connection errors that should trigger retry
var connectionErrorPattern = regexp.MustCompile(`(?i)(connection timed out|operation timed out|no route to host|host is unreachable|connection refused|network is unreachable|could not resolve hostname|name or service not known)`)

// IsConnectionError checks if the error output indicates a connection failure
func IsConnectionError(output string) bool {
	return connectionErrorPattern.MatchString(output)
}

// EscapeForSingleQuotes escapes a string for embedding in single quotes
// by replacing ' with '\” (end quote, escaped quote, start quote)
func EscapeForSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// Run executes an SSH command and returns stdout, stderr, and error
func Run(host string, command string) (string, string, error) {
	cmd := execCommand("ssh", host, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// RunWithTimeout executes an SSH command with a timeout and connection options
// to prevent hanging on unreachable hosts or password prompts
func RunWithTimeout(host string, command string, timeout time.Duration) (string, string, error) {
	cmd := exec.Command("ssh",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		host, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Start the command
	if err := cmd.Start(); err != nil {
		return "", "", err
	}

	// Wait with timeout
	// Buffer size 1 prevents goroutine leak if timeout occurs before Wait() completes
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return stdout.String(), stderr.String(), err
	case <-time.After(timeout):
		cmd.Process.Kill()
		return "", "", fmt.Errorf("ssh command timed out after %v", timeout)
	}
}

// RunWithRetry executes an SSH command with retry logic for connection failures
func RunWithRetry(host string, command string) (string, string, error) {
	return RunWithRetryVerbose(host, command, true)
}

// RunWithRetryQuiet executes an SSH command with retry logic but no stderr output
func RunWithRetryQuiet(host string, command string) (string, string, error) {
	return RunWithRetryVerbose(host, command, false)
}

// RunWithRetryVerbose executes an SSH command with retry logic for connection failures
func RunWithRetryVerbose(host string, command string, verbose bool) (string, string, error) {
	var lastOutput, lastStderr string
	var lastErr error

	for attempt := 1; attempt <= MaxRetries; attempt++ {
		stdout, stderr, err := Run(host, command)
		lastOutput = stdout
		lastStderr = stderr
		lastErr = err

		if err == nil {
			return stdout, stderr, nil
		}

		// Check if it's a connection error that should be retried
		combined := stdout + stderr
		if IsConnectionError(combined) {
			if attempt < MaxRetries {
				if verbose {
					fmt.Fprintf(os.Stderr, "Connection failed (attempt %d/%d): %s\n", attempt, MaxRetries, strings.TrimSpace(combined))
					fmt.Fprintf(os.Stderr, "Retrying in %v...\n", RetryDelay)
				}
				time.Sleep(RetryDelay)
				continue
			}
			return stdout, stderr, fmt.Errorf("connection failed after %d attempts: %s", MaxRetries, strings.TrimSpace(combined))
		}

		// Non-connection error, don't retry
		return stdout, stderr, err
	}

	return lastOutput, lastStderr, lastErr
}

// RunInteractive runs an SSH command that may require terminal interaction
func RunInteractive(host string, command string) error {
	cmd := exec.Command("ssh", host, "-t", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunStreaming runs an SSH command and streams output to the provided writers
func RunStreaming(host string, command string, stdout, stderr io.Writer) error {
	cmd := exec.Command("ssh", host, command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// CopyTo copies a local file to a remote host using scp
func CopyTo(localPath, host, remotePath string) error {
	return CopyToWithRetryVerbose(localPath, host, remotePath, true)
}

// CopyToWithRetry copies a local file to a remote host with retry logic
func CopyToWithRetry(localPath, host, remotePath string) error {
	return CopyToWithRetryVerbose(localPath, host, remotePath, true)
}

// CopyToWithRetryVerbose copies a local file to a remote host with retry logic
func CopyToWithRetryVerbose(localPath, host, remotePath string, verbose bool) error {
	var lastErr error

	for attempt := 1; attempt <= MaxRetries; attempt++ {
		cmd := exec.Command("scp", "-q", localPath, fmt.Sprintf("%s:%s", host, remotePath))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()

		if err == nil {
			return nil
		}

		lastErr = err
		output := stderr.String()

		if IsConnectionError(output) {
			if attempt < MaxRetries {
				if verbose {
					fmt.Fprintf(os.Stderr, "SCP failed (attempt %d/%d): %s\n", attempt, MaxRetries, strings.TrimSpace(output))
					fmt.Fprintf(os.Stderr, "Retrying in %v...\n", RetryDelay)
				}
				time.Sleep(RetryDelay)
				continue
			}
			return fmt.Errorf("scp failed after %d attempts: %s", MaxRetries, strings.TrimSpace(output))
		}

		// Non-connection error, don't retry
		return err
	}

	return lastErr
}

// TmuxSessionExists checks if a tmux session exists on the remote host (with retry)
func TmuxSessionExists(host, sessionName string) (bool, error) {
	stdout, stderr, err := RunWithRetry(host, fmt.Sprintf("tmux has-session -t '%s' 2>&1 && echo YES || echo NO", sessionName))
	if err != nil {
		// Check if it's a connection error
		if IsConnectionError(stdout + stderr) {
			return false, err
		}
	}
	// Check last line for YES/NO
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	lastLine := ""
	if len(lines) > 0 {
		lastLine = strings.TrimSpace(lines[len(lines)-1])
	}
	return lastLine == "YES", nil
}

// TmuxSessionExistsQuick checks if a tmux session exists without retrying (for sync)
func TmuxSessionExistsQuick(host, sessionName string) (bool, error) {
	stdout, stderr, err := Run(host, fmt.Sprintf("tmux has-session -t '%s' 2>&1 && echo YES || echo NO", sessionName))
	if err != nil {
		// Check if it's a connection error
		if IsConnectionError(stdout + stderr) {
			return false, fmt.Errorf("connection error: %s", strings.TrimSpace(stdout+stderr))
		}
	}
	// Check last line for YES/NO
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	lastLine := ""
	if len(lines) > 0 {
		lastLine = strings.TrimSpace(lines[len(lines)-1])
	}
	return lastLine == "YES", nil
}

// ReadRemoteFileQuick reads a file from a remote host without retrying (for sync)
// Note: path is not quoted to allow tilde expansion
func ReadRemoteFileQuick(host, path string) (string, error) {
	stdout, stderr, err := Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", path))
	if err != nil {
		if IsConnectionError(stdout + stderr) {
			return "", fmt.Errorf("connection error: %s", strings.TrimSpace(stdout+stderr))
		}
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// TmuxListSessions lists all tmux sessions on a remote host
func TmuxListSessions(host string) ([]string, error) {
	stdout, _, err := Run(host, "tmux list-sessions -F '#{session_name}' 2>/dev/null || true")
	if err != nil {
		return nil, err
	}

	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" {
			sessions = append(sessions, line)
		}
	}
	return sessions, nil
}

// TmuxKillSession kills a tmux session on a remote host
func TmuxKillSession(host, sessionName string) error {
	_, _, err := Run(host, fmt.Sprintf("tmux kill-session -t '%s'", sessionName))
	return err
}

// TmuxCapturePaneOutput captures the last N lines from a tmux pane
func TmuxCapturePaneOutput(host, sessionName string, lines int) (string, error) {
	stdout, _, err := Run(host, fmt.Sprintf("tmux capture-pane -t '%s' -p | tail -%d", sessionName, lines))
	return stdout, err
}

// ReadRemoteFile reads a file from a remote host
// Note: path is not quoted to allow tilde expansion
func ReadRemoteFile(host, path string) (string, error) {
	stdout, _, err := Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", path))
	return strings.TrimSpace(stdout), err
}

// RemoteFileExists checks if a file exists on a remote host
// Note: path is not quoted to allow tilde expansion
func RemoteFileExists(host, path string) (bool, error) {
	stdout, _, err := Run(host, fmt.Sprintf("test -f %s && echo EXISTS || echo NOTEXISTS", path))
	if err != nil {
		return false, err
	}
	return strings.Contains(stdout, "EXISTS"), nil
}

// GetTmuxPanePID gets the PID of the process running in a tmux pane
func GetTmuxPanePID(host, sessionName string) (string, error) {
	stdout, _, err := Run(host, fmt.Sprintf("tmux list-panes -t '%s' -F '#{pane_pid}' 2>/dev/null | head -1", sessionName))
	return strings.TrimSpace(stdout), err
}

// HasChildProcesses checks if a process has child processes
func HasChildProcesses(host, pid string) (bool, error) {
	if pid == "" {
		return false, nil
	}
	stdout, _, err := Run(host, fmt.Sprintf("pgrep -P %s >/dev/null 2>&1 && echo YES || echo NO", pid))
	if err != nil {
		return false, err
	}
	return strings.Contains(stdout, "YES"), nil
}
