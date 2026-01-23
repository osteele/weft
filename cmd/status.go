package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

// Exit codes
const (
	ExitSuccess  = 0
	ExitFailed   = 1
	ExitRunning  = 2
	ExitNotFound = 3
)

var (
	statusSync        bool
	statusNoSync      bool
	statusFast        bool
	statusWait        bool
	statusWaitTimeout time.Duration
)

var statusCmd = &cobra.Command{
	Use:   "status [job-id]...",
	Short: "Check the status of jobs",
	Long: `Check the status of one or more jobs.

Without arguments, shows all active jobs (running, starting, queued)
and recent failures from the last 24 hours.

Job IDs can be specified individually or as ranges:
  - Single ID: 42
  - Range: 42:47 or 42::47 (expands to 42, 43, 44, 45, 46, 47)
  - Mixed: 42 50:52 60 (expands to 42, 50, 51, 52, 60)

Duplicate IDs are automatically removed with a warning.

Exit codes (single job only):
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Examples:
  remote-jobs status              # Show all active jobs
  remote-jobs status 42
  remote-jobs status 42:47        # Check jobs 42 through 47
  remote-jobs status 42 --fast    # Quick check with 2s timeout
  remote-jobs status 42:47 --watch # Wait for jobs 42-47 to complete`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&statusSync, "sync", false, "Perform full sync (30s timeout)")
	statusCmd.Flags().BoolVar(&statusNoSync, "no-sync", false, "Skip syncing job statuses before checking")
	statusCmd.Flags().BoolVar(&statusFast, "fast", false, "Use quick 2s timeout (default is 5s)")
	statusCmd.Flags().BoolVar(&statusWait, "wait", false, "Wait for the job(s) to complete before returning")
	statusCmd.Flags().BoolVar(&statusWait, "watch", false, "Wait for the job(s) to complete before returning (synonym for --wait)")
	statusCmd.Flags().DurationVar(&statusWaitTimeout, "wait-timeout", 0, "Maximum time to wait for completion (0 = no limit)")
}

func runStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// No args: show all active jobs
	if len(args) == 0 {
		return showActiveJobs(database)
	}

	// Parse job IDs (supports ranges like 123:127, deduplicates with warning)
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	var tracker *hostConnectionTracker
	if statusWait {
		statusSync = true
		statusNoSync = false
		tracker = newHostConnectionTracker()
	}

	// Check if all requested jobs are already in terminal state - skip sync if so
	needsSync := false
	hostsToSync := make(map[string]struct{})
	if !statusNoSync {
		for _, jobID := range jobIDs {
			job, err := db.GetJobByID(database, jobID)
			if err != nil || job == nil {
				continue
			}
			if !isTerminalStatus(job.Status) {
				needsSync = true
				if job.Host != "" {
					hostsToSync[job.Host] = struct{}{}
				}
			}
		}
	}

	// Sync logic: default 5s, fast 2s, full 30s, or skip
	if needsSync {
		hosts := mapKeys(hostsToSync)
		if statusSync {
			// Full sync requested (30s timeout)
			for _, host := range hosts {
				syncHost(database, host)
			}
			// Start queue runners (full sync mode)
			startQueueRunnersForHosts(database, hosts)
		} else if statusFast {
			// Fast sync (2s timeout) - skip queue starting for speed
			completed, unreachable := performFastSyncForHosts(database, hosts, false)
			if !completed {
				if note := buildStaleDataNote(database, unreachable); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
		} else {
			// Default sync (5s timeout)
			completed, unreachable := performSyncWithTimeoutForHosts(database, hosts, DefaultSyncTimeout, false)
			if !completed {
				if note := buildStaleDataNote(database, unreachable); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
			// Start queue runners (default mode)
			startQueueRunnersForHosts(database, hosts)
		}
	}

	waitRequests := make([]jobStatusRequest, 0, len(jobIDs))
	waitInputInvalid := false
	singleJob := len(jobIDs) == 1 && !statusWait
	for i, jobID := range jobIDs {
		if i > 0 && !statusWait {
			fmt.Println("---")
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: %v\n", jobID, err)
			if singleJob {
				os.Exit(ExitNotFound)
			}
			if statusWait {
				waitInputInvalid = true
			}
			continue
		}

		if statusWait {
			waitRequests = append(waitRequests, jobStatusRequest{ID: jobID, Job: job})
			continue
		}

		printSingleJobStatus(database, jobID, job, singleJob)
	}

	if statusWait {
		if len(waitRequests) == 0 {
			return fmt.Errorf("no valid job IDs to wait for")
		}
		results, err := waitForJobsCompletion(database, waitRequests, statusWaitTimeout, tracker)
		if err != nil {
			if errors.Is(err, errWaitTimeout) {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}
			return err
		}
		allSucceeded := allJobsSucceeded(waitRequests, results)
		if waitInputInvalid {
			allSucceeded = false
		}
		if allSucceeded {
			os.Exit(ExitSuccess)
		} else {
			os.Exit(ExitFailed)
		}
	}

	return nil
}

func printSingleJobStatus(database *sql.DB, jobID int64, job *db.Job, exitOnComplete bool) {
	if job == nil {
		fmt.Printf("Job %d not found\n", jobID)
		if exitOnComplete {
			os.Exit(ExitNotFound)
		}
		return
	}

	// If job is already marked as terminal, use cached result
	if isWaitTerminalStatus(job.Status) {
		printJobStatus(job, exitOnComplete)
		return
	}

	// Queue runner and SLURM jobs don't have tmux sessions to check.
	// They are managed by their respective backends and synced separately.
	if job.UsesQueueRunner() || job.UsesSlurm() {
		// For queued jobs, just display current status - don't mark dead
		printJobStatus(job, exitOnComplete)
		return
	}

	// Job is marked as running - verify actual status on remote
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExists(job.Host, tmuxSession)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Job %d: check session: %v\n", jobID, err)
		return
	}

	if !exists {
		// Session doesn't exist - check for status file
		statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
		result, err := ops.ReadStatusFile(job.Host, statusFile, 10*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: read status file: %v\n", jobID, err)
			return
		}

		if result != nil {
			// Job completed, update database
			exitCodeStr := strings.TrimSpace(result.Content)
			exitCode, err := strconv.Atoi(exitCodeStr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Job %d: invalid status file contents %q: %v\n", jobID, exitCodeStr, err)
				if exitOnComplete {
					os.Exit(ExitFailed)
				}
				return
			}
			if err := ops.RecordJobCompletion(database, job.ID, exitCode, result.Mtime); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusCompleted
			job.ExitCode = &exitCode
			// Use status file mtime as end time
			endTime := result.Mtime
			if endTime == 0 {
				endTime = time.Now().Unix()
			}
			job.EndTime = &endTime
		} else {
			// Job terminated unexpectedly
			if err := db.MarkDeadByID(database, job.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusFailed
		}
	} else if exitOnComplete {
		// Session still running - show last few lines of output (only for single job)
		output, _ := ssh.TmuxCapturePaneOutput(job.Host, tmuxSession, 5)
		if output != "" {
			fmt.Println("Last output:")
			fmt.Println(output)
		}
	}

	printJobStatus(job, exitOnComplete)
}

var errWaitTimeout = errors.New("wait timeout")

type jobStatusRequest struct {
	ID  int64
	Job *db.Job
}

func waitForJobCompletion(database *sql.DB, jobID int64, timeout time.Duration, tracker *hostConnectionTracker) (*db.Job, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, fmt.Errorf("job %d not found", jobID)
		}
		if isWaitTerminalStatus(job.Status) {
			return job, nil
		}
		if timeout > 0 && time.Now().After(deadline) {
			return job, fmt.Errorf("%w waiting for job %d", errWaitTimeout, jobID)
		}

		if shouldAttemptSync(job.Status) {
			if _, err := ops.SyncJob(database, job, ops.DefaultSyncOptions()); err != nil {
				if ssh.IsConnectionError(err.Error()) {
					if tracker != nil {
						tracker.MarkDown(job.Host)
					}
				} else {
					return nil, err
				}
			} else {
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return nil, err
				}
				if tracker != nil && job != nil {
					tracker.MarkUp(job.Host, !isWaitTerminalStatus(job.Status))
				}
				if job != nil && isWaitTerminalStatus(job.Status) {
					return job, nil
				}
			}
		}

		<-ticker.C
	}
}

