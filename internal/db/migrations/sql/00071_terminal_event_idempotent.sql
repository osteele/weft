-- Re-asserting an attempt's existing terminal status must not abort the write.
--
-- The attempt lifecycle triggers used a plain INSERT, so a second job.terminal
-- event for the same (job, attempt, kind, status) hit
-- idx_job_lifecycle_events_terminal_once and failed the enclosing transaction.
-- Any attempt whose terminal event already existed could therefore never be
-- recorded terminal again: wj8888 was reset to queued in place after
-- completing, and every subsequent settlement pass failed with
-- "UNIQUE constraint failed: index 'idx_job_lifecycle_events_terminal_once'",
-- leaving the job reporting queued 19 hours after the host finished it.
--
-- The index still holds one terminal event per attempt and status; the event is
-- a durable fact, so re-asserting it is a no-op rather than an error. Matches
-- jobs_lifecycle_attemptless_terminal_update, which already used OR IGNORE.

-- +goose Up
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_update;
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_insert;

-- +goose StatementBegin
CREATE TRIGGER job_attempts_lifecycle_status_update
AFTER UPDATE OF status ON job_attempts
FOR EACH ROW
WHEN NEW.status != OLD.status
BEGIN
    INSERT OR IGNORE INTO job_lifecycle_events (
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
CREATE TRIGGER job_attempts_lifecycle_status_insert
AFTER INSERT ON job_attempts
FOR EACH ROW
BEGIN
    INSERT OR IGNORE INTO job_lifecycle_events (
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
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_update;
DROP TRIGGER IF EXISTS job_attempts_lifecycle_status_insert;

-- +goose StatementBegin
CREATE TRIGGER job_attempts_lifecycle_status_update
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
           CASE WHEN NEW.status = 'completed' AND NEW.exit_code IS NOT NULL AND NEW.exit_code != 0
                THEN 'failed' ELSE NEW.status END,
           COALESCE(NEW.end_time, unixepoch()),
           COALESCE(j.project, ''), COALESCE(j.project_root, ''),
           COALESCE(j.submitter_session, '')
    FROM jobs AS j WHERE j.id = NEW.job_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER job_attempts_lifecycle_status_insert
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
