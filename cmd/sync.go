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
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync job statuses from all remote hosts",
	Long: `Sync job statuses by checking all hosts with running jobs.

Automatically finds hosts with running jobs and updates their status
in the local database. Also starts queue runners on hosts with queued jobs.
Connection failures are silently ignored.

Examples:
  remote-jobs sync                    # Sync all hosts
  remote-jobs sync --verbose          # Show progress
  remote-jobs sync --no-queue-start   # Don't start queue runners`,
	RunE: runSync,
}

var (
	syncVerbose      bool
	syncNoQueueStart bool
)

var (
	syncJobFunc      = ops.SyncJob
	syncJobQuickFunc = ops.SyncJobQuick
)

const (
	// FastSyncTimeout is used for --fast mode in list/status commands
	FastSyncTimeout = 2 * time.Second
	// DefaultSyncTimeout is used for default syncs in status commands
	DefaultSyncTimeout = 5 * time.Second
	// NormalSyncTimeout is used for explicit sync commands
	NormalSyncTimeout = 30 * time.Second
)

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
	syncCmd.Flags().BoolVar(&syncNoQueueStart, "no-queue-start", false, "Don't auto-start queue runners")
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

	syncOpts := ops.DefaultSyncOptions()
	var updated int
	for _, job := range jobs {
		changed, err := syncJobFunc(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	// Clean up draft jobs that still have remote state lingering
	drafts, err := db.ListDraftJobsPendingSync(database, host)
	if err != nil {
		return updated, err
	}
	for _, job := range drafts {
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
// Returns true if sync completed, false if timed out, along with hosts that timed out.
func performSyncWithTimeoutForHosts(database *sql.DB, hosts []string, timeout time.Duration, verbose bool) (bool, []string) {
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
			_, err := syncHostWithTimeout(database, h, timeout)
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
		case <-time.After(timeout):
			// Timed out
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

// syncHostWithTimeout syncs a host with a specific timeout
func syncHostWithTimeout(database *sql.DB, host string, timeout time.Duration) (int, error) {
	// This is a simplified version of syncHost that uses quick timeouts
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	syncOpts := ops.SyncOptions{Timeout: timeout}
	var updated int
	for _, job := range jobs {
		// Use quick check with timeout
		changed, err := syncJobQuickFunc(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	drafts, err := db.ListDraftJobsPendingSync(database, host)
	if err != nil {
		return updated, err
	}
	for _, job := range drafts {
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
		age := "unknown"
		if info, err := db.LoadCachedHostInfo(database, host); err == nil && info != nil && info.LastUpdated > 0 {
			ageDuration := time.Since(time.Unix(info.LastUpdated, 0))
			if ageDuration < 0 {
				ageDuration = 0
			}
			age = fmt.Sprintf("%s ago", db.FormatDuration(int64(ageDuration.Seconds())))
		}
		summaries = append(summaries, fmt.Sprintf("%s: %s", host, age))
	}
	return summaries
}
