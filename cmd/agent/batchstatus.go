package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/runner"
)

// batchStatus reads job state files and runner state to produce batch status
// output for the given job IDs. Output format: one JOB|... line per job.
func batchStatus(jobIDs []int64) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		homeDir = "/tmp"
	}
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	stateFile := filepath.Join(homeDir, ".cache", "weft", "queue", opsqueue.StateFileName())

	// Load runner state
	state, err := runner.LoadState(stateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: load state %s: %v\n", stateFile, err)
		state = runner.NewState()
	}

	pendingSet := make(map[int64]bool, len(state.Pending))
	for _, id := range state.Pending {
		pendingSet[id] = true
	}

	for _, jobID := range jobIDs {
		idStr := strconv.FormatInt(jobID, 10)

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

		// Check if pending. A pending entry with no payload cannot launch; if
		// terminal artifacts exist, surface those instead of hiding them behind
		// stale runner state.
		if pendingSet[jobID] && (jobPayloadExists(stateFile, jobID) || !terminalArtifactExists(logDir, jobID)) {
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

		// Check for the preflight-rejected sentinel. This file is written by
		// the runner when a preflight check (currently: source provenance)
		// fails before the attempt was stamped with a startTime. We surface it
		// distinctly so local sync can record the attempt without a
		// misleading exit_code / start_time / end_time.
		preflightFile := filepath.Join(logDir, fmt.Sprintf("%d.preflight_rejected", jobID))
		if _, err := os.Stat(preflightFile); err == nil {
			ts := fileMtime(preflightFile)
			fr := readFailureReason(logDir, jobID)
			fmt.Printf("JOB|%d|PREFLIGHT_REJECTED|%d|%s\n", jobID, ts, fr)
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
			runID := completionRunID(logDir, jobID)
			fmt.Printf("JOB|%d|COMPLETED|%d|%d|%d|%s\n", jobID, exitCode, mtime, runID, fr)
			continue
		}

		// Fall back to finished state when the status file is unavailable.
		if finished, ok := state.Finished[idStr]; ok {
			fr := readFailureReason(logDir, jobID)
			runID := completionRunID(logDir, jobID)
			fmt.Printf("JOB|%d|COMPLETED|%d|%d|%d|%s\n", jobID, finished.ExitCode, finished.FinishedAt, runID, fr)
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

func jobPayloadExists(stateFile string, jobID int64) bool {
	queueDir := filepath.Dir(stateFile)
	path := filepath.Join(queueDir, fmt.Sprintf("job-%d.json", jobID))
	_, err := os.Stat(path)
	return err == nil
}

func terminalArtifactExists(logDir string, jobID int64) bool {
	paths := []string{
		filepath.Join(logDir, fmt.Sprintf("%d.preflight_rejected", jobID)),
		filepath.Join(logDir, fmt.Sprintf("%d.status", jobID)),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func readFailureReason(logDir string, jobID int64) string {
	path := filepath.Join(logDir, fmt.Sprintf("%d.failure_reason", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func completionRunID(logDir string, jobID int64) int64 {
	path := filepath.Join(logDir, fmt.Sprintf("%d.completion.json", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return 0
	}
	return rec.RunID
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
	pgidPath := filepath.Join(logDir, fmt.Sprintf("%d.pgid", jobID))
	if pgid, ok := runner.ReadPIDFile(pgidPath); ok && runner.CheckPIDAlive(pgid) {
		if runner.CheckProcessStopped(pgid) {
			return "paused"
		}
		return "running"
	}

	pidPath := filepath.Join(logDir, fmt.Sprintf("%d.pid", jobID))
	if pid, ok := runner.ReadPIDFile(pidPath); ok && runner.CheckPIDAlive(pid) {
		if runner.CheckProcessStopped(pid) {
			return "paused"
		}
		return "running"
	}

	return ""
}

// parseBatchStatusArgs parses "batch-status id1 id2 ..." arguments.
// It still accepts "--queue default" for compatibility, but rejects any other queue.
func parseBatchStatusArgs(args []string) (jobIDs []int64, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--queue" && i+1 < len(args) {
			if args[i+1] != opsqueue.AgentLegacyQueueArg {
				return nil, fmt.Errorf("unsupported queue %q; only %q is supported", args[i+1], opsqueue.AgentLegacyQueueArg)
			}
			i++
			continue
		}
		if args[i] == "--queue" {
			return nil, errors.New("missing value after --queue")
		}
		if id, err := strconv.ParseInt(args[i], 10, 64); err == nil {
			jobIDs = append(jobIDs, id)
		}
	}
	return jobIDs, nil
}
