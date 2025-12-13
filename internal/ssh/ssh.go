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

// FriendlyError returns a user-friendly error message for SSH failures
// It hides implementation details like "create log dir" and shows clearer messages
func FriendlyError(host, stderr string, err error) string {
	combined := stderr
	if err != nil {
		combined += " " + err.Error()
	}

	// Check for connection errors
	if IsConnectionError(combined) {
		return fmt.Sprintf("SSH connection to %s failed", host)
	}

	// Check for exit status 255 which typically means SSH connection failed
	if strings.Contains(combined, "exit status 255") {
		return fmt.Sprintf("SSH connection to %s failed", host)
	}

	// Check for permission denied
	if strings.Contains(strings.ToLower(combined), "permission denied") {
		return fmt.Sprintf("SSH permission denied on %s", host)
	}

	// Check for host key verification
	if strings.Contains(strings.ToLower(combined), "host key verification") {
		return fmt.Sprintf("SSH host key verification failed for %s", host)
	}

	// Default: return a generic SSH error with host
	if stderr != "" {
		return fmt.Sprintf("SSH error on %s: %s", host, strings.TrimSpace(stderr))
	}
	if err != nil {
		return fmt.Sprintf("SSH error on %s: %s", host, err.Error())
	}
	return fmt.Sprintf("SSH error on %s", host)
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

// ProcessStats holds process statistics
type ProcessStats struct {
	PID       string
	Running   bool
	CPUUser   string  // User CPU time (e.g., "1h23m45s")
	CPUSys    string  // System CPU time
	CPUPct    float64 // CPU utilization % (requires delta calculation)
	MemoryRSS string  // Resident memory (e.g., "1.2GB")
	MemoryPct string  // Memory percentage
	Threads   int     // Thread count
	GPUs      []ProcessGPU
	Error     string
	// Raw values for CPU% calculation (used by caller to track deltas)
	CPUUserTicks int64 // Raw user CPU ticks
	CPUSysTicks  int64 // Raw system CPU ticks
	Timestamp    int64 // Unix timestamp of measurement
}

// ProcessGPU holds GPU usage for a process
type ProcessGPU struct {
	Index       int
	MemUsed     string // e.g., "1234MiB"
	Utilization int    // GPU utilization % (0-100)
}

// GetProcessStats fetches process statistics from a remote host
// The pidFile should contain the PID to query
func GetProcessStats(host, pidFile string) (*ProcessStats, error) {
	// Build a command that outputs all stats we need in a parseable format
	// This runs in a single SSH call for efficiency
	cmd := fmt.Sprintf(`
		PID=$(cat %s 2>/dev/null)
		if [ -z "$PID" ]; then
			echo "PID:NOTFOUND"
			exit 0
		fi
		echo "PID:$PID"
		echo "TIMESTAMP:$(date +%%s)"

		# Check if process is running
		if ! kill -0 $PID 2>/dev/null; then
			echo "RUNNING:NO"
			exit 0
		fi
		echo "RUNNING:YES"

		# Get CPU times from /proc/PID/stat (fields 14=utime, 15=stime in clock ticks)
		if [ -f /proc/$PID/stat ]; then
			STAT=$(cat /proc/$PID/stat 2>/dev/null)
			UTIME=$(echo "$STAT" | awk '{print $14}')
			STIME=$(echo "$STAT" | awk '{print $15}')
			CLK_TCK=$(getconf CLK_TCK 2>/dev/null || echo 100)
			if [ -n "$UTIME" ] && [ -n "$STIME" ]; then
				UTIME_SEC=$((UTIME / CLK_TCK))
				STIME_SEC=$((STIME / CLK_TCK))
				echo "CPU_USER:$UTIME_SEC"
				echo "CPU_SYS:$STIME_SEC"
				echo "CPU_USER_TICKS:$UTIME"
				echo "CPU_SYS_TICKS:$STIME"
				echo "CLK_TCK:$CLK_TCK"
			fi
		fi

		# Get memory and thread count from /proc/PID/status
		if [ -f /proc/$PID/status ]; then
			RSS_KB=$(grep VmRSS /proc/$PID/status 2>/dev/null | awk '{print $2}')
			if [ -n "$RSS_KB" ]; then
				echo "MEM_RSS_KB:$RSS_KB"
			fi
			THREADS=$(grep Threads /proc/$PID/status 2>/dev/null | awk '{print $2}')
			if [ -n "$THREADS" ]; then
				echo "THREADS:$THREADS"
			fi
		fi

		# Get total memory for percentage calculation
		MEM_TOTAL_KB=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}')
		if [ -n "$MEM_TOTAL_KB" ]; then
			echo "MEM_TOTAL_KB:$MEM_TOTAL_KB"
		fi

		# Get GPU utilization (per-GPU)
		nvidia-smi --query-gpu=index,utilization.gpu --format=csv,noheader,nounits 2>/dev/null | while read line; do
			GPU_IDX=$(echo "$line" | cut -d',' -f1 | tr -d ' ')
			GPU_UTIL=$(echo "$line" | cut -d',' -f2 | tr -d ' ')
			echo "GPU_UTIL:$GPU_IDX:$GPU_UTIL"
		done

		# Get GPU memory usage from process (if available)
		nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory --format=csv,noheader,nounits 2>/dev/null | while read line; do
			APP_PID=$(echo "$line" | cut -d',' -f1 | tr -d ' ')
			if [ "$APP_PID" = "$PID" ]; then
				GPU_UUID=$(echo "$line" | cut -d',' -f2 | tr -d ' ')
				GPU_MEM=$(echo "$line" | cut -d',' -f3 | tr -d ' ')
				# Get GPU index from UUID
				GPU_IDX=$(nvidia-smi --query-gpu=index,uuid --format=csv,noheader 2>/dev/null | grep "$GPU_UUID" | cut -d',' -f1 | tr -d ' ')
				echo "GPU_MEM:$GPU_IDX:${GPU_MEM}MiB"
			fi
		done
	`, pidFile)

	stdout, _, err := RunWithTimeout(host, cmd, 15*time.Second)
	if err != nil {
		return &ProcessStats{Error: err.Error()}, err
	}

	return parseProcessStats(stdout), nil
}

// parseProcessStats parses the output of the process stats command
func parseProcessStats(output string) *ProcessStats {
	stats := &ProcessStats{}
	gpuUtils := make(map[int]int)  // GPU index -> utilization
	gpuMem := make(map[int]string) // GPU index -> memory used

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := parts[0]
		value := strings.TrimSpace(parts[1])

		switch key {
		case "PID":
			if value == "NOTFOUND" {
				stats.Error = "PID file not found"
				return stats
			}
			stats.PID = value
		case "TIMESTAMP":
			fmt.Sscanf(value, "%d", &stats.Timestamp)
		case "RUNNING":
			stats.Running = value == "YES"
		case "CPU_USER":
			stats.CPUUser = formatDuration(value)
		case "CPU_SYS":
			stats.CPUSys = formatDuration(value)
		case "CPU_USER_TICKS":
			fmt.Sscanf(value, "%d", &stats.CPUUserTicks)
		case "CPU_SYS_TICKS":
			fmt.Sscanf(value, "%d", &stats.CPUSysTicks)
		case "MEM_RSS_KB":
			stats.MemoryRSS = formatMemoryKB(value)
		case "MEM_TOTAL_KB":
			// Calculate percentage if we have RSS
			if stats.MemoryRSS != "" {
				stats.MemoryPct = calculateMemoryPct(output)
			}
		case "THREADS":
			fmt.Sscanf(value, "%d", &stats.Threads)
		case "GPU_UTIL":
			// Format: GPU_UTIL:index:utilization
			gpuParts := strings.SplitN(value, ":", 2)
			if len(gpuParts) == 2 {
				idx := 0
				util := 0
				fmt.Sscanf(gpuParts[0], "%d", &idx)
				fmt.Sscanf(gpuParts[1], "%d", &util)
				gpuUtils[idx] = util
			}
		case "GPU_MEM":
			// Format: GPU_MEM:index:memory
			gpuParts := strings.SplitN(value, ":", 2)
			if len(gpuParts) == 2 {
				idx := 0
				fmt.Sscanf(gpuParts[0], "%d", &idx)
				gpuMem[idx] = gpuParts[1]
			}
		}
	}

	// Combine GPU utilization and memory into GPU structs
	// Only add GPUs that have memory usage from our process
	for idx, mem := range gpuMem {
		gpu := ProcessGPU{
			Index:       idx,
			MemUsed:     mem,
			Utilization: gpuUtils[idx], // Will be 0 if not found
		}
		stats.GPUs = append(stats.GPUs, gpu)
	}

	return stats
}

