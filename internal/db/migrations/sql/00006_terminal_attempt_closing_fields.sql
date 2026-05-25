-- Enforce the spec invariants TerminalJobsHaveEndTime (job-lifecycle.allium)
-- and ClosedCloudAttemptHasOutcome (campaign-lifecycle.allium) at the DB
-- layer, via auto-stamping triggers that backstop every writer.
--
-- Background. These invariants were previously enforced only by the
-- per-call-site discipline of every writer that transitions an attempt to
-- a terminal status: stamp end_time in the same UPDATE, stamp cloud_outcome
-- in the same UPDATE for cloud rows. Recent regressions:
--
--   * wi2317 (2026-05): UpdateAttemptStatusAndLastSynced wrote
--     status='canceled' without end_time; the reconciler treated the row
--     as in-flight and silenced IdleAfterReadyTimeout on the surrounding
--     instance.
--
--   * 837 prod rows (as of 2026-05-26) have launch_id NOT NULL, end_time
--     set, and cloud_outcome NULL — closed cloud attempts whose writer
--     never stamped the outcome. job_status derives those as 'dead' and
--     orphan-recovery skips them: the job is wedged and can never
--     re-enter placement.
--
-- These triggers move the invariants from "every writer MUST remember" to
-- "the table enforces it for you," matching the structural shift made for
-- placement intents in 00004 + 00005.
--
-- Carve-out: cloud attempts whose last_synced_status is NULL are awaiting
-- agent backfill from R2; the writer intentionally leaves end_time NULL and
-- cloud_outcome NULL so a later sync can rewrite them from the agent's
-- authoritative completion record. See TerminalJobsHaveEndTime guidance
-- in specs/job-lifecycle.allium.

-- +goose Up
-- +goose StatementBegin
-- Backfill: stamp cloud_outcome on existing closed cloud attempts that
-- never got one. Mapping mirrors how completion writers classify the row
-- when they remember to stamp it themselves (cloud_job_completion.go):
--   completed + exit_code=0 → completed
--   completed + exit_code!=0 → failed (it ran but didn't succeed)
--   failed / dead → failed
--   killed / canceled → canceled
UPDATE job_attempts
   SET cloud_outcome = CASE
       WHEN status = 'completed' AND COALESCE(exit_code, -1) = 0 THEN 'completed'
       WHEN status = 'completed' THEN 'failed'
       WHEN status IN ('failed','dead')       THEN 'failed'
       WHEN status IN ('killed','canceled')   THEN 'canceled'
       ELSE 'failed'  -- defensive fallback for unexpected status values
       END
 WHERE launch_id IS NOT NULL
   AND end_time IS NOT NULL
   AND end_time > 0
   AND cloud_outcome IS NULL
   AND last_synced_status IS NOT NULL;
-- +goose StatementEnd

-- Backfill: stamp end_time on any existing terminal-but-open rows. The
-- expected count from a prod scan (2026-05-26) was 0, but it costs nothing
-- to run and protects against any older-history rows the scan missed.
-- Uses queued_at as the timestamp source (more accurate than now() for a
-- historical fixup); falls back to now() if queued_at is missing.
-- +goose StatementBegin
UPDATE job_attempts
   SET end_time = COALESCE(NULLIF(queued_at, 0), strftime('%s','now'))
 WHERE status IN ('completed','failed','dead','killed','canceled')
   AND (end_time IS NULL OR end_time = 0)
   AND NOT (launch_id IS NOT NULL AND last_synced_status IS NULL);
-- +goose StatementEnd

-- Trigger: auto-stamp end_time when status transitions to terminal.
-- Fires on INSERT and on UPDATE of status. Carve-out: cloud attempt
-- awaiting backfill (launch_id IS NOT NULL AND last_synced_status IS NULL).
--
-- SQLite is configured with recursive_triggers OFF (db.go), so the inner
-- UPDATE of end_time does NOT re-fire the baseline
-- job_attempts_open_state_close_or_move trigger that maintains the
-- job_open_attempts denormalization. We therefore inline the equivalent
-- DELETE here so the open-attempts mirror stays consistent.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_terminal_auto_stamp_end_time_insert
AFTER INSERT ON job_attempts
WHEN NEW.status IN ('completed','failed','dead','killed','canceled')
  AND (NEW.end_time IS NULL OR NEW.end_time = 0)
  AND NOT (NEW.launch_id IS NOT NULL AND NEW.last_synced_status IS NULL)
BEGIN
    UPDATE job_attempts
       SET end_time = strftime('%s','now')
     WHERE id = NEW.id;
    DELETE FROM job_open_attempts WHERE attempt_id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS job_attempts_terminal_auto_stamp_end_time_update
AFTER UPDATE OF status ON job_attempts
WHEN NEW.status IN ('completed','failed','dead','killed','canceled')
  AND OLD.status != NEW.status
  AND (NEW.end_time IS NULL OR NEW.end_time = 0)
  AND NOT (NEW.launch_id IS NOT NULL AND NEW.last_synced_status IS NULL)
BEGIN
    UPDATE job_attempts
       SET end_time = strftime('%s','now')
     WHERE id = NEW.id;
    DELETE FROM job_open_attempts WHERE attempt_id = NEW.id;
END;
-- +goose StatementEnd

-- Trigger: auto-derive cloud_outcome when a cloud attempt closes (end_time
-- set on a launch_id-NOT-NULL row) without one. Uses the same mapping as
-- the backfill above. Same backfill-pending carve-out applies.
--
-- A writer that explicitly sets cloud_outcome (e.g. 'orphaned',
-- 'superseded', 'preempted' — values not derivable from status) is
-- unaffected: the trigger's WHEN clause requires cloud_outcome IS NULL.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS cloud_attempts_auto_derive_outcome_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND NEW.end_time IS NOT NULL
  AND NEW.end_time > 0
  AND NEW.cloud_outcome IS NULL
  AND NEW.last_synced_status IS NOT NULL
BEGIN
    UPDATE job_attempts
       SET cloud_outcome = CASE
           WHEN NEW.status = 'completed' AND COALESCE(NEW.exit_code, -1) = 0 THEN 'completed'
           WHEN NEW.status = 'completed' THEN 'failed'
           WHEN NEW.status IN ('failed','dead')       THEN 'failed'
           WHEN NEW.status IN ('killed','canceled')   THEN 'canceled'
           ELSE 'failed'
           END
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS cloud_attempts_auto_derive_outcome_update
AFTER UPDATE OF end_time, status, exit_code, last_synced_status ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND NEW.end_time IS NOT NULL
  AND NEW.end_time > 0
  AND NEW.cloud_outcome IS NULL
  AND NEW.last_synced_status IS NOT NULL
BEGIN
    UPDATE job_attempts
       SET cloud_outcome = CASE
           WHEN NEW.status = 'completed' AND COALESCE(NEW.exit_code, -1) = 0 THEN 'completed'
           WHEN NEW.status = 'completed' THEN 'failed'
           WHEN NEW.status IN ('failed','dead')       THEN 'failed'
           WHEN NEW.status IN ('killed','canceled')   THEN 'canceled'
           ELSE 'failed'
           END
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS job_attempts_terminal_auto_stamp_end_time_insert;
DROP TRIGGER IF EXISTS job_attempts_terminal_auto_stamp_end_time_update;
DROP TRIGGER IF EXISTS cloud_attempts_auto_derive_outcome_insert;
DROP TRIGGER IF EXISTS cloud_attempts_auto_derive_outcome_update;
-- The backfill UPDATEs have no logical inverse; rows backfilled on
-- roll-forward retain their derived values on roll-back.
-- +goose StatementEnd
