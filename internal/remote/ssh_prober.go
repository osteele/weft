package remote

import (
	"time"
)

// SSHProber implements the Prober interface using SSH.
// It wraps an SSHHost and converts errors to ProbeResult values.
type SSHProber struct {
	host *SSHHost
}

// NewSSHProber creates a new SSHProber for the given hostname.
func NewSSHProber(hostname string, timeout time.Duration) *SSHProber {
	return &SSHProber{host: NewSSHHost(hostname, timeout)}
}

// ProberForHost creates a Prober for the given hostname.
func ProberForHost(hostname string, timeout time.Duration) Prober {
	return NewSSHProber(hostname, timeout)
}

// Host returns the underlying Host implementation.
func (p *SSHProber) Host() Host {
	return p.host
}

// ProbeInQueue checks if a job is in the queue's pending list.
// Returns ProbeUnknown on error, otherwise ProbeTrue or ProbeFalse.
func (p *SSHProber) ProbeInQueue(queueName string, jobID int64) ProbeResult {
	result, err := p.host.IsJobInQueue(queueName, jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

// ProbeCurrent checks if a job is the currently running job.
// Returns ProbeUnknown on error, otherwise ProbeTrue or ProbeFalse.
func (p *SSHProber) ProbeCurrent(queueName string, jobID int64) ProbeResult {
	result, err := p.host.IsJobCurrent(queueName, jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

// ProbeCompleted checks if a job has completed.
// Returns (ProbeUnknown, nil) on error, (ProbeFalse, nil) if not completed,
// or (ProbeTrue, info) if completed.
func (p *SSHProber) ProbeCompleted(jobID int64) (ProbeResult, *CompletionInfo) {
	info, err := p.host.GetJobCompletion(jobID)
	if err != nil {
		return ProbeUnknown, nil
	}
	if info == nil {
		return ProbeFalse, nil
	}
	return ProbeTrue, info
}

// ProbeProcessRunning checks if the job's process is still running.
// Returns ProbeUnknown on error, otherwise ProbeTrue or ProbeFalse.
func (p *SSHProber) ProbeProcessRunning(jobID int64) ProbeResult {
	result, err := p.host.IsProcessRunning(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

// ProbeProcessPaused checks if the job's process is paused.
func (p *SSHProber) ProbeProcessPaused(jobID int64) ProbeResult {
	result, err := p.host.IsProcessPaused(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}