func waitForJobsCompletion(database *sql.DB, jobs []jobStatusRequest, timeout time.Duration, tracker *hostConnectionTracker) (map[int64]*db.Job, error) {
	final := make(map[int64]*db.Job, len(jobs))
	pending := make(map[int64]struct{})
	order := make([]int64, 0, len(jobs))
	lastReported := make(map[int64]string)

	for _, req := range jobs {
		final[req.ID] = req.Job
		if req.Job == nil {
			fmt.Printf("Job %d not found\n", req.ID)
			continue
		}
		lastReported[req.ID] = req.Job.Status
		printJobStatusLine(req.Job)
		if isWaitTerminalStatus(req.Job.Status) {
			continue
		}
		pending[req.ID] = struct{}{}
		order = append(order, req.ID)
	}

	if len(pending) == 0 {
		return final, nil
	}

	fmt.Printf("Waiting for %d job(s)", len(pending))
	if timeout > 0 {
		fmt.Printf(" (timeout: %s)", timeout)
	}
	fmt.Println()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	reportChange := func(job *db.Job) {
		if job == nil {
			return
		}
		if last, ok := lastReported[job.ID]; !ok || last != job.Status {
			lastReported[job.ID] = job.Status
			printJobStatusLine(job)
		}
	}

	for len(pending) > 0 {
		for _, id := range order {
			if _, ok := pending[id]; !ok {
				continue
			}
			job, err := db.GetJobByID(database, id)
			if err != nil {
				return final, err
			}
			final[id] = job
			if job == nil {
				fmt.Printf("Job %d not found\n", id)
				delete(pending, id)
				continue
			}
			reportChange(job)
			if isWaitTerminalStatus(job.Status) {
				delete(pending, id)
				continue
			}
			if shouldAttemptSync(job.Status) {
				if _, err := ops.SyncJob(database, job, ops.DefaultSyncOptions()); err != nil {
					if ssh.IsConnectionError(err.Error()) {
						if tracker != nil {
							tracker.MarkDown(job.Host)
						}
					} else {
						return final, err
					}
					continue
				}
				refreshed, err := db.GetJobByID(database, id)
				if err != nil {
					return final, err
				}
				if refreshed != nil {
					final[id] = refreshed
					if tracker != nil {
						tracker.MarkUp(refreshed.Host, !isWaitTerminalStatus(refreshed.Status))
					}
					job = refreshed
				}
				reportChange(job)
				if job != nil && isWaitTerminalStatus(job.Status) {
					delete(pending, id)
				}
			}
		}

		if len(pending) == 0 {
			break
		}

		if timeout > 0 && time.Now().After(deadline) {
			ids := make([]int64, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			return final, fmt.Errorf("%w waiting for jobs: %s", errWaitTimeout, formatJobIDList(ids))
		}

		<-ticker.C
	}

	return final, nil
}

func allJobsSucceeded(requests []jobStatusRequest, final map[int64]*db.Job) bool {
	for _, req := range requests {
		job := final[req.ID]
		if job == nil {
			return false
		}
		if job.EffectiveStatus() != db.StatusCompleted {
			return false
		}
		if job.ExitCode == nil || *job.ExitCode != 0 {
			return false
		}
	}
	return true
}

func formatJobIDList(ids []int64) string {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func mapKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isWaitTerminalStatus(status string) bool {
	switch status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return true
	default:
		return false
	}
}

func isTerminalStatus(status string) bool {
	switch status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled, db.StatusDraft:
		return true
	default:
		return false
	}
}

func shouldAttemptSync(status string) bool {
	switch status {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused, db.StatusQueued:
		return true
	default:
		return false
	}
}

func printJobStatusLine(job *db.Job) {
	if job == nil {
		return
	}
	line := fmt.Sprintf("Job %d (%s): %s", job.ID, job.Host, job.Status)
	if job.ExitCode != nil {
		line = fmt.Sprintf("%s (exit %d)", line, *job.ExitCode)
	}
	fmt.Println(line)
}

func printJobStatus(job *db.Job, exitOnComplete bool) {
	fmt.Printf("Job ID:   %d\n", job.ID)
	fmt.Printf("Host:     %s\n", job.Host)
	fmt.Printf("Status:   %s\n", job.Status)

	if job.Description != "" {
		fmt.Printf("Desc:     %s\n", job.Description)
	}

	if job.StartTime > 0 {
		startTime := time.Unix(job.StartTime, 0)
		fmt.Printf("Started:  %s\n", startTime.Format("2006-01-02 15:04:05"))
	}

	if job.EndTime != nil {
		endTime := time.Unix(*job.EndTime, 0)
		fmt.Printf("Ended:    %s\n", endTime.Format("2006-01-02 15:04:05"))
		if job.StartTime > 0 {
			duration := *job.EndTime - job.StartTime
			fmt.Printf("Duration: %s\n", db.FormatDuration(duration))
		}
	} else if job.Status == db.StatusRunning && job.StartTime > 0 {
		duration := time.Now().Unix() - job.StartTime
		fmt.Printf("Running:  %s\n", db.FormatDuration(duration))
	}

	if job.Status == db.StatusKilled {
		fmt.Printf("Exit:     killed\n")
	}
	if job.Status == db.StatusCanceled {
		fmt.Printf("Exit:     canceled\n")
	}

	if job.ExitCode != nil {
		fmt.Printf("Exit:     %d\n", *job.ExitCode)
	}

	fmt.Printf("Details:  remote-jobs info %d  # Show directory, command, env vars\n", job.ID)

	// Print usage hints
	if exitOnComplete && usageHintsEnabled() {
		fmt.Println()
		fmt.Printf("Hints:    remote-jobs log %d        # View job output\n", job.ID)
		if job.Status == db.StatusRunning || job.Status == db.StatusQueued || job.Status == db.StatusStarting {
			fmt.Printf("          remote-jobs status %d --watch  # Don't exit until the job completes\n", job.ID)
		}
	}

	// Set exit code based on status (only for single job)
	if exitOnComplete {
		switch job.Status {
		case db.StatusCompleted:
			if job.ExitCode != nil && *job.ExitCode == 0 {
				os.Exit(ExitSuccess)
			} else {
				os.Exit(ExitFailed)
			}
		case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
			os.Exit(ExitFailed)
		case db.StatusRunning, db.StatusQueued, db.StatusStarting:
			os.Exit(ExitRunning)
		default:
			os.Exit(ExitNotFound)
		}
	}
}

