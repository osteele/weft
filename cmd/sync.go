package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
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
  weft sync                    # Sync all hosts with active jobs
  weft sync studio             # Sync only studio
  weft sync cool30 cool100     # Sync specific hosts
  weft sync --verbose          # Show progress
  weft sync --no-queue-start   # Don't start queue runners
  weft sync --timeout 10s      # Use 10 second timeout per host`,
	RunE: runSync,
}

var (
	syncVerbose      bool
	syncNoQueueStart bool
	syncTimeout      time.Duration
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
					fmt.Printf("  %s: offline\n", host)
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

	// Deploy agent binary to reachable hosts that need updates (before starting runners)
	deployAgentsToHosts(hosts)

	// Start queue runners on hosts with queued jobs (unless --no-queue-start)
	if !syncNoQueueStart {
		startQueueRunnersForQueuedHosts(database)
	}

	// Prune old cached log files and placement outcomes need config
	cfg, _ := config.Load()

	// Sync placement outcomes for pending_placement jobs
	placementUpdated := syncPlacementOutcomes(cfg, database, syncVerbose)
	totalUpdated += placementUpdated

	// Check Vast.ai instances for completed results (CLI fallback for coordinator)
	vastaiUpdated := syncVastaiInstances(cfg, database, syncVerbose)
	totalUpdated += vastaiUpdated
	if cfg.LogCacheMaxAge > 0 {
		maxAge := time.Duration(cfg.LogCacheMaxAge) * 24 * time.Hour
		if pruned, err := logcache.Prune(maxAge); err == nil && pruned > 0 && syncVerbose {
			fmt.Printf("Pruned %d old cached log file(s)\n", pruned)
		}
	}

	// Print summary
	if hostsUnreachable > 0 {
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) offline)\n",
			totalUpdated, hostsReached, hostsUnreachable)
	} else {
		fmt.Printf("Synced %d job(s) on %d host(s)\n", totalUpdated, hostsReached)
	}

	return nil
}

// syncHost syncs all active jobs (running and queued) for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (int, error) {
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      syncTimeout,
		NoQueueStart: true, // Queue runners are started separately in runSync
	}, nil)
	return result.Updated, err
}

// syncPlacementOutcomes checks for jobs in pending_placement status and polls
// the coordinator for placement outcomes. Returns the number of jobs updated.
func syncPlacementOutcomes(cfg *config.Config, database *sql.DB, verbose bool) int {
	jobs, err := db.ListJobs(database, db.StatusPendingPlacement, "", 100, nil, "")
	if err != nil || len(jobs) == 0 {
		return 0
	}

	coordHost := cfg.GetCoordinatorHost()
	coordConfig := coordinator.DefaultConfig()
	updated := 0

	for _, job := range jobs {
		intentID := job.RemoteID
		if intentID == "" {
			continue
		}

		outcome, err := intent.ReadOutcome(coordHost, coordConfig.ArchiveDir, intentID)
		if err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "  placement check for job %d: %v\n", job.ID, err)
			}
			continue
		}
		if outcome == nil {
			continue // no outcome yet
		}

		// Update local job with placement result
		if outcome.Host != "" {
			if err := db.SetJobPlacement(database, job.ID, outcome.Host, strings.Join(outcome.Reasons, "; ")); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: update placement for job %d: %v\n", job.ID, err)
				continue
			}
			// Transition to queued status now that we know the host
			if _, err := database.Exec("UPDATE jobs SET host = ?, status = ? WHERE id = ?", outcome.Host, db.StatusQueued, job.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: update host for job %d: %v\n", job.ID, err)
				continue
			}
			updated++
			if verbose {
				fmt.Printf("  job %d placed on %s (%s)\n", job.ID, outcome.Host, strings.Join(outcome.Reasons, "; "))
			}
		} else if outcome.Error != "" {
			fmt.Fprintf(os.Stderr, "Warning: placement failed for job %d: %s\n", job.ID, outcome.Error)
		}
	}

	return updated
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
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      timeout,
		SkipSamples:  true,
		UseBatchSync: true,
		NoQueueStart: true,
	}, nil)
	return result.Updated, err
}

