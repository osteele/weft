-- Clear stale pending_status values when an attempt transitions to running.
--
-- Background: R2 .started marker sync once updated job_attempts.status
-- directly to 'running'. If the attempt still carried a satisfied
-- pending_status such as 'queued', EffectiveStatus() kept reporting the job as
-- queued while the raw attempt was running.
--
-- The invariant belongs at the table boundary: when an attempt becomes
-- running, queued/pending_placement/running/starting intents are satisfied.
-- Stop intents such as canceled or killed are intentionally preserved so they
-- can still propagate.
--
-- Do not trigger on pending_status updates alone. A user can set
-- pending_status='queued' on an already-running attempt as a fresh requeue
-- intent, and that must survive until reconciliation handles it.

-- +goose Up
-- +goose StatementBegin
UPDATE job_attempts
   SET pending_status = NULL,
       pending_at = NULL
 WHERE status = 'running'
   AND pending_status IN ('queued','pending_placement','running','starting')
   AND start_time IS NOT NULL
   AND (pending_at IS NULL OR pending_at <= start_time);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_clear_satisfied_pending_on_running_insert
AFTER INSERT ON job_attempts
WHEN NEW.status = 'running'
  AND NEW.pending_status IN ('queued','pending_placement','running','starting')
BEGIN
    UPDATE job_attempts
       SET pending_status = NULL,
           pending_at = NULL
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_clear_satisfied_pending_on_running_update
AFTER UPDATE OF status ON job_attempts
WHEN NEW.status = 'running'
  AND OLD.status != NEW.status
  AND NEW.pending_status IN ('queued','pending_placement','running','starting')
BEGIN
    UPDATE job_attempts
       SET pending_status = NULL,
           pending_at = NULL
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS job_attempts_clear_satisfied_pending_on_running_insert;
DROP TRIGGER IF EXISTS job_attempts_clear_satisfied_pending_on_running_update;
-- The conservative backfill has no inverse.
-- +goose StatementEnd
