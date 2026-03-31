package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/transferbw"
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
	syncHosts        []string
)

const (
	// FastSyncTimeout is the per-SSH-call timeout for quick syncs
	FastSyncTimeout = 2 * time.Second
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
	syncCmd.Flags().BoolVar(&syncNoQueueStart, "no-queue-start", false, "Don't auto-start queue runners")
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
	if len(syncHosts) > 0 || len(args) > 0 {
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

	if len(hosts) == 0 {
		fmt.Println("No active jobs to sync")
		return nil
	}

	var totalUpdated, hostsReached, hostsUnreachable int

	for _, host := range hosts {
		if syncVerbose {
			fmt.Printf("Checking %s...\n", host)
		}

		result, err := syncHost(database, host)
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

		totalUpdated += result.Updated
		reportHostSyncWarnings(host, result)
		hostsReached++
		if syncVerbose && result.Updated > 0 {
			fmt.Printf("  %s: %d job(s) updated\n", host, result.Updated)
		}
	}

	// Deploy agent binary to reachable hosts that need updates (before starting runners)
	deployAgentsToHosts(hosts)

	// Start queue runners on hosts with queued jobs (unless --no-queue-start)
	if !syncNoQueueStart {
		startQueueRunnersForQueuedHosts(database)
	}

	// Prune old cached log files
	cfg, _ := config.Load()

	totalUpdated += syncCloudState(cfg, database, campaign.NewReconciler(), syncVerbose).Updated

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
func syncHost(database *sql.DB, host string) (ops.HostSyncResult, error) {
	result, err := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout:      syncTimeout,
		NoQueueStart: true, // Queue runners are started separately in runSync
	}, nil)
	return result, err
}

// performSyncWithTimeout performs a sync with specified timeout for list/status commands
// Returns true if sync completed, false if timed out along with the hosts that timed out
func performSyncWithTimeout(database *sql.DB, timeout time.Duration, verbose bool) (bool, []string) {
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil || len(hosts) == 0 {
		return true, nil
	}
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailed(database, hosts, timeout, verbose)
	emitWarnings(warnings)
	return completed, unreachable
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
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailed(database, hosts, sshTimeout, verbose)
	emitWarnings(warnings)
	return completed, unreachable
}

func performSyncWithTimeoutForHostsDetailed(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool) (bool, []string, []string) {
	return performSyncWithTimeoutForHostsDetailedWithOptions(database, hosts, sshTimeout, verbose, false)
}

func performSyncWithTimeoutForHostsDetailedWithOptions(database *sql.DB, hosts []string, sshTimeout time.Duration, verbose bool, startQueueRunner bool) (bool, []string, []string) {
	if len(hosts) == 0 {
		var err error
		hosts, err = db.ListUniqueActiveHosts(database)
		if err != nil || len(hosts) == 0 {
			return true, nil, nil
		}
	}
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		return true, nil, nil
	}

	// Set timeout for SSH operations
	// We'll use goroutines with a timeout context
	allCompleted := true
	var unreachable []string
	var warnings []string
	hostWaitTimeout := syncHostWaitTimeout(startQueueRunner)
	for _, host := range hosts {
		// Try quick sync, but don't wait if it times out
		done := make(chan hostSyncOutcome, 1)
		go func(h string) {
			result, err := syncHostWithTimeoutDetailed(database, h, sshTimeout, startQueueRunner)
			done <- hostSyncOutcome{result: result, err: err}
		}(host)

		select {
		case outcome := <-done:
			if outcome.err != nil {
				allCompleted = false
				unreachable = append(unreachable, host)
				if verbose && !ssh.IsConnectionError(outcome.err.Error()) {
					fmt.Fprintf(os.Stderr, "Warning: quick sync %s failed: %v\n", host, outcome.err)
				}
				continue
			}
			warnings = append(warnings, hostSyncWarnings(host, outcome.result)...)
		case <-time.After(hostWaitTimeout):
			// Overall host sync timed out - host likely unreachable
			allCompleted = false
			unreachable = append(unreachable, host)
		}
	}

	return allCompleted, unreachable, warnings
}

// performFastSyncForHosts performs a quick sync with fast timeout for a host subset.
func performFastSyncForHosts(database *sql.DB, hosts []string, verbose bool) (bool, []string) {
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
			return ensureQueueRunnerStarted(h, defaultQueueName)
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
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, warning)
	}
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

