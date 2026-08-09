-- Attempt-scoped publication state is independent of command execution
-- status. Reports are monotonic snapshots emitted by the agent after command
-- exit and while deferred data-plane work drains.

-- +goose Up
CREATE TABLE IF NOT EXISTS attempt_publication_state (
    attempt_id INTEGER PRIMARY KEY REFERENCES job_attempts(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    observed_at INTEGER NOT NULL,
    execution_state TEXT NOT NULL CHECK (execution_state IN ('pending', 'complete', 'unknown')),
    execution_completed_at INTEGER,
    required_artifacts_state TEXT NOT NULL CHECK (required_artifacts_state IN ('pending', 'ready', 'failed', 'unknown')),
    required_artifacts_ready_at INTEGER,
    drain_state TEXT NOT NULL CHECK (drain_state IN ('pending', 'ready', 'failed', 'unknown')),
    drain_completed_at INTEGER,
    queued_items INTEGER,
    queued_bytes INTEGER,
    inflight_items INTEGER,
    inflight_bytes INTEGER,
    oldest_queued_age_ms INTEGER,
    retained_bytes INTEGER,
    worker_limit INTEGER,
    workers_busy INTEGER,
    last_progress_at INTEGER,
    unknown_reason TEXT,
    detail TEXT,
    CHECK (queued_items IS NULL OR queued_items >= 0),
    CHECK (queued_bytes IS NULL OR queued_bytes >= 0),
    CHECK (inflight_items IS NULL OR inflight_items >= 0),
    CHECK (inflight_bytes IS NULL OR inflight_bytes >= 0),
    CHECK (oldest_queued_age_ms IS NULL OR oldest_queued_age_ms >= 0),
    CHECK (retained_bytes IS NULL OR retained_bytes >= 0),
    CHECK (worker_limit IS NULL OR worker_limit > 0),
    CHECK (workers_busy IS NULL OR workers_busy >= 0)
);

CREATE TABLE IF NOT EXISTS attempt_publication_artifacts (
    attempt_id INTEGER NOT NULL REFERENCES job_attempts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    path TEXT,
    state TEXT NOT NULL CHECK (state IN ('pending', 'ready', 'failed', 'unknown')),
    ready_at INTEGER,
    payload_key TEXT,
    detail TEXT,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    PRIMARY KEY (attempt_id, name)
);

CREATE INDEX IF NOT EXISTS idx_attempt_publication_artifacts_state
    ON attempt_publication_artifacts(attempt_id, state);

-- +goose Down
DROP INDEX IF EXISTS idx_attempt_publication_artifacts_state;
DROP TABLE IF EXISTS attempt_publication_artifacts;
DROP TABLE IF EXISTS attempt_publication_state;
