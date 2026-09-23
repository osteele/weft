package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

// CommandProcessor reads the append-only JSONL command log and processes
// new commands since the last cursor position.
type CommandProcessor struct {
	commandsFile string
	queueDir     string
	logDir       string
}

// CommandResult describes the outcome of processing all new commands.
type CommandResult struct {
	StopRequested        bool
	RestartRequested     bool
	RestartEnv           []string
	RecoveredCompletions []PostJobCapture
}

// NewCommandProcessor creates a processor for the given commands file.
// logDir is where job artifacts (.status, .log, .meta, …) live; it may be
// empty in tests that don't exercise the archive-on-add path.
func NewCommandProcessor(commandsFile, queueDir, logDir string) *CommandProcessor {
	return &CommandProcessor{
		commandsFile: commandsFile,
		queueDir:     queueDir,
		logDir:       logDir,
	}
}

// jobIsLive reports whether jobID is currently in Running or matches the
// Current job marker. Callers must already hold state.mu (read or write).
// Used by the OpAdd handler to avoid archiving in-flight artifacts of a
// running attempt when a duplicate OpAdd arrives.
func (cp *CommandProcessor) jobIsLive(state *State, jobID int64) bool {
	if _, ok := state.Running[strconv.FormatInt(jobID, 10)]; ok {
		return true
	}
	if state.Current != nil && *state.Current == jobID {
		return true
	}
	return false
}

