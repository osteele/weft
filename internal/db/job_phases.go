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
	UploadResultsBytes   *int64 // size of results dir
	UploadWorkspaceBytes *int64 // size of workspace dir

	// Cache state before job (bytes, for campaign jobs)
	CacheHFBytes *int64 // ~/.cache/huggingface size before job
	CacheUVBytes *int64 // ~/.cache/uv size before job

	// Cache state after job (bytes)
	CacheUVPostBytes *int64 // ~/.cache/uv size after job
	CacheHFPostBytes *int64 // ~/.cache/huggingface size after job

	// uv sync timing
	UVSyncSeconds *int64 // total uv sync time (seconds)

	// GPU monitoring summary
	PeakGPUMemMiB *int // peak GPU memory used (MiB)
	MeanGPUUtil   *int // mean GPU utilization (%)
	PeakGPUUtil   *int // peak GPU utilization (%)
}

// UpsertJobPhaseTimings inserts or replaces phase timing data for a job.
func UpsertJobPhaseTimings(db *sql.DB, t *JobPhaseTimings) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO job_phase_timings
		 (job_id, wrapper_start, setup_start, setup_end, run_start, run_end,
		  upload_start, upload_end, upload_results_bytes, upload_workspace_bytes,
		  cache_hf_bytes, cache_uv_bytes, cache_uv_post_bytes, cache_hf_post_bytes,
		  uv_sync_seconds, peak_gpu_mem_mib, mean_gpu_util, peak_gpu_util)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.JobID, t.WrapperStart, t.SetupStart, t.SetupEnd, t.RunStart, t.RunEnd,
		t.UploadStart, t.UploadEnd, t.UploadResultsBytes, t.UploadWorkspaceBytes,
		t.CacheHFBytes, t.CacheUVBytes, t.CacheUVPostBytes, t.CacheHFPostBytes,
		t.UVSyncSeconds, t.PeakGPUMemMiB, t.MeanGPUUtil, t.PeakGPUUtil,
	)
	return err
}

// GetJobPhaseTimings retrieves phase timing data for a job.
func GetJobPhaseTimings(db *sql.DB, jobID int64) (*JobPhaseTimings, error) {
	row := db.QueryRow(
		`SELECT job_id, wrapper_start, setup_start, setup_end, run_start, run_end,
		        upload_start, upload_end, upload_results_bytes, upload_workspace_bytes,
		        cache_hf_bytes, cache_uv_bytes, cache_uv_post_bytes, cache_hf_post_bytes,
		        uv_sync_seconds, peak_gpu_mem_mib, mean_gpu_util, peak_gpu_util
		 FROM job_phase_timings WHERE job_id = ?`, jobID,
	)

	var t JobPhaseTimings
	err := row.Scan(
		&t.JobID, &t.WrapperStart, &t.SetupStart, &t.SetupEnd, &t.RunStart, &t.RunEnd,
		&t.UploadStart, &t.UploadEnd, &t.UploadResultsBytes, &t.UploadWorkspaceBytes,
		&t.CacheHFBytes, &t.CacheUVBytes, &t.CacheUVPostBytes, &t.CacheHFPostBytes,
		&t.UVSyncSeconds, &t.PeakGPUMemMiB, &t.MeanGPUUtil, &t.PeakGPUUtil,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}
