package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
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
		Logger:       ops.NewQuietSyncLogger(),
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
		Logger:       ops.NewQuietSyncLogger(),
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
	if repaired, err := db.ResetJobsOnTerminalCloudInstances(database); err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: terminal cloud job repair: %v\n", err)
		}
	} else if repaired > 0 {
		updated += int(repaired)
		if verbose {
			fmt.Printf("Repaired %d stale cloud job assignment(s) on terminal instances\n", repaired)
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

		// Check current job status directly — skip if already terminal
		var currentStatus string
		var latestRunID sql.NullInt64
		if err := database.QueryRow("SELECT status, latest_run_id FROM jobs WHERE id = ? AND tombstoned = 0", jobID).Scan(&currentStatus, &latestRunID); err != nil || db.IsTerminalStatus(currentStatus) {
			// Job not found or already terminal — clean up stale R2 markers
			_ = r2Client.DeletePrefix(ctx, r2keys.JobPrefix(jobID)+"/")
			continue
		}

		runID := int64(0)
		if latestRunID.Valid {
			runID = latestRunID.Int64
		}
		if !markers.HasCompletedMarker(jobID, r2keys.JobAttemptComplete(jobID, runID)) {
			continue
		}
		resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)
		cleanupPrefix := r2keys.JobRunPrefix(jobID, runID)

		// Download results to temp dir
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-cloud-%d-*", jobID))
		if err != nil {
			continue
		}

		if err := r2Client.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
			os.RemoveAll(tmpDir)
			continue
		}

		exitCode, startTimeUnix, endTimeUnix, failureReason := parseCloudJobResult(tmpDir, jobIDStr)
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

		updatedInstanceID, err := recordCloudJobCompletion(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason)
		if err != nil {
			log.Printf("sync: failed to update cloud job %d status: %v", jobID, err)
			continue
		}
		if updatedInstanceID > 0 {
			updatedInstanceIDs[updatedInstanceID] = struct{}{}
		}
		updated++
		completedJobIDs[jobID] = true

		if err := importCloudTimeseriesFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.timeseries.jsonl", jobID)), "single"); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d final timeseries import failed: %v\n", jobID, err)
		}
		if err := importCloudTelemetryFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.telemetry.jsonl", jobID))); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %d final telemetry import failed: %v\n", jobID, err)
		}

		// Extract and store phase timing data
		if timings := coordinator.ExtractPhaseTimings(jobID, tmpDir); timings != nil {
			if err := db.UpsertJobPhaseTimings(database, timings); err != nil {
				log.Printf("sync: failed to store phase timings for job %d: %v", jobID, err)
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

		// Cleanup R2
		_ = r2Client.DeletePrefix(ctx, cleanupPrefix+"/")
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
			log.Printf("sync: failed to check started-marker eligibility for job %d: %v", jobID, err)
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
			`UPDATE jobs SET status = ?, start_time = ? WHERE id = ?`,
			db.StatusRunning, startTimeUnix, jobID,
		); err != nil {
			log.Printf("sync: failed to update cloud job %d to running: %v", jobID, err)
			continue
		}
		if err := db.PersistLatestRunSnapshot(database, jobID, ""); err != nil {
			log.Printf("sync: failed to persist cloud job %d running snapshot: %v", jobID, err)
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

	return updated
}

// jobEligibleForStartedMarker returns the current run ID for a queued job that
// is still attached to a live cloud instance. This prevents stale R2 .started
// markers from resurrecting jobs that were already reset from failed instances.
func jobEligibleForStartedMarker(database *sql.DB, jobID int64) (int64, bool, error) {
	var (
		currentStatus   string
		latestRunID     sql.NullInt64
		cloudInstanceID sql.NullInt64
		cloudInstStatus sql.NullString
	)
	if err := database.QueryRow(
		`SELECT status, latest_run_id, cloud_instance_id
		 FROM jobs
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
		`SELECT status FROM cloud_instances WHERE id = ?`,
		cloudInstanceID.Int64,
	).Scan(&cloudInstStatus); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	if campaign.IsInstanceTerminal(cloudInstStatus.String) {
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
		log.Printf("sync: refine termination reason for instance %d: %v", instanceID, err)
	}
}

func recordCloudJobCompletion(database *sql.DB, jobID int64, exitCode int, startTimeUnix, endTimeUnix int64, failureReason string) (int64, error) {
	status := db.StatusCompleted
	outcome := db.AttemptOutcomeCompleted
	if exitCode != 0 {
		status = db.StatusFailed
		outcome = db.AttemptOutcomeFailed
	}

	var cloudInstanceID sql.NullInt64
	if err := database.QueryRow(`SELECT cloud_instance_id FROM jobs WHERE id = ? AND tombstoned = 0`, jobID).Scan(&cloudInstanceID); err != nil {
		return 0, err
	}

	if _, err := database.Exec(
		`UPDATE jobs
		 SET status = ?, exit_code = ?, start_time = ?, end_time = ?, last_synced_status = ?,
		     failure_reason = COALESCE(NULLIF(?, ''), failure_reason)
		 WHERE id = ?`,
		status, exitCode, startTimeUnix, endTimeUnix, status, failureReason, jobID,
	); err != nil {
		return 0, err
	}
	if err := db.PersistLatestRunSnapshot(database, jobID, ""); err != nil {
		return 0, err
	}
	if err := db.CloseJobCloudAttempt(database, jobID, outcome); err != nil {
		return 0, err
	}
	if cloudInstanceID.Valid {
		return cloudInstanceID.Int64, nil
	}
	return 0, nil
}

func r2Config(cfg *config.Config) r2.Config {
	return r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}
}

// parseCloudJobResult reads the exit code, start time, end time, and failure
// reason from a downloaded R2 results directory.
// Returns nil exitCode if no valid result was found.
func parseCloudJobResult(tmpDir, jobIDStr string) (exitCode *int, startTimeUnix, endTimeUnix int64, failureReason string) {
	// Try completion JSON (agent format: <jobID>.completion.json)
	completionPath := filepath.Join(tmpDir, jobIDStr+".completion.json")
	if data, err := os.ReadFile(completionPath); err == nil {
		var rec struct {
			ExitCode      int    `json:"exit_code"`
			StartTime     int64  `json:"start_time"`
			EndTime       int64  `json:"end_time"`
			FailureReason string `json:"failure_reason"`
		}
		if json.Unmarshal(data, &rec) == nil {
			return &rec.ExitCode, rec.StartTime, rec.EndTime, rec.FailureReason
		}
	}

	// Fallback: <jobID>.status (exit code as text)
	statusPath := filepath.Join(tmpDir, jobIDStr+".status")
	if data, err := os.ReadFile(statusPath); err == nil {
		var code int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code); err == nil {
			return &code, 0, 0, ""
		}
	}

	// Legacy fallback: standalone exit_code file
	if data, err := os.ReadFile(filepath.Join(tmpDir, "exit_code")); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			endTimeBytes, _ := os.ReadFile(filepath.Join(tmpDir, "end_time"))
			et, _ := strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)
			return &code, 0, et, ""
		}
	}

	return nil, 0, 0, ""
}

// syncCloudLiveTimeseries imports live JSONL telemetry from R2 for running or
// unresolved cloud jobs that have .started markers but no .complete marker.
func syncCloudLiveTimeseries(ctx context.Context, r2Client *r2.Client, database *sql.DB, jobID int64) error {
	var status string
	var backend sql.NullString
	var latestRunID sql.NullInt64
	if err := database.QueryRow("SELECT status, backend, latest_run_id FROM jobs WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &backend, &latestRunID); err != nil {
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
	if err := database.QueryRow("SELECT status, latest_run_id FROM jobs WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &latestRunID); err != nil {
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
