// Package ops provides unified job operations shared between CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Option represents an optional value that may or may not be present.
// Used for trinary logic where we need to distinguish "unknown" from "false".
type Option[T any] struct {
	value T
	valid bool
}

// Some creates an Option containing a value
func Some[T any](v T) Option[T] {
	return Option[T]{value: v, valid: true}
}

// None creates an empty Option (unknown/missing value)
func None[T any]() Option[T] {
	return Option[T]{}
}

// IsSome returns true if the Option contains a value
func (o Option[T]) IsSome() bool { return o.valid }

// IsNone returns true if the Option is empty
func (o Option[T]) IsNone() bool { return !o.valid }

// Unwrap returns the contained value (caller must check IsSome first)
func (o Option[T]) Unwrap() T { return o.value }

// SyncOptions configures sync behavior
type SyncOptions struct {
	Timeout time.Duration
}

// DefaultSyncOptions returns default sync options
func DefaultSyncOptions() SyncOptions {
	return SyncOptions{
		Timeout: 5 * time.Second,
	}
}

func effectiveSyncTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 5 * time.Second
	}
	return timeout
}

type queueState int

const (
	queueStateUnknown queueState = iota
	queueStateRunning
	queueStateQueued
	queueStateDead
)

type quickStatus struct {
	State     queueState
	ExitCode  *int
	Uncertain bool
}

type remoteQueue interface {
	StatusFile(host string, jobID int64, timeout time.Duration) (int, Option[bool])
	CurrentJob(host, queueName string, jobID int64, timeout time.Duration) Option[bool]
	InQueue(host, queueName string, jobID int64, timeout time.Duration) Option[bool]
	ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool]
	QuickStatus(host, queueName string, jobID int64, timeout time.Duration) (quickStatus, error)
	Metadata(host string, jobID int64, timeout time.Duration) (string, error)
}

var queueRemoteClient remoteQueue = sshQueueRemote{}

func setQueueRemoteClientForTesting(client remoteQueue) func() {
	prev := queueRemoteClient
	queueRemoteClient = client
	return func() { queueRemoteClient = prev }
}

