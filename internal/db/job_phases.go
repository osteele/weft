package db

import (
	"database/sql"
)

// JobPhaseTimings holds per-job phase timing data extracted from cloud wrapper results.
type JobPhaseTimings struct {
	JobID        int64
	WrapperStart *int64 // wrapper script start
	SetupStart   *int64 // setup phase start
	SetupEnd     *int64 // setup phase end
	RunStart     *int64 // job execution start
	RunEnd       *int64 // job execution end
	UploadStart  *int64 // upload phase start
	UploadEnd    *int64 // upload phase end

	// Transfer sizes (bytes)
	UploadResultsBytes    *int64 // size of results dir
	UploadWorkspaceBytes  *int64 // size of workspace dir
	OutputUploadFiles     *int   // files uploaded from convention-based output dirs
	OutputUploadRetries   *int   // total retries for output dir upload
	OutputUploadDuration  *int64 // total output dir upload time (ms)
	ResultsUploadFiles    *int   // files uploaded from the per-job results dir
	ResultsUploadRetries  *int   // retries for per-job results upload
	ResultsUploadDuration *int64 // per-job results upload time (ms)

	// Cache state before job (bytes, for campaign jobs)
	CacheHFBytes *int64 // ~/.cache/huggingface size before job
	CacheUVBytes *int64 // ~/.cache/uv size before job

	// Cache state after job (bytes)
	CacheUVPostBytes *int64 // ~/.cache/uv size after job
	CacheHFPostBytes *int64 // ~/.cache/huggingface size after job

	// uv sync timing
	UVSyncSeconds *int64 // total uv sync time (seconds)

	// Disk usage at job completion (bytes)
	DiskUsedBytes  *int64 // root filesystem used bytes (post-job)
	DiskTotalBytes *int64 // root filesystem total bytes

	// GPU monitoring summary
	PeakGPUMemMiB *int // peak GPU memory used (MiB)
	MeanGPUUtil   *int // mean GPU utilization (%)
	PeakGPUUtil   *int // peak GPU utilization (%)
}

