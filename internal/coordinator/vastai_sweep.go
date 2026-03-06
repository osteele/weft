package coordinator

import (
	"context"
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
)

// loadR2Client creates an R2 client from the app config.
// Returns nil, nil if R2 is not configured.
func (c *Coordinator) loadR2Client() (*r2.Client, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil || cfg.Vastai.R2.Bucket == "" {
		return nil, cfg, nil
	}
	client, err := r2.New(r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	})
	return client, cfg, err
}

// sweepVastaiResults polls R2 for completed Vast.ai job results and processes them.
// It also checks for orphaned instances that should be destroyed.
func (c *Coordinator) sweepVastaiResults() {
	r2Client, cfg, err := c.loadR2Client()
	if err != nil {
		c.logger.Printf("r2 client: %v", err)
		return
	}
	if r2Client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Check for completed results
	completedJobIDs, err := r2Client.ListCompleted(ctx, "jobs/")
	if err != nil {
		c.logger.Printf("r2 list completed: %v", err)
		return
	}

	for _, jobIDStr := range completedJobIDs {
		jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
		if err != nil {
			continue
		}
		c.processCompletedVastaiJob(ctx, r2Client, jobID)
	}

	// 2. Check for orphaned instances
	c.checkOrphanedVastaiInstances(cfg)
}

