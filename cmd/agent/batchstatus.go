package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
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

		// A matching completion record is authoritative even when the runner
		// has not yet released its persisted execution slot.
		if state.Current != nil && *state.Current == jobID {
			running := state.Running[idStr]
			if reportCompletedStatusFile(logDir, jobID, running.RunID) {
				continue
			}
			gpuDevs := gpuDevicesForJob(state, idStr)
			source := sourceExecutionStatus(logDir, jobID)
			processState, processEvidence := inspectProcessState(logDir, jobID)
			switch processState {
			case "paused":
				fmt.Printf("JOB|%d|PAUSED|%s%s\n", jobID, gpuDevs, source)
			case "":
				if processEvidence == opsqueue.ObservationAbsent && statusFileObservation(logDir, jobID, running.RunID) == opsqueue.ObservationAbsent {
					fmt.Printf("JOB|%d|UNRESOLVED_CANDIDATE|%d\n", jobID, running.RunID)
					continue
				}
				fmt.Printf("JOB|%d|CURRENT|%s%s\n", jobID, gpuDevs, source)
			default:
				fmt.Printf("JOB|%d|CURRENT|%s%s\n", jobID, gpuDevs, source)
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
		if running, ok := state.Running[idStr]; ok {
			if reportCompletedStatusFile(logDir, jobID, running.RunID) {
				continue
			}
			gpuDevs := gpuDevicesForJob(state, idStr)
			source := sourceExecutionStatus(logDir, jobID)
			processState, processEvidence := inspectProcessState(logDir, jobID)
			switch processState {
			case "paused":
				fmt.Printf("JOB|%d|PAUSED|%s%s\n", jobID, gpuDevs, source)
			case "":
				if processEvidence == opsqueue.ObservationAbsent && statusFileObservation(logDir, jobID, running.RunID) == opsqueue.ObservationAbsent {
					fmt.Printf("JOB|%d|UNRESOLVED_CANDIDATE|%d\n", jobID, running.RunID)
					continue
				}
				fmt.Printf("JOB|%d|RUNNING|%s%s\n", jobID, gpuDevs, source)
			default:
				fmt.Printf("JOB|%d|RUNNING|%s%s\n", jobID, gpuDevs, source)
			}
			continue
		}

		if rejected, ok := state.Rejected[idStr]; ok && rejected.RunID > 0 {
			fmt.Printf("JOB|%d|PREFLIGHT_REJECTED|%d|%s|%s|%d\n", jobID, rejected.RejectedAt, rejected.FailureReason, version, rejected.RunID)
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
			fmt.Printf("JOB|%d|PREFLIGHT_REJECTED|%d|%s|%s\n", jobID, ts, fr, version)
			continue
		}

		// Check status file
		if reportCompletedStatusFile(logDir, jobID, 0) {
			continue
		}

		// Fall back to finished state when the status file is unavailable.
		if finished, ok := state.Finished[idStr]; ok {
			fr := readFailureReason(logDir, jobID)
			runID := completionRunID(logDir, jobID)
			fmt.Printf("JOB|%d|COMPLETED|%d|%d|%d|%s%s\n", jobID, finished.ExitCode, finished.FinishedAt, runID, fr, sourceExecutionStatus(logDir, jobID))
			continue
		}

		// Check if process exists (not in state but has pid file)
		gpuDevs := gpuDevicesForJob(state, idStr)
		processState, _ := inspectProcessState(logDir, jobID)
		switch processState {
		case "paused":
			fmt.Printf("JOB|%d|PAUSED|%s%s\n", jobID, gpuDevs, sourceExecutionStatus(logDir, jobID))
		case "running":
			fmt.Printf("JOB|%d|RUNNING|%s%s\n", jobID, gpuDevs, sourceExecutionStatus(logDir, jobID))
		default:
			fmt.Printf("JOB|%d|DEAD\n", jobID)
		}
	}
}

// sourceExecutionStatus appends additive, pipe-delimited worker provenance.
// Older clients ignore these fields; newer clients persist them on the
// attempt. Values are machine-generated identifiers and never contain pipes.
func sourceExecutionStatus(logDir string, jobID int64) string {
	data, err := os.ReadFile(filepath.Join(logDir, fmt.Sprintf("%d.meta", jobID)))
	if err != nil {
		return ""
	}
	meta := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			meta[key] = value
		}
	}
	if meta["source_dispatch_mode"] == "" {
		return ""
	}
	return fmt.Sprintf("|%s|%s|%s|%s|%s|%s|%s|%s",
		meta["source_dispatch_mode"],
		meta["source_identity_kind"],
		meta["source_dispatched_sha256"],
		meta["source_verified_sha256"],
		meta["source_verification"],
		meta["source_verified_at"],
		meta["agent_version"],
		meta["source_root_count"],
	)
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

func reportCompletedStatusFile(logDir string, jobID, expectedRunID int64) bool {
	statusFile := filepath.Join(logDir, fmt.Sprintf("%d.status", jobID))
	statusContent, err := os.ReadFile(statusFile)
	if err != nil {
		return false
	}
	runID := completionRunID(logDir, jobID)
	if expectedRunID != 0 && runID != expectedRunID {
		return false
	}
	var exitCode int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(statusContent)), "%d", &exitCode); err != nil {
		return false
	}
	mtime := fileMtime(statusFile)
	failureReason := readFailureReason(logDir, jobID)
	fmt.Printf("JOB|%d|COMPLETED|%d|%d|%d|%s%s\n", jobID, exitCode, mtime, runID, failureReason, sourceExecutionStatus(logDir, jobID))
	return true
}

func readFailureReason(logDir string, jobID int64) string {
	path := filepath.Join(logDir, fmt.Sprintf("%d.failure_reason", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return db.SanitizeFailureReason(strings.TrimSpace(string(data)))
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

// checkProcessState reads the PID files and returns "running", "paused", or
// empty when no live process is confirmed.
func checkProcessState(logDir string, jobID int64) string {
	state, _ := inspectProcessState(logDir, jobID)
	return state
}

func statusFileObservation(logDir string, jobID, expectedRunID int64) string {
	path := filepath.Join(logDir, fmt.Sprintf("%d.status", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return opsqueue.ObservationAbsent
		}
		return opsqueue.ObservationUnknown
	}
	var exitCode int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &exitCode); err != nil {
		return opsqueue.ObservationUnknown
	}
	if expectedRunID != 0 && completionRunID(logDir, jobID) != expectedRunID {
		return opsqueue.ObservationUnknown
	}
	return opsqueue.ObservationPresent
}

func inspectProcessState(logDir string, jobID int64) (string, string) {
	unknown := false
	for _, suffix := range []string{"pgid", "pid"} {
		path := filepath.Join(logDir, fmt.Sprintf("%d.%s", jobID, suffix))
		pid, found, err := runner.ReadPIDFileDetailed(path)
		if err != nil {
			unknown = true
			continue
		}
		if !found || !runner.CheckPIDAlive(pid) || runner.CheckProcessZombie(pid) {
			continue
		}
		if runner.CheckProcessStopped(pid) {
			return "paused", opsqueue.ObservationPresent
		}
		return "running", opsqueue.ObservationPresent
	}
	if unknown {
		return "", opsqueue.ObservationUnknown
	}
	return "", opsqueue.ObservationAbsent
}

func processObservation(logDir string, jobID int64) string {
	_, observation := inspectProcessState(logDir, jobID)
	return observation
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