// SyncJob checks and updates a single job's status, returning true if status changed.
// This is the full sync version that uses multiple SSH calls for maximum accuracy.
func SyncJob(database *sql.DB, job *db.Job, opts SyncOptions) (bool, error) {
	// Jobs without a session name were started by the queue runner
	// They don't have individual tmux sessions, so use pattern-based file lookup
	if job.SessionName == "" {
		return SyncQueueRunnerJob(database, job, opts)
	}

	// Regular jobs have their own tmux sessions
	timeout := effectiveSyncTimeout(opts.Timeout)
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return false, err
	}

	if exists {
		// Session is running - update status if still marked as starting
		if job.Status == db.StatusStarting {
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuickTimeout(job.Host, statusFile, timeout)
	if err != nil {
		return false, err
	}

	if content != "" {
		// Job completed
		exitCode, err := strconv.Atoi(content)
		if err != nil {
			return false, fmt.Errorf("parse exit code for job %d on %s: %w", job.ID, job.Host, err)
		}
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Session doesn't exist and no status file - check for pending operations
	hasPending, err := db.HasPendingDeferredOperationForJob(database, job.ID)
	if err != nil {
		return false, err
	}
	if hasPending {
		return false, nil
	}

	// Session doesn't exist and no status file - this is UNCERTAIN, not dead.
	// Could be a race condition during job startup/shutdown.
	// Don't mark dead based on absence of evidence.
	return false, nil
}

// SyncQueueRunnerJob checks and updates a queue runner job's status using pattern-based file lookup.
// Uses multiple SSH calls with trinary logic for maximum accuracy.
func SyncQueueRunnerJob(database *sql.DB, job *db.Job, opts SyncOptions) (bool, error) {
	timeout := effectiveSyncTimeout(opts.Timeout)

	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	// Probe 1: Check if status file exists (job completed)
	exitCode, completed := probeStatusFile(job.Host, job.ID, timeout)
	if completed.IsSome() && completed.Unwrap() {
		endTime := time.Now().Unix()
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Probe 2: Check if job is the current job in queue runner
	isCurrent := probeCurrentJob(job.Host, queueName, job.ID, timeout)
	if isCurrent.IsSome() && isCurrent.Unwrap() {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		switch job.Status {
		case db.StatusQueued:
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusStarting:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Probe 3: Check if job is in queue file (waiting)
	inQueue := probeInQueue(job.Host, queueName, job.ID, timeout)
	if inQueue.IsSome() && inQueue.Unwrap() {
		// Job is queued - if DB says running, fix it
		if job.Status == db.StatusRunning {
			if err := db.MarkQueuedByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Probe 4: Check if process is running via PID
	processRunning := probeProcessRunning(job.Host, job.ID, timeout)
	if processRunning.IsSome() && processRunning.Unwrap() {
		return false, nil
	}

	// Only mark dead if ALL probes returned definitive false (not None/unknown)
	// This is trinary AND: unknown (None) propagates - we don't turn unknowns into knowns
	allDefinitelyFalse := completed.IsSome() && !completed.Unwrap() &&
		isCurrent.IsSome() && !isCurrent.Unwrap() &&
		inQueue.IsSome() && !inQueue.Unwrap() &&
		processRunning.IsSome() && !processRunning.Unwrap()

	if allDefinitelyFalse {
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	}

	// At least one probe returned None (unknown) - don't change status
	return false, nil
}

// SyncJobQuick is an optimized version of SyncJob that uses a single SSH command.
// Suitable for TUI where latency matters more than perfect accuracy.
func SyncJobQuick(database *sql.DB, job *db.Job, opts SyncOptions) (bool, error) {
	timeout := effectiveSyncTimeout(opts.Timeout)

	if job.SessionName == "" {
		// Queue runner job - use optimized check
		return SyncQueueRunnerJobQuick(database, job, opts)
	}

	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	// Session doesn't exist - check for status file
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuickTimeout(job.Host, statusFile, timeout)
	if err != nil {
		return false, err
	}

	if content != "" {
		exitCode, err := strconv.Atoi(content)
		if err != nil {
			return false, fmt.Errorf("parse exit code for job %d on %s: %w", job.ID, job.Host, err)
		}
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	hasPending, err := db.HasPendingDeferredOperationForJob(database, job.ID)
	if err != nil {
		return false, err
	}
	if hasPending {
		return false, nil
	}

	// No status file - mark as dead
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// SyncQueueRunnerJobQuick is an optimized version for queue runner jobs that combines
// all status checks into a single SSH command to reduce latency.
func SyncQueueRunnerJobQuick(database *sql.DB, job *db.Job, opts SyncOptions) (bool, error) {
	timeout := effectiveSyncTimeout(opts.Timeout)

	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	result, err := queueRemoteClient.QuickStatus(job.Host, queueName, job.ID, timeout)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return false, nil
		}
		return false, err
	}

	if result.ExitCode != nil {
		UpdateStartTimeFromMetadata(database, job, timeout)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, *result.ExitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	if result.Uncertain {
		return false, nil
	}

	switch result.State {
	case queueStateRunning:
		// Job is running - update start time from metadata if not set
		UpdateStartTimeFromMetadata(database, job, timeout)
		switch job.Status {
		case db.StatusQueued:
			if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		case db.StatusStarting:
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	case queueStateQueued:
		// Job is queued - if DB says running, fix it
		if job.Status == db.StatusRunning {
			if err := db.MarkQueuedByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	case queueStateDead:
		// Before marking dead, check if job has pending deferred operations
		// (e.g., queue_job not yet synced to remote)
		hasPending, _ := db.HasPendingDeferredOperationForJob(database, job.ID)
		if hasPending {
			// Job has pending operations - don't mark as dead
			return false, nil
		}
		// Job has died unexpectedly
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

// UpdateStartTimeFromMetadata reads the metadata file for a queued job and updates its start_time if not already set
func UpdateStartTimeFromMetadata(database *sql.DB, job *db.Job, timeout time.Duration) error {
	// Only update if start_time is not set
	if job.StartTime > 0 {
		return nil
	}

	if timeout == 0 {
		timeout = 5 * time.Second
	}

	content, err := queueRemoteClient.Metadata(job.Host, job.ID, timeout)
	if err != nil || strings.TrimSpace(content) == "" {
		return nil // No metadata file or couldn't read it
	}

	// Parse metadata
	metadata := session.ParseMetadata(content)
	if startTimeStr, ok := metadata["start_time"]; ok {
		startTime, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			return fmt.Errorf("parse metadata start time for job %d: %w", job.ID, err)
		}
		if startTime <= 0 {
			return nil
		}
		// Update database with actual start time from metadata
		if err := db.UpdateStartTime(database, job.ID, startTime); err != nil {
			return fmt.Errorf("update start time for job %d: %w", job.ID, err)
		}
		// Update in-memory job struct too for current sync cycle
		job.StartTime = startTime
	}
	return nil
}

// Probe functions for trinary logic

// probeStatusFile checks if a job has a status file (completed)
// Returns (exitCode, Some(true)) if completed, (0, Some(false)) if definitely not, (0, None) on error
func probeStatusFile(host string, jobID int64, timeout time.Duration) (int, Option[bool]) {
	return queueRemoteClient.StatusFile(host, jobID, timeout)
}

// probeCurrentJob checks if a job is the current job in the queue runner
func probeCurrentJob(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.CurrentJob(host, queueName, jobID, timeout)
}

// probeInQueue checks if a job is in the queue file (waiting to run)
func probeInQueue(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.InQueue(host, queueName, jobID, timeout)
}

// probeProcessRunning checks if the job's process is still running via PID file
func probeProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.ProcessRunning(host, jobID, timeout)
}

type sshQueueRemote struct{}

func (sshQueueRemote) StatusFile(host string, jobID int64, timeout time.Duration) (int, Option[bool]) {
	statusPattern := session.StatusFilePattern(jobID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return 0, None[bool]()
	}

	exitCodeStr := strings.TrimSpace(stdout)
	if exitCodeStr == "" {
		return 0, Some(false)
	}

	exitCode, err := strconv.Atoi(exitCodeStr)
	if err != nil {
		return 0, None[bool]()
	}
	return exitCode, Some(true)
}

func (sshQueueRemote) CurrentJob(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	cmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return None[bool]()
	}

	currentJobID := strings.TrimSpace(stdout)
	if currentJobID == fmt.Sprintf("%d", jobID) {
		return Some(true)
	}
	return Some(false)
}

func (sshQueueRemote) InQueue(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	// Match job ID at start of line followed by either real tab or literal \t (for backwards compat)
	cmd := fmt.Sprintf("grep -E -q '^%d(	|\\\\t)' %s 2>/dev/null && echo YES || echo NO", jobID, queueFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return None[bool]()
	}

	result := strings.TrimSpace(stdout)
	switch result {
	case "YES":
		return Some(true)
	case "NO":
		return Some(false)
	default:
		return None[bool]()
	}
}

func (sshQueueRemote) ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	pidPattern := session.PidFilePattern(jobID)
	cmd := fmt.Sprintf("pid=$(cat %s 2>/dev/null | head -1); [ -n \"$pid\" ] && ps -p $pid > /dev/null 2>&1 && echo YES || echo NO", pidPattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return None[bool]()
	}

	result := strings.TrimSpace(stdout)
	switch result {
	case "YES":
		return Some(true)
	case "NO":
		return Some(false)
	default:
		return None[bool]()
	}
}

func (sshQueueRemote) QuickStatus(host, queueName string, jobID int64, timeout time.Duration) (quickStatus, error) {
	statusPattern := session.StatusFilePattern(jobID)
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	pidPattern := session.PidFilePattern(jobID)
	combinedCmd := fmt.Sprintf(`
		status_file=$(ls %s 2>/dev/null | head -1)
		if [ -n "$status_file" ] && [ -f "$status_file" ]; then
			cat "$status_file" 2>/dev/null | head -1
		elif [ -f %s ] && [ "$(cat %s 2>/dev/null)" = "%d" ]; then
			pid_file=$(ls %s 2>/dev/null | head -1)
			if [ -n "$pid_file" ]; then
				pid=$(cat "$pid_file" 2>/dev/null | head -1)
				if [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1; then
					echo RUNNING
				else
					echo DEAD
				fi
			else
				echo RUNNING
			fi
		# Match either real tab or literal \t for backwards compatibility
		elif grep -E -q '^%d(	|\\t)' %s 2>/dev/null; then
			echo QUEUED
		elif pid_file=$(ls %s 2>/dev/null | head -1) && [ -n "$pid_file" ]; then
			pid=$(cat "$pid_file" 2>/dev/null | head -1)
			if [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1; then
				echo RUNNING
			else
				echo DEAD
			fi
		else
			echo DEAD
		fi
	`, statusPattern,
		currentFile, currentFile, jobID,
		pidPattern,
		jobID, queueFile,
		pidPattern)

	stdout, _, err := ssh.RunWithTimeout(host, combinedCmd, timeout)
	if err != nil {
		return quickStatus{}, err
	}

	result := strings.TrimSpace(stdout)
	switch result {
	case "RUNNING":
		return quickStatus{State: queueStateRunning}, nil
	case "QUEUED":
		return quickStatus{State: queueStateQueued}, nil
	case "DEAD":
		return quickStatus{State: queueStateDead}, nil
	case "UNCERTAIN", "":
		return quickStatus{State: queueStateUnknown, Uncertain: true}, nil
	default:
		exitCode, err := strconv.Atoi(result)
		if err != nil {
			return quickStatus{State: queueStateUnknown, Uncertain: true}, nil
		}
		return quickStatus{ExitCode: &exitCode}, nil
	}
}

func (sshQueueRemote) Metadata(host string, jobID int64, timeout time.Duration) (string, error) {
	metadataPattern := session.MetadataFilePattern(jobID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", metadataPattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return "", err
	}
	return stdout, nil
}
