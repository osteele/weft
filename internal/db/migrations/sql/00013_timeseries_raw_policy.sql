-- +goose Up
CREATE TABLE IF NOT EXISTS job_timeseries_raw_objects (
	job_id INTEGER NOT NULL,
	attempt_id INTEGER NOT NULL,
	kind TEXT NOT NULL,
	r2_key TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	etag TEXT,
	confirmed_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (attempt_id, kind),
	FOREIGN KEY (attempt_id) REFERENCES job_attempts(id)
);

CREATE INDEX IF NOT EXISTS idx_job_timeseries_raw_objects_job
	ON job_timeseries_raw_objects(job_id);

CREATE TABLE IF NOT EXISTS job_timeseries_summaries (
	job_id INTEGER NOT NULL,
	attempt_id INTEGER NOT NULL PRIMARY KEY,
	sample_count INTEGER NOT NULL DEFAULT 0,
	ts_min INTEGER NOT NULL DEFAULT 0,
	ts_max INTEGER NOT NULL DEFAULT 0,
	peak_disk_used_bytes INTEGER NOT NULL DEFAULT 0,
	last_disk_total_bytes INTEGER NOT NULL DEFAULT 0,
	peak_rss_kb INTEGER NOT NULL DEFAULT 0,
	peak_gpu_mem_mib INTEGER NOT NULL DEFAULT 0,
	peak_gpu_util_pct INTEGER NOT NULL DEFAULT 0,
	peak_gpu_temp_c INTEGER NOT NULL DEFAULT 0,
	mean_gpu_util_pct REAL NOT NULL DEFAULT 0,
	mean_gpu_temp_c REAL NOT NULL DEFAULT 0,
	tenant TEXT,
	updated_at INTEGER NOT NULL,
	FOREIGN KEY (attempt_id) REFERENCES job_attempts(id)
);

CREATE INDEX IF NOT EXISTS idx_job_timeseries_summaries_job
	ON job_timeseries_summaries(job_id);

-- +goose Down
DROP INDEX IF EXISTS idx_job_timeseries_summaries_job;
DROP TABLE IF EXISTS job_timeseries_summaries;
DROP INDEX IF EXISTS idx_job_timeseries_raw_objects_job;
DROP TABLE IF EXISTS job_timeseries_raw_objects;
