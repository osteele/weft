package cloudsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
)

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
