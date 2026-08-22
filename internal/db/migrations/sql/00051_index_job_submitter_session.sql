-- +goose Up
CREATE INDEX IF NOT EXISTS idx_jobs_submitter_session
ON jobs(submitter_session)
WHERE submitter_session IS NOT NULL AND submitter_session != '';

-- +goose Down
DROP INDEX IF EXISTS idx_jobs_submitter_session;
