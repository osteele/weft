// Package ops provides unified job operations shared between CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/logcache"
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

// StatusFileResult contains the result of reading a status file
type StatusFileResult struct {
	Content string
	Mtime   int64
}

// ReadStatusFile reads a job's status file and returns its content and modification time.
// This is the unified way to read status files across all sync paths.
func ReadStatusFile(host, statusFile string, timeout time.Duration) (*StatusFileResult, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	content, mtime, err := ssh.ReadRemoteFileWithMtime(host, statusFile, timeout)
	if err != nil {
		return nil, err
	}
	if content == "" {
		return nil, nil // File doesn't exist
	}
	return &StatusFileResult{Content: content, Mtime: mtime}, nil
}

// RecordJobCompletion records a job's completion in the database using the status file mtime.
// This is the unified way to record job completion across all sync paths.
func RecordJobCompletion(database *sql.DB, jobID int64, exitCode int, mtime int64) error {
	// Use status file mtime as end time (when job actually completed)
	// Fall back to current time if mtime not available
	endTime := mtime
	if endTime == 0 {
		endTime = time.Now().Unix()
	}
	return db.RecordCompletionByID(database, jobID, exitCode, endTime)
}

// CacheCompletedJobLog attempts to cache a completed job's log file locally.
// This is best-effort: errors are ignored since caching is optional.
func CacheCompletedJobLog(job *db.Job) {
	if job == nil {
		return
	}

	cfg, _ := config.Load()
	maxSize := cfg.LogCacheMaxSize
	if maxSize <= 0 {
		return // Caching disabled
	}

	// Get the log file path on the remote host
	logFile := session.JobLogFile(job.ID, job.StartTime, job.SessionName)

	// Try to cache it (ignores errors - caching is best-effort)
	logcache.CacheFromRemote(job.Host, logFile, job.ID, maxSize)
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
	Mtime     int64 // File modification time (unix timestamp) when job completed
	Uncertain bool
}

