-- wb79: make idle job_status queries cheap.
--
-- job_status is a VIEW, so the status filter and the tombstoned predicate
-- cannot be served by an index on the view itself. The two expensive scans
-- live in the view definition:
--
--   * the jobs scan visits every jobs row to find the untombstoned ones;
--   * per untombstoned row, the latest-authoritative-attempt scalar subquery
--     probes job_attempts and then fetches each candidate attempt's data page
--     to evaluate abandoned_at / move_intent_id.
--
-- These two indexes make both scans index-only where the data volume is
-- largest. Results are unchanged: CREATE INDEX is pure DDL.

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_job_attempts_authoritative_covering
	ON job_attempts(job_id, attempt_number DESC, id DESC, abandoned_at, move_intent_id);

CREATE INDEX IF NOT EXISTS idx_jobs_untombstoned
	ON jobs(id) WHERE tombstoned = 0;

-- +goose Down
DROP INDEX IF EXISTS idx_job_attempts_authoritative_covering;
DROP INDEX IF EXISTS idx_jobs_untombstoned;
