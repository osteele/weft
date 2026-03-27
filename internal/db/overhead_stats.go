package db

import (
	"database/sql"
)

// OverheadObservation holds one completed cloud instance's phase durations and covariates.
type OverheadObservation struct {
	// Instance metadata (covariates)
	Provider     string
	GPUClass     string
	DataCenter   string
	DLPerf       float64
	InetDownMbps float64
	InetUpMbps   float64
	Reliability  float64

	// Phase durations (seconds, nil if timestamps unavailable)
	StartupSec  *float64 // ready_at - created_at
	SSHSetupSec *float64 // wrapper_start - ready_at
	JobSetupSec *float64 // setup_end - setup_start
	UploadSec   *float64 // upload_end - upload_start

	// Job-level covariates
	CacheHFBytes     *int64 // pre-job HF cache size (nil/0 = cold)
	CacheHFPostBytes *int64 // post-job HF cache size
	UVSyncSeconds    *int64

	// Upload size covariates
	UploadResultsBytes   *int64
	UploadWorkspaceBytes *int64
}

// QueryOverheadObservations returns phase-timing observations for all completed
// cloud instances that have associated job phase data. Each row joins a
// cloud_instances record with its first job's phase timings.
func QueryOverheadObservations(database *sql.DB) ([]OverheadObservation, error) {
	query := `
		SELECT
			ci.provider,
			ci.gpu_class,
			ci.data_center,
			ci.dl_perf,
			ci.inet_down_mbps,
			ci.inet_up_mbps,
			ci.reliability,
			-- Phase durations (seconds)
			CASE WHEN ci.ready_at IS NOT NULL
				THEN CAST(ci.ready_at - ci.created_at AS REAL)
				ELSE NULL END,
			CASE WHEN ci.ready_at IS NOT NULL AND jpt.wrapper_start IS NOT NULL
				THEN CAST(jpt.wrapper_start - ci.ready_at AS REAL)
				ELSE NULL END,
			CASE WHEN jpt.setup_start IS NOT NULL AND jpt.setup_end IS NOT NULL
				THEN CAST(jpt.setup_end - jpt.setup_start AS REAL)
				ELSE NULL END,
			CASE WHEN jpt.upload_start IS NOT NULL AND jpt.upload_end IS NOT NULL
				THEN CAST(jpt.upload_end - jpt.upload_start AS REAL)
				ELSE NULL END,
			-- Covariates
			jpt.cache_hf_bytes,
			jpt.cache_hf_post_bytes,
			jpt.uv_sync_seconds,
			jpt.upload_results_bytes,
			jpt.upload_workspace_bytes
		FROM cloud_instances ci
		JOIN job_attempts ja ON ja.cloud_instance_id = ci.id
		JOIN jobs j ON j.id = ja.job_id AND j.tombstoned = 0
		JOIN job_phase_timings jpt ON jpt.job_id = j.id
		WHERE ci.status = 'completed'
		GROUP BY ci.id
		ORDER BY ci.created_at ASC
	`
	rows, err := database.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var obs []OverheadObservation
	for rows.Next() {
		var o OverheadObservation
		var provider, gpuClass, dataCenter sql.NullString
		var dlPerf, inetDown, inetUp, reliability sql.NullFloat64
		var startupSec, sshSetupSec, jobSetupSec, uploadSec sql.NullFloat64
		var cacheHF, cacheHFPost, uvSync, uploadResults, uploadWorkspace sql.NullInt64

		if err := rows.Scan(
			&provider, &gpuClass, &dataCenter,
			&dlPerf, &inetDown, &inetUp, &reliability,
			&startupSec, &sshSetupSec, &jobSetupSec, &uploadSec,
			&cacheHF, &cacheHFPost, &uvSync, &uploadResults, &uploadWorkspace,
		); err != nil {
			return nil, err
		}

		o.Provider = provider.String
		o.GPUClass = gpuClass.String
		o.DataCenter = dataCenter.String
		o.DLPerf = dlPerf.Float64
		o.InetDownMbps = inetDown.Float64
		o.InetUpMbps = inetUp.Float64
		o.Reliability = reliability.Float64

		o.StartupSec = nullFloat(startupSec)
		o.SSHSetupSec = nullFloat(sshSetupSec)
		o.JobSetupSec = nullFloat(jobSetupSec)
		o.UploadSec = nullFloat(uploadSec)
		o.CacheHFBytes = nullInt(cacheHF)
		o.CacheHFPostBytes = nullInt(cacheHFPost)
		o.UVSyncSeconds = nullInt(uvSync)
		o.UploadResultsBytes = nullInt(uploadResults)
		o.UploadWorkspaceBytes = nullInt(uploadWorkspace)

		obs = append(obs, o)
	}
	return obs, rows.Err()
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