// sameAttemptTerminalRecord returns durable completion evidence for a duplicate add.
func (cp *CommandProcessor) sameAttemptTerminalRecord(state *State, job *opsqueue.CommandJob) (CompletionRecord, bool) {
	if job == nil || job.RunID == 0 || cp.logDir == "" || cp.jobIsLive(state, job.ID) {
		return CompletionRecord{}, false
	}
	completionFile := filepath.Join(cp.logDir, fmt.Sprintf("%d.completion.json", job.ID))
	data, err := os.ReadFile(completionFile)
	if err != nil {
		return CompletionRecord{}, false
	}
	var parsed struct {
		CompletionRecord
		ExitCode *int `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return CompletionRecord{}, false
	}
	rec := parsed.CompletionRecord
	if rec.RunID != job.RunID || rec.RunID == 0 || rec.EndTime <= 0 || parsed.ExitCode == nil {
		return CompletionRecord{}, false
	}
	rec.ExitCode = *parsed.ExitCode
	return rec, true
}

// ProcessCommands reads new lines from the commands file and applies them to the state.
// Returns a CommandResult indicating whether stop/restart was requested.
func (cp *CommandProcessor) ProcessCommands(state *State) (CommandResult, error) {
	result := CommandResult{}

	f, err := os.Open(cp.commandsFile)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("open commands file: %w", err)
	}
	defer f.Close()

	lineNum := 0
	reader := bufio.NewReader(f)

	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return result, fmt.Errorf("read commands: %w", readErr)
		}
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		lineNum++

		// Skip already-processed lines
		if lineNum <= state.CursorLineValue() {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		var cmd opsqueue.QueueCommand
		if err := json.Unmarshal(line, &cmd); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skip malformed queue command at line %d: %v\n", lineNum, err)
			state.SetCursorLine(lineNum)
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if cmd.ProtocolVersion > opsqueue.QueueProtocolVersion {
			return result, fmt.Errorf(
				"queue command at line %d requires protocol %d; this agent supports %d",
				lineNum, cmd.ProtocolVersion, opsqueue.QueueProtocolVersion)
		}

		state.mu.Lock()
		switch cmd.Op {
		case opsqueue.OpAdd:
			if cmd.Job != nil {
				jobIDStr := strconv.FormatInt(cmd.Job.ID, 10)
				if rejected, ok := state.Rejected[jobIDStr]; ok && cmd.Job.RunID > 0 && rejected.RunID == cmd.Job.RunID {
					state.removePendingLocked(cmd.Job.ID)
					break
				}
				if rec, dup := cp.sameAttemptTerminalRecord(state, cmd.Job); dup {
					state.removePendingLocked(cmd.Job.ID)
					state.Finished[jobIDStr] = FinishedJobState{
						RunID:    rec.RunID,
						ExitCode: rec.ExitCode, FinishedAt: rec.EndTime, ObservedAt: time.Now().Unix(),
					}
					if rec.ExitCode == 0 {
						if err := RecordDeclaredArtifacts(cmd.Job.ID, cmd.Job.Produces, cmd.Job.Outputs); err != nil {
							WriteManifestErrorFile(NewJobPaths(cp.logDir, cmd.Job.ID), "recovered duplicate dispatch: "+err.Error())
						}
					}
					result.RecoveredCompletions = append(result.RecoveredCompletions, PostJobCapture{
						JobID: cmd.Job.ID, RunID: rec.RunID, WorkDir: rec.RuntimeWorkingDir,
						LogDir: cp.logDir, ExitCode: rec.ExitCode, StartTime: rec.StartTime,
						EndTime: rec.EndTime, OutputFiles: append([]OutputFile(nil), rec.OutputFiles...),
						OutputDirs: append([]string(nil), rec.OutputDirs...),
					})
					break
				}
				live := cp.jobIsLive(state, cmd.Job.ID)
				// Write job data file first so a write failure leaves prior
				// artifacts intact (no archived-but-unrun limbo).
				if err := writeJobFile(cp.queueDir, cmd.Job); err != nil {
					// Log but don't fail.
					fmt.Fprintf(os.Stderr, "warning: write job file: %v\n", err)
				} else {
					// Archive any prior on-host artifacts (.status, .log,
					// .meta, …) so a re-added job ID is treated as a fresh
					// attempt. Without this, a leftover primary .status from
					// a previous completed/failed run causes startJob() to
					// silently skip the job via JobCompleted.
					//
					// Skip when the job is currently running on this host:
					// archive would rename live .heartbeat/.timeseries/.pid
					// out from under the running monitor. A duplicate OpAdd
					// for a live job is legitimate (host_sync forward
					// reconcile, retry paths) — mergeResourceFields
					// in writeJobFile already covers that case.
					if cp.logDir != "" && !live {
						if err := ArchiveExistingFiles(cp.logDir, cmd.Job.ID); err != nil {
							// Don't silently swallow: a rename failure
							// leaves the primary .status in place and
							// would silently re-introduce the skip bug.
							fmt.Fprintf(os.Stderr, "warning: archive prior artifacts for job %d: %v\n", cmd.Job.ID, err)
						}
					}
					if !live {
						delete(state.Rejected, jobIDStr)
						state.addPendingLocked(cmd.Job.ID)
						// Cancel any pending stop — new work arrived.
						state.StopRequested = false
					}
				}
			}

		case opsqueue.OpPriority:
			state.priorityPendingLocked(cmd.JobID)

		case opsqueue.OpCancel:
			state.removePendingLocked(cmd.JobID)
			delete(state.Rejected, strconv.FormatInt(cmd.JobID, 10))
			removeJobFile(cp.queueDir, cmd.JobID)

		case opsqueue.OpStop:
			state.StopRequested = true
			result.StopRequested = true

		case opsqueue.OpRestart:
			state.Cursor = cmd.Timestamp
			state.CursorLine = lineNum
			state.mu.Unlock()
			result.RestartRequested = true
			result.RestartEnv = append([]string(nil), cmd.Env...)
			return result, nil

		default:
			state.mu.Unlock()
			return result, fmt.Errorf("unsupported queue command operation %q at line %d", cmd.Op, lineNum)
		}

		state.Cursor = cmd.Timestamp
		state.CursorLine = lineNum
		state.mu.Unlock()

		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	return result, nil
}

// writeJobFile writes job data to a JSON file in the queue directory.
// If the file already exists, GPU and other resource fields from the existing
// file are preserved when the new data is missing them (handles duplicate add
// commands where a second entry may lack fields present in the first).
func writeJobFile(queueDir string, job *opsqueue.CommandJob) error {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, job.ID)

	// If file exists, merge resource fields from the existing entry
	if existingData, err := os.ReadFile(path); err == nil {
		var existing opsqueue.CommandJob
		if json.Unmarshal(existingData, &existing) == nil {
			mergeResourceFields(job, &existing)
		}
	}

	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// mergeResourceFields preserves resource fields from existing when the new
// entry is missing them. This handles duplicate add commands where a second
// entry may lack fields present in the first.
func mergeResourceFields(job, existing *opsqueue.CommandJob) {
	if job.SourceSHA == "" && existing.SourceSHA != "" {
		job.SourceSHA = existing.SourceSHA
	}
	if job.SourceR2Key == "" && existing.SourceR2Key != "" {
		job.SourceR2Key = existing.SourceR2Key
	}
	if job.SourceManifest == nil && existing.SourceManifest != nil {
		job.SourceManifest = existing.SourceManifest
	}
	if job.CPU == nil && existing.CPU != nil {
		job.CPU = existing.CPU
	}
	if job.GPU == "" && existing.GPU != "" {
		job.GPU = existing.GPU
	}
	if job.GPUClass == "" && existing.GPUClass != "" {
		job.GPUClass = existing.GPUClass
	}
	if job.GPUMem == nil && existing.GPUMem != nil {
		job.GPUMem = existing.GPUMem
	}
	if len(job.OutputDirs) == 0 && len(existing.OutputDirs) > 0 {
		job.OutputDirs = existing.OutputDirs
	}
	if len(job.Outputs) == 0 && len(existing.Outputs) > 0 {
		job.Outputs = existing.Outputs
	}
	if len(job.Produces) == 0 && len(existing.Produces) > 0 {
		job.Produces = existing.Produces
	}
	if len(job.Needs) == 0 && len(existing.Needs) > 0 {
		job.Needs = existing.Needs
	}
	if len(job.ArtifactNeeds) == 0 && len(existing.ArtifactNeeds) > 0 {
		job.ArtifactNeeds = existing.ArtifactNeeds
	}
}

// removeJobFile removes the job data file from the queue directory.
func removeJobFile(queueDir string, jobID int64) {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, jobID)
	os.Remove(path)
}

// ReadJobFile reads job data from the queue directory.
func ReadJobFile(queueDir string, jobID int64) (*opsqueue.CommandJob, error) {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, jobID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var job opsqueue.CommandJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
