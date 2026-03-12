package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// TimeseriesSample represents a single telemetry sample for a job.
type TimeseriesSample struct {
	JobRunID       *int64 `json:"-"`
	Ts             int64  `json:"ts"`
	CPUPct         int    `json:"cpu_pct"`
	RSSKB          int64  `json:"rss_kb"`
	GPUMiB         int    `json:"gpu_mib,omitempty"`
	DiskFreeBytes  int64  `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes int64  `json:"disk_total_bytes,omitempty"`
	HostRSSKB      int64  `json:"host_rss_kb,omitempty"`
	HostMemTotalKB int64  `json:"host_mem_total_kb,omitempty"`
	GPUUtilPct     int    `json:"gpu_util_pct,omitempty"`
	GPUMemUsedMiB  int    `json:"gpu_mem_used_mib,omitempty"`
	GPUMemTotalMiB int    `json:"gpu_mem_total_mib,omitempty"`
	GPUTempC       int    `json:"gpu_temp_c,omitempty"`
	GPUClockMHz    int    `json:"gpu_clock_mhz,omitempty"`
	Tenant         string `json:"tenant,omitempty"`
}

// InsertTimeseries bulk-inserts time series samples for a job.
// Existing samples (same job_id + ts) are silently skipped.
func InsertTimeseries(database *sql.DB, jobID int64, samples []TimeseriesSample) error {
	if len(samples) == 0 {
		return nil
	}
	runID, err := latestRunIDForJob(database, jobID)
	if err != nil {
		return fmt.Errorf("resolve latest run: %w", err)
	}

	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Build batch insert with OR IGNORE for idempotency
	const batchSize = 100
	for i := 0; i < len(samples); i += batchSize {
		end := i + batchSize
		if end > len(samples) {
			end = len(samples)
		}
		batch := samples[i:end]

		var placeholders []string
		var args []any
		for _, s := range batch {
			s.JobRunID = runID
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, jobID, s.Ts, s.CPUPct, s.RSSKB, s.GPUMiB,
				s.DiskFreeBytes, s.DiskTotalBytes, s.HostRSSKB, s.HostMemTotalKB,
				s.GPUUtilPct, s.GPUMemUsedMiB, s.GPUMemTotalMiB, s.GPUTempC, s.GPUClockMHz, s.Tenant, s.JobRunID)
		}

		query := `INSERT OR IGNORE INTO job_timeseries
			(job_id, ts, cpu_pct, rss_kb, gpu_mib, disk_free_bytes, disk_total_bytes, host_rss_kb, host_mem_total_kb, gpu_util_pct, gpu_mem_used_mib, gpu_mem_total_mib, gpu_temp_c, gpu_clock_mhz, tenant, job_run_id)
			VALUES ` + strings.Join(placeholders, ", ")

		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("insert timeseries batch: %w", err)
		}
	}

	return tx.Commit()
}

// GetTimeseries reads all time series samples for a job, ordered by timestamp.
func GetTimeseries(database *sql.DB, jobID int64) ([]TimeseriesSample, error) {
	return getTimeseriesWhere(database, `WHERE job_id = ? ORDER BY ts`, jobID)
}

// GetTimeseriesByRun reads all time series samples for a specific execution
// attempt, ordered by timestamp.
func GetTimeseriesByRun(database *sql.DB, runID int64) ([]TimeseriesSample, error) {
	return getTimeseriesWhere(database, `WHERE job_run_id = ? ORDER BY ts`, runID)
}

func getTimeseriesWhere(database *sql.DB, where string, args ...any) ([]TimeseriesSample, error) {
	rows, err := database.Query(`
		SELECT job_run_id, ts, cpu_pct, rss_kb, gpu_mib, COALESCE(disk_free_bytes, 0), COALESCE(disk_total_bytes, 0), host_rss_kb, host_mem_total_kb,
		       gpu_util_pct, gpu_mem_used_mib, gpu_mem_total_mib,
		       COALESCE(gpu_temp_c, 0), COALESCE(gpu_clock_mhz, 0), tenant
		FROM job_timeseries
		`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("query timeseries: %w", err)
	}
	defer rows.Close()

	var samples []TimeseriesSample
	for rows.Next() {
		var s TimeseriesSample
		var jobRunID sql.NullInt64
		var tenant sql.NullString
		if err := rows.Scan(&jobRunID, &s.Ts, &s.CPUPct, &s.RSSKB, &s.GPUMiB,
			&s.DiskFreeBytes, &s.DiskTotalBytes, &s.HostRSSKB, &s.HostMemTotalKB, &s.GPUUtilPct, &s.GPUMemUsedMiB, &s.GPUMemTotalMiB,
			&s.GPUTempC, &s.GPUClockMHz, &tenant); err != nil {
			return nil, fmt.Errorf("scan timeseries row: %w", err)
		}
		if jobRunID.Valid {
			s.JobRunID = &jobRunID.Int64
		}
		if tenant.Valid {
			s.Tenant = tenant.String
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// GetTimeseriesLastTS returns the latest timestamp for a job's timeseries data.
// Returns 0 if no samples exist.
func GetTimeseriesLastTS(database *sql.DB, jobID int64) (int64, error) {
	var ts sql.NullInt64
	runID, err := latestRunIDForJob(database, jobID)
	if err != nil {
		return 0, fmt.Errorf("resolve latest run: %w", err)
	}
	query := `SELECT MAX(ts) FROM job_timeseries WHERE job_id = ?`
	args := []any{jobID}
	if runID != nil {
		query += ` AND job_run_id = ?`
		args = append(args, *runID)
	}
	err = database.QueryRow(query, args...).Scan(&ts)
	if err != nil {
		return 0, fmt.Errorf("query max ts: %w", err)
	}
	if ts.Valid {
		return ts.Int64, nil
	}
	return 0, nil
}
