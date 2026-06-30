// Package ops provides unified job operations shared between CLI and TUI.
package ops

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hooks"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/notify"
	"github.com/osteele/weft/internal/opscore"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/secrets"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
)

// Re-export Option types from opscore.
type Option[T any] = opscore.Option[T]

func Some[T any](v T) Option[T] { return opscore.Some(v) }
func None[T any]() Option[T]    { return opscore.None[T]() }

// SyncOptions configures sync behavior
type SyncOptions struct {
	Timeout     time.Duration
	SkipSamples bool
}

// StatusFileResult contains the result of reading a status file
type StatusFileResult struct {
	Content string
	Mtime   int64
}

// SyncResult contains the outcome of a sync operation.
type SyncResult struct {
	Updated       bool // Whether the job status was updated in the database
	HostContacted bool // Whether the remote host was successfully contacted
}

// Merge combines two SyncResults, preserving true values from either.
func (r SyncResult) Merge(other SyncResult) SyncResult {
	return SyncResult{
		Updated:       r.Updated || other.Updated,
		HostContacted: r.HostContacted || other.HostContacted,
	}
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
	// Capture the prior status before recording: RecordCompletionByID
	// deliberately accepts re-records on already-terminal jobs (a status
	// file is authoritative evidence), so success alone does not mean a NEW
	// transition. Notify only when the job was previously non-terminal.
	wasTerminal := db.JobIsTerminal(database, jobID)
	// Use status file mtime as end time (when job actually completed)
	// Fall back to current time if mtime not available
	endTime := mtime
	if endTime == 0 {
		endTime = time.Now().Unix()
	}
	if err := db.RecordCompletionByID(database, jobID, exitCode, endTime); err != nil {
		return err
	}
	if !wasTerminal {
		notify.JobTerminal(database, jobID, terminalStatusForExit(exitCode), &exitCode)
	}
	return nil
}

// terminalStatusForExit maps an exit code to the terminal status used for
// notifications, mirroring the oplog complete/fail branching.
func terminalStatusForExit(exitCode int) string {
	if exitCode == 0 {
		return db.StatusCompleted
	}
	return db.StatusFailed
}

// CacheCompletedJobLog attempts to cache a completed job's log file locally.
// This is best-effort: errors are ignored since caching is optional.
// A zero timeout uses the default SSH timeout.
func CacheCompletedJobLog(job *db.Job, timeout time.Duration) {
	if job == nil {
		return
	}

	if logcache.Exists(job.ID) {
		return // Already cached
	}

	cfg, _ := config.Load()
	maxSize := cfg.LogCacheMaxSize
	if maxSize <= 0 {
		return // Caching disabled
	}

	// Get the log file path on the remote host
	logFile := session.JobLogFile(job.ID, job.StartTime, job.SessionName)

	// Try to cache it (ignores errors - caching is best-effort)
	logcache.CacheFromRemote(job.Host, logFile, job.ID, maxSize, timeout)
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
	queueStatePaused
	queueStateQueued
	queueStateDead
	// queueStatePreflightRejected signals that the runner refused to start an
	// attempt before any startTime was stamped. Carries a populated
	// FailureReason; ExitCode is always nil and Mtime carries the rejection
	// timestamp.
	queueStatePreflightRejected
)

type quickStatus struct {
	State     queueState
	ExitCode  *int
	Mtime     int64 // File modification time (unix timestamp) when job completed
	Uncertain bool
}

type remoteQueue interface {
	// StatusFile returns (exitCode, mtime, completed) where mtime is the file modification time
	StatusFile(host string, jobID int64, expectedRunID *int64, timeout time.Duration) (int, int64, Option[bool])
	CurrentJob(host string, jobID int64, timeout time.Duration) Option[bool]
	InQueue(host string, jobID int64, timeout time.Duration) Option[bool]
	ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool]
	ProcessPaused(host string, jobID int64, timeout time.Duration) Option[bool]
	QuickStatus(host string, jobID int64, timeout time.Duration) (quickStatus, error)
	Metadata(host string, jobID int64, timeout time.Duration) (string, error)
	Samples(host string, jobID int64, timeout time.Duration) (string, error)
	Rusage(host string, jobID int64, timeout time.Duration) (string, error)
}

