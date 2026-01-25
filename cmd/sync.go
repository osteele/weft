package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/logcache"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/remote"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync [host...]",
	Short: "Sync job statuses from remote hosts",
	Long: `Sync job statuses by checking remote hosts.

If hosts are specified, only those hosts are synced. Otherwise, all hosts
with running or queued jobs are synced. Also starts queue runners on hosts
with queued jobs. Connection failures are silently ignored.

Examples:
  remote-jobs sync                    # Sync all hosts with active jobs
  remote-jobs sync studio             # Sync only studio
  remote-jobs sync cool30 cool100     # Sync specific hosts
  remote-jobs sync --verbose          # Show progress
  remote-jobs sync --no-queue-start   # Don't start queue runners
  remote-jobs sync --timeout 10s      # Use 10 second timeout per host`,
	RunE: runSync,
}

var (
	syncVerbose      bool
	syncNoQueueStart bool
	syncTimeout      time.Duration
)

var (
	syncJobFunc      = ops.SyncJob
	syncJobQuickFunc = ops.SyncJobQuick
)

const (
	// FastSyncTimeout is the per-SSH-call timeout for quick syncs
	FastSyncTimeout = 2 * time.Second
	// FastSyncHostTimeout is the overall timeout per host for quick syncs
	// Must be long enough to sync multiple jobs (each with FastSyncTimeout)
	FastSyncHostTimeout = 30 * time.Second
	// DefaultSyncTimeout is used for default syncs in status commands
	DefaultSyncTimeout = 5 * time.Second
	// NormalSyncTimeout is used for explicit sync commands
	NormalSyncTimeout = 30 * time.Second
)

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
	syncCmd.Flags().BoolVar(&syncNoQueueStart, "no-queue-start", false, "Don't auto-start queue runners")
	syncCmd.Flags().DurationVarP(&syncTimeout, "timeout", "t", NormalSyncTimeout, "Timeout per host (e.g., 10s, 1m)")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var hosts []string
	if len(args) > 0 {
		// Filter to specified hosts only
		hosts = args
	} else {
		// Get all unique hosts with running or queued jobs
		var err error
		hosts, err = db.ListUniqueActiveHosts(database)
		if err != nil {
			return fmt.Errorf("list hosts: %w", err)
		}
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

	// Start queue runners on hosts with queued jobs (unless --no-queue-start)
	if !syncNoQueueStart {
		startQueueRunnersForQueuedHosts(database)
	}

	// Prune old cached log files
	cfg, _ := config.Load()
	if cfg.LogCacheMaxAge > 0 {
		maxAge := time.Duration(cfg.LogCacheMaxAge) * 24 * time.Hour
		if pruned, err := logcache.Prune(maxAge); err == nil && pruned > 0 && syncVerbose {
			fmt.Printf("Pruned %d old cached log file(s)\n", pruned)
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

	syncOpts := ops.SyncOptions{Timeout: syncTimeout}
	var updated int
	seenJobs := make(map[int64]bool)

	for _, job := range jobs {
		seenJobs[job.ID] = true
		changed, err := syncJobFunc(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	// Reconcile jobs with pending operations (kill, cancel, etc.)
	// These may have non-active status but need remote reconciliation
	pendingJobs, err := db.ListJobsPendingReconciliation(database, host)
	if err != nil {
		return updated, err
	}
	for _, job := range pendingJobs {
		if seenJobs[job.ID] {
			continue // Already synced above
		}
		seenJobs[job.ID] = true
		_, err := ops.SyncAndReconcile(database, job, ops.ReconcileOptions{Timeout: syncOpts.Timeout})
		if err != nil {
			// Log but continue - don't fail entire sync for one job
			if syncVerbose {
				fmt.Fprintf(os.Stderr, "  Warning: reconcile job %d: %v\n", job.ID, err)
			}
			continue
		}
		updated++
	}

	// Check jobs that may have been restarted by the queue runner
	// (failed/dead jobs with queue_name that might be running again)
	// Optimization: Only check jobs with files modified since our last check
	lastCheck := db.GetLastRestartCheck(database, host)
	checkTime := time.Now()

	restartedJobs, err := db.ListPotentiallyRestartedJobs(database, host)
	if err != nil {
		return updated, err
	}

	// If we have a last check time, filter to jobs with recent activity
	if !lastCheck.IsZero() && len(restartedJobs) > 0 {
		sshHost := remote.NewSSHHost(host, syncTimeout)
		recentJobIDs, err := sshHost.GetRecentlyModifiedJobIDs(lastCheck)
		if err == nil && recentJobIDs != nil {
			// Build set of recently modified job IDs
			recentSet := make(map[int64]bool)
			for _, id := range recentJobIDs {
				recentSet[id] = true
			}
			// Filter to only jobs with recent activity
			var filtered []*db.Job
			for _, job := range restartedJobs {
				if recentSet[job.ID] {
					filtered = append(filtered, job)
				}
			}
			if syncVerbose && len(restartedJobs) != len(filtered) {
				fmt.Printf("  Filtered %d potentially restarted jobs to %d with recent activity\n",
					len(restartedJobs), len(filtered))
			}
			restartedJobs = filtered
		}
	}

	for _, job := range restartedJobs {
		if seenJobs[job.ID] {
			continue // Already synced above
		}
		seenJobs[job.ID] = true
		changed, err := syncJobFunc(database, job, syncOpts)
		if err != nil {
			if syncVerbose {
				fmt.Fprintf(os.Stderr, "  Warning: check restarted job %d: %v\n", job.ID, err)
			}
			continue
		}
		if changed {
			updated++
		}
	}

	// Update last restart check time
	if err := db.UpdateLastRestartCheck(database, host, checkTime); err != nil {
		if syncVerbose {
			fmt.Fprintf(os.Stderr, "  Warning: update restart check time: %v\n", err)
		}
	}

	// Clean up draft jobs that still have remote state lingering
	drafts, err := db.ListDraftJobsPendingSync(database, host)
	if err != nil {
		return updated, err
	}
	for _, job := range drafts {
		if seenJobs[job.ID] {
			continue // Already synced above
		}
		changed, err := ops.SyncDraftJob(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	if err := db.RecordHostSync(database, host, time.Now()); err != nil {
		return updated, err
	}

	return updated, nil
}

// performSyncWithTimeout performs a sync with specified timeout for list/status commands
// Returns true if sync completed, false if timed out along with the hosts that timed out
func performSyncWithTimeout(database *sql.DB, timeout time.Duration, verbose bool) (bool, []string) {
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil || len(hosts) == 0 {
		return true, nil
	}
	return performSyncWithTimeoutForHosts(database, hosts, timeout, verbose)
}

// performFastSync performs a quick sync with fast timeout for list/status commands
// Returns true if sync completed, false if timed out, along with hosts that timed out
func performFastSync(database *sql.DB, verbose bool) (bool, []string) {
	return performSyncWithTimeout(database, FastSyncTimeout, verbose)
}

// performSyncWithTimeoutForHosts performs a sync with specified timeout for a host subset.
// The sshTimeout is used for individual SSH calls; overall host timeout is FastSyncHostTimeout.
// Returns true if sync completed, false if timed out, along with hosts that timed out.
func performSyncWithTimeoutForHosts(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool) (bool, []string) {
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		return true, nil
	}

	// Set timeout for SSH operations
	// We'll use goroutines with a timeout context
	allCompleted := true
	var unreachable []string
	for _, host := range hosts {
		// Try quick sync, but don't wait if it times out
		done := make(chan error, 1)
		go func(h string) {
			_, err := syncHostWithTimeout(database, h, sshTimeout)
			done <- err
		}(host)

		select {
		case err := <-done:
			if err != nil {
				allCompleted = false
				unreachable = append(unreachable, host)
				if verbose && !ssh.IsConnectionError(err.Error()) {
					fmt.Fprintf(os.Stderr, "Warning: quick sync %s failed: %v\n", host, err)
				}
			}
		case <-time.After(FastSyncHostTimeout):
			// Overall host sync timed out - host likely unreachable
			allCompleted = false
			unreachable = append(unreachable, host)
		}
	}

	return allCompleted, unreachable
}

// performFastSyncForHosts performs a quick sync with fast timeout for a host subset.
func performFastSyncForHosts(database *sql.DB, hosts []string, verbose bool) (bool, []string) {
	return performSyncWithTimeoutForHosts(database, hosts, FastSyncTimeout, verbose)
}

// syncHostWithTimeout syncs a host with a specific timeout.
// Returns (updated count, error). Only returns error if host is truly unreachable
// (all SSH calls failed). Individual job sync failures are tolerated.
func syncHostWithTimeout(database *sql.DB, host string, timeout time.Duration) (int, error) {
	// This is a simplified version of syncHost that uses quick timeouts
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	syncOpts := ops.SyncOptions{Timeout: timeout, SkipSamples: true}
	var updated int
	var successCount, failCount int
	seenJobs := make(map[int64]bool)
	var queueRunnerJobs []*db.Job
	var tmuxJobs []*db.Job

	for _, job := range jobs {
		seenJobs[job.ID] = true
		if job.UsesQueueRunner() {
			queueRunnerJobs = append(queueRunnerJobs, job)
		} else {
			tmuxJobs = append(tmuxJobs, job)
		}
	}

	if len(queueRunnerJobs) > 0 {
		updatedCount, err := syncQueueRunnerJobsBatch(database, host, queueRunnerJobs, timeout)
		if err != nil {
			failCount++
		} else {
			successCount++
			updated += updatedCount
		}
	}
	for _, job := range tmuxJobs {
		// Use quick check with timeout
		changed, err := syncJobQuickFunc(database, job, syncOpts)
		if err != nil {
			failCount++
			continue // Don't fail entire sync for one job
		}
		successCount++
		if changed {
			updated++
		}
	}

	// Check potentially restarted jobs (failed/dead queue runner jobs)
	// Optimization: Only check jobs with files modified since our last check
	lastCheck := db.GetLastRestartCheck(database, host)
	checkTime := time.Now()

	restartedJobs, err := db.ListPotentiallyRestartedJobs(database, host)
	if err != nil {
		return updated, err
	}

	// If we have a last check time, filter to jobs with recent activity
	if !lastCheck.IsZero() && len(restartedJobs) > 0 {
		sshHost := remote.NewSSHHost(host, timeout)
		recentJobIDs, err := sshHost.GetRecentlyModifiedJobIDs(lastCheck)
		if err == nil && recentJobIDs != nil {
			recentSet := make(map[int64]bool)
			for _, id := range recentJobIDs {
				recentSet[id] = true
			}
			var filtered []*db.Job
			for _, job := range restartedJobs {
				if recentSet[job.ID] {
					filtered = append(filtered, job)
				}
			}
			restartedJobs = filtered
		}
	}

	var restartedQueueJobs []*db.Job
	for _, job := range restartedJobs {
		if seenJobs[job.ID] {
			continue
		}
		seenJobs[job.ID] = true
		restartedQueueJobs = append(restartedQueueJobs, job)
	}
	if len(restartedQueueJobs) > 0 {
		updatedCount, err := syncQueueRunnerJobsBatch(database, host, restartedQueueJobs, timeout)
		if err != nil {
			failCount++
		} else {
			successCount++
			updated += updatedCount
		}
	}

	// Update last restart check time
	_ = db.UpdateLastRestartCheck(database, host, checkTime)

	drafts, err := db.ListDraftJobsPendingSync(database, host)
	if err != nil {
		return updated, err
	}
	for _, job := range drafts {
		if seenJobs[job.ID] {
			continue
		}
		changed, err := ops.SyncDraftJob(database, job, syncOpts)
		if err != nil {
			failCount++
			continue // Don't fail entire sync for one job
		}
		successCount++
		if changed {
			updated++
		}
	}

	// Only consider host unreachable if ALL calls failed
	if successCount == 0 && failCount > 0 {
		return updated, fmt.Errorf("all %d sync attempts failed", failCount)
	}

	if err := db.RecordHostSync(database, host, time.Now()); err != nil {
		return updated, err
	}

	return updated, nil
}

func syncQueueRunnerJobsBatch(database *sql.DB, host string, jobs []*db.Job, timeout time.Duration) (int, error) {
	return ops.BatchSyncQueueRunnerJobs(database, host, queuefile.DefaultQueueName, jobs, timeout)
}

// buildStaleDataNote renders a warning that results are from cached data.
func buildStaleDataNote(database *sql.DB, hosts []string) string {
	names := uniqueHosts(hosts)
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)

	summaries := hostAgeSummaries(database, names)
	if len(summaries) == 0 {
		return ""
	}

	subject := "hosts " + strings.Join(names, ", ") + " are"
	if len(names) == 1 {
		subject = "host " + names[0] + " is"
	}

	return fmt.Sprintf(
		"Because %s not currently reachable, these results are from cached data (%s). Try again later. Attempts to use ssh directly or inspect the local or remote filesystem won't reveal newer information.",
		subject,
		strings.Join(summaries, ", "),
	)
}

func uniqueHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	var names []string
	for _, host := range hosts {
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		names = append(names, host)
	}
	return names
}

func hostAgeSummaries(database *sql.DB, hosts []string) []string {
	var summaries []string
	for _, host := range hosts {
		info, err := db.LoadCachedHostInfo(database, host)
		if err != nil || info == nil || info.LastUpdated == 0 {
			// Unknown age - include with "unknown" label
			summaries = append(summaries, fmt.Sprintf("%s: unknown", host))
			continue
		}
		ageDuration := time.Since(time.Unix(info.LastUpdated, 0))
		if ageDuration < 0 {
			ageDuration = 0
		}
		// Only warn if cache is more than 1 minute old
		if ageDuration < time.Minute {
			continue
		}
		age := fmt.Sprintf("%s ago", db.FormatDuration(int64(ageDuration.Seconds())))
		summaries = append(summaries, fmt.Sprintf("%s: %s", host, age))
	}
	return summaries
}