// UpsertJobPhaseTimings inserts or merges phase timing data for a job.
func UpsertJobPhaseTimings(db *sql.DB, t *JobPhaseTimings) error {
	_, err := db.Exec(
		`INSERT INTO job_phase_timings
		 (job_id, wrapper_start, setup_start, setup_end, run_start, run_end,
		  upload_start, upload_end, upload_results_bytes, upload_workspace_bytes,
		  output_upload_files, output_upload_retries, output_upload_duration_ms,
		  results_upload_files, results_upload_retries, results_upload_duration_ms,
		  cache_hf_bytes, cache_uv_bytes, cache_uv_post_bytes, cache_hf_post_bytes,
		  uv_sync_seconds, disk_used_bytes, disk_total_bytes,
		  peak_gpu_mem_mib, mean_gpu_util, peak_gpu_util)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(job_id) DO UPDATE SET
		  wrapper_start = COALESCE(excluded.wrapper_start, job_phase_timings.wrapper_start),
		  setup_start = COALESCE(excluded.setup_start, job_phase_timings.setup_start),
		  setup_end = COALESCE(excluded.setup_end, job_phase_timings.setup_end),
		  run_start = COALESCE(excluded.run_start, job_phase_timings.run_start),
		  run_end = COALESCE(excluded.run_end, job_phase_timings.run_end),
		  upload_start = COALESCE(excluded.upload_start, job_phase_timings.upload_start),
		  upload_end = COALESCE(excluded.upload_end, job_phase_timings.upload_end),
		  upload_results_bytes = COALESCE(excluded.upload_results_bytes, job_phase_timings.upload_results_bytes),
		  upload_workspace_bytes = COALESCE(excluded.upload_workspace_bytes, job_phase_timings.upload_workspace_bytes),
		  output_upload_files = COALESCE(excluded.output_upload_files, job_phase_timings.output_upload_files),
		  output_upload_retries = COALESCE(excluded.output_upload_retries, job_phase_timings.output_upload_retries),
		  output_upload_duration_ms = COALESCE(excluded.output_upload_duration_ms, job_phase_timings.output_upload_duration_ms),
		  results_upload_files = COALESCE(excluded.results_upload_files, job_phase_timings.results_upload_files),
		  results_upload_retries = COALESCE(excluded.results_upload_retries, job_phase_timings.results_upload_retries),
		  results_upload_duration_ms = COALESCE(excluded.results_upload_duration_ms, job_phase_timings.results_upload_duration_ms),
		  cache_hf_bytes = COALESCE(excluded.cache_hf_bytes, job_phase_timings.cache_hf_bytes),
		  cache_uv_bytes = COALESCE(excluded.cache_uv_bytes, job_phase_timings.cache_uv_bytes),
		  cache_uv_post_bytes = COALESCE(excluded.cache_uv_post_bytes, job_phase_timings.cache_uv_post_bytes),
		  cache_hf_post_bytes = COALESCE(excluded.cache_hf_post_bytes, job_phase_timings.cache_hf_post_bytes),
		  uv_sync_seconds = COALESCE(excluded.uv_sync_seconds, job_phase_timings.uv_sync_seconds),
		  disk_used_bytes = COALESCE(excluded.disk_used_bytes, job_phase_timings.disk_used_bytes),
		  disk_total_bytes = COALESCE(excluded.disk_total_bytes, job_phase_timings.disk_total_bytes),
		  peak_gpu_mem_mib = COALESCE(excluded.peak_gpu_mem_mib, job_phase_timings.peak_gpu_mem_mib),
		  mean_gpu_util = COALESCE(excluded.mean_gpu_util, job_phase_timings.mean_gpu_util),
		  peak_gpu_util = COALESCE(excluded.peak_gpu_util, job_phase_timings.peak_gpu_util)`,
		t.JobID, t.WrapperStart, t.SetupStart, t.SetupEnd, t.RunStart, t.RunEnd,
		t.UploadStart, t.UploadEnd, t.UploadResultsBytes, t.UploadWorkspaceBytes,
		t.OutputUploadFiles, t.OutputUploadRetries, t.OutputUploadDuration,
		t.ResultsUploadFiles, t.ResultsUploadRetries, t.ResultsUploadDuration,
		t.CacheHFBytes, t.CacheUVBytes, t.CacheUVPostBytes, t.CacheHFPostBytes,
		t.UVSyncSeconds, t.DiskUsedBytes, t.DiskTotalBytes,
		t.PeakGPUMemMiB, t.MeanGPUUtil, t.PeakGPUUtil,
	)
	return err
}

// GetJobPhaseTimings retrieves phase timing data for a job.
func GetJobPhaseTimings(db *sql.DB, jobID int64) (*JobPhaseTimings, error) {
	row := db.QueryRow(
		`SELECT job_id, wrapper_start, setup_start, setup_end, run_start, run_end,
		        upload_start, upload_end, upload_results_bytes, upload_workspace_bytes,
		        output_upload_files, output_upload_retries, output_upload_duration_ms,
		        results_upload_files, results_upload_retries, results_upload_duration_ms,
		        cache_hf_bytes, cache_uv_bytes, cache_uv_post_bytes, cache_hf_post_bytes,
		        uv_sync_seconds, disk_used_bytes, disk_total_bytes,
		        peak_gpu_mem_mib, mean_gpu_util, peak_gpu_util
		 FROM job_phase_timings WHERE job_id = ?`, jobID,
	)

	var t JobPhaseTimings
	err := row.Scan(
		&t.JobID, &t.WrapperStart, &t.SetupStart, &t.SetupEnd, &t.RunStart, &t.RunEnd,
		&t.UploadStart, &t.UploadEnd, &t.UploadResultsBytes, &t.UploadWorkspaceBytes,
		&t.OutputUploadFiles, &t.OutputUploadRetries, &t.OutputUploadDuration,
		&t.ResultsUploadFiles, &t.ResultsUploadRetries, &t.ResultsUploadDuration,
		&t.CacheHFBytes, &t.CacheUVBytes, &t.CacheUVPostBytes, &t.CacheHFPostBytes,
		&t.UVSyncSeconds, &t.DiskUsedBytes, &t.DiskTotalBytes,
		&t.PeakGPUMemMiB, &t.MeanGPUUtil, &t.PeakGPUUtil,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}
