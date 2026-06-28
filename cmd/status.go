package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/explain"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/status"
	"github.com/osteele/weft/internal/syncorch"
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
var statusSyncHostsFunc = syncStatusHostsWithBounds

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

	// Collect requested jobs so the cached-first sync policy can decide once.
	var jobsForSync []*db.Job
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			continue
		}
		jobsForSync = append(jobsForSync, job)
	}
	needsSync := liveSyncNeededForJobs(database, jobsForSync, statusSync || statusWait, statusNoSync)

	// Queue-runner startup is only needed when actively progressing work
	// (e.g. --wait); plain read-only status checks should not pay the
	// ssh/rclone-deploy cost.
	startRunners := statusWait

	// Sync logic: default bounded quick sync, --fast/--sync/--ssh-timeout tune the bound.
	if needsSync {
		sshTimeout, hostTimeout := statusHostSyncBounds()
		if statusWait {
			hostsToSync := make(map[string]struct{})
			needsRentalSync := false
			for _, job := range jobsForSync {
				if status.IsTerminal(job.Status) {
					continue
				}
				if job.HasInventoryHost() {
					hostsToSync[job.Host] = struct{}{}
				}
				if job.IsRentalJob() {
					needsRentalSync = true
				}
			}
			hosts := mapKeys(hostsToSync)
			if len(hosts) > 0 {
				completed, unreachable, slow := statusSyncHostsFunc(database, hosts, sshTimeout, hostTimeout, startRunners)
				if !completed {
					if note := buildStaleDataNote(database, unreachable, slow); note != "" {
						fmt.Fprintln(os.Stderr, note)
					}
				}
			}
			if needsRentalSync {
				syncRentalJobsStatusFunc(database, preDisplayCloudSyncTimeout(statusSync))
			}
		} else {
			outcome := targetedSyncJobs(database, jobsForSync, sshTimeout, hostTimeout, preDisplayCloudSyncTimeout(statusSync))
			if !outcome.completed() {
				if note := buildTargetedStaleDataNote(database, outcome); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
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
		printSingleJobStatus(database, jobID, job, singleJob, true)
	}

	if statusWait {
		if len(waitRequests) == 0 {
			return fmt.Errorf("no valid job IDs to wait for")
		}
		results, err := waitForJobsCompletion(cmd.Context(), database, waitRequests, statusWaitTimeout, tracker)
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

	// If the effective state is already terminal, use cached result unless
	// the caller explicitly requested sync. Queue-runner status files are
	// authoritative same-run evidence and can arrive after a transient failed
	// mark, so explicit sync still gets a chance to repair them.
	if isWaitTerminalStatus(job.EffectiveStatus()) && !(statusSync && job.HasInventoryHost() && job.UsesQueueRunner()) {
		printJobStatus(database, job, exitOnComplete)
		return
	}

	// For explicit sync of a terminal queue-runner job, still perform a
	// direct per-job probe. The earlier targeted host sync can skip terminal
	// rows, but a same-run status file may be the authoritative correction.
	if statusSync && job.HasInventoryHost() && job.UsesQueueRunner() && isWaitTerminalStatus(job.EffectiveStatus()) {
		if _, syncErr := ops.SyncJob(database, job, ops.DefaultSyncOptions()); syncErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: sync failed for %s: %v\n", ids.FormatJobID(job.ID), syncErr)
		}
		updated, err := db.GetJobByID(database, jobID)
		if err == nil && updated != nil {
			job = updated
		}
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

	printJobStatus(database, job, exitOnComplete)
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

func waitForJobsCompletion(ctx context.Context, database *sql.DB, jobs []jobStatusRequest, timeout time.Duration, tracker *hostConnectionTracker) (map[int64]*db.Job, error) {
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

	if usedDaemon, err := waitForJobsCompletionViaDaemon(ctx, database, final, pending, order, lastReported, timeout); usedDaemon {
		return final, err
	}
	return waitForJobsCompletionPolling(database, final, pending, order, lastReported, timeout, tracker)
}

func waitForJobsCompletionViaDaemon(ctx context.Context, database *sql.DB, final map[int64]*db.Job, pending map[int64]struct{}, order []int64, lastReported map[int64]string, timeout time.Duration) (bool, error) {
	if len(order) == 0 {
		return true, nil
	}
	paths := daemoncontrol.DefaultPaths()
	if _, _, err := ensureDaemonStartedFunc(paths, 2*time.Second); err != nil {
		return false, nil
	}
	watcher, err := dialDaemonWatchJobs(ctx, paths.SocketFile, order, timeout, 2*time.Second)
	if err != nil {
		return false, nil
	}
	defer watcher.Close()

	reportChange := func(job *db.Job) {
		if job == nil {
			return
		}
		if last, ok := lastReported[job.ID]; !ok || last != job.Status {
			lastReported[job.ID] = job.Status
			printJobStatusLine(job)
		}
	}
	pendingTimeoutErr := func() error {
		pendingIDs := make([]int64, 0, len(pending))
		for id := range pending {
			pendingIDs = append(pendingIDs, id)
		}
		return fmt.Errorf("%w waiting for jobs: %s", errWaitTimeout, ids.FormatJobIDListCompact(pendingIDs))
	}

	received := false
	for len(pending) > 0 {
		event, err := watcher.Next()
		if err != nil {
			if !received && errors.Is(err, io.EOF) {
				return false, nil
			}
			return true, fmt.Errorf("daemon watch: %w", err)
		}
		received = true
		switch event.Type {
		case daemonapi.EventSnapshot, daemonapi.EventDone:
			for _, snapshot := range event.Jobs {
				if _, ok := pending[snapshot.ID]; !ok {
					continue
				}
				job, err := db.GetJobByID(database, snapshot.ID)
				if err != nil {
					return true, err
				}
				final[snapshot.ID] = job
				if job == nil {
					fmt.Printf("Job %s not found\n", ids.FormatJobID(snapshot.ID))
					delete(pending, snapshot.ID)
					continue
				}
				reportChange(job)
				if isWaitTerminalStatus(job.Status) {
					delete(pending, snapshot.ID)
				}
			}
			if event.Type == daemonapi.EventDone && len(pending) > 0 {
				return true, pendingTimeoutErr()
			}
		case daemonapi.EventError:
			if event.Error == context.DeadlineExceeded.Error() {
				return true, pendingTimeoutErr()
			}
			return true, fmt.Errorf("daemon watch: %s", event.Error)
		}
	}
	return true, nil
}

func dialDaemonWatchJobs(ctx context.Context, socketPath string, order []int64, timeout time.Duration, wait time.Duration) (*daemonapi.Watcher, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		watcher, err := daemonapi.DialWatchJobs(ctx, socketPath, order, timeout)
		if err == nil {
			return watcher, nil
		}
		lastErr = err
		if wait <= 0 || time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitForJobsCompletionPolling(database *sql.DB, final map[int64]*db.Job, pending map[int64]struct{}, order []int64, lastReported map[int64]string, timeout time.Duration, tracker *hostConnectionTracker) (map[int64]*db.Job, error) {
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
			pendingIDs := make([]int64, 0, len(pending))
			for id := range pending {
				pendingIDs = append(pendingIDs, id)
			}
			return final, fmt.Errorf("%w waiting for jobs: %s", errWaitTimeout, ids.FormatJobIDListCompact(pendingIDs))
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

func statusHostSyncBounds() (time.Duration, time.Duration) {
	sshTimeout := DefaultSyncTimeout
	if statusFast && statusSSHTimeout == 0 {
		sshTimeout = FastSyncTimeout
	}
	if statusSync {
		sshTimeout = NormalSyncTimeout
	}
	if statusSSHTimeout > 0 {
		sshTimeout = statusSSHTimeout
	}

	hostTimeout := FastSyncHostTimeout
	if statusSync {
		hostTimeout = NormalSyncTimeout
	}
	if statusSSHTimeout > hostTimeout {
		hostTimeout = statusSSHTimeout
	}
	return sshTimeout, hostTimeout
}

func syncStatusHostsWithBounds(database *sql.DB, hosts []string, sshTimeout, hostTimeout time.Duration, startQueueRunners bool) (bool, []string, []string) {
	result := syncorch.SyncHosts(database, syncorch.SyncOptions{
		Hosts:            hosts,
		SSHTimeout:       sshTimeout,
		HostTimeout:      hostTimeout,
		StartQueueRunner: startQueueRunners,
		EnsureQueueRunner: func(host string) (bool, error) {
			return ensureQueueRunnerStarted(host)
		},
	})
	emitWarnings(result.Warnings)
	return result.Completed, result.Unreachable, result.Slow
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

func printJobStatus(database *sql.DB, job *db.Job, exitOnComplete bool) {
	effectiveStatus := job.EffectiveStatus()
	display := queueblock.Display(job, nil)
	x := explain.ForJob(database, job, time.Now())

	fmt.Printf("Job ID:   %s\n", ids.FormatJobID(job.ID))
	fmt.Printf("Host:     %s\n", job.TargetDisplay())
	if display.Kind != "" {
		fmt.Printf("Status:   %s\n", display.Status)
		fmt.Printf("Reason:   %s\n", display.Reason)
	} else {
		fmt.Printf("Status:   %s\n", effectiveStatus)
	}
	printPlacementLines(queuedPlacementLines(database, job), 10)
	if x.SuggestedAction != "" && x.SuggestedAction != "none" {
		fmt.Printf("Explain:  %s\n", x.SuggestedAction)
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

	printJobLocalDiagnostics(database, job)

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

	// No-arg status is a broad read-only overview. Use cached state by
	// default so it does not fan out to every on-prem host; --sync requests
	// an explicit live refresh.
	if statusSync && !statusNoSync {
		hosts, err := db.ListUniqueActiveHosts(database)
		if err == nil && len(hosts) > 0 {
			sshTimeout, hostTimeout := statusHostSyncBounds()
			completed, unreachable, slow := statusSyncHostsFunc(database, hosts, sshTimeout, hostTimeout, false)
			if !completed {
				if note := buildStaleDataNote(database, unreachable, slow); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
		}
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

// printDiagnosisSummary prints the structured diagnosis for a failed job.
// Resolves via ResolveJobDiagnosis (stored → log-cache fallback) so the
// summary appears regardless of whether the remediation pipeline ran.
func printDiagnosisSummary(job *db.Job) {
	d := ResolveJobDiagnosis(job)
	if d == nil {
		return
	}

	fmt.Printf("Diagnosis: %s (%s)\n", d.Message, d.Pattern)
	if d.Solution != "" {
		fmt.Printf("Solution:  %s\n", d.Solution)
	}
	for _, line := range d.GPUOOMDetailLines() {
		// 11-space hang indent matches the value column of "Diagnosis: ".
		fmt.Printf("           %s\n", line)
	}
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
