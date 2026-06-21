package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
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

const TimeseriesRawKind = "timeseries"

// TimeseriesSummary holds derived values for one job attempt. It is small
// enough to keep in SQLite even when raw samples live in R2/cache.
type TimeseriesSummary struct {
	JobID              int64   `json:"job_id"`
	AttemptID          int64   `json:"attempt_id"`
	SampleCount        int     `json:"sample_count"`
	TSMin              int64   `json:"ts_min"`
	TSMax              int64   `json:"ts_max"`
	PeakDiskUsedBytes  int64   `json:"peak_disk_used_bytes"`
	LastDiskTotalBytes int64   `json:"last_disk_total_bytes"`
	PeakRSSKB          int64   `json:"peak_rss_kb"`
	PeakGPUMemMiB      int     `json:"peak_gpu_mem_mib"`
	PeakGPUUtilPct     int     `json:"peak_gpu_util_pct"`
	PeakGPUTempC       int     `json:"peak_gpu_temp_c"`
	MeanGPUUtilPct     float64 `json:"mean_gpu_util_pct"`
	MeanGPUTempC       float64 `json:"mean_gpu_temp_c"`
	Tenant             string  `json:"tenant,omitempty"`
}

// TimeseriesRawObject records the durable R2 object for raw per-sample data.
type TimeseriesRawObject struct {
	JobID       int64
	AttemptID   int64
	Kind        string
	R2Key       string
	SizeBytes   int64
	ETag        string
	ConfirmedAt int64
	CreatedAt   int64
}

// ParseTimeseriesJSONL parses raw JSONL samples, skipping malformed lines and
// samples at or before minTS. Tenant, when non-empty, overrides file content.
func ParseTimeseriesJSONL(data string, minTS int64, tenant string) []TimeseriesSample {
	var samples []TimeseriesSample
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s TimeseriesSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		if s.Ts <= minTS {
			continue
		}
		if tenant != "" {
			s.Tenant = tenant
		}
		samples = append(samples, s)
	}
	return samples
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
	return insertTimeseriesForRun(database, jobID, runID, samples)
}

// InsertTimeseriesForRun bulk-inserts time series samples for a specific
// execution attempt. Existing samples (same job_id + ts) are silently skipped.
func InsertTimeseriesForRun(database *sql.DB, jobID, runID int64, samples []TimeseriesSample) error {
	if len(samples) == 0 {
		return nil
	}
	return insertTimeseriesForRun(database, jobID, &runID, samples)
}

func insertTimeseriesForRun(database *sql.DB, jobID int64, runID *int64, samples []TimeseriesSample) error {
	return RetryOnDatabaseLocked(context.Background(), "insert timeseries", func() error {
		return insertTimeseriesForRunOnce(database, jobID, runID, samples)
	})
}

