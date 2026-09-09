-- Represent terminal job transitions that happen before any execution attempt.

-- +goose Up
-- SQLite cannot drop a NOT NULL constraint directly. Update the stored table
-- declaration using the same constrained schema migration used for attempt
-- status vocabulary changes.
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(sql, 'attempt_id        INTEGER NOT NULL', 'attempt_id        INTEGER REFERENCES job_attempts(id) ON DELETE CASCADE')
 WHERE type = 'table' AND name = 'job_lifecycle_events';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd

DROP INDEX IF EXISTS idx_job_lifecycle_events_terminal_once;
CREATE UNIQUE INDEX idx_job_lifecycle_events_terminal_once
    ON job_lifecycle_events(job_id, COALESCE(attempt_id, 0), event_kind, status)
    WHERE event_kind = 'job.terminal';

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS jobs_lifecycle_attemptless_terminal_update
AFTER UPDATE OF requested_status ON jobs
FOR EACH ROW
WHEN NEW.requested_status IN ('killed', 'canceled', 'skipped')
 AND COALESCE(OLD.requested_status, '') != NEW.requested_status
 AND NOT EXISTS (
     SELECT 1 FROM authoritative_job_attempts WHERE job_id = NEW.id
 )
BEGIN
    INSERT OR IGNORE INTO job_lifecycle_events (
        event_id, job_id, event_sequence, attempt_id, attempt_number,
        event_kind, status, occurred_at, project, project_root, submitter_session
    )
    VALUES (
        lower(hex(randomblob(16))), NEW.id,
        (SELECT COALESCE(MAX(event_sequence), 0) + 1
           FROM job_lifecycle_events WHERE job_id = NEW.id),
        NULL, 0, 'job.terminal', NEW.requested_status, unixepoch(),
        COALESCE(NEW.project, ''), COALESCE(NEW.project_root, ''),
        COALESCE(NEW.submitter_session, '')
    );
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS jobs_lifecycle_attemptless_terminal_update;
DELETE FROM job_lifecycle_events WHERE attempt_id IS NULL;
DROP INDEX IF EXISTS idx_job_lifecycle_events_terminal_once;
CREATE UNIQUE INDEX idx_job_lifecycle_events_terminal_once
    ON job_lifecycle_events(job_id, attempt_id, event_kind, status)
    WHERE event_kind = 'job.terminal';
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(sql, 'attempt_id        INTEGER REFERENCES job_attempts(id) ON DELETE CASCADE', 'attempt_id        INTEGER NOT NULL')
 WHERE type = 'table' AND name = 'job_lifecycle_events';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