// syncCloudJobResults checks for completed cloud job results in R2.
// This covers both legacy Vast.ai-backend jobs and campaign-launched queue-runner jobs.
// Uses per-job DB lookups rather than pre-filtering by cloud_instance_id, so results
// are synced even if the instance association was cleared by a concurrent reset.
func syncCloudJobResults(cfg *config.Config, database *sql.DB, verbose bool) int {
	updated := 0
	if repaired, err := db.ResetJobsOnTerminalLaunches(database); err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: terminal cloud job repair: %v\n", err)
		}
	} else if len(repaired) > 0 {
		updated += len(repaired)
		if verbose {
			fmt.Printf("Repaired %d stale cloud job assignment(s) on terminal instances\n", len(repaired))
		}
	}

	if cfg == nil || cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return updated
	}

	r2Cfg := r2Config(cfg)
	r2Client, err := r2.New(r2Cfg)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 client: %v\n", err)
		}
		return 0
	}

	// Use a generous overall timeout — each completed job may need to download
	// result files, and a tight timeout causes later jobs to be skipped.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	updatedInstanceIDs := make(map[int64]struct{})
	markers, err := r2Client.ListJobMarkers(ctx, "jobs/")
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 list: %v\n", err)
		}
		return 0
	}

	if verbose && (len(markers.Completed) > 0 || len(markers.Started) > 0) {
		fmt.Printf("Checking %d completed + %d started cloud job marker(s)...\n",
			len(markers.Completed), len(markers.Started))
	}

	// Track jobs processed as completed so the started-markers loop skips them.
	completedJobIDs := make(map[int64]bool, len(markers.Completed))

	for _, jobIDStr := range markers.Completed {
		jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
		if jobID == 0 {
			continue
		}

		if markers.IsProcessed(jobID) {
			continue
		}

		// Check current job status directly — skip if already terminal
		var currentStatus string
		var latestRunID sql.NullInt64
		if err := database.QueryRow("SELECT status, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0", jobID).Scan(&currentStatus, &latestRunID); err != nil || db.IsTerminalStatus(currentStatus) {
			// Keep live-log chunks: they are the primary log source for `weft log`.
			_ = r2Client.PutMarker(ctx, r2keys.JobProcessed(jobID))
			continue
		}

		runID := int64(0)
		if latestRunID.Valid {
			runID = latestRunID.Int64
		}
		if !markers.HasCompletedMarker(jobID, r2keys.JobAttemptComplete(jobID, runID)) {
			// Fallback: the latest attempt may have a different run_id than the
			// one that actually ran (e.g., after cleanupStaleAttempts replaced
			// the attempt). Check for any completed marker for this job.
			if altKey, ok := markers.AnyCompletedKey(jobID); ok {
				runID = r2keys.ExtractRunID(altKey)
			} else {
				continue
			}
		}
		resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)

		// Download results to temp dir
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-cloud-%d-*", jobID))
		if err != nil {
			continue
		}

		if err := r2Client.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
			os.RemoveAll(tmpDir)
			continue
		}

		exitCode, startTimeUnix, endTimeUnix, failureReason := db.ParseCloudJobResult(tmpDir, jobIDStr)
		if exitCode == nil {
			os.RemoveAll(tmpDir)
			continue
		}

		// If no start_time from completion record, try reading .started marker from R2
		if startTimeUnix == 0 {
			if data, err := r2Client.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
				startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			}
		}

		updatedInstanceID, err := db.RecordCloudJobCompletion(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason)
		if err != nil {
			slog.Warn("failed to update cloud job status", "component", "sync", "job_id", jobID, "error", err)
			continue
		}
		if updatedInstanceID > 0 {
			updatedInstanceIDs[updatedInstanceID] = struct{}{}
		}
		updated++
		completedJobIDs[jobID] = true

		// Log cloud job completion/failure to local ops log
		host := ""
		if updatedInstanceID > 0 {
			host = db.LaunchHost(updatedInstanceID)
		}
		if *exitCode == 0 {
			oplog.LogJob(oplog.OpJobComplete, jobID, host, oplog.WithDetailf("cloud exit=0"))
		} else {
			oplog.LogJob(oplog.OpJobFail, jobID, host, oplog.WithDetailf("cloud exit=%d", *exitCode))
		}

		if err := importCloudTimeseriesFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.timeseries.jsonl", jobID)), "single"); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d final timeseries import failed: %v\n", jobID, err)
		}
		if err := importCloudTelemetryFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.telemetry.jsonl", jobID))); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d final telemetry import failed: %v\n", jobID, err)
		}

		// Extract and store phase timing data
		if timings := coordinator.ExtractPhaseTimings(jobID, tmpDir); timings != nil {
			if err := db.UpsertJobPhaseTimings(database, timings); err != nil {
				slog.Warn("failed to store phase timings", "component", "sync", "job_id", jobID, "error", err)
			}
			if launch, err := db.GetLaunch(database, updatedInstanceID); err == nil && launch != nil {
				recordCloudDownloadObservation(database, launch.Provider, launch.DataCenter, timings)
			}
		}

		// Cache logs locally before cleaning up
		coordinator.WriteVastaiLogsToCache(jobID, tmpDir)

		if verbose {
			statusLabel := db.StatusCompleted
			if *exitCode != 0 {
				statusLabel = db.StatusFailed
			}
			fmt.Printf("  cloud job %d: %s (exit %d)\n", jobID, statusLabel, *exitCode)
		}

		// Keep live-log chunks: they are the primary log source for `weft log`.
		_ = r2Client.PutMarker(ctx, r2keys.JobAttemptProcessed(jobID, runID))
		_ = r2Client.DeletePrefix(ctx, resultPrefix)
		os.RemoveAll(tmpDir)
	}

	// Check for .started markers to transition queued jobs to running
	for _, jobIDStr := range markers.Started {
		jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
		if jobID == 0 || completedJobIDs[jobID] {
			continue
		}

		runID, ok, err := jobEligibleForStartedMarker(database, jobID)
		if err != nil {
			slog.Warn("failed to check started-marker eligibility", "component", "sync", "job_id", jobID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		if !markers.HasStartedMarker(jobID, r2keys.JobAttemptStarted(jobID, runID)) {
			continue
		}
		var startTimeUnix int64
		if data, err := r2Client.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
			startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		}

		if _, err := database.Exec(
			`UPDATE job_attempts SET status = ?, start_time = ?
			 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL ORDER BY attempt_number DESC LIMIT 1)`,
			db.StatusRunning, startTimeUnix, jobID,
		); err != nil {
			slog.Warn("failed to update cloud job to running", "component", "sync", "job_id", jobID, "error", err)
			continue
		}
		updated++
		if verbose {
			fmt.Printf("  cloud job %d: started\n", jobID)
		}
	}

	// Import live timeseries checkpoints for started jobs that are not complete.
	for _, jobIDStr := range markers.Started {
		jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
		if jobID == 0 || completedJobIDs[jobID] {
			continue
		}
		if err := syncCloudLiveTimeseries(ctx, r2Client, database, jobID); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d live timeseries sync failed: %v\n", jobID, err)
		}
		if err := syncCloudLiveTelemetry(ctx, r2Client, database, jobID); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d live telemetry sync failed: %v\n", jobID, err)
		}
	}

	for instanceID := range updatedInstanceIDs {
		updateInstanceTerminationReason(database, instanceID)
	}

	// Safety net: finalize jobs stuck "running" on completed launches.
	// Checks R2 for late-arriving .complete markers before marking dead.
	// Also called (DB-only) from syncRentalJobsStatus for immediate repair
	// when cloud sync times out; this call uses R2 for better accuracy.
	if repaired, err := campaign.FinalizeStuckJobsWithR2Check(database, r2Client); err != nil {
		slog.Warn("failed to finalize stuck jobs", "component", "sync", "error", err)
	} else {
		for _, jobID := range repaired {
			slog.Info("finalized stuck job on completed launch", "component", "sync", "job_id", jobID)
		}
		updated += len(repaired)
	}

	// Backfill HF download bandwidth observations from historical phase timings.
	// Idempotent — skips datacenters that already have observations.
	backfillHFDownloadObservations(database)

	return updated
}

