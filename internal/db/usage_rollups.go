package db

import (
	"database/sql"
	"time"
)

// SumGPUHoursSince returns GPU-hours accrued within the window [sinceUnix, now],
// split into cloud-rental and on-prem totals. "Accrued within the window" means
// each instance/job's runtime is clipped to the window, so a 30h instance
// contributes ~24h (× its GPU count) to a 24h window rather than its full life.
//
// Cloud GPU-hours come from the launches table (num_gpus × clipped runtime).
// On-prem GPU-hours come from job_attempts with no launch (launch_id IS NULL),
// so cloud time already counted via launches is not double-counted; the GPU
// count is derived from the CUDA_VISIBLE_DEVICES string on the job.
func SumGPUHoursSince(database *sql.DB, sinceUnix int64) (cloudHrs, onpremHrs float64, err error) {
	now := time.Now().Unix()

	// Cloud: overlap seconds = max(0, min(end|now, now) - max(launched_at, since)).
	cloudQuery := `
		SELECT COALESCE(SUM(
			num_gpus * MAX(0, MIN(COALESCE(ended_at, ?), ?) - MAX(launched_at, ?))
		), 0) / 3600.0
		FROM launches
		WHERE launched_at IS NOT NULL AND num_gpus > 0`
	if err = database.QueryRow(cloudQuery, now, now, sinceUnix).Scan(&cloudHrs); err != nil {
		return 0, 0, err
	}

	// On-prem: same overlap math against job_attempts; GPU count from the
	// CUDA_VISIBLE_DEVICES string ("0,1" → 2) via comma-count + 1. Summing
	// across all non-tombstoned attempts is correct — each attempt consumed GPU
	// time. Attempts with a launch are excluded (counted above via launches).
	onpremQuery := `
		SELECT COALESCE(SUM(
			(LENGTH(j.gpu) - LENGTH(REPLACE(j.gpu, ',', '')) + 1)
			* MAX(0, MIN(COALESCE(ja.end_time, ?), ?) - MAX(ja.start_time, ?))
		), 0) / 3600.0
		FROM job_attempts ja
		JOIN jobs j ON j.id = ja.job_id AND j.tombstoned = 0
		WHERE ja.launch_id IS NULL
		  AND ja.start_time IS NOT NULL
		  AND j.gpu IS NOT NULL AND TRIM(j.gpu) <> ''`
	if err = database.QueryRow(onpremQuery, now, now, sinceUnix).Scan(&onpremHrs); err != nil {
		return 0, 0, err
	}

	return cloudHrs, onpremHrs, nil
}

// SumTransferBytesSince returns HuggingFace download and R2 upload byte totals
// for jobs whose latest attempt completed within the window [sinceUnix, now].
//
// These byte counters are written only by the cloud wrapper (on-prem paths
// leave them NULL), so the result inherently covers cloud jobs only — callers
// should label it as such. HF download is approximated by the per-job cache
// delta (post − pre, clamped at zero); R2 upload sums the results and workspace
// upload sizes. job_phase_timings has one row per job (PK job_id), so windowing
// uses a correlated subquery on job_attempts to avoid fanning out the byte sums.
func SumTransferBytesSince(database *sql.DB, sinceUnix int64) (hfDownBytes, r2UpBytes int64, err error) {
	now := time.Now().Unix()
	query := `
		SELECT
			COALESCE(SUM(MAX(0, COALESCE(jpt.cache_hf_post_bytes, 0) - COALESCE(jpt.cache_hf_bytes, 0))), 0),
			COALESCE(SUM(COALESCE(jpt.upload_results_bytes, 0) + COALESCE(jpt.upload_workspace_bytes, 0)), 0)
		FROM job_phase_timings jpt
		JOIN jobs j ON j.id = jpt.job_id AND j.tombstoned = 0
		WHERE COALESCE(
			(SELECT MAX(COALESCE(ja.end_time, ja.start_time)) FROM job_attempts ja WHERE ja.job_id = j.id),
			?
		) >= ?`
	if err = database.QueryRow(query, now, sinceUnix).Scan(&hfDownBytes, &r2UpBytes); err != nil {
		return 0, 0, err
	}
	return hfDownBytes, r2UpBytes, nil
}
