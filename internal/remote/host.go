package remote

import (
	"time"
)

// CompletionInfo contains details about a completed job.
type CompletionInfo struct {
	ExitCode int
	EndTime  int64 // Unix timestamp from status file mtime
}

// QueueEntry represents a job to be added to a queue.
type QueueEntry struct {
	JobID        int64
	WorkingDir   string
	Command      string
	Description  string
	EnvVars      []string
	DepSpec      string
	CPUAllotment *int
}

// Host provides semantic operations on a remote host.
// Implementations handle the underlying transport (SSH, local, mock).
//
// This abstraction enables:
// - Testing sync logic by mocking semantic operations, not SSH strings
// - Testing SSH implementation separately with shell command validation
// - Swapping transports (SSH, local exec, containers) without changing sync logic
type Host interface {
	// Queue operations
	IsJobInQueue(queueName string, jobID int64) (bool, error)
	IsJobCurrent(queueName string, jobID int64) (bool, error)
	AppendToQueue(queueName string, entry QueueEntry) error
	RemoveFromQueue(queueName string, jobID int64) error

	// Job status operations
	GetJobCompletion(jobID int64) (*CompletionInfo, error) // nil if not completed
	IsProcessRunning(jobID int64) (bool, error)

	// Metadata operations
	GetJobMetadata(jobID int64) (map[string]string, error)
	GetJobSamples(jobID int64) (string, error)

	// Tmux operations (for non-queue-runner jobs)
	TmuxSessionExists(sessionName string) (bool, error)
	StartTmuxSession(sessionName, command string) error
	KillTmuxSession(sessionName string) error
}

// HostFactory creates Host instances for a given hostname.
type HostFactory interface {
	ForHost(hostname string, timeout time.Duration) Host
}

// ProbeResult represents a trinary result: true, false, or unknown.
// Use when distinguishing "definitely not X" from "couldn't determine" matters.
type ProbeResult int

const (
	ProbeUnknown ProbeResult = iota // Could not determine (timeout, error)
	ProbeTrue                       // Definitely true
	ProbeFalse                      // Definitely false
)

// Prober provides low-level probing with trinary results.
// Used by sync logic that needs to handle uncertainty explicitly.
type Prober interface {
	ProbeInQueue(queueName string, jobID int64) ProbeResult
	ProbeCurrent(queueName string, jobID int64) ProbeResult
	ProbeCompleted(jobID int64) (ProbeResult, *CompletionInfo)
	ProbeProcessRunning(jobID int64) ProbeResult
}
