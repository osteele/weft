-- Index the per-job dispatch-event lookup that the dispatch backoff gate runs
-- on every wake-snapshot read. lifecycle_events carries indexes on event_kind,
-- launch_id and occurred_at but none on job_id, so the run query degraded to a
-- scan once it moved from the on-demand diagnose path into a hot path that
-- runs once per queued job per snapshot.

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_le_job_kind_occurred
    ON lifecycle_events(job_id, event_kind, occurred_at);

-- +goose Down
DROP INDEX IF EXISTS idx_le_job_kind_occurred;
