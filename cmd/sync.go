package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/status"
	"github.com/osteele/weft/internal/syncorch"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync [host...]",
	Short: "Sync job statuses from remote hosts",
	Long: `Sync job statuses by checking remote hosts and cloud instances.

If hosts are specified, only those hosts are synced. Otherwise, all hosts
with running or queued jobs are synced. Connection failures are silently ignored.

Use --full to also deploy agent binaries and start queue runners.

Examples:
  weft sync                    # Sync all hosts + cloud instances
  weft sync studio             # Sync only studio
  weft sync --full             # Also deploy agents and start queue runners
  weft sync --verbose          # Show progress
  weft sync --timeout 10s      # Use 10 second timeout per host`,
	RunE: runSync,
}

var (
	syncVerbose      bool
	syncFull         bool
	syncNoQueueStart bool
	syncTimeout      time.Duration
	syncHosts        []string
)

const (
	// FastSyncTimeout is the per-SSH-call timeout for quick syncs.
	// 5s tolerates slow-but-alive handshakes without making the CLI feel sluggish
	// on healthy paths; the overall per-host budget remains FastSyncHostTimeout.
	FastSyncTimeout = 5 * time.Second
	// FastSyncHostTimeout is the overall timeout per host for quick syncs
	// Must be long enough to sync multiple jobs (each with FastSyncTimeout)
	FastSyncHostTimeout = 30 * time.Second
	// NormalSyncHostTimeout is the overall timeout per host for background full syncs.
	// Full syncs may need to stage code, assert the agent, and fetch missing inputs.
	NormalSyncHostTimeout = 10 * time.Minute
	// DefaultSyncTimeout is used for default syncs in status commands
	DefaultSyncTimeout = 5 * time.Second
	// NormalSyncTimeout is used for explicit sync commands
	NormalSyncTimeout = 30 * time.Second
)

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
	syncCmd.Flags().BoolVar(&syncFull, "full", false, "Run full sync including agent deployment and queue runner startup")
	syncCmd.Flags().BoolVar(&syncNoQueueStart, "no-queue-start", false, "Don't auto-start queue runners (requires --full)")
	_ = syncCmd.Flags().MarkHidden("no-queue-start")
	syncCmd.Flags().DurationVarP(&syncTimeout, "timeout", "t", NormalSyncTimeout, "Timeout per host (e.g., 10s, 1m)")
	syncCmd.Flags().StringArrayVar(&syncHosts, "host", nil, "Host(s) to sync (repeatable; also accepted as positional args)")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var hosts []string
	explicitHosts := len(syncHosts) > 0 || len(args) > 0
	if explicitHosts {
		// Merge --host flags and positional args, deduplicating
		seen := make(map[string]bool)
		for _, h := range append(syncHosts, args...) {
			if !seen[h] {
				hosts = append(hosts, h)
				seen[h] = true
			}
		}
	} else {
		// Get all unique hosts with running or queued jobs
		var err error
		hosts, err = db.ListUniqueActiveHosts(database)
		if err != nil {
			return fmt.Errorf("list hosts: %w", err)
		}
	}

	cfg, _ := config.Load()

	result := syncorch.SyncAll(database, cfg, syncorch.SyncOptions{
		Hosts:             hosts,
		SSHTimeout:        syncTimeout,
		HostTimeout:       NormalSyncHostTimeout,
		CloudMode:         syncCloudMode(explicitHosts),
		Verbose:           syncVerbose,
		Full:              syncFull,
		StartQueueRunner:  false,
		EnsureQueueRunner: nil,
	})
	emitWarnings(result.Warnings)
	totalUpdated := result.HostsUpdated + result.CloudUpdated
	hostsReached := result.HostsReached
	hostsUnreachable := len(result.HostsUnreachable)
	hostsSlow := len(result.HostsSlow)
	if !explicitHosts {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		skyUpdated, skyMissing, skyErr := syncSkyBindings(ctx, database, "")
		cancel()
		if skyErr != nil {
			fmt.Fprintf(os.Stderr, "warning: SkyPilot sync skipped: %v\n", skyErr)
		} else {
			totalUpdated += skyUpdated
			if syncVerbose && (skyUpdated > 0 || skyMissing > 0) {
				fmt.Fprintf(os.Stderr, "Synced %d SkyPilot job(s); %d missing from SkyPilot queue\n", skyUpdated, skyMissing)
			}
		}
	}

	// Deploy agent binary and start queue runners only in full mode (after sync completes)
	if syncFull {
		deployAgentsToHosts(hosts)
		if !syncNoQueueStart {
			startQueueRunnersForQueuedHosts(database)
		}
	}

	if cfg.LogCacheMaxAge > 0 {
		maxAge := time.Duration(cfg.LogCacheMaxAge) * 24 * time.Hour
		if pruned, err := logcache.Prune(maxAge); err == nil && pruned > 0 && syncVerbose {
			fmt.Printf("Pruned %d old cached log file(s)\n", pruned)
		}
	}

	// Print summary
	switch {
	case hostsUnreachable > 0 && hostsSlow > 0:
		fmt.Printf("Synced %d job(s) on %d host(s) (%d offline, %d slow)\n",
			totalUpdated, hostsReached, hostsUnreachable, hostsSlow)
	case hostsUnreachable > 0:
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) offline)\n",
			totalUpdated, hostsReached, hostsUnreachable)
	case hostsSlow > 0:
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) slow)\n",
			totalUpdated, hostsReached, hostsSlow)
	default:
		fmt.Printf("Synced %d job(s) on %d host(s)\n", totalUpdated, hostsReached)
	}

	return nil
}

