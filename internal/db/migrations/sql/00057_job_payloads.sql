-- Admission-time payloads are immutable, logical-job-scoped input artifacts.
-- Their content-addressed bytes use the existing artifact/R2 stores; this table
-- owns only the job/name association and retrieval metadata.

-- +goose Up
CREATE TABLE IF NOT EXISTS job_payloads (
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    stored_path TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL CHECK (size_bytes >= 0),
    sha256      TEXT NOT NULL,
    r2_key      TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (job_id, name)
);
CREATE INDEX IF NOT EXISTS idx_job_payloads_sha256 ON job_payloads(sha256);

-- +goose Down
DROP INDEX IF EXISTS idx_job_payloads_sha256;
DROP TABLE IF EXISTS job_payloads;
