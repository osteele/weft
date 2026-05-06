package syncorch

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

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/transferbw"
)

const cloudInstanceOpslogLookback = 7 * 24 * time.Hour

const (
	// opslogSyncTimeout is the overall budget for the opslog sync phase.
	opslogSyncTimeout = 60 * time.Second
	// opslogRequestTimeout caps each individual R2 opslog fetch.
	opslogRequestTimeout = 10 * time.Second
)

// syncCloudJobResults checks for completed cloud job results in R2.
// This covers both legacy Vast.ai-backend jobs and campaign-launched queue-runner jobs.
// Uses per-job DB lookups rather than pre-filtering by cloud_instance_id, so results
// are synced even if the instance association was cleared by a concurrent reset.
func SyncCloudJobResults(parent context.Context, cfg *config.Config, database *sql.DB, verbose bool) int {
	if parent == nil {
		parent = context.Background()
	}
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

	r2Cfg := R2Config(cfg)
	r2Client, err := r2.New(r2Cfg)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: R2 client: %v\n", err)
		}
		return 0
	}

	// Cap this phase at 60s, derived from the parent so caller cancellation
	// also propagates here.
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()

	updatedInstanceIDs := make(map[int64]struct{})
	markers, err := r2Client.ListJobMarkers(ctx, "jobs/")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			slog.Debug("cloud sync skipped", "component", "sync", "reason", "R2 storage unreachable")
		} else if verbose {
			slog.Warn("R2 list failed", "component", "sync", "error", err)
		}
		return 0
	}

	if verbose && (len(markers.Completed) > 0 || len(markers.Started) > 0) {
		fmt.Printf("Checking %d completed + %d started cloud job marker(s)...\n",
			len(markers.Completed), len(markers.Started))
	}

	// Track jobs processed as completed so the started-markers loop skips them.
	completedJobIDs := make(map[int64]bool, len(markers.Completed))

	// Process completed markers in parallel (bounded concurrency).
	const syncJobMarkerParallel = 10
	{
		sem := make(chan struct{}, syncJobMarkerParallel)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, jobIDStr := range markers.Completed {
			if ctx.Err() != nil {
				break
			}
			jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
			if jobID == 0 || markers.IsProcessed(jobID) {
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func(jobID int64, jobIDStr string) {
				defer func() { <-sem; wg.Done() }()
				result := syncOneCompletedJobMarker(ctx, database, r2Client, markers, jobID, jobIDStr, verbose)
				if result.completed {
					mu.Lock()
					updated++
					completedJobIDs[jobID] = true
					if result.updatedInstanceID > 0 {
						updatedInstanceIDs[result.updatedInstanceID] = struct{}{}
					}
					mu.Unlock()
				}
			}(jobID, jobIDStr)
		}
		wg.Wait()
	}

	// Check for .started markers to transition queued jobs to running (parallel).
	{
		sem := make(chan struct{}, syncJobMarkerParallel)
		var mu sync.Mutex
		var eligibilityWarnings sync.Map
		var wg sync.WaitGroup

		for _, jobIDStr := range markers.Started {
			if ctx.Err() != nil {
				break
			}
			jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
			if jobID == 0 || completedJobIDs[jobID] {
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func(jobID int64) {
				defer func() { <-sem; wg.Done() }()

				runID, ok, err := JobEligibleForStartedMarker(database, jobID)
				if err != nil {
					if _, loaded := eligibilityWarnings.LoadOrStore(err.Error(), struct{}{}); !loaded {
						slog.Warn("failed to check started-marker eligibility", "component", "sync", "job_id", jobID, "error", err, "note", "further warnings with the same error are suppressed")
					}
					return
				}
				if !ok {
					return
				}
				if !markers.HasStartedMarker(jobID, r2keys.JobAttemptStarted(jobID, runID)) {
					return
				}
				var startTimeUnix int64
				if data, err := r2Client.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
					startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
				}

				if _, err := database.ExecContext(ctx,
					`UPDATE job_attempts SET status = ?, start_time = ?
					 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL ORDER BY attempt_number DESC LIMIT 1)`,
					db.StatusRunning, startTimeUnix, jobID,
				); err != nil {
					slog.Warn("failed to update cloud job to running", "component", "sync", "job_id", jobID, "error", err)
					return
				}
				mu.Lock()
				updated++
				mu.Unlock()
				if verbose {
					fmt.Printf("  cloud job %s: started\n", ids.FormatJobID(jobID))
				}
			}(jobID)
		}
		wg.Wait()
	}

	// Import live timeseries checkpoints for started jobs (parallel).
	{
		sem := make(chan struct{}, syncJobMarkerParallel)
		var wg sync.WaitGroup

		for _, jobIDStr := range markers.Started {
			if ctx.Err() != nil {
				break
			}
			jobID, _ := strconv.ParseInt(jobIDStr, 10, 64)
			if jobID == 0 || completedJobIDs[jobID] {
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func(jobID int64) {
				defer func() { <-sem; wg.Done() }()
				if err := syncCloudLiveTimeseries(ctx, r2Client, database, jobID); err != nil && verbose {
					fmt.Fprintf(os.Stderr, "Warning: cloud job %s live timeseries sync failed: %v\n", ids.FormatJobID(jobID), err)
				}
				if err := syncCloudLiveTelemetry(ctx, r2Client, database, jobID); err != nil && verbose {
					fmt.Fprintf(os.Stderr, "Warning: cloud job %s live telemetry sync failed: %v\n", ids.FormatJobID(jobID), err)
				}
			}(jobID)
		}
		wg.Wait()
	}
	// Use a fresh context for opslog sync — the shared ctx may be nearly
	// expired after job-marker and timeseries syncing consumed most of its budget.
	// Still derived from parent so caller cancellation propagates.
	opslogCtx, opslogCancel := context.WithTimeout(parent, opslogSyncTimeout)
	defer opslogCancel()
	if err := SyncCloudInstanceOpslogs(opslogCtx, r2Client, database, nil, verbose); err != nil && verbose {
		fmt.Fprintf(os.Stderr, "Warning: instance ops log sync failed: %v\n", err)
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
	BackfillHFDownloadObservations(database)

	return updated
}

// AllowCompletedMarkerFallback reports whether a sync may use AnyCompletedKey
// to recover when the marker isn't found at the current latest_run_id.
//
// This fires in two cases:
//   - Queued jobs with no launch_id (legacy fallback for orphaned R2 markers).
//   - Terminal jobs that need backfill: cleanupStaleAttempts may have advanced
//     latest_run_id past the run_id where the agent uploaded the marker, so we
//     must look across all run_ids to recover authoritative timestamps.
func AllowCompletedMarkerFallback(currentStatus string, launchID sql.NullInt64, needsBackfill bool) bool {
	if currentStatus == db.StatusQueued && !launchID.Valid {
		return true
	}
	if db.IsTerminalStatus(currentStatus) && needsBackfill {
		return true
	}
	return false
}

func ShouldMarkCloudJobProcessed(currentStatus string, needsBackfill bool, source string) bool {
	if db.IsTerminalStatus(currentStatus) && needsBackfill {
		return false
	}
	return source == campaign.SourceResults
}

type completedMarkerResult struct {
	updatedInstanceID int64
	completed         bool
}

// syncOneCompletedJobMarker processes a single completed-job R2 marker.
func syncOneCompletedJobMarker(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	markers *r2.JobMarkers,
	jobID int64,
	jobIDStr string,
	verbose bool,
) completedMarkerResult {
	// Check current job status directly.
	var currentStatus string
	var latestRunID, launchID sql.NullInt64
	if err := database.QueryRow(
		"SELECT status, latest_run_id, launch_id FROM job_status WHERE id = ? AND tombstoned = 0",
		jobID,
	).Scan(&currentStatus, &latestRunID, &launchID); err != nil {
		_ = r2Client.PutMarker(ctx, r2keys.JobProcessed(jobID))
		return completedMarkerResult{}
	}

	needsBackfill := false
	if db.IsTerminalStatus(currentStatus) {
		var backfillErr error
		needsBackfill, backfillErr = db.NeedsCloudCompletionBackfill(database, jobID)
		if backfillErr != nil {
			slog.Warn("failed to evaluate cloud completion backfill need", "component", "sync", "job_id", jobID, "error", backfillErr)
			return completedMarkerResult{}
		}
		if !needsBackfill {
			slog.Debug("skipping cloud completion sync for terminal job with complete metadata",
				"component", "sync", "job_id", jobID, "reason", "terminal_complete_skip")
			_ = r2Client.PutMarker(ctx, r2keys.JobProcessed(jobID))
			return completedMarkerResult{}
		}
		slog.Debug("attempting cloud completion backfill for terminal job",
			"component", "sync", "job_id", jobID, "reason", "terminal_incomplete_backfill")
	}

	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	if !markers.HasCompletedMarker(jobID, r2keys.JobAttemptComplete(jobID, runID)) {
		if !AllowCompletedMarkerFallback(currentStatus, launchID, needsBackfill) {
			return completedMarkerResult{}
		}
		if altKey, ok := markers.AnyCompletedKey(jobID); ok {
			runID = r2keys.ExtractRunID(altKey)
		} else {
			return completedMarkerResult{}
		}
	}
	resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)
	completeKey := r2keys.JobAttemptComplete(jobID, runID)

	var (
		exitCode      *int
		startTimeUnix int64
		endTimeUnix   int64
		failureReason string
		source        = campaign.SourceResults
		tmpDir        string
		haveResults   bool
	)

	markerData, markerLastModified, markerErr := r2Client.GetObjectWithMeta(ctx, completeKey)
	if markerErr != nil {
		return completedMarkerResult{}
	}

	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-cloud-%d-*", jobID))
	if err == nil {
		if err := r2Client.DownloadResults(ctx, resultPrefix, tmpDir); err == nil {
			exitCode, startTimeUnix, endTimeUnix, failureReason = db.ParseCloudJobResult(tmpDir, jobIDStr)
			if exitCode != nil {
				haveResults = true
			}
		}
	}

	if exitCode == nil {
		if code, parseErr := strconv.Atoi(strings.TrimSpace(string(markerData))); parseErr == nil {
			exitCode = &code
			source = campaign.SourceMarkerFallback
			slog.Debug("using exit code from .complete marker (results not yet available)",
				"component", "sync", "job_id", jobID, "run_id", runID, "reason", "marker_only_backfill")
		} else {
			os.RemoveAll(tmpDir)
			return completedMarkerResult{}
		}
	}

	if startTimeUnix == 0 {
		if data, err := r2Client.GetObject(ctx, r2keys.JobAttemptStarted(jobID, runID)); err == nil {
			startTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		}
	}

	updatedInstanceID, err := db.RecordCloudJobCompletion(database, jobID, *exitCode, startTimeUnix, endTimeUnix, failureReason, markerLastModified)
	if err != nil {
		slog.Warn("failed to update cloud job status", "component", "sync", "job_id", jobID, "error", err)
		os.RemoveAll(tmpDir)
		return completedMarkerResult{}
	}
	result := completedMarkerResult{updatedInstanceID: updatedInstanceID, completed: true}

	host := ""
	if updatedInstanceID > 0 {
		host = db.LaunchHost(updatedInstanceID)
	}
	if *exitCode == 0 {
		oplog.LogJob(oplog.OpJobComplete, jobID, host, oplog.WithDetailf("cloud exit=0 source=%s", source))
	} else {
		oplog.LogJob(oplog.OpJobFail, jobID, host, oplog.WithDetailf("cloud exit=%d source=%s", *exitCode, source))
	}

	if haveResults {
		if err := importCloudTimeseriesFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.timeseries.jsonl", jobID)), "single"); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %s final timeseries import failed: %v\n", ids.FormatJobID(jobID), err)
		}
		if err := importCloudTelemetryFile(database, jobID, filepath.Join(tmpDir, fmt.Sprintf("%d.telemetry.jsonl", jobID))); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "Warning: cloud job %s final telemetry import failed: %v\n", ids.FormatJobID(jobID), err)
		}

		if timings := coordinator.ExtractPhaseTimings(jobID, tmpDir); timings != nil {
			if err := db.UpsertJobPhaseTimings(database, timings); err != nil {
				slog.Warn("failed to store phase timings", "component", "sync", "job_id", jobID, "error", err)
			}
			if launch, err := db.GetLaunch(database, updatedInstanceID); err == nil && launch != nil {
				recordCloudDownloadObservation(database, launch.Provider, launch.DataCenter, timings)
			}
		}

		coordinator.WriteVastaiLogsToCache(jobID, tmpDir)
	}

	if verbose {
		statusLabel := db.StatusCompleted
		if *exitCode != 0 {
			statusLabel = db.StatusFailed
		}
		fmt.Printf("  cloud job %s: %s (exit %d)\n", ids.FormatJobID(jobID), statusLabel, *exitCode)
	}

	if source == campaign.SourceResults {
		_ = r2Client.PutMarker(ctx, r2keys.JobAttemptProcessed(jobID, runID))
		_ = r2Client.DeletePrefix(ctx, resultPrefix)
	}
	if ShouldMarkCloudJobProcessed(currentStatus, needsBackfill, source) {
		_ = r2Client.PutMarker(ctx, r2keys.JobProcessed(jobID))
	}
	os.RemoveAll(tmpDir)
	return result
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
func BackfillHFDownloadObservations(database *sql.DB) {
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
func JobEligibleForStartedMarker(database *sql.DB, jobID int64) (int64, bool, error) {
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

func R2Config(cfg *config.Config) r2.Config {
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
	if err := database.QueryRowContext(ctx, "SELECT status, backend, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &backend, &latestRunID); err != nil {
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
	if err := database.QueryRowContext(ctx, "SELECT status, latest_run_id FROM job_status WHERE id = ? AND tombstoned = 0", jobID).Scan(&status, &latestRunID); err != nil {
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

func SyncCloudInstanceOpslogs(ctx context.Context, r2Client *r2.Client, database *sql.DB, instanceIDs map[int64]struct{}, verbose bool) error {
	if r2Client == nil || database == nil {
		return nil
	}

	var ids []int64
	if instanceIDs != nil {
		ids = make([]int64, 0, len(instanceIDs))
		for id := range instanceIDs {
			if id > 0 {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	} else {
		var err error
		ids, err = db.ListLaunchIDsNeedingOpslogSync(database, cloudInstanceOpslogLookback)
		if err != nil {
			return fmt.Errorf("list launches needing opslog sync: %w", err)
		}
	}

	if len(ids) == 0 {
		return nil
	}

	if verbose {
		fmt.Printf("Syncing ops logs for %d instance(s)...\n", len(ids))
	}

	// Bounded parallelism for R2 reads
	const maxParallel = 10
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var timeoutCount, errorCount int

	for _, instanceID := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func(id int64) {
			defer func() { <-sem; wg.Done() }()

			result, err := syncCloudInstanceOpslog(ctx, r2Client, id)
			switch result {
			case opslogSynced:
				_ = db.MarkOpslogSynced(database, id)
			case opslogNotFound:
				_ = db.MarkOpslogNotFound(database, id)
			case opslogError:
				mu.Lock()
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					_ = db.MarkOpslogTimeout(database, id)
					timeoutCount++
				} else {
					errorCount++
				}
				mu.Unlock()
			}
		}(instanceID)
	}
	wg.Wait()
	if timeoutCount > 0 {
		slog.Warn("opslog sync timed out", "component", "sync", "count", timeoutCount)
	}
	if errorCount > 0 {
		slog.Warn("opslog sync errors", "component", "sync", "count", errorCount)
	}
	return nil
}

// opslogResult describes the outcome of a single opslog sync attempt.
type opslogResult int

const (
	opslogSynced   opslogResult = iota // successfully fetched and cached
	opslogNotFound                     // R2 key does not exist
	opslogError                        // transient error (timeout, network)
)

func syncCloudInstanceOpslog(ctx context.Context, r2Client *r2.Client, instanceID int64) (opslogResult, error) {
	if r2Client == nil || instanceID <= 0 {
		return opslogNotFound, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, opslogRequestTimeout)
	defer cancel()
	data, err := r2Client.GetObject(reqCtx, r2keys.InstanceOpslog(instanceID))
	if err != nil {
		if r2.IsNotFound(err) {
			return opslogNotFound, nil
		}
		return opslogError, fmt.Errorf("get instance ops log: %w", err)
	}
	if len(data) == 0 {
		return opslogNotFound, nil
	}
	if err := oplog.WriteSyncedInstanceLog(instanceID, data); err != nil {
		return opslogError, fmt.Errorf("cache instance ops log: %w", err)
	}
	return opslogSynced, nil
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
