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
	Use:   "status <job-id>...",
	Short: "Check the status of one or more jobs",
	Long: `Check the status of one or more jobs.

Exit codes (single job only):
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Examples:
  remote-jobs status 42
  remote-jobs status 42 --fast     # Quick check with 2s timeout
  remote-jobs status 42 --wait     # Wait for job to complete
  remote-jobs log 42               # View job output`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&statusSync, "sync", false, "Perform full sync (30s timeout)")
	statusCmd.Flags().BoolVar(&statusNoSync, "no-sync", false, "Skip syncing job statuses before checking")
	statusCmd.Flags().BoolVar(&statusFast, "fast", false, "Use quick 2s timeout (default is 5s)")
	statusCmd.Flags().BoolVar(&statusWait, "wait", false, "Wait for the job(s) to complete before returning")
	statusCmd.Flags().DurationVar(&statusWaitTimeout, "wait-timeout", 0, "Maximum time to wait for completion (0 = no limit)")
}

func runStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var tracker *hostConnectionTracker
	if statusWait {
		statusSync = true
		statusNoSync = false
		tracker = newHostConnectionTracker()
	}

	// Check if all requested jobs are already in terminal state - skip sync if so
	needsSync := false
	if !statusNoSync {
		for _, arg := range args {
			jobID, err := strconv.ParseInt(arg, 10, 64)
			if err != nil {
				continue
			}
			job, err := db.GetJobByID(database, jobID)
			if err != nil || job == nil {
				continue
			}
			if !isTerminalStatus(job.Status) {
				needsSync = true
				break
			}
		}
	}

	// Sync logic: default 5s, fast 2s, full 30s, or skip
	if needsSync {
		if statusSync {
			// Full sync requested (30s timeout)
			hosts, err := db.ListUniqueActiveHosts(database)
			if err == nil && len(hosts) > 0 {
				for _, host := range hosts {
					syncHost(database, host)
				}
			}
			// Start queue runners (full sync mode)
			startQueueRunnersForQueuedHosts(database)
		} else if statusFast {
			// Fast sync (2s timeout) - skip queue starting for speed
			completed := performFastSync(database, false)
			if !completed {
				fmt.Fprintf(os.Stderr, "Note: Some hosts timed out. Run with --sync for full sync.\n")
			}
		} else {
			// Default sync (5s timeout)
			completed := performSyncWithTimeout(database, DefaultSyncTimeout, false)
			if !completed {
				fmt.Fprintf(os.Stderr, "Note: Some hosts timed out. Use --fast for quicker response or --sync for full sync.\n")
			}
			// Start queue runners (default mode)
			startQueueRunnersForQueuedHosts(database)
		}
	}

	waitRequests := make([]jobStatusRequest, 0, len(args))
	waitInputInvalid := false
	singleJob := len(args) == 1 && !statusWait
	for i, arg := range args {
		if i > 0 && !statusWait {
			fmt.Println("---")
		}

		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid job ID: %s\n", arg)
			if singleJob {
				os.Exit(ExitNotFound)
			}
			if statusWait {
				waitInputInvalid = true
			}
			continue
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

	// If job is already marked as completed or dead, use cached result
	if job.Status == db.StatusCompleted || job.Status == db.StatusDead {
		printJobStatus(job, exitOnComplete)
		return
	}

	// Queue runner jobs (no session name) don't have tmux sessions to check
	// They are managed by the queue runner and synced separately
	if job.SessionName == "" {
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
		content, err := ssh.ReadRemoteFile(job.Host, statusFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: read status file: %v\n", jobID, err)
			return
		}

		if content != "" {
			// Job completed, update database
			exitCodeStr := strings.TrimSpace(content)
			exitCode, err := strconv.Atoi(exitCodeStr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Job %d: invalid status file contents %q: %v\n", jobID, exitCodeStr, err)
				if exitOnComplete {
					os.Exit(ExitFailed)
				}
				return
			}
			endTime := time.Now().Unix()
			if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusCompleted
			job.ExitCode = &exitCode
			job.EndTime = &endTime
		} else {
			// No status file - check for pending operations before marking dead
			hasPending, _ := db.HasPendingDeferredOperationForJob(database, job.ID)
			if hasPending {
				// Job has pending operations - don't mark as dead
				printJobStatus(job, exitOnComplete)
				return
			}
			// Job died unexpectedly
			if err := db.MarkDeadByID(database, job.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusDead
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
		if isTerminalStatus(job.Status) {
			return job, nil
		}
		if timeout > 0 && time.Now().After(deadline) {
			return job, fmt.Errorf("%w waiting for job %d", errWaitTimeout, jobID)
		}

		if shouldAttemptSync(job.Status) {
			if _, err := syncJob(database, job); err != nil {
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
					tracker.MarkUp(job.Host, !isTerminalStatus(job.Status))
				}
				if job != nil && isTerminalStatus(job.Status) {
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
	for _, req := range jobs {
		final[req.ID] = req.Job
		if req.Job == nil {
			fmt.Printf("Job %d not found\n", req.ID)
			continue
		}
		if isTerminalStatus(req.Job.Status) {
			printJobStatus(req.Job, false)
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
			if isTerminalStatus(job.Status) {
				printJobStatus(job, false)
				delete(pending, id)
				continue
			}
			if shouldAttemptSync(job.Status) {
				if _, err := syncJob(database, job); err != nil {
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
						tracker.MarkUp(refreshed.Host, !isTerminalStatus(refreshed.Status))
					}
					job = refreshed
				}
				if job != nil && isTerminalStatus(job.Status) {
					printJobStatus(job, false)
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
		if job.Status != db.StatusCompleted {
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

func isTerminalStatus(status string) bool {
	switch status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed:
		return true
	default:
		return false
	}
}

func shouldAttemptSync(status string) bool {
	switch status {
	case db.StatusRunning, db.StatusStarting, db.StatusQueued:
		return true
	default:
		return false
	}
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

	if job.ExitCode != nil {
		fmt.Printf("Exit:     %d\n", *job.ExitCode)
	}

	// Print usage hints
	if exitOnComplete {
		fmt.Println()
		fmt.Printf("Hints:    remote-jobs log %d        # View job output\n", job.ID)
		if job.Status == db.StatusRunning || job.Status == db.StatusQueued || job.Status == db.StatusStarting {
			fmt.Printf("          remote-jobs status %d --wait  # Don't exit until the remote job completes\n", job.ID)
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
		case db.StatusDead:
			os.Exit(ExitFailed)
		case db.StatusRunning, db.StatusQueued, db.StatusStarting:
			os.Exit(ExitRunning)
		default:
			os.Exit(ExitNotFound)
		}
	}
}