func syncCloudMode(explicitHosts bool) syncorch.CloudMode {
	if explicitHosts {
		return syncorch.CloudDisabled
	}
	return syncorch.CloudUnbounded
}

// syncHost syncs all active jobs (running and queued) for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (ops.HostSyncResult, error) {
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      syncTimeout,
		NoQueueStart: true, // Queue runners are started separately in runSync
	}, nil)
	return result, err
}

// performSyncWithTimeout performs a sync with specified timeout for list/status commands
// Returns (completed, unreachable, slow) — unreachable = hard connection failures,
// slow = non-connection errors or deadline expirations.
func performSyncWithTimeout(database *sql.DB, timeout time.Duration, verbose bool) (bool, []string, []string) {
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil || len(hosts) == 0 {
		return true, nil, nil
	}
	completed, unreachable, slow, warnings := performSyncWithTimeoutForHostsDetailed(database, hosts, timeout, verbose)
	emitWarnings(warnings)
	return completed, unreachable, slow
}

// performFastSync performs a quick sync with fast timeout for list/status commands
// Returns (completed, unreachable, slow).
func performFastSync(database *sql.DB, verbose bool) (bool, []string, []string) {
	return performSyncWithTimeout(database, FastSyncTimeout, verbose)
}

// performSyncWithTimeoutForHosts performs a sync with specified timeout for a host subset.
// The sshTimeout is used for individual SSH calls; overall host timeout is FastSyncHostTimeout.
// Returns (completed, unreachable, slow).
func performSyncWithTimeoutForHosts(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool) (bool, []string, []string) {
	completed, unreachable, slow, warnings := performSyncWithTimeoutForHostsDetailed(database, hosts, sshTimeout, verbose)
	emitWarnings(warnings)
	return completed, unreachable, slow
}

func performSyncWithTimeoutForHostsDetailed(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool) (bool, []string, []string, []string) {
	return performSyncWithTimeoutForHostsDetailedWithOptions(database, hosts, sshTimeout, verbose, false)
}

