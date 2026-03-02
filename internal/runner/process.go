package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
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

	// Build environment: start with current env, then overlay job env
	cmd.Env = os.Environ()
	for _, ev := range envVars {
		cmd.Env = append(cmd.Env, ev)
	}

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
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.Contains(state, "T")
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

// CleanupPIDFiles removes PID, PGID, paused, and heartbeat files for a job.
func CleanupPIDFiles(paths JobPaths) {
	os.Remove(paths.PID)
	os.Remove(paths.PGID)
	os.Remove(paths.Paused)
	os.Remove(paths.Heartbeat)
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