// recordCloudDownloadObservation derives effective HF download bandwidth from
// phase timings and records it as a transfer observation. Only records for cold
// starts where significant data was downloaded during setup.
func recordCloudDownloadObservation(database *sql.DB, provider, datacenter string, timings *db.JobPhaseTimings) {
	if timings.CacheHFPostBytes == nil || timings.SetupStart == nil || timings.SetupEnd == nil || datacenter == "" {
		return
	}
	preBytes := int64(0)
	if timings.CacheHFBytes != nil {
		preBytes = *timings.CacheHFBytes
	}
	downloaded := *timings.CacheHFPostBytes - preBytes
	setupDuration := *timings.SetupEnd - *timings.SetupStart
	if downloaded <= 0 || setupDuration <= 0 {
		return
	}
	src := transferbw.HFEndpoint()
	dst := transferbw.CloudEndpoint(provider, datacenter, "")
	_ = transferbw.RecordObservation(database, src, dst, downloaded,
		time.Duration(setupDuration)*time.Second)
}

var backfillOnce sync.Once

// backfillHFDownloadObservations populates transfer_observations from historical
// job_phase_timings for cold-start jobs. Runs at most once per process.
func backfillHFDownloadObservations(database *sql.DB) {
	backfillOnce.Do(func() {
		rows, err := database.Query(`
			SELECT l.provider, l.data_center,
			       (jpt.cache_hf_post_bytes - COALESCE(jpt.cache_hf_bytes, 0)) AS downloaded,
			       (jpt.setup_end - jpt.setup_start) AS setup_sec
			FROM job_phase_timings jpt
			JOIN job_attempts ja ON ja.job_id = jpt.job_id
			JOIN launches l ON ja.launch_id = l.id
			WHERE jpt.setup_start > 0 AND jpt.setup_end > 0
			AND jpt.cache_hf_post_bytes > COALESCE(jpt.cache_hf_bytes, 0)
			AND (jpt.setup_end - jpt.setup_start) > 0
			AND l.data_center != ''
			AND l.provider != ''
			AND ('cloud:' || l.provider || ':' || l.data_center) NOT IN (
				SELECT dest_key FROM transfer_observations WHERE source_key = 'hf'
			)`)
		if err != nil {
			return
		}
		defer rows.Close()

		recorded := 0
		for rows.Next() {
			var provider, datacenter string
			var downloaded, setupSec int64
			if err := rows.Scan(&provider, &datacenter, &downloaded, &setupSec); err != nil {
				continue
			}
			src := transferbw.HFEndpoint()
			dst := transferbw.CloudEndpoint(provider, datacenter, "")
			_ = transferbw.RecordObservation(database, src, dst, downloaded,
				time.Duration(setupSec)*time.Second)
			recorded++
		}
		if recorded > 0 {
			slog.Info("backfilled HF download observations", "component", "sync", "count", recorded)
		}
	})
}