func performSyncWithTimeoutForHostsDetailedWithOptions(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool, startQueueRunner bool) (bool, []string, []string, []string) {
	hostTimeout := syncHostWaitTimeout(startQueueRunner)
	result := syncorch.SyncHosts(database, syncorch.SyncOptions{
		Hosts:            hosts,
		SSHTimeout:       sshTimeout,
		HostTimeout:      hostTimeout,
		Verbose:          verbose,
		StartQueueRunner: startQueueRunner,
		EnsureQueueRunner: func(host string) (bool, error) {
			return ensureQueueRunnerStarted(host)
		},
	})
	return result.Completed, result.Unreachable, result.Slow, result.Warnings
}

// performFastSyncForHosts performs a quick sync with fast timeout for a host subset.
func performFastSyncForHosts(database *sql.DB, hosts []string, verbose bool) (bool, []string, []string) {
	return performSyncWithTimeoutForHosts(database, hosts, FastSyncTimeout, verbose)
}

// syncHostWithTimeout syncs a host with a specific timeout.
// Returns (updated count, error). Only returns error if host is truly unreachable
// (all SSH calls failed). Individual job sync failures are tolerated.
func syncHostWithTimeout(database *sql.DB, host string, timeout time.Duration) (ops.HostSyncResult, error) {
	return syncHostWithTimeoutDetailed(database, host, timeout, false)
}

func syncHostWithTimeoutDetailed(database *sql.DB, host string, timeout time.Duration, startQueueRunner bool) (ops.HostSyncResult, error) {
	onQueueStart := func(string) (bool, error) { return false, nil }
	if startQueueRunner {
		onQueueStart = func(h string) (bool, error) {
			return ensureQueueRunnerStarted(h)
		}
	}
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      timeout,
		SkipSamples:  true,
		UseBatchSync: true,
		NoQueueStart: !startQueueRunner,
		Logger:       ops.NewSilentSyncLogger(),
	}, onQueueStart)
	return result, err
}

func syncHostAfterQueueChange(database *sql.DB, host string) error {
	if host == "" {
		return nil
	}
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      ops.TimeoutFast.Duration(),
		SkipSamples:  true,
		UseBatchSync: true,
		NoQueueStart: true,
		Logger:       ops.NewSilentSyncLogger(),
	}, nil)
	reportHostSyncWarnings(host, result)
	return err
}

type hostSyncOutcome struct {
	result ops.HostSyncResult
	err    error
}

type targetedSyncOutcome struct {
	refreshed   int
	unreachable []string
	deadline    []string
	errors      map[string][]string
}

func (o targetedSyncOutcome) completed() bool {
	return len(o.unreachable) == 0 && len(o.deadline) == 0 && len(o.errors) == 0
}

var (
	syncJobForDisplayFunc          = ops.SyncJob
	syncAndReconcileForDisplayFunc = ops.SyncAndReconcile
)

func syncHostWaitTimeout(startQueueRunner bool) time.Duration {
	if startQueueRunner {
		return NormalSyncHostTimeout
	}
	return FastSyncHostTimeout
}

func hostSyncWarnings(host string, result ops.HostSyncResult) []string {
	var warnings []string
	if result.QueueDispatchError != "" {
		warnings = append(warnings, fmt.Sprintf("Warning: queued jobs were not dispatched on %s: %s", host, result.QueueDispatchError))
	}
	if result.QueueRunnerError != "" {
		warnings = append(warnings, fmt.Sprintf("Warning: queue runner error on %s: %s", host, result.QueueRunnerError))
	}
	return warnings
}

func reportHostSyncWarnings(host string, result ops.HostSyncResult) {
	emitWarnings(hostSyncWarnings(host, result))
}

func emitWarnings(warnings []string) {
	writeWarnings(os.Stderr, warnings)
}

func reportQueueChangeSyncFailure(host string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Auto-sync failed for %s: %v. Changes are saved locally and will be applied automatically when the host is reachable.\n", host, err)
}