// showActiveJobs displays all active jobs (running, starting, queued) and recent failures
func showActiveJobs(database *sql.DB) error {
	// Sync first if not disabled
	if !statusNoSync {
		if statusSync {
			hosts, err := db.ListUniqueActiveHosts(database)
			if err == nil && len(hosts) > 0 {
				for _, host := range hosts {
					syncHost(database, host)
				}
			}
		} else if statusFast {
			performFastSync(database, false)
		} else {
			performSyncWithTimeout(database, DefaultSyncTimeout, false)
		}

		// Start queue runners on hosts with queued or running queue-runner jobs
		startQueueRunnersForQueuedHosts(database)
	}

	// Get running jobs
	running, err := db.ListAllRunning(database)
	if err != nil {
		return fmt.Errorf("list running jobs: %w", err)
	}

	// Get starting jobs
	starting, err := db.ListJobs(database, db.StatusStarting, "", 50, nil, "")
	if err != nil {
		return fmt.Errorf("list starting jobs: %w", err)
	}

	// Get queued jobs
	queued, err := db.ListJobs(database, db.StatusQueued, "", 50, nil, "")
	if err != nil {
		return fmt.Errorf("list queued jobs: %w", err)
	}

	// Get recent failed jobs
	failed, err := db.ListRecentFailed(database, 10)
	if err != nil {
		return fmt.Errorf("list failed jobs: %w", err)
	}

	if len(running) == 0 && len(starting) == 0 && len(queued) == 0 && len(failed) == 0 {
		fmt.Println("No active jobs")
		return nil
	}

	printed := false

	// Print running jobs
	if len(running) > 0 {
		fmt.Printf("Running (%d):\n", len(running))
		for _, job := range running {
			printJobSummary(job)
		}
		printed = true
	}

	// Print starting jobs
	if len(starting) > 0 {
		if printed {
			fmt.Println()
		}
		fmt.Printf("Starting (%d):\n", len(starting))
		for _, job := range starting {
			printJobSummary(job)
		}
		printed = true
	}

	// Print queued jobs
	if len(queued) > 0 {
		if printed {
			fmt.Println()
		}
		fmt.Printf("Queued (%d):\n", len(queued))
		for _, job := range queued {
			printJobSummary(job)
		}
		printed = true
	}

	// Print recent failed jobs
	if len(failed) > 0 {
		if printed {
			fmt.Println()
		}
		fmt.Printf("Recent failures (last 24h):\n")
		for _, job := range failed {
			printFailedJobSummary(job)
		}
	}

	return nil
}

func printJobSummary(job *db.Job) {
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = job.EffectiveCommand()
	}
	if len(desc) > 60 {
		desc = desc[:57] + "..."
	}
	fmt.Printf("  %4d  %-10s  %s\n", job.ID, job.Host, desc)
}

func printFailedJobSummary(job *db.Job) {
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = job.EffectiveCommand()
	}
	if len(desc) > 50 {
		desc = desc[:47] + "..."
	}

	// Format the failure reason
	var reason string
	switch job.Status {
	case db.StatusDead:
		reason = "start-failed"
	case db.StatusFailed:
		reason = "crashed"
	default:
		if job.ExitCode != nil {
			reason = fmt.Sprintf("exit %d", *job.ExitCode)
		} else {
			reason = "failed"
		}
	}

	fmt.Printf("  %4d  %-10s  %-8s  %s\n", job.ID, job.Host, reason, desc)
}
