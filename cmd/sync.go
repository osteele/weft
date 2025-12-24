package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync job statuses from all remote hosts",
	Long: `Sync job statuses by checking all hosts with running jobs.

Automatically finds hosts with running jobs and updates their status
in the local database. Connection failures are silently ignored.

Examples:
  remote-jobs sync              # Sync all hosts
  remote-jobs sync --verbose    # Show progress`,
	RunE: runSync,
}

var syncVerbose bool

const (
	// FastSyncTimeout is used for quick syncs in list/status commands
	FastSyncTimeout = 2 * time.Second
	// NormalSyncTimeout is used for explicit sync commands
	NormalSyncTimeout = 30 * time.Second
)

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all unique hosts with running or queued jobs
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	if len(hosts) == 0 {
		fmt.Println("No active jobs to sync")
		return nil
	}

	var totalUpdated, hostsReached, hostsUnreachable int

	for _, host := range hosts {
		if syncVerbose {
			fmt.Printf("Checking %s...\n", host)
		}

		updated, err := syncHost(database, host)
		if err != nil {
			// Check if it's a connection error
			if ssh.IsConnectionError(err.Error()) {
				hostsUnreachable++
				if syncVerbose {
					fmt.Printf("  %s: unreachable\n", host)
				}
				continue
			}
			// Non-connection error - log warning but continue
			fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			continue
		}

		hostsReached++
		totalUpdated += updated
		if syncVerbose && updated > 0 {
			fmt.Printf("  %s: %d job(s) updated\n", host, updated)
		}
	}

	// Print summary
	if hostsUnreachable > 0 {
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) unreachable)\n",
			totalUpdated, hostsReached, hostsUnreachable)
	} else {
		fmt.Printf("Synced %d job(s) on %d host(s)\n", totalUpdated, hostsReached)
	}

	return nil
}

// syncHost syncs all active jobs (running and queued) for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (int, error) {
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	var updated int
	for _, job := range jobs {
		changed, err := syncJob(database, job)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	// Execute any deferred operations for this host
	if err := executeDeferredOperations(database, host); err != nil {
		// Don't fail the sync if deferred operations fail
		if syncVerbose {
			fmt.Fprintf(os.Stderr, "Warning: failed to execute deferred operations for %s: %v\n", host, err)
		}
	}

	return updated, nil
}

