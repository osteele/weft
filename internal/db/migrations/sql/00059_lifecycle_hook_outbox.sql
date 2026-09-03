-- Durable lifecycle events and independent per-hook delivery receipts.

-- +goose Up
CREATE TABLE IF NOT EXISTS job_lifecycle_events (
    event_id          TEXT PRIMARY KEY,
    job_id            INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    event_sequence    INTEGER NOT NULL,
    attempt_id        INTEGER NOT NULL,
    attempt_number    INTEGER NOT NULL,
    event_kind        TEXT NOT NULL,
    status            TEXT NOT NULL,
    occurred_at       INTEGER NOT NULL,
    project           TEXT NOT NULL,
    project_root      TEXT NOT NULL,
    submitter_session TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_lifecycle_events_job
    ON job_lifecycle_events(job_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_job_lifecycle_events_occurred
    ON job_lifecycle_events(occurred_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_lifecycle_events_sequence
    ON job_lifecycle_events(job_id, event_sequence);
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_lifecycle_events_terminal_once
    ON job_lifecycle_events(job_id, attempt_id, event_kind, status)
    WHERE event_kind = 'job.terminal';

CREATE TABLE IF NOT EXISTS lifecycle_hook_registrations (
    hook_id       TEXT PRIMARY KEY,
    registered_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS lifecycle_hook_deliveries (
    event_id       TEXT NOT NULL REFERENCES job_lifecycle_events(event_id) ON DELETE CASCADE,
    hook_id        TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT 'pending'
                   CHECK (state IN ('pending', 'delivered', 'rejected')),
    disposition    TEXT,
    attempts       INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    lease_token    TEXT,
    lease_until    INTEGER,
    last_error     TEXT,
    delivered_at  INTEGER,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (event_id, hook_id)
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_hook_deliveries_due
    ON lifecycle_hook_deliveries(state, next_attempt_at, lease_until);

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_lifecycle_status_update
AFTER UPDATE OF status ON job_attempts
FOR EACH ROW
WHEN NEW.status != OLD.status
BEGIN
    INSERT INTO job_lifecycle_events (
        event_id, job_id, event_sequence, attempt_id, attempt_number,
        event_kind, status, occurred_at, project, project_root, submitter_session
    )
    SELECT lower(hex(randomblob(16))), NEW.job_id,
           (SELECT COALESCE(MAX(event_sequence), 0) + 1
              FROM job_lifecycle_events WHERE job_id = NEW.job_id),
           NEW.id, NEW.attempt_number,
           CASE WHEN NEW.status IN ('completed', 'failed', 'dead', 'killed', 'canceled', 'skipped')
                THEN 'job.terminal' ELSE 'job.status_changed' END,
           -- Mirror job_status view normalization: completed + nonzero
           -- exit is the failed terminal status everywhere downstream.
           CASE WHEN NEW.status = 'completed' AND NEW.exit_code IS NOT NULL AND NEW.exit_code != 0
                THEN 'failed' ELSE NEW.status END,
           COALESCE(NEW.end_time, unixepoch()),
           COALESCE(j.project, ''), COALESCE(j.project_root, ''),
           COALESCE(j.submitter_session, '')
    FROM jobs AS j WHERE j.id = NEW.job_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_lifecycle_status_insert
AFTER INSERT ON job_attempts
FOR EACH ROW
BEGIN
    INSERT INTO job_lifecycle_events (
        event_id, job_id, event_sequence, attempt_id, attempt_number,
        event_kind, status, occurred_at, project, project_root, submitter_session
    )
    SELECT lower(hex(randomblob(16))), NEW.job_id,
           (SELECT COALESCE(MAX(event_sequence), 0) + 1
              FROM job_lifecycle_events WHERE job_id = NEW.job_id),
           NEW.id, NEW.attempt_number,
           CASE WHEN NEW.status IN ('completed', 'failed', 'dead', 'killed', 'canceled', 'skipped')
                THEN 'job.terminal' ELSE 'job.status_changed' END,
           CASE WHEN NEW.status = 'completed' AND NEW.exit_code IS NOT NULL AND NEW.exit_code != 0
                THEN 'failed' ELSE NEW.status END,
           COALESCE(NEW.end_time, unixepoch()),
           COALESCE(j.project, ''), COALESCE(j.project_root, ''),
           COALESCE(j.submitter_session, '')
    FROM jobs AS j WHERE j.id = NEW.job_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_insert;
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_update;
DROP INDEX IF EXISTS idx_lifecycle_hook_deliveries_due;
DROP TABLE IF EXISTS lifecycle_hook_deliveries;
DROP TABLE IF EXISTS lifecycle_hook_registrations;
DROP INDEX IF EXISTS idx_job_lifecycle_events_occurred;
DROP INDEX IF EXISTS idx_job_lifecycle_events_job;
DROP INDEX IF EXISTS idx_job_lifecycle_events_sequence;
DROP INDEX IF EXISTS idx_job_lifecycle_events_terminal_once;
DROP TABLE IF EXISTS job_lifecycle_events;