type remoteQueue interface {
	// StatusFile returns (exitCode, mtime, completed) where mtime is the file modification time
	StatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool])
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
	// Jobs without a session name are managed by the queue runner and should use
	// pattern-based file lookup (handles restarts with different timestamps).
	// Jobs WITH a session name were started via start_now or run and have their own tmux session.
	if job.SessionName == "" && job.QueueName != "" {
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

	// Session doesn't exist - if job is still starting, attempt to launch it now.
	if job.Status == db.StatusStarting {
		if err := startStartingJob(database, job, timeout); err != nil {
			return false, err
		}
		return true, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	// First try exact path (uses job.StartTime)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	result, err := ReadStatusFile(job.Host, statusFile, timeout)
	if err != nil {
		return false, err
	}

	// If exact path not found and job has a QueueName, try pattern-based lookup.
	// This handles jobs that were started via start_now, killed, then re-ran via queue runner
	// (the new run creates status files with a different timestamp).
	if result == nil && job.QueueName != "" {
		exitCode, mtime, found := queueRemoteClient.StatusFile(job.Host, job.ID, timeout)
		if found.IsSome() && found.Unwrap() {
			if err := RecordJobCompletion(database, job.ID, exitCode, mtime); err != nil {
				return false, err
			}
			CacheCompletedJobLog(job)
			return true, nil
		}
	}

	if result != nil {
		// Job completed
		exitCode, err := strconv.Atoi(result.Content)
		if err != nil {
			return false, fmt.Errorf("parse exit code for job %d on %s: %w", job.ID, job.Host, err)
		}
		if err := RecordJobCompletion(database, job.ID, exitCode, result.Mtime); err != nil {
			return false, err
		}
		CacheCompletedJobLog(job)
		return true, nil
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
	exitCode, mtime, completed := probeStatusFile(job.Host, job.ID, timeout)
	if completed.IsSome() && completed.Unwrap() {
		if err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
			return false, err
		}
		if err := RecordJobCompletion(database, job.ID, exitCode, mtime); err != nil {
			return false, err
		}
		CacheCompletedJobLog(job)
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
		case db.StatusStarting, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			// Job is actually running - fix the status
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
		if job.PendingStatus != nil && (*job.PendingStatus == db.StatusCanceled || *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead) {
			if err := removeFromQueueFile(job.Host, queueName, job.ID, timeout); err != nil {
				return false, err
			}
			finalStatus := db.StatusCanceled
			if *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead {
				finalStatus = db.StatusKilled
			}
			if err := db.ClearPendingAndUpdateStatus(database, job.ID, finalStatus); err != nil {
				return false, err
			}
			return true, nil
		}

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
		// Process is running - if job is marked dead/failed, fix it
		if job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning && job.Status == db.StatusQueued {
		if err := startQueuedJobNow(database, job, queueName, timeout); err != nil {
			return false, err
		}
		return true, nil
	}

	// Ensure queued job is present in queue if it was recorded locally while host was unreachable.
	if inQueue.IsSome() && !inQueue.Unwrap() && job.Status == db.StatusQueued && job.PendingStatus == nil {
		if err := appendQueueEntryForJob(database, job, queueName, timeout); err != nil {
			return false, err
		}
		return true, nil
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

	if job.SessionName == "" && job.QueueName != "" {
		// Queue runner job (no session name) - use optimized check with pattern-based file lookup
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

	// Session doesn't exist - check for status file (exact path first)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	result, err := ReadStatusFile(job.Host, statusFile, timeout)
	// Note: err is ignored here - we'll try pattern-based lookup as fallback

	// If exact path not found (or errored) and job has a QueueName, try pattern-based lookup.
	// This handles jobs that were started via start_now, killed, then re-ran via queue runner,
	// or cases where the exact path check fails due to network issues.
	if result == nil && job.QueueName != "" {
		exitCode, mtime, found := queueRemoteClient.StatusFile(job.Host, job.ID, timeout)
		if found.IsSome() && found.Unwrap() {
			if err := RecordJobCompletion(database, job.ID, exitCode, mtime); err != nil {
				return false, err
			}
			CacheCompletedJobLog(job)
			return true, nil
		}
	}

	// If we had an error from exact path and pattern-based didn't find anything, propagate the error
	if err != nil && result == nil {
		return false, err
	}

	if result != nil {
		exitCode, err := strconv.Atoi(result.Content)
		if err != nil {
			return false, fmt.Errorf("parse exit code for job %d on %s: %w", job.ID, job.Host, err)
		}
		if err := RecordJobCompletion(database, job.ID, exitCode, result.Mtime); err != nil {
			return false, err
		}
		CacheCompletedJobLog(job)
		return true, nil
	}

	// No status file found - but only mark dead if job was actually running.
	// Queued jobs shouldn't be marked dead just because they haven't run yet.
	// Jobs with pending_status have deferred operations and shouldn't be marked dead.
	if job.Status == db.StatusQueued || job.PendingStatus != nil {
		return false, nil
	}
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
		if err := RecordJobCompletion(database, job.ID, *result.ExitCode, result.Mtime); err != nil {
			return false, err
		}
		CacheCompletedJobLog(job)
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
		// Job not found on remote - could be dead or never synced
		if job.Status == db.StatusQueued {
			// Job is queued locally but not on remote - push it to the queue
			if err := appendQueueEntryForJob(database, job, queueName, timeout); err != nil {
				// If push fails (e.g., host unreachable), don't change status
				return false, nil
			}
			return true, nil
		}
		// Before marking dead, do a secondary probe for the status file.
		// QuickStatus may have returned DEAD due to a transient issue.
		exitCode, mtime, found := queueRemoteClient.StatusFile(job.Host, job.ID, timeout)
		if found.IsSome() && found.Unwrap() {
			// Status file exists - job actually completed, not dead
			// Log this anomaly for debugging (QuickStatus said DEAD but file exists)
			log.Printf("sync: QuickStatus returned DEAD for job %d on %s but status file exists (exit=%d, mtime=%d) - recovering as completed",
				job.ID, job.Host, exitCode, mtime)
			UpdateStartTimeFromMetadata(database, job, timeout)
			if err := RecordJobCompletion(database, job.ID, exitCode, mtime); err != nil {
				return false, err
			}
			CacheCompletedJobLog(job)
			return true, nil
		}
		// Confirmed dead - no status file found
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

// SyncDraftJob ensures that a job marked as draft has no remote execution state.
// It removes queued entries or kills running processes before marking the job clean.
func SyncDraftJob(database *sql.DB, job *db.Job, opts SyncOptions) (bool, error) {
	if job == nil || job.Status != db.StatusDraft {
		return false, nil
	}

	timeout := effectiveSyncTimeout(opts.Timeout)

	// Queue-runner managed jobs (no session name) need queue/runner cleanup.
	if job.SessionName == "" && job.QueueName != "" {
		handled, err := syncDraftQueueJob(job, timeout)
		if err != nil {
			return false, err
		}
		if !handled {
			return false, nil
		}
	} else {
		handled, err := syncDraftTmuxJob(job, timeout)
		if err != nil {
			return false, err
		}
		if !handled {
			return false, nil
		}
	}

	if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDraft); err != nil {
		return false, err
	}
	return true, nil
}

func syncDraftQueueJob(job *db.Job, timeout time.Duration) (bool, error) {
	queueName := job.QueueName
	if queueName == "" {
		queueName = DefaultQueueName
	}

	result, err := queueRemoteClient.QuickStatus(job.Host, queueName, job.ID, timeout)
	if err != nil {
		return false, err
	}
	if result.Uncertain {
		return false, nil
	}

	switch result.State {
	case queueStateRunning:
		if err := applyKillToRemote(job, timeout); err != nil {
			return false, err
		}
		return true, nil
	case queueStateQueued:
		if err := removeFromQueueFile(job.Host, queueName, job.ID, timeout); err != nil {
			return false, err
		}
		return true, nil
	case queueStateDead:
		return true, nil
	default:
		return false, nil
	}
}

func syncDraftTmuxJob(job *db.Job, timeout time.Duration) (bool, error) {
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return false, err
	}
	if exists {
		if err := applyKillToRemote(job, timeout); err != nil {
			return false, err
		}
		return true, nil
	}
	return true, nil
}

// appendQueueEntryForJob ensures the queued job exists in the remote queue file.
func appendQueueEntryForJob(database *sql.DB, job *db.Job, queueName string, timeout time.Duration) error {
	if queueName == "" {
		queueName = DefaultQueueName
	}
	entry := QueueEntry{
		JobID:       job.ID,
		WorkingDir:  job.WorkingDir,
		Command:     job.Command,
		Description: job.Description,
		EnvVars:     job.EnvVars,
	}
	opts := AppendQueueEntryOptions{Timeout: timeout}
	if err := AppendQueueEntry(job.Host, queueName, entry, opts); err != nil {
		return err
	}
	return db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued)
}

// startJobFromRecord starts a job using the data stored in the job record.
func startJobFromRecord(job *db.Job, envVars []string, timeout time.Duration) error {
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	if tmuxSession == "" {
		tmuxSession = session.TmuxSessionName(job.ID)
	}

	// Simple file paths (no timestamp in primary files)
	logFile := session.SimpleLogFile(job.ID)
	statusFile := session.SimpleStatusFile(job.ID)
	metadataFile := session.SimpleMetadataFile(job.ID)
	pidFile := session.SimplePidFile(job.ID)

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Ensure log directory exists and archive any old files
	mkdirAndArchive := fmt.Sprintf("mkdir -p %s %s; %s", session.LogDir, artifacts.RemoteArtifactsDir, session.ArchiveCommand(job.ID))
	if _, stderr, err := ssh.RunWithTimeout(job.Host, mkdirAndArchive, timeout); err != nil {
		return fmt.Errorf("create log directory: %s", ssh.FriendlyError(job.Host, stderr, err))
	}

	description := job.Description
	if description == "" {
		description = job.EffectiveDescription()
	}
	metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, description, job.StartTime)
	metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	_, _, _ = ssh.RunWithTimeout(job.Host, metadataCmd, timeout)

	envVars = artifacts.MergeEnvVars(envVars, job.ID)
	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      job.ID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    envVars,
	})

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.RunWithTimeout(job.Host, tmuxCmd, timeout); err != nil {
		return fmt.Errorf("start tmux: %s", ssh.FriendlyError(job.Host, stderr, err))
	}

	return nil
}

