package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/status"
	"github.com/spf13/cobra"
)

// Exit codes
const (
	ExitSuccess  = 0
	ExitFailed   = 1
	ExitNotFound = 3
)

var (
	statusSync        bool
	statusNoSync      bool
	statusFast        bool
	statusWait        bool
	statusWaitTimeout time.Duration
	statusSSHTimeout  time.Duration
)

var syncRentalJobsStatusFunc = syncRentalJobsStatusWithTimeout

var statusCmd = &cobra.Command{
	Use:   "status [id]...",
	Short: "Check the status of jobs or instances",
	Long: `Check the status of one or more jobs.

Without arguments, shows all active jobs (running, starting, queued)
and recent failures from the last 24 hours.

Job/instance IDs can be specified with prefixes:
  - Job ID: wj42
  - Instance ID: wi42
  - Mixed with inferred type: wj42 43 or wi42 43

Bare numeric IDs are treated as job IDs. Prefix instance IDs with wi.

When targeting jobs, IDs can be specified individually or as ranges:
  - Single ID: 42
  - Single ID (prefixed): wj42
  - Range: 42:47, 42::47, or 42...47 (expands to 42, 43, 44, 45, 46, 47)
  - Range (prefixed): wj42:wj47 or wj42:47
  - List: 42,43,44
  - Mixed: 42 50:52 60,61 (expands to 42, 50, 51, 52, 60, 61)

Duplicate IDs are automatically removed with a warning.

Exit codes (single job only):
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Examples:
  weft status              # Show all active jobs
  weft status 42
  weft status wj42
  weft status 42:47        # Check jobs 42 through 47
  weft status wj42:wj47    # Check jobs 42 through 47
  weft status 42...47      # Check jobs 42 through 47
  weft status 42,43,44     # Check multiple jobs in one argument
  weft status 42 --fast    # Quick check with 2s timeout
  weft status 42 -t 2m     # Use 2 minute SSH timeout (slow connections)
  weft status 42:47 --wait  # Wait for jobs 42-47 to complete`,
	RunE: runStatus,
}

var runInstanceStatusFromStatusFunc = runInstanceStatus

func init() {
	rootCmd.AddCommand(statusCmd)
	addStatusFlags(statusCmd)
}

func addStatusFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&statusSync, "sync", false, "Perform full sync (30s timeout)")
	cmd.Flags().BoolVar(&statusNoSync, "no-sync", false, "Skip syncing job statuses before checking")
	cmd.Flags().BoolVar(&statusFast, "fast", false, "Use quick 2s timeout (default is 5s)")
	cmd.Flags().BoolVar(&statusWait, "wait", false, "Wait for the job(s) to complete before returning")
	cmd.Flags().DurationVar(&statusWaitTimeout, "wait-timeout", 0, "Maximum time to wait for completion (0 = no limit)")
	cmd.Flags().DurationVar(&statusWaitTimeout, "timeout", 0, "Alias for --wait-timeout")
	cmd.Flags().MarkHidden("timeout")
	cmd.Flags().DurationVarP(&statusSSHTimeout, "ssh-timeout", "t", 0, "SSH timeout for slow connections (e.g., 2m, 120s)")
}

func runStatus(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && isTopLevelStatusCommand(cmd) {
		kind, err := resolveStatusIDTargetKind(args)
		if err != nil {
			return err
		}
		if kind == idTargetInstance {
			return runInstanceStatusFromStatusFunc(cmd, args)
		}
	}

	return runJobStatus(cmd, args)
}

func resolveStatusIDTargetKind(args []string) (idTargetKind, error) {
	kind, err := resolveIDTargetKind(args)
	if err == nil {
		return kind, nil
	}
	if canParseJobIDArgs(args) {
		return idTargetJob, nil
	}
	return kind, err
}

func canParseJobIDArgs(args []string) bool {
	for _, arg := range args {
		if _, err := parseJobIDArg(arg); err != nil {
			return false
		}
	}
	return true
}

func isTopLevelStatusCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "status" && cmd.Parent() == rootCmd
}