func syncHostAfterQueueChange(database *sql.DB, host string) error {
	if host == "" {
		return nil
	}
	_, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      ops.TimeoutFast.Duration(),
		SkipSamples:  true,
		UseBatchSync: true,
		NoQueueStart: true,
	}, nil)
	return err
}

func reportQueueChangeSyncFailure(host string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Auto-sync failed for %s: %v. Changes are saved locally and will be applied automatically when the host is reachable.\n", host, err)
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
		"Because %s currently offline, these results are from cached data (%s). Try again later. Attempts to use ssh directly or inspect the local or remote filesystem won't reveal newer information.",
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

// syncVastaiInstances checks for completed Vast.ai job results in R2.
// This is a CLI fallback for when the coordinator is not running.
func syncVastaiInstances(cfg *config.Config, database *sql.DB, verbose bool) int {
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return 0
	}

	jobs, err := db.ListActiveVastaiJobs(database)
	if err != nil || len(jobs) == 0 {
		return 0
	}

	if verbose {
		fmt.Printf("Checking %d active Vast.ai job(s)...\n", len(jobs))
	}

	r2Cfg := r2Config(cfg)
	r2Client, err := r2.New(r2Cfg)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 client: %v\n", err)
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	updated := 0
	completedJobIDs, err := r2Client.ListCompleted(ctx, "jobs/")
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 list: %v\n", err)
		}
		return 0
	}

	// Build lookup of active job IDs
	activeJobIDs := make(map[string]bool)
	for _, j := range jobs {
		activeJobIDs[fmt.Sprintf("%d", j.ID)] = true
	}

	for _, jobIDStr := range completedJobIDs {
		if !activeJobIDs[jobIDStr] {
			continue
		}

		jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
		prefix := fmt.Sprintf("jobs/%d", jobID)

		// Download results to temp dir
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-vastai-%d-*", jobID))
		if err != nil {
			continue
		}

		if err := r2Client.DownloadResults(ctx, prefix+"/results/", tmpDir); err != nil {
			os.RemoveAll(tmpDir)
			continue
		}

		// Read exit code
		exitCodeBytes, err := os.ReadFile(filepath.Join(tmpDir, "exit_code"))
		if err != nil {
			os.RemoveAll(tmpDir)
			continue
		}
		exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitCodeBytes)))

		// Read end time
		endTimeBytes, _ := os.ReadFile(filepath.Join(tmpDir, "end_time"))
		endTimeUnix, _ := strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)

		status := db.StatusCompleted
		if exitCode != 0 {
			status = db.StatusFailed
		}

		_, _ = database.Exec(
			`UPDATE jobs SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
			status, exitCode, endTimeUnix, status, jobID,
		)
		updated++

		if verbose {
			fmt.Printf("  Vast.ai job %d: %s (exit %d)\n", jobID, status, exitCode)
		}

		// Cleanup
		_ = r2Client.DeletePrefix(ctx, prefix+"/")
		os.RemoveAll(tmpDir)
	}

	return updated
}

func r2Config(cfg *config.Config) r2.Config {
	return r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}
}

// deployAgentsToHosts deploys the agent binary to hosts that have an outdated
// or missing version. Errors are logged as warnings — agent deploy is best-effort.
func deployAgentsToHosts(hosts []string) {
	for _, host := range hosts {
		spec := inventory.FindHost(host)
		if spec == nil {
			continue // Host not in inventory, skip agent deploy
		}
		deployed, err := agentdeploy.EnsureAgentUpToDate(host, *spec)
		if err != nil {
			if !ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: agent deploy to %s failed: %v\n", host, err)
			}
			continue
		}
		if deployed && syncVerbose {
			fmt.Printf("  %s: agent binary updated\n", host)
		}
	}
}