// jobEligibleForStartedMarker returns the current run ID for a queued job that
// is still attached to a live cloud instance. This prevents stale R2 .started
// markers from resurrecting jobs that were already reset from failed instances.
func jobEligibleForStartedMarker(database *sql.DB, jobID int64) (int64, bool, error) {
	var (
		currentStatus   string
		latestRunID     sql.NullInt64
		cloudInstanceID sql.NullInt64
		launchStatus    sql.NullString
	)
	if err := database.QueryRow(
		`SELECT status, latest_run_id, launch_id
		 FROM job_status
		 WHERE id = ? AND tombstoned = 0`,
		jobID,
	).Scan(&currentStatus, &latestRunID, &cloudInstanceID); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	if currentStatus != db.StatusQueued || !cloudInstanceID.Valid {
		return 0, false, nil
	}
	if err := database.QueryRow(
		`SELECT status FROM launches WHERE id = ?`,
		cloudInstanceID.Int64,
	).Scan(&launchStatus); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	if campaign.IsInstanceTerminal(launchStatus.String) {
		return 0, false, nil
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	return runID, true, nil
}

func updateInstanceTerminationReason(database *sql.DB, instanceID int64) {
	if err := db.RefineInstanceTerminationReason(database, instanceID); err != nil {
		slog.Warn("failed to refine termination reason", "component", "sync", "instance_id", instanceID, "error", err)
	}
}

func r2Config(cfg *config.Config) r2.Config {
	return r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}
}