var queueRemoteClient remoteQueue = sshQueueRemote{}

func setQueueRemoteClientForTesting(client remoteQueue) func() {
	prev := queueRemoteClient
	queueRemoteClient = client
	return func() { queueRemoteClient = prev }
}

// SyncJob checks and updates a single job's status.
// Returns SyncResult indicating whether the job was updated and whether the host was contacted.
// This is the full sync version that uses multiple SSH calls for maximum accuracy.
func SyncJob(database *sql.DB, job *db.Job, opts SyncOptions) (result SyncResult, err error) {
	oldStatus := job.Status
	defer func() {
		if err != nil || !result.Updated {
			return
		}
		updated, fetchErr := db.GetJobByID(database, job.ID)
		if fetchErr != nil || updated == nil {
			return
		}
		if hooks.ShouldFireHook(oldStatus, updated.Status) {
			hooks.RunOnJobComplete(updated)
			RecordJobOutputs(database, updated)
		}
	}()
	// SLURM-managed jobs use SLURM probes regardless of session name.
	if job.UsesSlurm() {
		return SyncSlurmJob(database, job, opts)
	}

	// Queue-runner jobs use pattern-based file lookup (handles restarts with different timestamps).
	// Jobs WITH a session name were started via start_now or run and have their own tmux session.
	if job.UsesQueueRunner() {
		return SyncQueueRunnerJob(database, job, opts)
	}

	defer func() {
		if err != nil {
			return
		}
		if opts.SkipSamples {
			return
		}
		metaUpdated, metaErr := updateJobCPUSamples(database, job, opts.Timeout)
		if metaErr != nil {
			err = metaErr
			return
		}
		if metaUpdated {
			result.Updated = true
		}
	}()

	// Regular jobs have their own tmux sessions
	timeout := effectiveSyncTimeout(opts.Timeout)
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return SyncResult{}, err
	}
	// Successfully contacted host via SSH
	result.HostContacted = true

	if exists {
		paused := queueRemoteClient.ProcessPaused(job.Host, job.ID, timeout)
		if paused.IsSome() && paused.Unwrap() {
			if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			switch job.Status {
			case db.StatusQueued, db.StatusStarting, db.StatusRunning:
				if err := db.MarkPausedByID(database, job.ID); err != nil {
					return SyncResult{HostContacted: true}, err
				}
				return SyncResult{Updated: true, HostContacted: true}, nil
			case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
				if err := db.MarkPausedFromTerminal(database, job.ID); err != nil {
					return SyncResult{HostContacted: true}, err
				}
				return SyncResult{Updated: true, HostContacted: true}, nil
			case db.StatusPaused:
				return SyncResult{HostContacted: true}, nil
			}
		}

		if job.Status == db.StatusPaused {
			if err := db.MarkRunningFromPaused(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}

		// Session is running - update status if still marked as starting
		if job.Status == db.StatusStarting {
			if err := db.MarkRunningByID(database, job.ID); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}
		return SyncResult{HostContacted: true}, nil
	}

	// Session doesn't exist - if job is still starting, attempt to launch it now.
	if job.Status == db.StatusStarting {
		if err := startStartingJob(database, job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	// First try exact path (uses job.StartTime)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	sfResult, err := ReadStatusFile(job.Host, statusFile, timeout)
	if err != nil {
		return SyncResult{HostContacted: true}, err
	}

	if sfResult != nil {
		// Job completed
		var exitCode int
		if _, err := fmt.Sscanf(sfResult.Content, "%d", &exitCode); err != nil {
			return SyncResult{HostContacted: true}, fmt.Errorf("parse exit code for job %s on %s from %q: %w", ids.FormatJobID(job.ID), job.Host, sfResult.Content, err)
		}
		if err := RecordJobCompletion(database, job.ID, exitCode, sfResult.Mtime); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		CacheCompletedJobLog(job, timeout)
		// Fetch resource usage data (best-effort)
		_, _ = updateJobResourceUsage(database, job, timeout)
		_ = syncJobTimeseries(database, job, timeout)
		_ = syncJobTelemetry(database, job, timeout)
		return SyncResult{Updated: true, HostContacted: true}, nil
	}

	// Session doesn't exist and no status file.
	// For running jobs, this means the job vanished (crashed, killed externally, etc.)
	// For other states (queued, starting), this could be a race during startup.
	if job.Status == db.StatusRunning || job.Status == db.StatusPaused {
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}
	return SyncResult{HostContacted: true}, nil
}

// SyncQueueRunnerJob checks and updates a queue runner job's status using pattern-based file lookup.
// Uses multiple SSH calls with trinary logic for maximum accuracy.
// Falls back to R2 when SSH to inventory hosts fails.
func SyncQueueRunnerJob(database *sql.DB, job *db.Job, opts SyncOptions) (SyncResult, error) {
	timeout := effectiveSyncTimeout(opts.Timeout)

	// Use the SSH-based prober and host implementations
	prober := remote.NewSSHProber(job.Host, timeout)
	host := remote.NewSSHHost(job.Host, timeout)

	result, err := SyncQueueRunnerJobWithProber(database, job, prober, host, opts)
	if err != nil && !result.HostContacted {
		// SSH failed — try R2 fallback for inventory hosts
		if r2Result, r2Err := syncJobStatusFromR2(database, job); r2Err == nil {
			return r2Result, nil
		}
	}
	return result, err
}

// SyncDraftJob ensures that a job marked as draft has no remote execution state.
// It removes queued entries or kills running processes before marking the job clean.
func SyncDraftJob(database *sql.DB, job *db.Job, opts SyncOptions) (SyncResult, error) {
	if job == nil || job.Status != db.StatusDraft {
		return SyncResult{}, nil
	}

	timeout := effectiveSyncTimeout(opts.Timeout)

	if job.UsesSlurm() {
		if err := cancelSlurmJob(job, timeout); err != nil {
			return SyncResult{}, err
		}
		// SLURM cancel succeeded - host was contacted
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDraft); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	} else if job.UsesQueueRunner() {
		// Queue-runner managed jobs (no session name) need queue/runner cleanup.
		result, err := syncDraftQueueJob(job, timeout)
		if err != nil {
			return result, err
		}
		if !result.HostContacted {
			return result, nil
		}
	} else {
		result, err := syncDraftTmuxJob(job, timeout)
		if err != nil {
			return result, err
		}
		if !result.HostContacted {
			return result, nil
		}
	}

	if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDraft); err != nil {
		return SyncResult{HostContacted: true}, err
	}
	return SyncResult{Updated: true, HostContacted: true}, nil
}

