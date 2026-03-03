package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/osteele/weft/internal/runner"
)

// batchStatus reads job state files and runner state to produce batch status
// output for the given job IDs. Output format: one JOB|... line per job.
func batchStatus(queueName string, jobIDs []int64) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		homeDir = "/tmp"
	}
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	stateFile := filepath.Join(homeDir, ".cache", "weft", "queue", queueName+".state.json")

	// Load runner state
	state, _ := runner.LoadState(stateFile)

	pendingSet := make(map[int64]bool, len(state.Pending))
	for _, id := range state.Pending {
		pendingSet[id] = true
	}

	for _, jobID := range jobIDs {
		idStr := strconv.FormatInt(jobID, 10)

		// Check finished map first
		if finished, ok := state.Finished[idStr]; ok {
			fr := readFailureReason(logDir, jobID)
			fmt.Printf("JOB|%d|COMPLETED|%d|%d|%s\n", jobID, finished.ExitCode, finished.FinishedAt, fr)
			continue
		}

		// Check status file
		statusFile := filepath.Join(logDir, fmt.Sprintf("%d.status", jobID))
		if statusContent, err := os.ReadFile(statusFile); err == nil {
			exitCodeStr := strings.TrimSpace(string(statusContent))
			var exitCode int
			fmt.Sscanf(exitCodeStr, "%d", &exitCode)
			mtime := fileMtime(statusFile)
			fr := readFailureReason(logDir, jobID)
			fmt.Printf("JOB|%d|COMPLETED|%d|%d|%s\n", jobID, exitCode, mtime, fr)
			continue
		}

		// Check if current
		if state.Current != nil && *state.Current == jobID {
			gpuDevs := gpuDevicesForJob(state, idStr)
			processState := checkProcessState(logDir, jobID)
			switch processState {
			case "paused":
				fmt.Printf("JOB|%d|PAUSED|%s\n", jobID, gpuDevs)
			default:
				fmt.Printf("JOB|%d|CURRENT|%s\n", jobID, gpuDevs)
			}
			continue
		}

		// Check if pending
		if pendingSet[jobID] {
			fmt.Printf("JOB|%d|QUEUED\n", jobID)
			continue
		}

		// Check if running (in running map but not current)
		if _, ok := state.Running[idStr]; ok {
			gpuDevs := gpuDevicesForJob(state, idStr)
			processState := checkProcessState(logDir, jobID)
			switch processState {
			case "paused":
				fmt.Printf("JOB|%d|PAUSED|%s\n", jobID, gpuDevs)
			default:
				fmt.Printf("JOB|%d|RUNNING|%s\n", jobID, gpuDevs)
			}
			continue
		}

		// Check if process exists (not in state but has pid file)
		gpuDevs := gpuDevicesForJob(state, idStr)
		processState := checkProcessState(logDir, jobID)
		switch processState {
		case "paused":
			fmt.Printf("JOB|%d|PAUSED|%s\n", jobID, gpuDevs)
		case "running":
			fmt.Printf("JOB|%d|RUNNING|%s\n", jobID, gpuDevs)
		default:
			fmt.Printf("JOB|%d|DEAD\n", jobID)
		}
	}
}

func readFailureReason(logDir string, jobID int64) string {
	path := filepath.Join(logDir, fmt.Sprintf("%d.failure_reason", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func fileMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func gpuDevicesForJob(state *runner.State, idStr string) string {
	if rs, ok := state.Running[idStr]; ok {
		return strings.Join(rs.GPUDevices, ",")
	}
	return ""
}

// checkProcessState reads the PID file and checks process state.
// Returns "running", "paused", or "" (not found).
func checkProcessState(logDir string, jobID int64) string {
	pidFile := filepath.Join(logDir, fmt.Sprintf("%d.pid", jobID))
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return ""
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return ""
	}

	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return ""
	}
	// Signal 0 checks if process exists without sending a signal
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return ""
	}

	// Check if stopped (paused) by reading /proc/PID/stat on Linux
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	statData, err := os.ReadFile(statPath)
	if err == nil {
		// Format: pid (comm) state ...
		statStr := string(statData)
		// Find state field after the closing paren
		if idx := strings.LastIndex(statStr, ") "); idx >= 0 {
			fields := strings.Fields(statStr[idx+2:])
			if len(fields) > 0 && fields[0] == "T" {
				return "paused"
			}
		}
	}

	return "running"
}

// parseBatchStatusArgs parses "batch-status [queue] id1 id2 ..." arguments.
func parseBatchStatusArgs(args []string) (queueName string, jobIDs []int64) {
	queueName = "default"
	for i := 0; i < len(args); i++ {
		if args[i] == "--queue" && i+1 < len(args) {
			queueName = args[i+1]
			i++
			continue
		}
		if id, err := strconv.ParseInt(args[i], 10, 64); err == nil {
			jobIDs = append(jobIDs, id)
		}
	}
	return
}
