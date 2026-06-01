package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process tracks a running job process.
type Process struct {
	Cmd  *exec.Cmd
	PID  int
	PGID int
}

// StartProcess starts a job command as a new process group.
// The command runs as `bash -c '<command>'` with stdout/stderr going to logFile.
// Environment variables and working directory are applied.
func StartProcess(command, workingDir string, envVars []string, logFile string) (*Process, error) {
	logF, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if workingDir != "" {
		// Expand ~ to home directory
		if strings.HasPrefix(workingDir, "~/") {
			home, _ := os.UserHomeDir()
			workingDir = home + workingDir[1:]
		} else if workingDir == "~" {
			home, _ := os.UserHomeDir()
			workingDir = home
		}
		cmd.Dir = workingDir
	}

	// Build environment: start with current env, then overlay job env.
	// Later entries override earlier ones with the same key (last-writer-wins),
	// which is critical for CUDA_VISIBLE_DEVICES set by GPU class resolution.
	cmd.Env = mergeEnvVars(os.Environ(), envVars)

	if err := cmd.Start(); err != nil {
		logF.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}

	// Close the log file in the parent - the child has its own fd
	logF.Close()

	pid := cmd.Process.Pid
	// With Setpgid, the child's PGID equals its PID
	pgid := pid

	return &Process{
		Cmd:  cmd,
		PID:  pid,
		PGID: pgid,
	}, nil
}

// mergeEnvVars combines base and overlay env vars with last-writer-wins semantics.
// If the same key appears in both base and overlay (or multiple times in overlay),
// the last occurrence wins. This ensures that e.g. CUDA_VISIBLE_DEVICES from GPU
// class resolution overrides any earlier value from dotenv or the parent process.
func mergeEnvVars(base, overlay []string) []string {
	// Track key positions for deduplication
	keyIndex := make(map[string]int, len(base)+len(overlay))
	result := make([]string, 0, len(base)+len(overlay))

	addVar := func(ev string) {
		key, _, _ := strings.Cut(ev, "=")
		if idx, exists := keyIndex[key]; exists {
			// Replace existing entry in-place
			result[idx] = ev
		} else {
			keyIndex[key] = len(result)
			result = append(result, ev)
		}
	}

	for _, ev := range base {
		addVar(ev)
	}
	for _, ev := range overlay {
		addVar(ev)
	}

	homeDir := envValue(result, "HOME")
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	for i, ev := range result {
		result[i] = expandLeadingTildeEnvValue(ev, homeDir)
	}

	return result
}

func ensureWritableTMPDIR(envVars []string, workingDir, logDir string, jobID int64) []string {
	if envValue(envVars, "TMPDIR") != "" {
		return envVars
	}

	candidates := make([]string, 0, 2)
	if workingDir != "" {
		candidates = append(candidates, filepath.Join(workingDir, "output", "tmp"))
	}
	if logDir != "" {
		candidates = append(candidates, filepath.Join(logDir, "tmp", strconv.FormatInt(jobID, 10)))
	}

	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			return append(envVars, "TMPDIR="+dir)
		}
	}
	return envVars
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, ev := range env {
		if strings.HasPrefix(ev, prefix) {
			return strings.TrimPrefix(ev, prefix)
		}
	}
	return ""
}

func expandLeadingTildeEnvValue(ev, homeDir string) string {
	if homeDir == "" {
		return ev
	}
	key, value, ok := strings.Cut(ev, "=")
	if !ok {
		return ev
	}
	switch {
	case value == "~":
		return key + "=" + homeDir
	case strings.HasPrefix(value, "~/"):
		return key + "=" + filepath.Join(homeDir, value[2:])
	default:
		return ev
	}
}

// WritePIDFiles writes the PID and PGID files for a job.
func (p *Process) WritePIDFiles(paths JobPaths) error {
	if err := os.WriteFile(paths.PID, []byte(fmt.Sprintf("%d\n", p.PID)), 0644); err != nil {
		return err
	}
	return os.WriteFile(paths.PGID, []byte(fmt.Sprintf("%d\n", p.PGID)), 0644)
}

// Kill sends SIGKILL to the process group.
func (p *Process) Kill() error {
	return syscall.Kill(-p.PGID, syscall.SIGKILL)
}

// Signal sends a signal to the process group.
func (p *Process) Signal(sig syscall.Signal) error {
	return syscall.Kill(-p.PGID, sig)
}

// IsRunning checks if the process is still alive.
func (p *Process) IsRunning() bool {
	return p.Cmd.ProcessState == nil
}

// CheckPIDAlive checks if a process with the given PID exists.
func CheckPIDAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// CheckProcessStopped checks if a process is in stopped state (T).
func CheckProcessStopped(pid int) bool {
	state, ok := processStateChar(pid)
	if !ok {
		return false
	}
	return strings.Contains(state, "T")
}