func syncDraftQueueJob(job *db.Job, timeout time.Duration) (SyncResult, error) {
	result, err := queueRemoteClient.QuickStatus(job.Host, job.ID, timeout)
	if err != nil {
		return SyncResult{}, err
	}
	if result.Uncertain {
		// Host unreachable or uncertain state
		return SyncResult{}, nil
	}

	// Got a definitive result - host was contacted
	switch result.State {
	case queueStateRunning, queueStatePaused:
		if err := applyKillToRemote(job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	case queueStateQueued:
		if err := removeFromQueueFile(job.Host, job.ID, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	case queueStateDead:
		return SyncResult{Updated: true, HostContacted: true}, nil
	default:
		return SyncResult{HostContacted: true}, nil
	}
}

func syncDraftTmuxJob(job *db.Job, timeout time.Duration) (SyncResult, error) {
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuickTimeout(job.Host, tmuxSession, timeout)
	if err != nil {
		return SyncResult{}, err
	}
	// Successfully contacted host
	if exists {
		if err := applyKillToRemote(job, timeout); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	}
	return SyncResult{Updated: true, HostContacted: true}, nil
}

// startJobFromRecord starts a job using the data stored in the job record.
func startJobFromRecord(job *db.Job, envVars []string, timeout time.Duration) error {
	resolvedEnv, err := secrets.ResolveEnvVars(envVars)
	if err != nil {
		return err
	}
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

	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      job.ID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		EnvVars:    resolvedEnv,
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
func startQueuedJobNow(database *sql.DB, job *db.Job, timeout time.Duration) error {
	_ = removeFromQueueFile(job.Host, job.ID, timeout)

	tmuxSession := session.TmuxSessionName(job.ID)
	if err := db.UpdateQueuedToRunningWithSession(database, job.ID, tmuxSession); err != nil {
		return err
	}

	updated, err := db.GetJobByID(database, job.ID)
	if err != nil || updated == nil {
		return fmt.Errorf("refresh job %s: %w", ids.FormatJobID(job.ID), err)
	}

	if err := startJobFromRecord(updated, job.EnvVars, timeout); err != nil {
		_ = db.UpdateJobRunningToQueued(database, job.ID)
		return err
	}

	if err := db.ClearPendingStatus(database, job.ID); err != nil {
		return err
	}
	return db.UpdateLastSyncedStatus(database, job.ID, db.StatusRunning)
}

// UpdateTimesFromMetadata reads the metadata file and updates start_time if unset.
// Returns the end_time from metadata (0 if absent) and the parsed metadata map,
// which callers can use to avoid redundant SSH calls for the same data.
func UpdateTimesFromMetadata(database *sql.DB, job *db.Job, timeout time.Duration) (int64, map[string]string, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	content, err := queueRemoteClient.Metadata(job.Host, job.ID, timeout)
	if err != nil || strings.TrimSpace(content) == "" {
		return 0, nil, nil // No metadata file or couldn't read it
	}

	metadata := session.ParseMetadata(content)

	// Update start_time if not already set
	if job.StartTime == 0 {
		if startTimeStr, ok := metadata["start_time"]; ok {
			startTime, parseErr := strconv.ParseInt(startTimeStr, 10, 64)
			if parseErr != nil {
				return 0, nil, fmt.Errorf("parse metadata start time for job %s: %w", ids.FormatJobID(job.ID), parseErr)
			}
			if startTime > 0 {
				if dbErr := db.UpdateStartTime(database, job.ID, startTime); dbErr != nil {
					return 0, nil, fmt.Errorf("update start time for job %s: %w", ids.FormatJobID(job.ID), dbErr)
				}
				job.StartTime = startTime
			}
		}
	}

	// Parse end_time from metadata (written by queue-runner BUILD >= 47)
	var endTime int64
	if endTimeStr, ok := metadata["end_time"]; ok {
		endTime, _ = strconv.ParseInt(endTimeStr, 10, 64)
	}

	return endTime, metadata, nil
}

// UpdateStartTimeFromMetadata reads the metadata file for a queued job and updates its start_time if not already set.
// Returns the parsed metadata map for reuse by callers that need additional fields.
func UpdateStartTimeFromMetadata(database *sql.DB, job *db.Job, timeout time.Duration) (map[string]string, error) {
	_, metadata, err := UpdateTimesFromMetadata(database, job, timeout)
	return metadata, err
}

// Probe functions for trinary logic

// probeStatusFile checks if a job has a status file (completed)
// Returns (exitCode, mtime, Some(true)) if completed, (0, 0, Some(false)) if definitely not, (0, 0, None) on error
func probeStatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool]) {
	return queueRemoteClient.StatusFile(host, jobID, nil, timeout)
}

// probeCurrentJob checks if a job is the current job in the queue runner
func probeCurrentJob(host string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.CurrentJob(host, jobID, timeout)
}

// probeInQueue checks if a job is in the queue file (waiting to run)
func probeInQueue(host string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.InQueue(host, jobID, timeout)
}

// probeProcessRunning checks if the job's process is still running via PID file
func probeProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	return queueRemoteClient.ProcessRunning(host, jobID, timeout)
}