// buildStaleDataNote renders a warning that results are from cached data.
// unreachable lists hosts whose sync produced a hard connection failure (offline);
// slow lists hosts whose sync hit a non-connection error or our deadline (alive but
// did not respond in time).
func buildStaleDataNote(database *sql.DB, unreachable, slow []string) string {
	unreachableNames := uniqueHosts(unreachable)
	// A host classified both ways in the same call is reported as unreachable only.
	slowNames := slices.DeleteFunc(uniqueHosts(slow), func(h string) bool {
		return slices.Contains(unreachableNames, h)
	})
	sort.Strings(unreachableNames)
	sort.Strings(slowNames)

	clause := func(names []string, tmpl string) string {
		if len(names) == 0 {
			return ""
		}
		summaries := hostAgeSummaries(database, names)
		if len(summaries) == 0 {
			return ""
		}
		subject := "hosts " + strings.Join(names, ", ")
		if len(names) == 1 {
			subject = "host " + names[0]
		}
		return fmt.Sprintf(tmpl, subject, strings.Join(summaries, ", "))
	}

	var clauses []string
	if c := clause(unreachableNames, "Could not reach %s (%s); showing cached data."); c != "" {
		clauses = append(clauses, c)
	}
	if c := clause(slowNames, "Could not refresh data from %s in time (%s); showing cached data — the host may be reachable but slow."); c != "" {
		clauses = append(clauses, c)
	}
	return strings.Join(clauses, " ")
}

func buildTargetedStaleDataNote(database *sql.DB, outcome targetedSyncOutcome) string {
	var clauses []string
	if c := targetedStaleClause(database, outcome.unreachable, "Could not reach %s; showing cached status (%s)."); c != "" {
		clauses = append(clauses, c)
	}
	if c := targetedStaleClause(database, outcome.deadline, "Live refresh for %s did not finish within the quick-refresh budget; showing cached status (%s)."); c != "" {
		clauses = append(clauses, c)
	}
	if len(outcome.errors) > 0 {
		hosts := make([]string, 0, len(outcome.errors))
		for host := range outcome.errors {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		summaries := hostAgeSummaries(database, hosts)
		for _, host := range hosts {
			detail := strings.Join(outcome.errors[host], "; ")
			if detail == "" {
				detail = "status probe failed"
			}
			age := "unknown"
			for _, summary := range summaries {
				if strings.HasPrefix(summary, host+": ") {
					age = strings.TrimPrefix(summary, host+": ")
					break
				}
			}
			clauses = append(clauses, fmt.Sprintf("Live refresh for %s failed during status probe (%s); showing cached status (%s).", host, detail, age))
		}
	}
	return strings.Join(clauses, " ")
}

func targetedStaleClause(database *sql.DB, hosts []string, tmpl string) string {
	names := uniqueHosts(hosts)
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	summaries := hostAgeSummaries(database, names)
	if len(summaries) == 0 {
		return ""
	}
	subject := strings.Join(names, ", ")
	if len(names) > 1 {
		subject = "hosts " + subject
	}
	return fmt.Sprintf(tmpl, subject, strings.Join(summaries, ", "))
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

func allowCompletedMarkerFallback(currentStatus string, launchID sql.NullInt64, needsBackfill bool) bool {
	return syncorch.AllowCompletedMarkerFallback(currentStatus, launchID, needsBackfill)
}

func jobEligibleForStartedMarker(database *sql.DB, jobID int64) (int64, bool, error) {
	return syncorch.JobEligibleForStartedMarker(database, jobID)
}

func r2Config(cfg *config.Config) r2.Config {
	return syncorch.R2Config(cfg)
}

func backfillHFDownloadObservations(database *sql.DB) {
	syncorch.BackfillHFDownloadObservations(database)
}

var syncTargetedCloudCompletionsFunc = syncTargetedCloudCompletions

func syncCloudInstanceOpslogs(ctx context.Context, r2Client *r2.Client, database *sql.DB, instanceIDs map[int64]struct{}, verbose bool) error {
	return syncorch.SyncCloudInstanceOpslogs(ctx, r2Client, database, instanceIDs, verbose)
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
			if errors.Is(err, agentdeploy.ErrAgentNotAvailable) {
				fmt.Fprintf(os.Stderr, "Warning: agent binary for %s/%s is unavailable and no builder succeeded\n", spec.OS, spec.Arch)
			} else if !ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: agent deploy to %s failed: %v\n", host, err)
			}
			continue
		}
		if deployed && syncVerbose {
			fmt.Printf("  %s: agent binary updated\n", host)
		}
	}
}

