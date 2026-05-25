package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// loadR2Client creates an R2 client from the app config.
// Returns nil, nil if R2 is not configured.
func (c *Coordinator) loadR2Client() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil || cfg.Vastai.R2.Bucket == "" {
		return nil, nil
	}
	client, err := r2.New(r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	})
	return client, err
}

// sweepVastaiResults polls R2 for completed Vast.ai job results and processes them.
func (c *Coordinator) sweepVastaiResults() *r2.Client {
	r2Client, err := c.loadR2Client()
	if err != nil {
		c.logger.Warn("failed to create r2 client", "error", err)
		return nil
	}
	if r2Client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Check for completed results
	completedJobIDs, err := r2Client.ListCompleted(ctx, "jobs/")
	if err != nil {
		c.logger.Warn("failed to list r2 completed jobs", "error", err)
		return r2Client
	}

	for _, jobIDStr := range completedJobIDs {
		jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
		if err != nil {
			continue
		}
		c.processCompletedVastaiJob(ctx, r2Client, jobID)
	}

	return r2Client
}

// processCompletedVastaiJob downloads results from R2, updates the job in the DB,
// writes logs to the log cache, and cleans up the R2 prefix.
func (c *Coordinator) processCompletedVastaiJob(ctx context.Context, r2Client *r2.Client, jobID int64) {
	var latestRunID sql.NullInt64
	if err := c.db.QueryRow(`SELECT latest_run_id FROM job_status WHERE id = ?`, jobID).Scan(&latestRunID); err != nil && err != sql.ErrNoRows {
		c.logger.Warn("failed to get latest_run_id", "job_id", jobID, "error", err)
	}
	runID := int64(0)
	if latestRunID.Valid {
		runID = latestRunID.Int64
	}
	resultPrefix := r2keys.JobAttemptResultsPrefix(jobID, runID)

	// Download results to temp dir
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-vastai-%d-*", jobID))
	if err != nil {
		c.logger.Warn("failed to create temp dir", "job_id", jobID, "error", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := r2Client.DownloadResults(ctx, resultPrefix, tmpDir); err != nil {
		c.logger.Warn("failed to download results", "job_id", jobID, "error", err)
		return
	}

	// Try agent format first: completion.json has structured exit info
	var exitCode int
	var endTimeUnix int64
	var failureReason string

	completionPath := filepath.Join(tmpDir, fmt.Sprintf("%d.completion.json", jobID))
	if cData, err := os.ReadFile(completionPath); err == nil {
		var completion struct {
			ExitCode      int    `json:"exit_code"`
			EndTime       int64  `json:"end_time"`
			FailureReason string `json:"failure_reason"`
		}
		if json.Unmarshal(cData, &completion) == nil {
			exitCode = completion.ExitCode
			endTimeUnix = completion.EndTime
			failureReason = completion.FailureReason
		}
	}

	// Fall back to legacy format: separate exit_code and end_time files
	if endTimeUnix == 0 {
		exitCodeBytes, err := os.ReadFile(filepath.Join(tmpDir, "exit_code"))
		if err != nil {
			c.logger.Warn("failed to read exit_code", "job_id", jobID, "error", err)
			return
		}
		exitCode, _ = strconv.Atoi(strings.TrimSpace(string(exitCodeBytes)))

		endTimeBytes, err := os.ReadFile(filepath.Join(tmpDir, "end_time"))
		if err != nil {
			c.logger.Warn("failed to read end_time", "job_id", jobID, "error", err)
			return
		}
		endTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)

		if exitCode != 0 && failureReason == "" {
			failureReason = detectFailureReason(tmpDir, exitCode)
		}
	}

	// Update job in DB — always StatusCompleted; exit code stored separately.
	// Clear pending_status: the job already ran on the cloud instance, so any
	// pending intent (e.g. pending_status=queued set before this run) is moot.
	// Stamp cloud_outcome in the same UPDATE per
	// ClosedCloudAttemptHasOutcome (campaign-lifecycle.allium): exit_code=0
	// → 'completed', non-zero → 'failed' (the same derivation the
	// cloud_attempts_auto_derive_outcome_* trigger uses in 00006).
	cloudOutcome := db.AttemptOutcomeCompleted
	if exitCode != 0 {
		cloudOutcome = db.AttemptOutcomeFailed
	}
	_, err = c.db.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ?, failure_reason = ?, pending_status = NULL,
		    cloud_outcome = CASE WHEN cloud_outcome IS NULL THEN ? ELSE cloud_outcome END
		 WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL ORDER BY attempt_number DESC LIMIT 1)`,
		db.StatusCompleted, exitCode, endTimeUnix, db.StatusCompleted, failureReason, cloudOutcome, jobID,
	)
	if err != nil {
		c.logger.Warn("failed to update job", "job_id", jobID, "error", err)
		return
	}
	var cloudInstanceID sql.NullInt64
	if err := c.db.QueryRow(`SELECT launch_id FROM job_status WHERE id = ?`, jobID).Scan(&cloudInstanceID); err == nil && cloudInstanceID.Valid && cloudInstanceID.Int64 > 0 {
		if err := db.RefineInstanceTerminationReason(c.db, cloudInstanceID.Int64); err != nil {
			c.logger.Warn("failed to refine termination reason", "instance_id", cloudInstanceID.Int64, "error", err)
		}
	}

	// Close the cloud attempt record
	outcome := db.AttemptOutcomeCompleted
	if exitCode != 0 {
		outcome = db.AttemptOutcomeFailed
	}
	if err := db.CloseLaunchAttempt(c.db, jobID, outcome); err != nil {
		c.logger.Warn("failed to close attempt", "job_id", jobID, "error", err)
	}

	// Extract and store phase timing data
	if timings := ExtractPhaseTimings(jobID, tmpDir); timings != nil {
		if err := db.UpsertJobPhaseTimings(c.db, timings); err != nil {
			c.logger.Warn("failed to store phase timings", "job_id", jobID, "error", err)
		}
	}

	// Write logs to log cache
	WriteVastaiLogsToCache(jobID, tmpDir)

	// Keep live-log chunks: they are the primary log source for `weft log`.
	_ = r2Client.PutMarker(ctx, r2keys.JobAttemptProcessed(jobID, runID))
	_ = r2Client.DeletePrefix(ctx, resultPrefix)

	c.logger.Info("processed vastai job", "job_id", jobID, "exit_code", exitCode, "status", db.StatusCompleted)
	if exitCode == 0 {
		oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetailf("vastai exit=%d", exitCode))
	} else {
		oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("vastai exit=%d reason=%s", exitCode, failureReason))
	}
}

// detectFailureReason examines debug artifacts to determine why a job failed.
func detectFailureReason(tmpDir string, exitCode int) string {
	if exitCode == 137 {
		return "oom"
	}

	dmesgPath := filepath.Join(tmpDir, "debug", "dmesg.log")
	if dmesgBytes, err := os.ReadFile(dmesgPath); err == nil {
		dmesg := string(dmesgBytes)
		if strings.Contains(dmesg, "Out of memory") || strings.Contains(dmesg, "oom-kill") || strings.Contains(dmesg, "Killed process") {
			return "oom"
		}
	}

	nvidiaSmiPath := filepath.Join(tmpDir, "debug", "nvidia-smi.log")
	if nvBytes, err := os.ReadFile(nvidiaSmiPath); err == nil {
		nvOutput := string(nvBytes)
		if strings.Contains(nvOutput, "out of memory") || strings.Contains(nvOutput, "CUDA_ERROR_OUT_OF_MEMORY") {
			return "gpu_oom"
		}
	}

	if exitCode == 1 {
		return "error"
	}
	return fmt.Sprintf("exit_%d", exitCode)
}

// ExtractPhaseTimings reads phase timing data from the results dir.
// It first tries the structured phases.json format (agent-based wrapper),
// then falls back to individual phase_* files (legacy bash wrapper).
// Returns nil if no phase files are found (non-instrumented wrapper).
func ExtractPhaseTimings(jobID int64, tmpDir string) *db.JobPhaseTimings {
	// Try structured phases.json first (from agent-based wrapper)
	if timings := extractStructuredPhaseTimings(jobID, tmpDir); timings != nil {
		mergeTelemetryPhaseMetrics(timings, filepath.Join(tmpDir, fmt.Sprintf("%d.telemetry.jsonl", jobID)))
		return timings
	}
	timings := extractLegacyPhaseTimings(jobID, tmpDir)
	if timings != nil {
		mergeTelemetryPhaseMetrics(timings, filepath.Join(tmpDir, fmt.Sprintf("%d.telemetry.jsonl", jobID)))
	}
	return timings
}

// extractStructuredPhaseTimings reads a phases.json file written by weft-agent run-job.
func extractStructuredPhaseTimings(jobID int64, tmpDir string) *db.JobPhaseTimings {
	phasesPath := filepath.Join(tmpDir, fmt.Sprintf("%d.phases.json", jobID))
	data, err := os.ReadFile(phasesPath)
	if err != nil {
		return nil
	}

	var phases struct {
		WrapperStart int64 `json:"wrapper_start"`
		SetupStart   int64 `json:"setup_start"`
		SetupEnd     int64 `json:"setup_end"`
		RunStart     int64 `json:"run_start"`
		RunEnd       int64 `json:"run_end"`
		UploadStart  int64 `json:"upload_start"`
		UploadEnd    int64 `json:"upload_end"`
		CachePre     *struct {
			HFBytes int64 `json:"hf_bytes"`
			UVBytes int64 `json:"uv_bytes"`
		} `json:"cache_pre"`
		CachePost *struct {
			HFBytes        int64 `json:"hf_bytes"`
			UVBytes        int64 `json:"uv_bytes"`
			DiskUsedBytes  int64 `json:"disk_used_bytes"`
			DiskTotalBytes int64 `json:"disk_total_bytes"`
		} `json:"cache_post"`
		SetupSeconds *int64 `json:"setup_seconds"`
	}
	if err := json.Unmarshal(data, &phases); err != nil {
		return nil
	}

	t := &db.JobPhaseTimings{JobID: jobID}
	if phases.WrapperStart > 0 {
		t.WrapperStart = &phases.WrapperStart
	}
	if phases.SetupStart > 0 {
		t.SetupStart = &phases.SetupStart
	}
	if phases.SetupEnd > 0 {
		t.SetupEnd = &phases.SetupEnd
	}
	if phases.RunStart > 0 {
		t.RunStart = &phases.RunStart
	}
	if phases.RunEnd > 0 {
		t.RunEnd = &phases.RunEnd
	}
	if phases.UploadStart > 0 {
		t.UploadStart = &phases.UploadStart
	}
	if phases.UploadEnd > 0 {
		t.UploadEnd = &phases.UploadEnd
	}
	if phases.CachePre != nil {
		t.CacheHFBytes = &phases.CachePre.HFBytes
		t.CacheUVBytes = &phases.CachePre.UVBytes
	}
	if phases.CachePost != nil {
		t.CacheHFPostBytes = &phases.CachePost.HFBytes
		t.CacheUVPostBytes = &phases.CachePost.UVBytes
		if phases.CachePost.DiskUsedBytes > 0 {
			t.DiskUsedBytes = &phases.CachePost.DiskUsedBytes
		}
		if phases.CachePost.DiskTotalBytes > 0 {
			t.DiskTotalBytes = &phases.CachePost.DiskTotalBytes
		}
	}
	t.UVSyncSeconds = phases.SetupSeconds

	// Also try to read completion.json for peak metrics
	completionPath := filepath.Join(tmpDir, fmt.Sprintf("%d.completion.json", jobID))
	if cData, err := os.ReadFile(completionPath); err == nil {
		var completion struct {
			PeakRSSKB    int64 `json:"peak_rss_kb"`
			MaxGPUMemMiB int   `json:"max_gpu_mem_mib"`
			OutputUpload *struct {
				FileCount       int   `json:"file_count"`
				Bytes           int64 `json:"bytes"`
				RetryCount      int   `json:"retry_count"`
				DurationMS      int64 `json:"duration_ms"`
				StartedAtUnix   int64 `json:"started_at_unix"`
				CompletedAtUnix int64 `json:"completed_at_unix"`
			} `json:"output_upload"`
			ResultsUpload *struct {
				FileCount       int   `json:"file_count"`
				Bytes           int64 `json:"bytes"`
				RetryCount      int   `json:"retry_count"`
				DurationMS      int64 `json:"duration_ms"`
				StartedAtUnix   int64 `json:"started_at_unix"`
				CompletedAtUnix int64 `json:"completed_at_unix"`
			} `json:"results_upload"`
		}
		if json.Unmarshal(cData, &completion) == nil {
			if completion.MaxGPUMemMiB > 0 {
				t.PeakGPUMemMiB = &completion.MaxGPUMemMiB
			}
			if completion.OutputUpload != nil {
				if completion.OutputUpload.Bytes > 0 {
					t.UploadWorkspaceBytes = &completion.OutputUpload.Bytes
				}
				if completion.OutputUpload.FileCount > 0 {
					t.OutputUploadFiles = &completion.OutputUpload.FileCount
				}
				if completion.OutputUpload.RetryCount > 0 {
					t.OutputUploadRetries = &completion.OutputUpload.RetryCount
				}
				if completion.OutputUpload.DurationMS > 0 {
					t.OutputUploadDuration = &completion.OutputUpload.DurationMS
				}
				if t.UploadStart == nil && completion.OutputUpload.StartedAtUnix > 0 {
					t.UploadStart = &completion.OutputUpload.StartedAtUnix
				}
			}
			if completion.ResultsUpload != nil {
				if completion.ResultsUpload.Bytes > 0 {
					t.UploadResultsBytes = &completion.ResultsUpload.Bytes
				}
				if completion.ResultsUpload.FileCount > 0 {
					t.ResultsUploadFiles = &completion.ResultsUpload.FileCount
				}
				if completion.ResultsUpload.RetryCount > 0 {
					t.ResultsUploadRetries = &completion.ResultsUpload.RetryCount
				}
				if completion.ResultsUpload.DurationMS > 0 {
					t.ResultsUploadDuration = &completion.ResultsUpload.DurationMS
				}
				if t.UploadEnd == nil && completion.ResultsUpload.CompletedAtUnix > 0 {
					t.UploadEnd = &completion.ResultsUpload.CompletedAtUnix
				}
			}
		}
	}

	return t
}

func mergeTelemetryPhaseMetrics(t *db.JobPhaseTimings, path string) {
	if t == nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	samples := db.ParseTelemetrySamplesJSONL(string(data), 0)
	if len(samples) == 0 {
		return
	}
	summary := db.SummarizeTelemetry(samples, 0, nil)
	if summary == nil {
		return
	}
	var peakMem int
	var utilSum float64
	var utilCount int
	var peakUtil int
	for _, sample := range samples {
		for _, gpu := range sample.GPUs {
			if gpu.GPUUtilPct != nil && int(*gpu.GPUUtilPct) > peakUtil {
				peakUtil = int(*gpu.GPUUtilPct)
			}
		}
	}
	for _, gpu := range summary.GPUs {
		if gpu.GPUPeakMemMiB > peakMem {
			peakMem = gpu.GPUPeakMemMiB
		}
		if gpu.GPUMeanUtilPct != nil {
			utilSum += *gpu.GPUMeanUtilPct
			utilCount++
		}
	}
	if peakMem > 0 && t.PeakGPUMemMiB == nil {
		t.PeakGPUMemMiB = &peakMem
	}
	if utilCount > 0 && t.MeanGPUUtil == nil {
		mean := int(utilSum / float64(utilCount))
		t.MeanGPUUtil = &mean
	}
	if peakUtil > 0 && t.PeakGPUUtil == nil {
		t.PeakGPUUtil = &peakUtil
	}
}

// extractLegacyPhaseTimings reads phase_* files and GPU monitor CSV from the results dir
// (legacy bash wrapper format). Returns nil if no phase files are found.
func extractLegacyPhaseTimings(jobID int64, tmpDir string) *db.JobPhaseTimings {
	t := &db.JobPhaseTimings{JobID: jobID}
	found := false

	readInt64 := func(name string) *int64 {
		data, err := os.ReadFile(filepath.Join(tmpDir, name))
		if err != nil {
			return nil
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return nil
		}
		return &v
	}

	readTimestamp := func(name string) *int64 {
		v := readInt64(name)
		if v != nil {
			found = true
		}
		return v
	}

	// Phase timestamps (single-job wrapper names)
	t.WrapperStart = readTimestamp("phase_start")
	t.SetupStart = readTimestamp("phase_setup_start")
	t.SetupEnd = readTimestamp("phase_setup_end")
	t.RunStart = readTimestamp("phase_run_start")
	t.RunEnd = readTimestamp("phase_run_end")
	t.UploadStart = readTimestamp("phase_upload_start")
	t.UploadEnd = readTimestamp("phase_upload_end")

	// Campaign wrapper uses per-job names: phase_run_start_$JOB_ID
	if t.RunStart == nil {
		t.RunStart = readTimestamp(fmt.Sprintf("phase_run_start_%d", jobID))
	}
	if t.RunEnd == nil {
		t.RunEnd = readTimestamp(fmt.Sprintf("phase_run_end_%d", jobID))
	}

	// Upload sizes
	t.UploadResultsBytes = readInt64("upload_results_bytes")
	t.UploadWorkspaceBytes = readInt64("upload_workspace_bytes")

	// Cache state probes
	t.CacheHFBytes = readInt64(fmt.Sprintf("cache_hf_%d", jobID))
	t.CacheUVBytes = readInt64(fmt.Sprintf("cache_uv_%d", jobID))

	// uv sync timing — sum all lines in the file (one per invocation)
	readSummed := func(name string) *int64 {
		return readSummedInt64(filepath.Join(tmpDir, name))
	}
	t.UVSyncSeconds = readSummed(fmt.Sprintf("uv_sync_seconds_%d", jobID))
	if t.UVSyncSeconds == nil {
		t.UVSyncSeconds = readSummed("uv_sync_seconds")
	}

	// Post-job cache sizes
	t.CacheUVPostBytes = readInt64(fmt.Sprintf("cache_uv_post_%d", jobID))
	if t.CacheUVPostBytes == nil {
		t.CacheUVPostBytes = readInt64("cache_uv_post")
	}
	t.CacheHFPostBytes = readInt64(fmt.Sprintf("cache_hf_post_%d", jobID))
	if t.CacheHFPostBytes == nil {
		t.CacheHFPostBytes = readInt64("cache_hf_post")
	}

	// GPU monitor summary — single-job wrapper writes gpu_monitor.csv,
	// campaign wrapper writes gpu_monitor_$JOB_ID.csv
	gpuMonitorPath := filepath.Join(tmpDir, fmt.Sprintf("gpu_monitor_%d.csv", jobID))
	if _, err := os.Stat(gpuMonitorPath); err != nil {
		gpuMonitorPath = filepath.Join(tmpDir, "gpu_monitor.csv")
	}
	gpuPeak, gpuMean, gpuPeakUtil := parseGPUMonitor(gpuMonitorPath)
	if gpuPeak >= 0 {
		v := int(gpuPeak)
		t.PeakGPUMemMiB = &v
	}
	if gpuMean >= 0 {
		v := int(gpuMean)
		t.MeanGPUUtil = &v
	}
	if gpuPeakUtil >= 0 {
		v := int(gpuPeakUtil)
		t.PeakGPUUtil = &v
	}

	if !found {
		return nil
	}
	return t
}

// parseGPUMonitor reads a GPU monitor CSV and returns peak mem (MiB), mean util (%), peak util (%).
// Returns -1 for each value if the file doesn't exist or can't be parsed.
func parseGPUMonitor(path string) (peakMemMiB, meanUtil, peakUtil float64) {
	peakMemMiB, meanUtil, peakUtil = -1, -1, -1

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return
	}

	var totalUtil float64
	var count int
	var maxMem, maxUtil float64

	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		// Fields: utilization.gpu, memory.used (from nvidia-smi --query-gpu)
		util, err1 := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64)
		mem, err2 := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		if err1 != nil || err2 != nil {
			continue
		}
		count++
		totalUtil += util
		if mem > maxMem {
			maxMem = mem
		}
		if util > maxUtil {
			maxUtil = util
		}
	}

	if count > 0 {
		peakMemMiB = maxMem
		meanUtil = totalUtil / float64(count)
		peakUtil = maxUtil
	}
	return
}

// readSummedInt64 reads a file containing one integer per line and returns their sum.
// Returns nil if the file doesn't exist or contains no valid integers.
func readSummedInt64(path string) *int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var sum int64
	found := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	if !found {
		return nil
	}
	return &sum
}

// WriteVastaiLogsToCache writes stdout/stderr from R2 results into the local log cache.
// Supports both agent format ({jobID}.log) and legacy format (stdout.log + stderr.log).
func WriteVastaiLogsToCache(jobID int64, tmpDir string) {
	// Try agent format first: {jobID}.log
	agentLogPath := filepath.Join(tmpDir, fmt.Sprintf("%d.log", jobID))
	if agentLog, err := os.ReadFile(agentLogPath); err == nil && len(agentLog) > 0 {
		_ = logcache.Write(jobID, string(agentLog))
		return
	}

	// Fall back to legacy format: stdout.log + stderr.log
	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")

	var combined strings.Builder

	if stdout, err := os.ReadFile(stdoutPath); err == nil {
		combined.Write(stdout)
	}
	if stderr, err := os.ReadFile(stderrPath); err == nil {
		if combined.Len() > 0 {
			combined.WriteString("\n--- stderr ---\n")
		}
		combined.Write(stderr)
	}

	if combined.Len() > 0 {
		_ = logcache.Write(jobID, combined.String())
	}
}

// checkLaunchLimits destroys instances that exceed budget or time limits.
func (c *Coordinator) checkLaunchLimits(cfg *config.Config) {
	instances, err := db.ListLaunches(c.db)
	if err != nil {
		return
	}

	client := c.vastaiClient()
	if err := client.Available(); err != nil {
		return
	}

	for _, ci := range instances {
		if ci.Status != db.LaunchStatusRunning {
			continue
		}

		// Check time limit
		if ci.MaxTimeSeconds > 0 && ci.LaunchedAt != nil {
			elapsed := time.Since(time.Unix(*ci.LaunchedAt, 0))
			if elapsed > time.Duration(ci.MaxTimeSeconds)*time.Second {
				c.logger.Warn("instance exceeded time limit, destroying", "instance_id", ci.ID, "elapsed", elapsed)
				providerInstID := ci.EffectiveProviderID()
				if providerInstID != "" {
					if cl := c.cloudClient(ci.Provider); cl != nil {
						_ = cl.DestroyInstance(providerInstID)
					}
				}
				detail := fmt.Sprintf("exceeded time limit (%s elapsed, %ds max)", elapsed.Truncate(time.Second), ci.MaxTimeSeconds)
				_ = db.UpdateLaunchStatus(c.db, ci.ID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, detail)
				oplog.Log(oplog.OpLaunchLaunchFailed, oplog.WithDetailf(
					"launch_id=%d reason=%s detail=%s", ci.ID, db.TerminationReasonInfraFailure, detail))
			}
		}
	}
}
