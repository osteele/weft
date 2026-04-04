package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/osteele/weft/internal/opsqueue"
)

// CommandProcessor reads the append-only JSONL command log and processes
// new commands since the last cursor position.
type CommandProcessor struct {
	commandsFile string
	queueDir     string
}

// CommandResult describes the outcome of processing all new commands.
type CommandResult struct {
	StopRequested    bool
	RestartRequested bool
}

// NewCommandProcessor creates a processor for the given commands file.
func NewCommandProcessor(commandsFile, queueDir string) *CommandProcessor {
	return &CommandProcessor{
		commandsFile: commandsFile,
		queueDir:     queueDir,
	}
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
			// Skip malformed lines
			state.SetCursorLine(lineNum)
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		state.mu.Lock()
		switch cmd.Op {
		case opsqueue.OpAdd:
			if cmd.Job != nil {
				// Write job data file for later use.
				if err := writeJobFile(cp.queueDir, cmd.Job); err != nil {
					// Log but don't fail.
					fmt.Fprintf(os.Stderr, "warning: write job file: %v\n", err)
				} else {
					state.addPendingLocked(cmd.Job.ID)
					// Cancel any pending stop — new work arrived.
					state.StopRequested = false
				}
			}

		case opsqueue.OpPriority:
			state.priorityPendingLocked(cmd.JobID)

		case opsqueue.OpCancel:
			state.removePendingLocked(cmd.JobID)
			removeJobFile(cp.queueDir, cmd.JobID)

		case opsqueue.OpStop:
			state.StopRequested = true
			result.StopRequested = true

		case opsqueue.OpRestart:
			state.Cursor = cmd.Timestamp
			state.CursorLine = lineNum
			state.mu.Unlock()
			result.RestartRequested = true
			return result, nil
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
	if len(job.Tags) == 0 && len(existing.Tags) > 0 {
		job.Tags = existing.Tags
	}
	if len(job.OutputDirs) == 0 && len(existing.OutputDirs) > 0 {
		job.OutputDirs = existing.OutputDirs
	}
	if len(job.Produces) == 0 && len(existing.Produces) > 0 {
		job.Produces = existing.Produces
	}
	if len(job.Needs) == 0 && len(existing.Needs) > 0 {
		job.Needs = existing.Needs
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