// clearCurrentJobIfMatches clears the queue runner's current job marker if it matches the given job ID.
// This repairs inconsistent state that can occur when disk is full and state files can't be updated.
// Errors are ignored since this is a best-effort repair operation.
func clearCurrentJobIfMatches(host string, jobID int64, timeout time.Duration) {
	currentFile := opsqueue.CurrentFilePath()
	// Only clear if the current file contains this job ID
	cmd := fmt.Sprintf(`current=$(cat %s 2>/dev/null); if [ "$current" = "%d" ]; then echo -n "" > %s && echo "cleared"; fi`,
		currentFile, jobID, currentFile)
	_, _, _ = ssh.RunWithTimeout(host, cmd, timeout)
}

type sshQueueRemote struct{}

func (sshQueueRemote) StatusFile(host string, jobID int64, expectedRunID *int64, timeout time.Duration) (int, int64, Option[bool]) {
	statusPattern := session.StatusFilePattern(jobID)
	statusFile := session.SimpleStatusFile(jobID)
	var expected int64
	if expectedRunID != nil {
		expected = *expectedRunID
	}
	// Get both exit code content and file mtime in one command
	// stat -c %Y gives mtime as unix timestamp on Linux
	cmd := fmt.Sprintf(`if [ -f %s ]; then f=%s; else f=$(ls %s 2>/dev/null | head -1); fi; if [ -n "$f" ]; then c="${f%%.status}.completion.json"; rid=""; if [ -f "$c" ]; then rid=$(sed -n 's/.*"run_id"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$c" | head -1); fi; if [ %d -gt 0 ] && [ -n "$rid" ] && [ "$rid" != "%d" ]; then exit 0; fi; echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)"; fi`, statusFile, statusFile, statusPattern, expected, expected)
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

func (sshQueueRemote) CurrentJob(host string, jobID int64, timeout time.Duration) Option[bool] {
	currentFile := opsqueue.CurrentFilePath()
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

func (sshQueueRemote) InQueue(host string, jobID int64, timeout time.Duration) Option[bool] {
	// Check new state.json format - job is in pending array
	stateFile := opsqueue.StateFilePath()
	// Use jq without -e flag; check result value directly to avoid jq output pollution
	cmd := fmt.Sprintf("jq -e '.pending | index(%d) != null' %s >/dev/null 2>&1 && echo YES || echo NO", jobID, stateFile)
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

func (sshQueueRemote) ProcessPaused(host string, jobID int64, timeout time.Duration) Option[bool] {
	// Check PGID file first (preferred for queue-runner jobs), then fall back to PID file.
	// The PGID file contains the setsid process which is the actual command's process group leader.
	// The PID file contains the wrapper bash which may be in a different state than the command.
	pgidFile := session.SimplePgidFile(jobID)
	pidPattern := session.PidFilePattern(jobID)
	cmd := fmt.Sprintf(`pgid=$(cat %s 2>/dev/null | head -1); if [ -z "$pgid" ]; then pgid=$(cat %s 2>/dev/null | head -1); fi; if [ -n "$pgid" ]; then state=$(ps -o stat= -p $pgid 2>/dev/null | tr -d ' '); case "$state" in *T*) echo YES ;; "") echo NO ;; *) echo NO ;; esac; else echo NO; fi`, pgidFile, pidPattern)
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

// QuickStatus probes the remote host to determine a job's current state.
//
// Probe order (each is mutually exclusive via elif):
//  1. Status file (completed) — definitive, cheapest check
//  2. Current-job marker — queue runner knows its own state
//  3. State.json pending array — reliable queue membership
//  4. Exact PID file — common case for active jobs
//  5. Pattern-matched PID file — handles restarted/archived jobs
//  6. Default DEAD — no evidence of running
func (sshQueueRemote) QuickStatus(host string, jobID int64, timeout time.Duration) (quickStatus, error) {
	statusPattern := session.StatusFilePattern(jobID)
	statusFile := session.SimpleStatusFile(jobID)
	currentFile := opsqueue.CurrentFilePath()
	stateFile := opsqueue.StateFilePath()
	pidPattern := session.PidFilePattern(jobID)
	pidFile := session.SimplePidFile(jobID)
	// When status file exists, output "exitcode|mtime" to capture actual completion time
	// Prioritize exact file (e.g., 2806.status) over archived (2806-*.status) since
	// ls sorts archived files before current ones lexicographically
	combinedCmd := fmt.Sprintf(`
		check_pid_state() {
			local pid=$1
			local state=$(ps -o stat= -p $pid 2>/dev/null | tr -d ' ')
			case "$state" in *T*) echo PAUSED ;; "") echo DEAD ;; *) echo RUNNING ;; esac
		}

		if [ -f %s ]; then
			status_file=%s
		else
			status_file=$(ls %s 2>/dev/null | head -1)
		fi
		# 1. Status file (completed)
		if [ -n "$status_file" ] && [ -f "$status_file" ]; then
			exit_code=$(cat "$status_file" 2>/dev/null | head -1)
			mtime=$(stat -c %%Y "$status_file" 2>/dev/null || stat -f %%m "$status_file" 2>/dev/null)
			echo "${exit_code}|${mtime}"
		# 2. Current-job marker
		elif [ -f %s ] && [ "$(cat %s 2>/dev/null)" = "%d" ]; then
			if [ -f %s ]; then
				pid_file=%s
			else
				pid_file=$(ls %s 2>/dev/null | head -1)
			fi
			if [ -n "$pid_file" ]; then
				pid=$(cat "$pid_file" 2>/dev/null | head -1)
				if [ -n "$pid" ]; then
					check_pid_state "$pid"
				else
					echo DEAD
				fi
			else
				echo RUNNING
			fi
		# 3. State.json pending array
		elif [ -f %s ] && jq -e '.pending | index(%d)' %s >/dev/null 2>&1; then
			echo QUEUED
		# 4. Exact PID file
		elif [ -f %s ]; then
			pid=$(cat %s 2>/dev/null | head -1)
			if [ -n "$pid" ]; then
				check_pid_state "$pid"
			else
				echo DEAD
			fi
		# 5. Pattern-matched PID file
		elif pid_file=$(ls %s 2>/dev/null | head -1) && [ -n "$pid_file" ]; then
			pid=$(cat "$pid_file" 2>/dev/null | head -1)
			if [ -n "$pid" ]; then
				check_pid_state "$pid"
			else
				echo DEAD
			fi
		# 6. Default DEAD
		else
			echo DEAD
		fi
	`, statusFile, statusFile, statusPattern,
		currentFile, currentFile, jobID,
		pidFile, pidFile, pidPattern,
		stateFile, jobID, stateFile,
		pidFile, pidFile,
		pidPattern)

	stdout, _, err := ssh.RunWithTimeout(host, combinedCmd, timeout)
	if err != nil {
		return quickStatus{}, err
	}

	result := strings.TrimSpace(stdout)
	switch result {
	case "RUNNING":
		return quickStatus{State: queueStateRunning}, nil
	case "PAUSED":
		return quickStatus{State: queueStatePaused}, nil
	case "QUEUED":
		return quickStatus{State: queueStateQueued}, nil
	case "DEAD":
		return quickStatus{State: queueStateDead}, nil
	case "UNCERTAIN", "":
		return quickStatus{State: queueStateUnknown, Uncertain: true}, nil
	default:
		// Parse "exitcode|mtime" format
		parts := strings.Split(result, "|")
		var exitCode int
		if _, err := fmt.Sscanf(parts[0], "%d", &exitCode); err != nil {
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

func (sshQueueRemote) Samples(host string, jobID int64, timeout time.Duration) (string, error) {
	samplesPattern := session.SamplesFilePattern(jobID)
	cmd := fmt.Sprintf(`f=$(ls -t %s 2>/dev/null | head -1); if [ -n "$f" ]; then cat "$f"; fi`, samplesPattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

func (sshQueueRemote) Rusage(host string, jobID int64, timeout time.Duration) (string, error) {
	rusagePattern := session.RusageFilePattern(jobID)
	cmd := fmt.Sprintf(`f=$(ls -t %s 2>/dev/null | head -1); if [ -n "$f" ]; then cat "$f"; fi`, rusagePattern)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		return "", err
	}
	return stdout, nil
}