// quickSyncJobs performs a bounded sync for a set of jobs before display
// commands. It skips jobs in terminal states, separates on-prem vs rental jobs,
// and syncs each type appropriately. Sync failures are silently ignored.
func quickSyncJobs(database *sql.DB, jobs []*db.Job, sshTimeout, cloudTimeout time.Duration) {
	hostsToSync := make(map[string]struct{})
	needsRentalSync := false

	for _, job := range jobs {
		if status.IsTerminal(job.Status) {
			continue
		}
		if job.HasInventoryHost() {
			hostsToSync[job.Host] = struct{}{}
		}
		if job.UsesRentalPlacement() {
			needsRentalSync = true
		}
	}

	if len(hostsToSync) > 0 {
		hosts := mapKeys(hostsToSync)
		_, _, _ = performSyncWithTimeoutForHosts(database, hosts, sshTimeout, false)
	}
	if needsRentalSync {
		syncRentalJobsStatusFunc(database, cloudTimeout)
	}
}

func targetedSyncJobs(database *sql.DB, jobs []*db.Job, sshTimeout, hostTimeout, cloudTimeout time.Duration) targetedSyncOutcome {
	outcome := targetedSyncOutcome{errors: make(map[string][]string)}
	if hostTimeout <= 0 {
		hostTimeout = FastSyncHostTimeout
	}
	needsRentalSync := false
	var rentalJobIDs []int64

	for _, job := range jobs {
		if job == nil || status.IsTerminal(job.Status) {
			continue
		}
		if job.IsRentalJob() {
			needsRentalSync = true
			rentalJobIDs = append(rentalJobIDs, job.ID)
			continue
		}
		if !job.HasInventoryHost() {
			continue
		}

		done := make(chan error, 1)
		go func(job *db.Job) {
			if job.PendingStatus != nil {
				_, err := syncAndReconcileForDisplayFunc(database, job, ops.ReconcileOptions{Timeout: sshTimeout})
				done <- err
				return
			}
			_, err := syncJobForDisplayFunc(database, job, ops.SyncOptions{Timeout: sshTimeout, SkipSamples: true})
			done <- err
		}(job)

		select {
		case err := <-done:
			if err != nil {
				if ssh.IsConnectionError(err.Error()) {
					outcome.unreachable = append(outcome.unreachable, job.Host)
				} else {
					outcome.errors[job.Host] = append(outcome.errors[job.Host], summarizeSyncError(err))
				}
				continue
			}
			outcome.refreshed++
		case <-time.After(hostTimeout):
			outcome.deadline = append(outcome.deadline, job.Host)
		}
	}

	if needsRentalSync {
		syncTargetedCloudCompletionsFunc(database, rentalJobIDs, cloudTimeout)
		if syncRentalJobsStatusFunc(database, cloudTimeout) {
			outcome.refreshed++
		}
	}
	return outcome
}

func syncTargetedCloudCompletions(database *sql.DB, jobIDs []int64, timeout time.Duration) int {
	if len(jobIDs) == 0 {
		return 0
	}
	cfg, err := config.Load()
	if err != nil {
		return 0
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	return syncorch.SyncTargetedCloudJobResults(ctx, cfg, database, jobIDs, false)
}

func summarizeSyncError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "status probe failed"
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 120 {
		msg = msg[:117] + "..."
	}
	return msg
}