// syncJob checks and updates a single job's status, returning true if status changed
func syncJob(database *sql.DB, job *db.Job) (bool, error) {
	// Jobs without a session name were started by the queue runner
	// They don't have individual tmux sessions, so use pattern-based file lookup
	if job.SessionName == "" {
		return syncQueueRunnerJob(database, job)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		return false, err
	}

	if exists {
		// Job still running, no change
		return false, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		// Job completed
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// No status file - job died unexpectedly
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// updateStartTimeFromMetadata reads the metadata file for a queued job and updates its start_time if not already set
func updateStartTimeFromMetadata(database *sql.DB, job *db.Job) {
	// Only update if start_time is not set
	if job.StartTime > 0 {
		return
	}

	const timeout = 5 * time.Second
	metadataPattern := session.MetadataFilePattern(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", metadataPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil || strings.TrimSpace(stdout) == "" {
		return // No metadata file or couldn't read it
	}

	// Parse metadata
	metadata := session.ParseMetadata(stdout)
	if startTimeStr, ok := metadata["start_time"]; ok {
		if startTime, err := strconv.ParseInt(startTimeStr, 10, 64); err == nil && startTime > 0 {
			// Update database with actual start time from metadata
			db.UpdateStartTime(database, job.ID, startTime)
			// Update in-memory job struct too for current sync cycle
			job.StartTime = startTime
		}
	}
}

// syncQueueRunnerJob checks and updates a queue runner job's status using pattern-based file lookup
func syncQueueRunnerJob(database *sql.DB, job *db.Job) (bool, error) {
	const timeout = 5 * time.Second

	// Check if status file exists (job completed) using glob pattern
	// Queue runner creates files with its own timestamp, not the database start_time
	statusPattern := session.StatusFilePattern(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(stdout) != "" {
		// Job completed - read exit code and update start time from metadata
		exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
		endTime := time.Now().Unix()

		// Update start time from metadata if not already set
		updateStartTimeFromMetadata(database, job)

		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Check if job is in queue's .current file (actively running right now)
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	// Use || true to avoid exit code 1 when file doesn't exist
	currentCmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, currentCmd, timeout)
	if err != nil {
		return false, err
	}

	currentJobID := strings.TrimSpace(stdout)
	if currentJobID == fmt.Sprintf("%d", job.ID) {
		// Job is currently running - update start time from metadata if not set
		updateStartTimeFromMetadata(database, job)
		return false, nil
	}

	// Check if job is still in the queue file (waiting to run)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	grepCmd := fmt.Sprintf("grep -q '^%d	' %s 2>/dev/null && echo yes || echo no", job.ID, queueFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, grepCmd, timeout)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(stdout) == "yes" {
		// Job is still in queue, waiting to run
		return false, nil
	}

	// Check if the job's process is still running (via PID file)
	pidPattern := session.PidFilePattern(job.ID)
	pidCmd := fmt.Sprintf("pid=$(cat %s 2>/dev/null); [ -n \"$pid\" ] && ps -p $pid > /dev/null 2>&1 && echo running || echo not_running", pidPattern)
	stdout, _, err = ssh.RunWithTimeout(job.Host, pidCmd, timeout)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(stdout) == "running" {
		// Process is still running, don't mark as dead
		return false, nil
	}

	// Job is not current, not in queue, process not running, and has no status file - it's dead
	// (Either it died mid-execution, or was removed from queue)
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// executeDeferredOperations executes pending operations for a host
func executeDeferredOperations(database *sql.DB, host string) error {
	ops, err := db.GetDeferredOperations(database, host)
	if err != nil {
		return fmt.Errorf("get deferred operations: %w", err)
	}

	if len(ops) == 0 {
		return nil
	}

	if syncVerbose {
		fmt.Printf("  %s: executing %d deferred operation(s)\n", host, len(ops))
	}

	for _, op := range ops {
		var err error
		switch op.Operation {
		case db.OpKillJob:
			err = executeDeferredKill(host, op)
		case db.OpRemoveQueued:
			err = executeDeferredRemoveQueued(host, op)
		case db.OpMoveFromQueue:
			err = executeDeferredMoveFrom(host, op)
		default:
			err = fmt.Errorf("unknown operation: %s", op.Operation)
		}

		if err != nil {
			if syncVerbose {
				fmt.Fprintf(os.Stderr, "    Warning: operation %s for job %d failed: %v\n",
					op.Operation, op.JobID, err)
			}
			// Continue with other operations
			continue
		}

		// Remove completed operation
		if err := db.DeleteDeferredOperation(database, op.ID); err != nil {
			if syncVerbose {
				fmt.Fprintf(os.Stderr, "    Warning: failed to delete deferred operation %d: %v\n",
					op.ID, err)
			}
		} else if syncVerbose {
			fmt.Printf("    Completed: %s for job %d\n", op.Operation, op.JobID)
		}
	}

	return nil
}

// executeDeferredKill kills a job's tmux session
func executeDeferredKill(host string, op *db.DeferredOperation) error {
	tmuxSession := session.TmuxSessionName(op.JobID)
	return ssh.TmuxKillSession(host, tmuxSession)
}

// executeDeferredRemoveQueued removes a job from the queue file
func executeDeferredRemoveQueued(host string, op *db.DeferredOperation) error {
	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", op.JobID, queueFile)
	_, _, err := ssh.Run(host, removeCmd)
	return err
}

// executeDeferredMoveFrom removes a job from the old host's queue file (for job move)
func executeDeferredMoveFrom(host string, op *db.DeferredOperation) error {
	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", op.JobID, queueFile)
	_, _, err := ssh.Run(host, removeCmd)
	return err
}

// performFastSync performs a quick sync with fast timeout for list/status commands
// Returns true if sync completed, false if timed out
func performFastSync(database *sql.DB, verbose bool) bool {
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil || len(hosts) == 0 {
		return true
	}

	// Set fast timeout for SSH operations
	// We'll use goroutines with a timeout context
	allCompleted := true
	for _, host := range hosts {
		// Try quick sync, but don't wait if it times out
		done := make(chan bool, 1)
		go func(h string) {
			_, err := syncHostWithTimeout(database, h, FastSyncTimeout)
			done <- (err == nil)
		}(host)

		select {
		case <-done:
			// Sync completed
		case <-time.After(FastSyncTimeout):
			// Timed out
			allCompleted = false
		}
	}

	return allCompleted
}

// syncHostWithTimeout syncs a host with a specific timeout
func syncHostWithTimeout(database *sql.DB, host string, timeout time.Duration) (int, error) {
	// This is a simplified version of syncHost that uses quick timeouts
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	var updated int
	for _, job := range jobs {
		// Use quick check with timeout
		changed, err := syncJobQuick(database, job, timeout)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	return updated, nil
}

// syncJobQuick is a quick version of syncJob with timeout
func syncJobQuick(database *sql.DB, job *db.Job, timeout time.Duration) (bool, error) {
	if job.SessionName == "" {
		// Queue runner job - use optimized check
		return syncQueueRunnerJobQuick(database, job, timeout)
	}

	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	// Session doesn't exist - check for status file
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// No status file - mark as dead
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// syncQueueRunnerJobQuick is an optimized version for queue runner jobs that combines
// all status checks into a single SSH command to reduce latency
func syncQueueRunnerJobQuick(database *sql.DB, job *db.Job, timeout time.Duration) (bool, error) {
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	// Combine all checks into ONE SSH command for fast sync
	// This checks: status file, .current file, .queue file, and PID file
	// Returns: exit code (if completed), RUNNING, QUEUED, or DEAD
	statusPattern := session.StatusFilePattern(job.ID)
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	pidPattern := session.PidFilePattern(job.ID)

	combinedCmd := fmt.Sprintf(`
		# Check status file (completed?)
		if [ -f %s ]; then
			cat %s 2>/dev/null | head -1
		# Check if currently running in queue
		elif [ -f %s ] && [ "$(cat %s 2>/dev/null)" = "%d" ]; then
			echo RUNNING
		# Check if waiting in queue
		elif grep -q '^%d	' %s 2>/dev/null; then
			echo QUEUED
		# Check if process still running via PID
		elif pid=$(cat %s 2>/dev/null) && [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1; then
			echo RUNNING
		else
			echo DEAD
		fi
	`, statusPattern, statusPattern,
		currentFile, currentFile, job.ID,
		job.ID, queueFile,
		pidPattern)

	stdout, _, err := ssh.RunWithTimeout(job.Host, combinedCmd, timeout)
	if err != nil {
		// Connection error - don't update status
		return false, nil
	}

	result := strings.TrimSpace(stdout)

	// Parse result and update database
	switch result {
	case "RUNNING", "QUEUED":
		// Job is still active, no change needed
		return false, nil
	case "DEAD":
		// Job has died unexpectedly
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	case "":
		// Empty result (shouldn't happen with our logic, but handle gracefully)
		return false, nil
	default:
		// Numeric exit code - job completed
		exitCode, parseErr := strconv.Atoi(result)
		if parseErr != nil {
			// Unexpected output - don't change status
			return false, nil
		}
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}
}