func runJobStatus(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// No args: show all active jobs
	if len(args) == 0 {
		return showActiveJobs(database)
	}

	// Parse job IDs (supports ranges, ellipsis, and comma-separated lists).
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

	// Raise SSH connect timeout for slow connections
	if statusSSHTimeout > 0 {
		ssh.SetMinConnectTimeout(statusSSHTimeout)
	}

	// Check if all requested jobs are already in terminal state - skip sync if so
	needsSync := false
	hostsToSync := make(map[string]struct{})
	needsRentalSync := false
	if !statusNoSync {
		for _, jobID := range jobIDs {
			job, err := db.GetJobByID(database, jobID)
			if err != nil || job == nil {
				continue
			}
			if !status.IsTerminal(job.Status) {
				needsSync = true
				if job.HasInventoryHost() {
					hostsToSync[job.Host] = struct{}{}
				}
				if job.IsRentalJob() {
					needsRentalSync = true
				}
			}
		}
	}

	// Queue-runner startup is only needed when actively progressing work
	// (e.g. --wait); plain read-only status checks should not pay the
	// ssh/rclone-deploy cost.
	startRunners := statusWait

	// Sync logic: default 5s, fast 2s, full 30s, --ssh-timeout overrides, or skip
	if needsSync {
		hosts := mapKeys(hostsToSync)
		if statusSync {
			// Full sync requested (30s timeout)
			for _, host := range hosts {
				_, _ = syncHost(database, host)
			}
			if startRunners {
				startQueueRunnersForHosts(database, hosts)
			}
		} else if statusFast && statusSSHTimeout == 0 {
			// Fast sync (2s timeout) - skip queue starting for speed
			completed, unreachable, slow := performFastSyncForHosts(database, hosts, false)
			if !completed {
				if note := buildStaleDataNote(database, unreachable, slow); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
		} else {
			// Use --ssh-timeout if set, otherwise default 5s
			syncTimeout := DefaultSyncTimeout
			if statusSSHTimeout > 0 {
				syncTimeout = statusSSHTimeout
			}
			completed, unreachable, slow := performSyncWithTimeoutForHosts(database, hosts, syncTimeout, false)
			if !completed {
				if note := buildStaleDataNote(database, unreachable, slow); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
			if startRunners {
				startQueueRunnersForHosts(database, hosts)
			}
		}
		if needsRentalSync {
			syncRentalJobsStatusFunc(database, preDisplayCloudSyncTimeout(statusSync))
		}
	}

	waitRequests := make([]jobStatusRequest, 0, len(jobIDs))
	waitInputInvalid := false
	singleJob := len(jobIDs) == 1 && !statusWait
	printed := 0
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %s: %v\n", ids.FormatJobID(jobID), err)
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

		if printed > 0 {
			fmt.Println("---")
		}
		printed++
		printSingleJobStatus(database, jobID, job, singleJob, needsSync || statusNoSync)
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

func printSingleJobStatus(database *sql.DB, jobID int64, job *db.Job, exitOnComplete bool, alreadySynced bool) {
	if job == nil {
		fmt.Printf("Job %s not found\n", ids.FormatJobID(jobID))
		if exitOnComplete {
			os.Exit(ExitNotFound)
		}
		return
	}

	// Override queued status for jobs whose last cloud attempt failed
	applyAttemptOutcomeOverrides(database, []*db.Job{job})

	// If the effective state is already terminal, use cached result.
	if isWaitTerminalStatus(job.EffectiveStatus()) {
		printJobStatus(job, exitOnComplete)
		return
	}

	// Sync host to update job status from remote when a host exists.
	if !alreadySynced {
		if job.HasInventoryHost() {
			syncTimeout := 15 * time.Second
			if statusSSHTimeout > syncTimeout {
				syncTimeout = statusSSHTimeout
			}
			_, syncErr := ops.SyncHost(database, job.Host, ops.HostSyncOptions{
				Timeout: syncTimeout,
				Logger:  ops.NewQuietSyncLogger(),
			}, nil)
			if syncErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: sync failed for %s: %v\n", job.Host, syncErr)
			}
		} else if job.IsRentalJob() {
			syncRentalJobsStatus(database)
		}
	}

	// Re-read job from DB after sync
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		fmt.Fprintf(os.Stderr, "Job %s: failed to reload: %v\n", ids.FormatJobID(jobID), err)
		return
	}
	hydrateQueueBlockedReasons([]*db.Job{job})

	printJobStatus(job, exitOnComplete)
	if progress := jobProgressSummary(database, job); progress != "" {
		fmt.Printf("Progress: %s\n", progress)
	}
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
			return nil, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
		}
		if isWaitTerminalStatus(job.Status) {
			return job, nil
		}
		if timeout > 0 && time.Now().After(deadline) {
			return job, fmt.Errorf("%w waiting for job %s", errWaitTimeout, ids.FormatJobID(jobID))
		}

		if shouldAttemptSync(job.Status) {
			if job.IsRentalJob() {
				syncRentalJobsStatus(database)
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return nil, err
				}
				if job != nil && isWaitTerminalStatus(job.Status) {
					return job, nil
				}
			} else if _, err := ops.SyncJob(database, job, ops.DefaultSyncOptions()); err != nil {
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
			fmt.Printf("Job %s not found\n", ids.FormatJobID(req.ID))
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
				fmt.Printf("Job %s not found\n", ids.FormatJobID(id))
				delete(pending, id)
				continue
			}
			reportChange(job)
			if isWaitTerminalStatus(job.Status) {
				delete(pending, id)
				continue
			}
			if shouldAttemptSync(job.Status) {
				if job.IsRentalJob() {
					syncRentalJobsStatus(database)
					refreshed, err := db.GetJobByID(database, id)
					if err != nil {
						return final, err
					}
					if refreshed != nil {
						final[id] = refreshed
						job = refreshed
					}
					reportChange(job)
					if job != nil && isWaitTerminalStatus(job.Status) {
						delete(pending, id)
					}
					continue
				}
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

func formatJobIDList(jobIDs []int64) string {
	return ids.FormatJobIDListCompact(jobIDs)
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

func shouldAttemptSync(status string) bool {
	switch status {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused, db.StatusQueued:
		return true
	default:
		return false
	}
}

func preDisplayCloudSyncTimeout(fullSync bool) time.Duration {
	if fullSync {
		return NormalCloudSyncTimeout
	}
	return FastCloudSyncTimeout
}

func syncRentalJobsStatus(database *sql.DB) bool {
	return syncRentalJobsStatusWithTimeout(database, FastCloudSyncTimeout)
}

// syncRentalJobsStatusWithTimeout runs a bounded cloud sync for rental jobs.
// Also runs a fast DB-only repair for jobs stuck on completed launches.
// Returns true if the cloud sync completed within the timeout.
func syncRentalJobsStatusWithTimeout(database *sql.DB, timeout time.Duration) bool {
	cfg, err := config.Load()
	if err != nil {
		return true
	}

	// Try to create R2 client for stuck-job recovery. Falls back to DB-only
	// if R2 is not configured.
	var r2Client *r2.Client
	if cfg.Vastai.R2.Bucket != "" && cfg.Vastai.R2.AccessKeyID != "" {
		r2Client, _ = r2.New(r2Config(cfg))
	}

	// Finalize stuck jobs, checking R2 for late-arriving .complete markers.
	// Also called from syncCloudJobResults after full cloud sync completes.
	if repaired, err := campaign.FinalizeStuckJobsWithR2Check(database, r2Client); err != nil {
		slog.Warn("failed to finalize stuck jobs", "component", "sync", "error", err)
	} else {
		for _, jobID := range repaired {
			slog.Info("finalized stuck job on completed launch", "component", "sync", "job_id", jobID)
		}
	}
	backfillHFDownloadObservations(database)

	_, completed := syncCloudStateWithTimeout(cfg, database, campaign.NewReconciler(), timeout, false)
	return completed
}

func printJobStatusLine(job *db.Job) {
	if job == nil {
		return
	}
	line := fmt.Sprintf("Job %s (%s): %s", ids.FormatJobID(job.ID), job.Host, job.EffectiveStatus())
	if job.ExitCode != nil {
		line = fmt.Sprintf("%s (exit %d)", line, *job.ExitCode)
	}
	if reason := humanizeFailureReason(job.FailureReason); reason != "" {
		line = fmt.Sprintf("%s — %s", line, reason)
	}
	fmt.Println(line)
}

func printJobStatus(job *db.Job, exitOnComplete bool) {
	effectiveStatus := job.EffectiveStatus()
	display := queueblock.Display(job, nil)

	fmt.Printf("Job ID:   %s\n", ids.FormatJobID(job.ID))
	fmt.Printf("Host:     %s\n", job.TargetDisplay())
	if display.Blocked {
		fmt.Printf("Status:   %s\n", display.Status)
		fmt.Printf("Reason:   %s\n", display.Reason)
	} else {
		fmt.Printf("Status:   %s\n", effectiveStatus)
	}

	if job.EndTime != nil {
		if job.StartTime > 0 {
			duration := *job.EndTime - job.StartTime
			fmt.Printf("Duration: %s\n", db.FormatDuration(duration))
		}
	} else if effectiveStatus == db.StatusRunning && job.StartTime > 0 {
		duration := time.Now().Unix() - job.StartTime
		fmt.Printf("Running:  %s\n", db.FormatDuration(duration))
	}
	if effectiveStatus == db.StatusKilled {
		fmt.Printf("Exit:     killed\n")
	}
	if effectiveStatus == db.StatusCanceled {
		fmt.Printf("Exit:     canceled\n")
	}

	if job.ExitCode != nil {
		fmt.Printf("Exit:     %d\n", *job.ExitCode)
	}
	if reason := humanizeFailureReason(job.FailureReason); reason != "" {
		fmt.Printf("Reason:   %s\n", reason)
	}
	if job.FailureReason == "killed_stdout_silence" {
		fmt.Printf("Hint:     Emit a periodic progress line so the silence watchdog sees output.\n")
		fmt.Printf("          Weft parses any of these formats and displays the parsed progress:\n")
		fmt.Printf("            Progress: 42%%\n")
		fmt.Printf("            Progress: 9/14\n")
		fmt.Printf("            Progress: 9 of 14\n")
		fmt.Printf("          You can also write checkpoints to the job's output dir so the run can be resumed.\n")
	}

	// Show remediation info for failed jobs
	if job.ErrorDiagnosis != "" {
		printDiagnosisSummary(job)
	}

	fmt.Printf("Details:  weft info %s  # Show directory, command, env vars\n", ids.FormatJobID(job.ID))

	// Print usage hints
	if exitOnComplete && usageHintsEnabled() {
		fmt.Println()
		fmt.Printf("Hints:    weft log %s        # View job output\n", ids.FormatJobID(job.ID))
		if effectiveStatus == db.StatusRunning || effectiveStatus == db.StatusQueued || effectiveStatus == db.StatusStarting {
			fmt.Printf("          weft status %s --wait   # Don't exit until the job completes\n", ids.FormatJobID(job.ID))
		}
	}

}

// showActiveJobs displays all active jobs (running, starting, queued) and recent failures
func showActiveJobs(database *sql.DB) error {
	// Raise SSH connect timeout for slow connections
	if statusSSHTimeout > 0 {
		ssh.SetMinConnectTimeout(statusSSHTimeout)
	}

	// Sync first if not disabled
	if !statusNoSync {
		if statusSync {
			hosts, err := db.ListUniqueActiveHosts(database)
			if err == nil && len(hosts) > 0 {
				for _, host := range hosts {
					_, _ = syncHost(database, host)
				}
			}
		} else if statusFast && statusSSHTimeout == 0 {
			performFastSync(database, false)
		} else {
			syncTimeout := DefaultSyncTimeout
			if statusSSHTimeout > 0 {
				syncTimeout = statusSSHTimeout
			}
			performSyncWithTimeout(database, syncTimeout, false)
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
	queued = append(queued, jobsWithEffectiveStatus(running, db.StatusQueued)...)
	queued = append(queued, jobsWithEffectiveStatus(starting, db.StatusQueued)...)
	running = jobsWithEffectiveStatus(running, db.StatusRunning)
	starting = jobsWithEffectiveStatus(starting, db.StatusStarting)

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
	fmt.Printf("  %-8s  %-14s  %s\n", ids.FormatJobID(job.ID), job.HostWithGPU(), desc)
}

func jobsWithEffectiveStatus(jobs []*db.Job, status string) []*db.Job {
	if status == "" {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.EffectiveStatus() == status {
			filtered = append(filtered, job)
		}
	}
	return filtered
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

	// Show remediation indicator
	if job.RetryCount > 0 {
		reason += " (retried)"
	} else if job.ErrorDiagnosis != "" {
		reason += " (diagnosed)"
	}

	fmt.Printf("  %-8s  %-10s  %-14s  %s\n", ids.FormatJobID(job.ID), job.TargetDisplay(), reason, desc)
}

// printDiagnosisSummary prints the auto-remediation diagnosis for a failed job.
func printDiagnosisSummary(job *db.Job) {
	d, err := remediation.UnmarshalDiagnosis(job.ErrorDiagnosis)
	if err != nil || d == nil {
		return
	}

	fmt.Printf("Diagnosis: %s (%s)\n", d.Message, d.Pattern)
	if d.Remediable {
		if job.RetryCount > 0 {
			fmt.Printf("Remediation: auto-retried (retry #%d)\n", job.RetryCount)
		} else {
			fmt.Printf("Remediation: remediable but not retried\n")
		}
	}
	if len(d.MissingAssets) > 0 {
		fmt.Printf("Missing:   %s\n", strings.Join(d.MissingAssets, ", "))
	}
}