func insertTimeseriesForRunOnce(database *sql.DB, jobID int64, runID *int64, samples []TimeseriesSample) error {
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
			(job_id, ts, cpu_pct, rss_kb, gpu_mib, disk_free_bytes, disk_total_bytes, host_rss_kb, host_mem_total_kb, gpu_util_pct, gpu_mem_used_mib, gpu_mem_total_mib, gpu_temp_c, gpu_clock_mhz, tenant, attempt_id)
			VALUES ` + strings.Join(placeholders, ", ")

		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("insert timeseries batch: %w", err)
		}
	}

	return tx.Commit()
}

// SummarizeTimeseries computes the small retained summary for raw samples.
func SummarizeTimeseries(jobID, attemptID int64, samples []TimeseriesSample) *TimeseriesSummary {
	if len(samples) == 0 {
		return nil
	}
	summary := &TimeseriesSummary{
		JobID:     jobID,
		AttemptID: attemptID,
		TSMin:     math.MaxInt64,
	}
	var utilSum, tempSum float64
	var utilCount, tempCount int
	for _, s := range samples {
		summary.SampleCount++
		if s.Ts < summary.TSMin {
			summary.TSMin = s.Ts
		}
		if s.Ts > summary.TSMax {
			summary.TSMax = s.Ts
			summary.LastDiskTotalBytes = s.DiskTotalBytes
		}
		if s.DiskTotalBytes > 0 && s.DiskFreeBytes >= 0 && s.DiskTotalBytes >= s.DiskFreeBytes {
			used := s.DiskTotalBytes - s.DiskFreeBytes
			if used > summary.PeakDiskUsedBytes {
				summary.PeakDiskUsedBytes = used
			}
		}
		if s.RSSKB > summary.PeakRSSKB {
			summary.PeakRSSKB = s.RSSKB
		}
		if s.GPUMemUsedMiB > summary.PeakGPUMemMiB {
			summary.PeakGPUMemMiB = s.GPUMemUsedMiB
		}
		if s.GPUUtilPct > summary.PeakGPUUtilPct {
			summary.PeakGPUUtilPct = s.GPUUtilPct
		}
		if s.GPUTempC > summary.PeakGPUTempC {
			summary.PeakGPUTempC = s.GPUTempC
		}
		if s.GPUUtilPct > 0 {
			utilSum += float64(s.GPUUtilPct)
			utilCount++
		}
		if s.GPUTempC > 0 {
			tempSum += float64(s.GPUTempC)
			tempCount++
		}
		if summary.Tenant == "" && s.Tenant != "" {
			summary.Tenant = s.Tenant
		}
	}
	if utilCount > 0 {
		summary.MeanGPUUtilPct = math.Round(utilSum/float64(utilCount)*10) / 10
	}
	if tempCount > 0 {
		summary.MeanGPUTempC = math.Round(tempSum/float64(tempCount)*10) / 10
	}
	if summary.TSMin == math.MaxInt64 {
		summary.TSMin = 0
	}
	return summary
}

// UpsertTimeseriesSummary stores the retained summary for one attempt.
func UpsertTimeseriesSummary(database *sql.DB, summary *TimeseriesSummary) error {
	if summary == nil || summary.AttemptID == 0 {
		return nil
	}
	_, err := database.Exec(`
		INSERT INTO job_timeseries_summaries (
			job_id, attempt_id, sample_count, ts_min, ts_max,
			peak_disk_used_bytes, last_disk_total_bytes, peak_rss_kb,
			peak_gpu_mem_mib, peak_gpu_util_pct, peak_gpu_temp_c,
			mean_gpu_util_pct, mean_gpu_temp_c, tenant, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET
			job_id = excluded.job_id,
			sample_count = excluded.sample_count,
			ts_min = excluded.ts_min,
			ts_max = excluded.ts_max,
			peak_disk_used_bytes = excluded.peak_disk_used_bytes,
			last_disk_total_bytes = excluded.last_disk_total_bytes,
			peak_rss_kb = excluded.peak_rss_kb,
			peak_gpu_mem_mib = excluded.peak_gpu_mem_mib,
			peak_gpu_util_pct = excluded.peak_gpu_util_pct,
			peak_gpu_temp_c = excluded.peak_gpu_temp_c,
			mean_gpu_util_pct = excluded.mean_gpu_util_pct,
			mean_gpu_temp_c = excluded.mean_gpu_temp_c,
			tenant = excluded.tenant,
			updated_at = excluded.updated_at`,
		summary.JobID, summary.AttemptID, summary.SampleCount, summary.TSMin, summary.TSMax,
		summary.PeakDiskUsedBytes, summary.LastDiskTotalBytes, summary.PeakRSSKB,
		summary.PeakGPUMemMiB, summary.PeakGPUUtilPct, summary.PeakGPUTempC,
		summary.MeanGPUUtilPct, summary.MeanGPUTempC, nullString(summary.Tenant), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert timeseries summary: %w", err)
	}
	return nil
}

// GetTimeseriesSummaryByRun returns the retained summary for one attempt.
func GetTimeseriesSummaryByRun(database *sql.DB, runID int64) (*TimeseriesSummary, error) {
	row := database.QueryRow(`
		SELECT job_id, attempt_id, sample_count, ts_min, ts_max,
		       peak_disk_used_bytes, last_disk_total_bytes, peak_rss_kb,
		       peak_gpu_mem_mib, peak_gpu_util_pct, peak_gpu_temp_c,
		       mean_gpu_util_pct, mean_gpu_temp_c, tenant
		  FROM job_timeseries_summaries
		 WHERE attempt_id = ?`, runID)
	var s TimeseriesSummary
	var tenant sql.NullString
	if err := row.Scan(&s.JobID, &s.AttemptID, &s.SampleCount, &s.TSMin, &s.TSMax,
		&s.PeakDiskUsedBytes, &s.LastDiskTotalBytes, &s.PeakRSSKB,
		&s.PeakGPUMemMiB, &s.PeakGPUUtilPct, &s.PeakGPUTempC,
		&s.MeanGPUUtilPct, &s.MeanGPUTempC, &tenant); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get timeseries summary: %w", err)
	}
	if tenant.Valid {
		s.Tenant = tenant.String
	}
	return &s, nil
}

// RefreshTimeseriesSummaryFromRows recomputes the summary from raw rows still
// present in SQLite. It is useful while an attempt is running or before raw
// rows are pruned after durable object confirmation.
func RefreshTimeseriesSummaryFromRows(database *sql.DB, jobID, runID int64) error {
	samples, err := GetTimeseriesByRun(database, runID)
	if err != nil {
		return err
	}
	return UpsertTimeseriesSummary(database, SummarizeTimeseries(jobID, runID, samples))
}

// UpsertTimeseriesRawObject records a confirmed durable raw object.
func UpsertTimeseriesRawObject(database *sql.DB, obj TimeseriesRawObject) error {
	if obj.Kind == "" {
		obj.Kind = TimeseriesRawKind
	}
	now := time.Now().Unix()
	if obj.ConfirmedAt == 0 {
		obj.ConfirmedAt = now
	}
	if obj.CreatedAt == 0 {
		obj.CreatedAt = now
	}
	_, err := database.Exec(`
		INSERT INTO job_timeseries_raw_objects (
			job_id, attempt_id, kind, r2_key, size_bytes, etag, confirmed_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id, kind) DO UPDATE SET
			job_id = excluded.job_id,
			r2_key = excluded.r2_key,
			size_bytes = excluded.size_bytes,
			etag = excluded.etag,
			confirmed_at = excluded.confirmed_at`,
		obj.JobID, obj.AttemptID, obj.Kind, obj.R2Key, obj.SizeBytes, nullString(obj.ETag), obj.ConfirmedAt, obj.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert timeseries raw object: %w", err)
	}
	return nil
}

// GetTimeseriesRawObject returns durable raw-object metadata for one attempt.
func GetTimeseriesRawObject(database *sql.DB, runID int64, kind string) (*TimeseriesRawObject, error) {
	if kind == "" {
		kind = TimeseriesRawKind
	}
	row := database.QueryRow(`
		SELECT job_id, attempt_id, kind, r2_key, size_bytes, etag, confirmed_at, created_at
		  FROM job_timeseries_raw_objects
		 WHERE attempt_id = ? AND kind = ?`, runID, kind)
	var obj TimeseriesRawObject
	var etag sql.NullString
	if err := row.Scan(&obj.JobID, &obj.AttemptID, &obj.Kind, &obj.R2Key, &obj.SizeBytes, &etag, &obj.ConfirmedAt, &obj.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get timeseries raw object: %w", err)
	}
	if etag.Valid {
		obj.ETag = etag.String
	}
	return &obj, nil
}

// DeleteTimeseriesByRun removes raw SQLite samples after durable R2 retention
// has been confirmed. Summaries and raw-object metadata remain.
func DeleteTimeseriesByRun(database *sql.DB, runID int64) error {
	_, err := database.Exec(`DELETE FROM job_timeseries WHERE attempt_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("delete timeseries raw rows: %w", err)
	}
	return nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// GetTimeseries reads all time series samples for a job, ordered by timestamp.
func GetTimeseries(database *sql.DB, jobID int64) ([]TimeseriesSample, error) {
	return getTimeseriesWhere(database, `WHERE job_id = ? ORDER BY ts`, jobID)
}

// GetTimeseriesByRun reads all time series samples for a specific execution
// attempt, ordered by timestamp.
func GetTimeseriesByRun(database *sql.DB, runID int64) ([]TimeseriesSample, error) {
	return getTimeseriesWhere(database, `WHERE attempt_id = ? ORDER BY ts`, runID)
}

func getTimeseriesWhere(database *sql.DB, where string, args ...any) ([]TimeseriesSample, error) {
	rows, err := database.Query(`
		SELECT attempt_id, ts, cpu_pct, rss_kb, gpu_mib, COALESCE(disk_free_bytes, 0), COALESCE(disk_total_bytes, 0), host_rss_kb, host_mem_total_kb,
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
	runID, err := latestRunIDForJob(database, jobID)
	if err != nil {
		return 0, fmt.Errorf("resolve latest run: %w", err)
	}
	return getTimeseriesLastTS(database, jobID, runID)
}

// GetTimeseriesLastTSForRun returns the latest timestamp for one execution
// attempt. Returns 0 if no samples exist.
func GetTimeseriesLastTSForRun(database *sql.DB, jobID, runID int64) (int64, error) {
	return getTimeseriesLastTS(database, jobID, &runID)
}

func getTimeseriesLastTS(database *sql.DB, jobID int64, runID *int64) (int64, error) {
	var ts sql.NullInt64
	query := `SELECT MAX(ts) FROM job_timeseries WHERE job_id = ?`
	args := []any{jobID}
	if runID != nil {
		query += ` AND attempt_id = ?`
		args = append(args, *runID)
	}
	err := database.QueryRow(query, args...).Scan(&ts)
	if err != nil {
		return 0, fmt.Errorf("query max ts: %w", err)
	}
	if ts.Valid {
		return ts.Int64, nil
	}
	return 0, nil
}
