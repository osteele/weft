package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/ops"
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

	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++

		// Skip already-processed lines
		if lineNum <= state.CursorLine {
			continue
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var cmd ops.QueueCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			// Skip malformed lines
			state.CursorLine = lineNum
			continue
		}

		switch cmd.Op {
		case ops.OpAdd:
			if cmd.Job == nil {
				break
			}
			state.AddPending(cmd.Job.ID)
			// Write job data file for later use
			if err := writeJobFile(cp.queueDir, cmd.Job); err != nil {
				// Log but don't fail
				fmt.Fprintf(os.Stderr, "warning: write job file: %v\n", err)
			}
			// Cancel any pending stop — new work arrived
			if state.StopRequested {
				state.StopRequested = false
			}

		case ops.OpPriority:
			state.PriorityPending(cmd.JobID)

		case ops.OpCancel:
			state.RemovePending(cmd.JobID)
			removeJobFile(cp.queueDir, cmd.JobID)

		case ops.OpStop:
			state.StopRequested = true
			result.StopRequested = true

		case ops.OpRestart:
			// Save state before restart
			state.Cursor = cmd.Timestamp
			state.CursorLine = lineNum
			result.RestartRequested = true
			return result, nil
		}

		state.Cursor = cmd.Timestamp
		state.CursorLine = lineNum
	}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read commands: %w", err)
	}

	return result, nil
}

// writeJobFile writes job data to a JSON file in the queue directory.
func writeJobFile(queueDir string, job *ops.CommandJob) error {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, job.ID)
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// removeJobFile removes the job data file from the queue directory.
func removeJobFile(queueDir string, jobID int64) {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, jobID)
	os.Remove(path)
}

// ReadJobFile reads job data from the queue directory.
func ReadJobFile(queueDir string, jobID int64) (*ops.CommandJob, error) {
	path := fmt.Sprintf("%s/job-%d.json", queueDir, jobID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var job ops.CommandJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