// startStartingJob ensures jobs stuck in "starting" state resume once the host is reachable.
func startStartingJob(database *sql.DB, job *db.Job, timeout time.Duration) error {
	if err := startJobFromRecord(job, job.EnvVars, timeout); err != nil {
		return err
	}
	if err := db.UpdateJobRunning(database, job.ID); err != nil {
		return err
	}
	return db.UpdateLastSyncedStatus(database, job.ID, db.StatusRunning)
}

// startQueuedJobNow launches a queued job immediately (used for deferred start-now requests).
func startQueuedJobNow(database *sql.DB, job *db.Job, queueName string, timeout time.Duration) error {
	if queueName == "" {
		queueName = DefaultQueueName
	}

	_ = removeFromQueueFile(job.Host, queueName, job.ID, timeout)

	tmuxSession := session.TmuxSessionName(job.ID)
	if err := db.UpdateQueuedToRunningWithSession(database, job.ID, tmuxSession); err != nil {
		return err
	}

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil || updated == nil {
		return fmt.Errorf("refresh job %d: %w", job.ID, err)
	}

	if err := startJobFromRecord(updated, job.EnvVars, timeout); err != nil {
		_ = db.UpdateJobRunningToQueued(database, job.ID, queueName)
		return err
	}

	if err := db.ClearPendingStatus(database, job.ID); err != nil {
		return err
	}
	return db.UpdateLastSyncedStatus(database, job.ID, db.StatusRunning)
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
// Returns (exitCode, mtime, Some(true)) if completed, (0, 0, Some(false)) if definitely not, (0, 0, None) on error
func probeStatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool]) {
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

func (sshQueueRemote) StatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool]) {
	statusPattern := session.StatusFilePattern(jobID)
	// Get both exit code content and file mtime in one command
	// stat -c %Y gives mtime as unix timestamp on Linux
	cmd := fmt.Sprintf(`f=$(ls %s 2>/dev/null | head -1); if [ -n "$f" ]; then echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)"; fi`, statusPattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return 0, 0, None[bool]()
	}

	output := strings.TrimSpace(stdout)
	if output == "" {
		return 0, 0, Some(false)
	}

	// Parse "exitCode|mtime" format
	parts := strings.Split(output, "|")
	if len(parts) < 1 {
		return 0, 0, None[bool]()
	}

	exitCode, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, None[bool]()
	}

	var mtime int64
	if len(parts) >= 2 {
		mtime, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}

	return exitCode, mtime, Some(true)
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
	// When status file exists, output "exitcode|mtime" to capture actual completion time
	combinedCmd := fmt.Sprintf(`
		status_file=$(ls %s 2>/dev/null | head -1)
		if [ -n "$status_file" ] && [ -f "$status_file" ]; then
			exit_code=$(cat "$status_file" 2>/dev/null | head -1)
			mtime=$(stat -c %%Y "$status_file" 2>/dev/null || stat -f %%m "$status_file" 2>/dev/null)
			echo "${exit_code}|${mtime}"
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
		// Parse "exitcode|mtime" format
		parts := strings.Split(result, "|")
		exitCode, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return quickStatus{State: queueStateUnknown, Uncertain: true}, nil
		}
		var mtime int64
		if len(parts) >= 2 {
			mtime, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		}
		return quickStatus{ExitCode: &exitCode, Mtime: mtime}, nil
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