// formatDuration converts seconds to a human-readable duration
func formatDuration(seconds string) string {
	var sec int
	if _, err := fmt.Sscanf(seconds, "%d", &sec); err != nil {
		return seconds
	}

	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	} else if sec < 3600 {
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	} else {
		h := sec / 3600
		m := (sec % 3600) / 60
		s := sec % 60
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
}

// formatMemoryKB converts kB to human-readable format
func formatMemoryKB(kb string) string {
	var kbVal int
	if _, err := fmt.Sscanf(kb, "%d", &kbVal); err != nil {
		return kb + " kB"
	}

	if kbVal < 1024 {
		return fmt.Sprintf("%d kB", kbVal)
	} else if kbVal < 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(kbVal)/1024)
	} else {
		return fmt.Sprintf("%.1f GB", float64(kbVal)/(1024*1024))
	}
}

// calculateMemoryPct calculates memory percentage from the output
func calculateMemoryPct(output string) string {
	var rssKB, totalKB int

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEM_RSS_KB:") {
			fmt.Sscanf(strings.TrimPrefix(line, "MEM_RSS_KB:"), "%d", &rssKB)
		} else if strings.HasPrefix(line, "MEM_TOTAL_KB:") {
			fmt.Sscanf(strings.TrimPrefix(line, "MEM_TOTAL_KB:"), "%d", &totalKB)
		}
	}

	if totalKB > 0 {
		pct := float64(rssKB) * 100 / float64(totalKB)
		return fmt.Sprintf("%.1f%%", pct)
	}
	return ""
}
