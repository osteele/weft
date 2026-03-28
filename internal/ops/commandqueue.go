package ops

import "github.com/osteele/weft/internal/opsqueue"

// Re-export queue command protocol types and operations from opsqueue.
const (
	OpAdd      = opsqueue.OpAdd
	OpPriority = opsqueue.OpPriority
	OpCancel   = opsqueue.OpCancel
	OpStop     = opsqueue.OpStop
	OpRestart  = opsqueue.OpRestart
)

type CommandJob = opsqueue.CommandJob
type QueueCommand = opsqueue.QueueCommand
type AppendCommandOptions = opsqueue.AppendCommandOptions
type RunnerState = opsqueue.RunnerState
type RunnerJobState = opsqueue.RunnerJobState

func CommandsFileName() string { return opsqueue.CommandsFileName() }
func CommandsFilePath() string { return opsqueue.CommandsFilePath() }
func StateFileName() string    { return opsqueue.StateFileName() }
func StateFilePath() string    { return opsqueue.StateFilePath() }

func NewAddCommand(entry QueueEntry) QueueCommand { return opsqueue.NewAddCommand(entry) }
func NewPriorityCommand(jobID int64) QueueCommand { return opsqueue.NewPriorityCommand(jobID) }
func NewCancelCommand(jobID int64) QueueCommand   { return opsqueue.NewCancelCommand(jobID) }
func NewStopCommand() QueueCommand                { return opsqueue.NewStopCommand() }
func NewRestartCommand() QueueCommand             { return opsqueue.NewRestartCommand() }
func AppendCommand(host string, cmd QueueCommand, opts AppendCommandOptions) error {
	return opsqueue.AppendCommand(host, cmd, opts)
}
func AppendCommandLocal(commandsFile string, cmd QueueCommand) error {
	return opsqueue.AppendCommandLocal(commandsFile, cmd)
}