// processCompletedVastaiJob downloads results from R2, updates the job in the DB,
// writes logs to the log cache, and cleans up the R2 prefix.
func (c *Coordinator) processCompletedVastaiJob(ctx context.Context, r2Client *r2.Client, jobID int64) {
	prefix := fmt.Sprintf("jobs/%d", jobID)

	// Download results to temp dir
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-vastai-%d-*", jobID))
	if err != nil {
		c.logger.Printf("vastai sweep: create temp dir for job %d: %v", jobID, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := r2Client.DownloadResults(ctx, prefix+"/results/", tmpDir); err != nil {
		c.logger.Printf("vastai sweep: download results for job %d: %v", jobID, err)
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
			c.logger.Printf("vastai sweep: read exit_code for job %d: %v", jobID, err)
			return
		}
		exitCode, _ = strconv.Atoi(strings.TrimSpace(string(exitCodeBytes)))

		endTimeBytes, err := os.ReadFile(filepath.Join(tmpDir, "end_time"))
		if err != nil {
			c.logger.Printf("vastai sweep: read end_time for job %d: %v", jobID, err)
			return
		}
		endTimeUnix, _ = strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)

		if exitCode != 0 && failureReason == "" {
			failureReason = detectFailureReason(tmpDir, exitCode)
		}
	}

	// Update job in DB — always StatusCompleted; exit code stored separately
	_, err = c.db.Exec(
		`UPDATE jobs SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ?, failure_reason = ? WHERE id = ?`,
		db.StatusCompleted, exitCode, endTimeUnix, db.StatusCompleted, failureReason, jobID,
	)
	if err != nil {
		c.logger.Printf("vastai sweep: update job %d: %v", jobID, err)
		return
	}

	// Extract and store phase timing data
	if timings := extractPhaseTimings(jobID, tmpDir); timings != nil {
		if err := db.UpsertJobPhaseTimings(c.db, timings); err != nil {
			c.logger.Printf("vastai sweep: store phase timings for job %d: %v", jobID, err)
		}
	}

	// Write logs to log cache
	writeVastaiLogsToCache(jobID, tmpDir)

	// Clean up R2 prefix
	if err := r2Client.DeletePrefix(ctx, prefix+"/"); err != nil {
		c.logger.Printf("vastai sweep: cleanup R2 for job %d: %v", jobID, err)
	}

	c.logger.Printf("vastai sweep: processed job %d (exit=%d, status=%s)", jobID, exitCode, db.StatusCompleted)
	if exitCode == 0 {
		oplog.LogJob(oplog.OpJobCompleted, jobID, "", oplog.WithDetailf("vastai exit=%d", exitCode))
	} else {
		oplog.LogJob(oplog.OpJobFailed, jobID, "", oplog.WithDetailf("vastai exit=%d reason=%s", exitCode, failureReason))
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

// extractPhaseTimings reads phase timing data from the results dir.
// It first tries the structured phases.json format (agent-based wrapper),
// then falls back to individual phase_* files (legacy bash wrapper).
// Returns nil if no phase files are found (non-instrumented wrapper).
func extractPhaseTimings(jobID int64, tmpDir string) *db.JobPhaseTimings {
	// Try structured phases.json first (from agent-based wrapper)
	if timings := extractStructuredPhaseTimings(jobID, tmpDir); timings != nil {
		return timings
	}
	return extractLegacyPhaseTimings(jobID, tmpDir)
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
		CachePre     *struct {
			HFBytes int64 `json:"hf_bytes"`
			UVBytes int64 `json:"uv_bytes"`
		} `json:"cache_pre"`
		CachePost *struct {
			HFBytes int64 `json:"hf_bytes"`
			UVBytes int64 `json:"uv_bytes"`
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
	if phases.CachePre != nil {
		t.CacheHFBytes = &phases.CachePre.HFBytes
		t.CacheUVBytes = &phases.CachePre.UVBytes
	}
	if phases.CachePost != nil {
		t.CacheHFPostBytes = &phases.CachePost.HFBytes
		t.CacheUVPostBytes = &phases.CachePost.UVBytes
	}
	t.UVSyncSeconds = phases.SetupSeconds

	// Also try to read completion.json for peak metrics
	completionPath := filepath.Join(tmpDir, fmt.Sprintf("%d.completion.json", jobID))
	if cData, err := os.ReadFile(completionPath); err == nil {
		var completion struct {
			PeakRSSKB    int64 `json:"peak_rss_kb"`
			MaxGPUMemMiB int   `json:"max_gpu_mem_mib"`
		}
		if json.Unmarshal(cData, &completion) == nil {
			if completion.MaxGPUMemMiB > 0 {
				t.PeakGPUMemMiB = &completion.MaxGPUMemMiB
			}
		}
	}

	return t
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

// writeVastaiLogsToCache writes stdout/stderr from R2 results into the local log cache.
// Supports both agent format ({jobID}.log) and legacy format (stdout.log + stderr.log).
func writeVastaiLogsToCache(jobID int64, tmpDir string) {
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

// sweepCampaignResults polls R2 for completed campaign markers and processes results.
// "campaign" in R2 paths refers to the wrapper script running on a single cloud instance.
func (c *Coordinator) sweepCampaignResults() {
	r2Client, cfg, err := c.loadR2Client()
	if err != nil {
		c.logger.Printf("instance sweep: r2 client: %v", err)
		return
	}
	if r2Client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check for completed cloud instances (the R2 path uses "campaigns/" for legacy reasons)
	completedIDs, err := r2Client.ListCompleted(ctx, "campaigns/")
	if err != nil {
		c.logger.Printf("instance sweep: list completed: %v", err)
		return
	}

	for _, idStr := range completedIDs {
		instanceID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}

		ci, err := db.GetCloudInstance(c.db, instanceID)
		if err != nil || ci == nil {
			continue
		}
		if ci.Status != db.CloudInstanceStatusRunning {
			continue
		}

		// Process each job in the cloud instance
		jobs, err := db.GetCloudInstanceJobs(c.db, instanceID)
		if err != nil {
			c.logger.Printf("instance sweep: get jobs for instance %d: %v", instanceID, err)
			continue
		}

		allSucceeded := true
		for _, job := range jobs {
			if job.Status == db.StatusCompleted {
				continue // already processed
			}
			c.processCompletedVastaiJob(ctx, r2Client, job.ID)

			// Re-read to check exit code
			updated, err := db.GetJobByID(c.db, job.ID)
			if err == nil && updated != nil && updated.ExitCode != nil && *updated.ExitCode != 0 {
				allSucceeded = false
			}
		}

		// Update cloud instance status
		status := db.CloudInstanceStatusCompleted
		if !allSucceeded {
			status = db.CloudInstanceStatusFailed
		}
		if err := db.UpdateCloudInstanceStatus(c.db, instanceID, status); err != nil {
			c.logger.Printf("instance sweep: update instance %d status: %v", instanceID, err)
		}

		// Clean up R2 marker
		prefix := fmt.Sprintf("campaigns/%d/", instanceID)
		if err := r2Client.DeletePrefix(ctx, prefix); err != nil {
			c.logger.Printf("instance sweep: cleanup R2 for instance %d: %v", instanceID, err)
		}

		c.logger.Printf("instance sweep: processed instance %d (status=%s, jobs=%d)", instanceID, status, len(jobs))
	}

	// Enforce budget/time limits on running cloud instances
	c.checkCloudInstanceLimits(cfg)
}

// checkCloudInstanceLimits destroys instances that exceed budget or time limits.
func (c *Coordinator) checkCloudInstanceLimits(cfg *config.Config) {
	instances, err := db.ListCloudInstances(c.db)
	if err != nil {
		return
	}

	client := c.vastaiClient()
	if err := client.Available(); err != nil {
		return
	}

	for _, ci := range instances {
		if ci.Status != db.CloudInstanceStatusRunning {
			continue
		}

		// Check time limit
		if ci.MaxTimeSeconds > 0 && ci.LaunchedAt != nil {
			elapsed := time.Since(time.Unix(*ci.LaunchedAt, 0))
			if elapsed > time.Duration(ci.MaxTimeSeconds)*time.Second {
				c.logger.Printf("instance sweep: instance %d exceeded time limit (%v), destroying", ci.ID, elapsed)
				providerInstID := ci.EffectiveProviderID()
				if providerInstID != "" {
					if cl := c.cloudClient(ci.Provider); cl != nil {
						_ = cl.DestroyInstance(providerInstID)
					}
				}
				_ = db.UpdateCloudInstanceStatus(c.db, ci.ID, db.CloudInstanceStatusFailed)
			}
		}
	}
}

// checkOrphanedVastaiInstances looks for jobs that have been running too long
// or have instances that are no longer active.
func (c *Coordinator) checkOrphanedVastaiInstances(cfg *config.Config) {
	jobs, err := db.ListActiveVastaiJobs(c.db)
	if err != nil {
		c.logger.Printf("vastai sweep: list active jobs: %v", err)
		return
	}

	maxRuntime := 4 * time.Hour
	if cfg.Vastai.MaxRuntime != "" {
		if d, err := time.ParseDuration(cfg.Vastai.MaxRuntime); err == nil {
			maxRuntime = d
		}
	}

	client := c.vastaiClient()
	if err := client.Available(); err != nil {
		return // vastai CLI not available on coordinator
	}

	for _, job := range jobs {
		if job.VastaiInstanceID == nil {
			continue
		}

		age := time.Since(time.Unix(job.CreatedAt, 0))
		if age < maxRuntime {
			continue
		}

		// Job exceeded max runtime — check instance status
		instanceID := *job.VastaiInstanceID
		inst, err := client.ShowInstance(instanceID)
		if err != nil {
			c.logger.Printf("vastai sweep: instance %d not found for job %d, marking failed", instanceID, job.ID)
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "orphaned", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as orphaned: %v", job.ID, err)
			}
			continue
		}

		if inst.Status == "running" {
			c.logger.Printf("vastai sweep: destroying orphaned instance %d (job %d, age %v)", instanceID, job.ID, age)
			if err := client.DestroyInstance(instanceID); err != nil {
				c.logger.Printf("vastai sweep: failed to destroy instance %d: %v", instanceID, err)
			}
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "timeout", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as timed out: %v", job.ID, err)
			}
			oplog.LogJob(oplog.OpJobKill, job.ID, "", oplog.WithDetailf("vastai orphan timeout instance=%d", instanceID))
		} else if inst.Status == "exited" || inst.Status == "error" {
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "instance_exited", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as instance_exited: %v", job.ID, err)
			}
		}
	}
}
