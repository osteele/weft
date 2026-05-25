-- Refine the auto-confirm triggers added in 00004 to fire only when the
-- target launch is in a live (or completed) state. Inserting an attempt
-- row whose launch_id points at a failed/canceled launch is the precondition
-- for ResetLaunchJobs and HandleMoveTargetFailedBeforeStart — exactly the
-- recovery paths that need the MoveIntent to remain open so the source can
-- be restored. The original 00004 trigger preempted those flows.
--
-- The reconciler's prior semantics (placement_reconciler.go) only confirmed
-- when "the target accepted the job before terminal" — operationalized as
-- agent_ready_at OR an attempt with start_time. This migration encodes that
-- in the DB by gating on launch.status, which is a strict superset signal
-- (live status implies the launch has at least reached the agent-ready
-- threshold before failing, or is still alive). Including 'completed' covers
-- the race where a target finishes during the move handshake.
--
-- The obsolete-on-source-attempt-end and obsolete-on-job-requested-terminal
-- triggers from 00004 are unchanged: they observe orthogonal signals that
-- don't suffer the same false-positive.

-- +goose Up
-- +goose StatementBegin
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS move_intents_auto_obsolete_on_source_attempt_end;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM launches l
       WHERE l.id = NEW.launch_id
         AND l.status IN ('planned','launching','running','paused','grace','completed')
  )
BEGIN
    UPDATE move_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts insert on live launch ' || NEW.launch_id,
           target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
     WHERE job_id = NEW.job_id
       AND state = 'open'
       AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS placement_intents_auto_confirm_on_attempt_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM launches l
       WHERE l.id = NEW.launch_id
         AND l.status IN ('planned','launching','running','paused','grace','completed')
  )
BEGIN
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts insert on live launch ' || NEW.launch_id
     WHERE job_id = NEW.job_id AND state = 'open';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_launch_update
AFTER UPDATE OF launch_id ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND (OLD.launch_id IS NULL OR OLD.launch_id != NEW.launch_id)
  AND EXISTS (
      SELECT 1 FROM launches l
       WHERE l.id = NEW.launch_id
         AND l.status IN ('planned','launching','running','paused','grace','completed')
  )
BEGIN
    UPDATE move_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts.launch_id update to live launch ' || NEW.launch_id,
           target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
     WHERE job_id = NEW.job_id
       AND state = 'open'
       AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS placement_intents_auto_confirm_on_attempt_launch_update
AFTER UPDATE OF launch_id ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND (OLD.launch_id IS NULL OR OLD.launch_id != NEW.launch_id)
  AND EXISTS (
      SELECT 1 FROM launches l
       WHERE l.id = NEW.launch_id
         AND l.status IN ('planned','launching','running','paused','grace','completed')
  )
BEGIN
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts.launch_id update to live launch ' || NEW.launch_id
     WHERE job_id = NEW.job_id AND state = 'open';
END;
-- +goose StatementEnd

-- The 00004 obsolete-on-source-attempt-end trigger fired whenever the
-- source attempt's end_time went from NULL to a value. But closeOpenAttempts
-- (called by TransferJobLaunchID and other re-routing paths) sets end_time
-- without changing status — i.e. the move's own re-routing of the job
-- across launches looks identical to "the source attempt completed
-- naturally." That mis-fire obsoleted intents during their own placement.
-- Re-add the trigger with a tighter gate: only fire when the source
-- attempt has also reached a terminal status (set by the natural-completion
-- path, never by closeOpenAttempts).
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_end
AFTER UPDATE OF end_time ON job_attempts
WHEN NEW.end_time IS NOT NULL AND OLD.end_time IS NULL
  AND NEW.status IN ('completed','failed','dead','killed','canceled')
BEGIN
    UPDATE move_intents
       SET state = 'obsoleted',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-obsoleted by source_attempt end (status=' || NEW.status || ')'
     WHERE source_attempt_id = NEW.id AND state = 'open';
END;
-- +goose StatementEnd

-- Same case via status update on an attempt that already has end_time (the
-- order of writes can vary by code path; some set status first then end_time,
-- others the reverse, others set both atomically).
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_status_terminal
AFTER UPDATE OF status ON job_attempts
WHEN NEW.status IN ('completed','failed','dead','killed','canceled')
  AND OLD.status != NEW.status
  AND NEW.end_time IS NOT NULL
BEGIN
    UPDATE move_intents
       SET state = 'obsoleted',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-obsoleted by source_attempt terminal status ' || NEW.status
     WHERE source_attempt_id = NEW.id AND state = 'open';
END;
-- +goose StatementEnd

-- A launch can transition from launching → running after attempts have
-- already been created against it (e.g. queued before instance was ready).
-- This trigger catches that case: when the launch becomes live, any open
-- intent for a job whose attempt is already pointing at this launch should
-- auto-confirm.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS intents_auto_confirm_on_launch_becomes_live
AFTER UPDATE OF status ON launches
WHEN NEW.status IN ('running','grace','completed')
  AND (OLD.status IS NULL OR OLD.status != NEW.status)
BEGIN
    UPDATE move_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by launch ' || NEW.id || ' becoming ' || NEW.status,
           target_launch_id = COALESCE(target_launch_id, NEW.id)
     WHERE state = 'open'
       AND (target_launch_id IS NULL OR target_launch_id = NEW.id)
       AND EXISTS (
           SELECT 1 FROM job_attempts ja
            WHERE ja.job_id = move_intents.job_id AND ja.launch_id = NEW.id
       );
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-canceled by launch ' || NEW.id || ' becoming ' || NEW.status
     WHERE state = 'open'
       AND EXISTS (
           SELECT 1 FROM job_attempts ja
            WHERE ja.job_id = placement_intents.job_id AND ja.launch_id = NEW.id
       );
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS intents_auto_confirm_on_launch_becomes_live;

-- Restore the 00004-shape triggers (no launch-status gating).
CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
BEGIN
    UPDATE move_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts insert on launch ' || NEW.launch_id,
           target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
     WHERE job_id = NEW.job_id
       AND state = 'open'
       AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;
CREATE TRIGGER IF NOT EXISTS placement_intents_auto_confirm_on_attempt_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
BEGIN
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts insert on launch ' || NEW.launch_id
     WHERE job_id = NEW.job_id AND state = 'open';
END;
CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_launch_update
AFTER UPDATE OF launch_id ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND (OLD.launch_id IS NULL OR OLD.launch_id != NEW.launch_id)
BEGIN
    UPDATE move_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts.launch_id update to ' || NEW.launch_id,
           target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
     WHERE job_id = NEW.job_id
       AND state = 'open'
       AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;
CREATE TRIGGER IF NOT EXISTS placement_intents_auto_confirm_on_attempt_launch_update
AFTER UPDATE OF launch_id ON job_attempts
WHEN NEW.launch_id IS NOT NULL
  AND (OLD.launch_id IS NULL OR OLD.launch_id != NEW.launch_id)
BEGIN
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-confirmed by job_attempts.launch_id update to ' || NEW.launch_id
     WHERE job_id = NEW.job_id AND state = 'open';
END;
-- +goose StatementEnd