// syncCloudLiveTimeseries imports live JSONL telemetry from R2 for running or
// unresolved cloud jobs that have .started markers but no .complete marker.
func syncCloudLiveTimeseries(ctx context.Context, r2Client *r2.Client, database *sql.DB, jobID int64) error {
	var status string
	var backend sql.NullString
	var latestRunID sql.NullInt64
	if err := database.QueryRow("SELECT status, backend, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &backend, &latestRunID); err != nil {
		return nil
	}
	if db.IsTerminalStatus(status) {
		return nil
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	data, err := r2Client.GetObject(ctx, r2keys.JobAttemptLiveTimeseries(jobID, runID))
	if err != nil || len(data) == 0 {
		return nil
	}

	lastTS, err := db.GetTimeseriesLastTS(database, jobID)
	if err != nil {
		return fmt.Errorf("get last ts: %w", err)
	}

	tenant := "multi"
	if backend.Valid && backend.String == db.BackendVastai {
		tenant = "single"
	}

	var samples []db.TimeseriesSample
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s db.TimeseriesSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		if s.Ts <= lastTS {
			continue
		}
		s.Tenant = tenant
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return nil
	}

	if err := db.InsertTimeseries(database, jobID, samples); err != nil {
		return fmt.Errorf("insert timeseries: %w", err)
	}
	return nil
}

func syncCloudLiveTelemetry(ctx context.Context, r2Client *r2.Client, database *sql.DB, jobID int64) error {
	var status string
	var latestRunID sql.NullInt64
	if err := database.QueryRow("SELECT status, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &latestRunID); err != nil {
		return nil
	}
	if db.IsTerminalStatus(status) {
		return nil
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	data, err := r2Client.GetObject(ctx, r2keys.JobAttemptLiveTelemetry(jobID, runID))
	if err != nil || len(data) == 0 {
		return nil
	}

	lastTS, err := db.GetTelemetryLastTS(database, jobID)
	if err != nil {
		return fmt.Errorf("get telemetry last ts: %w", err)
	}

	samples := db.ParseTelemetrySamplesJSONL(string(data), lastTS)
	if len(samples) == 0 {
		return nil
	}

	if err := db.InsertTelemetrySamples(database, jobID, samples); err != nil {
		return fmt.Errorf("insert telemetry: %w", err)
	}
	return nil
}

func importCloudTimeseriesFile(database *sql.DB, jobID int64, path, tenant string) error {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	lastTS, err := db.GetTimeseriesLastTS(database, jobID)
	if err != nil {
		return err
	}
	var samples []db.TimeseriesSample
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var sample db.TimeseriesSample
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			continue
		}
		if sample.Ts <= lastTS {
			continue
		}
		sample.Tenant = tenant
		samples = append(samples, sample)
	}
	return db.InsertTimeseries(database, jobID, samples)
}

func importCloudTelemetryFile(database *sql.DB, jobID int64, path string) error {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	lastTS, err := db.GetTelemetryLastTS(database, jobID)
	if err != nil {
		return err
	}
	samples := db.ParseTelemetrySamplesJSONL(string(data), lastTS)
	if err := db.InsertTelemetrySamples(database, jobID, samples); err != nil {
		return err
	}
	return db.RefreshJobTelemetrySummary(database, jobID)
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
				fmt.Fprintf(os.Stderr, "Warning: agent binary for %s/%s not built; run 'just build-agents'\n", spec.OS, spec.Arch)
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

// quickSyncJobs performs a fast sync for a set of jobs before display commands.
// It skips jobs in terminal states, separates on-prem vs rental jobs, and syncs
// each type appropriately. Sync failures are silently ignored.
// Returns unreachable on-prem hosts (caller may optionally warn).
func quickSyncJobs(database *sql.DB, jobs []*db.Job, sshTimeout time.Duration) []string {
	hostsToSync := make(map[string]struct{})
	needsRentalSync := false

	for _, job := range jobs {
		if isTerminalStatus(job.Status) {
			continue
		}
		if job.HasInventoryHost() {
			hostsToSync[job.Host] = struct{}{}
		}
		if job.UsesRentalPlacement() {
			needsRentalSync = true
		}
	}

	var unreachable []string
	if len(hostsToSync) > 0 {
		hosts := mapKeys(hostsToSync)
		_, unreachable = performSyncWithTimeoutForHosts(database, hosts, sshTimeout, false)
	}
	if needsRentalSync {
		syncRentalJobsStatus(database)
	}
	return unreachable
}