// CheckProcessZombie reports whether a process is in zombie state (Z) —
// it has exited but its parent hasn't waited for it. Zombies still satisfy
// CheckPIDAlive (the kernel keeps a process table entry) so callers that
// distinguish "still doing work" from "gone" must check this separately.
func CheckProcessZombie(pid int) bool {
	state, ok := processStateChar(pid)
	if !ok {
		return false
	}
	return strings.Contains(state, "Z")
}

// processStateChar returns the ps `stat` column for pid, or false if the
// process cannot be queried (e.g. already reaped or insufficient permissions).
func processStateChar(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// reapZombie best-effort wait4s a zombie PID with WNOHANG so the kernel can
// clear its process-table entry. Returns silently when the caller isn't the
// parent (ECHILD) or the wait fails for any other reason — the only goal here
// is housekeeping; the zombie-detection code path has already produced the
// status file and recovery before this is called.
func reapZombie(pid int) {
	if pid <= 0 {
		return
	}
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// ReadPIDFile reads a PID from a file.
func ReadPIDFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	// Take last line (some files have multiple lines)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// KillProcessGroup sends SIGTERM then SIGKILL to a process group.
func KillProcessGroup(pgid int) {
	syscall.Kill(-pgid, syscall.SIGTERM)
	// Give it a moment, then force kill
	syscall.Kill(-pgid, syscall.SIGKILL)
}

// KillProcessGroupWithGrace sends SIGTERM to the process group, then SIGKILL
// after grace elapses. If reason is non-empty, it is written to the
// kill-reason file first so diagnostics can report why the job was killed.
// Callers own their own logging.
func KillProcessGroupWithGrace(pgid int, grace time.Duration, paths JobPaths, reason string) {
	if reason != "" {
		WriteKillReasonFile(paths, reason)
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(grace, func() {
		syscall.Kill(-pgid, syscall.SIGKILL)
	})
}

// CleanupPIDFiles removes PID, PGID, paused, and heartbeat files for a job.
func CleanupPIDFiles(paths JobPaths) {
	os.Remove(paths.PID)
	os.Remove(paths.PGID)
	os.Remove(paths.Paused)
	os.Remove(paths.Heartbeat)
}

// WrapCommandWithExitCapture wraps a command string so that bash writes the
// exit code to a status file and appends a log footer after the command finishes.
// This ensures exit information is recorded even if the Go runner process dies
// mid-job (e.g., during a runner restart) or the wrapper is killed via SIGTERM.
//
// A SIGTERM/SIGINT trap is installed so `weft kill` (which sends SIGTERM to the
// wrapper before escalating to SIGKILL) still produces a .status file before
// the wrapper exits. Without the trap, a SIGTERM'd wrapper dies without
// writing the status file, the kernel keeps a process-table entry until the
// parent waits, and after the agent re-execs there is no Wait() goroutine —
// leaving the wrapper as a zombie. refreshRunningJobs's zombie path recovers
// from that, but the trap is the primary fix; this is the redundant lower
// layer. SIGKILL cannot be trapped; killQueueRunnerJob writes a synthetic
// status file as a third layer for that case.
//
// StartProcess runs the result as bash -c '<wrapped>', so stdout/stderr go to
// the log file via Go's fd redirection.
func WrapCommandWithExitCapture(command, statusFile string) string {
	// On SIGTERM/SIGINT: write whatever $? is at trap entry. In the common
	// case (no user-installed signal handler), the foreground command also
	// received the signal via the process group and `$?` is the signal-exit
	// code (143 for SIGTERM, 130 for SIGINT). When the user has installed
	// their own handler and chose to exit 0, we honor that — overriding 0→143
	// would silently lose the user's signal that work finished cleanly.
	// If `$?` is empty (no foreground command was running and `$?` got reset
	// — extremely rare), we fall back to the signal's exit code so the
	// status file is never blank.
	return fmt.Sprintf(
		`_weft_status_file=%s; `+
			`_weft_on_signal() { _e=$?; if [ -z "$_e" ]; then _e=$1; fi; `+
			`echo "=== END exit=$_e ($2) $(date) ==="; `+
			`echo "$_e" > "$_weft_status_file"; exit "$_e"; }; `+
			`trap '_weft_on_signal 143 SIGTERM' TERM; `+
			`trap '_weft_on_signal 130 SIGINT' INT; `+
			`%s; EXIT_CODE=$?; `+
			`echo "=== END exit=$EXIT_CODE $(date) ==="; `+
			`echo "$EXIT_CODE" > "$_weft_status_file"; `+
			`exit "$EXIT_CODE"`,
		statusFile, command)
}

// GetProcessTree returns all PIDs in the process tree rooted at the given PID.
func GetProcessTree(rootPID int) []int {
	pids := []int{rootPID}
	queue := []int{rootPID}

	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]

		out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			child, err := strconv.Atoi(line)
			if err == nil {
				pids = append(pids, child)
				queue = append(queue, child)
			}
		}
	}

	return pids
}
