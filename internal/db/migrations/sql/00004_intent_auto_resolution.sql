-- Auto-resolve MoveIntent and PlacementIntent rows from observable state, so
-- the intent table cannot drift open after the placement it tracked has
-- actually landed (or the job has moved past the placement entirely).
--
-- Before this migration, an intent was closed only by an explicit Go-side
-- call to ResolveMoveIntent / ResolvePlacementIntent at every code path
-- that could complete a move. Whenever a new placement path was added
-- (cloud reuse, manual instance submit, alternative-target placement, etc.)
-- and that path forgot the close-the-intent call, the row leaked to
-- state='open' forever. The TUI's "Placing" bucket reads state='open' and
-- mislabels jobs that have actually been placed onto a running launch.
--
-- This migration moves the open→terminal transition into the database:
--   (a) inserting/updating a job_attempts row with a launch_id resolves
--       any open intent for that job as 'confirmed' (the placement
--       happened, regardless of which code path did it);
--   (b) the source_attempt of a MoveIntent reaching end_time resolves the
--       intent as 'obsoleted' (matches spec ObsoleteMoveOnSourceCompletion);
--   (c) jobs.requested_status transitioning to 'killed' or 'canceled'
--       resolves any open intent for that job (the user gave up).
--
-- The periodic Go-side reconciler (placement_reconciler.go) is removed in
-- the same change: its job is now done at the moment the source-of-truth
-- table mutates, atomically, in the same transaction. See
-- specs/job-move.allium for the updated rules.

-- +goose Up
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- An attempt row inserted without launch_id (the "queued, unplaced" shape)
-- and later updated with a launch_id is the same observable event — the
-- placement happened — and must trigger the same resolution.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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

-- spec/job-move.allium ObsoleteMoveOnSourceCompletion: when the source
-- attempt that the move was started against ends (any exit code), the
-- intent is obsoleted. The intent's source_attempt_id pinpoints which
-- attempt's completion fires this.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_end
AFTER UPDATE OF end_time ON job_attempts
WHEN NEW.end_time IS NOT NULL AND OLD.end_time IS NULL
BEGIN
    UPDATE move_intents
       SET state = 'obsoleted',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-obsoleted by source_attempt end'
     WHERE source_attempt_id = NEW.id AND state = 'open';
END;
-- +goose StatementEnd

-- User asked the job to stop (killed/canceled) before any placement landed.
-- MoveIntent uses 'obsoleted' (lifecycle ended without confirmation);
-- PlacementIntent uses 'canceled' (it has no 'obsoleted' state).
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS intents_auto_resolve_on_job_requested_terminal
AFTER UPDATE OF requested_status ON jobs
WHEN NEW.requested_status IN ('killed','canceled')
  AND (OLD.requested_status IS NULL OR OLD.requested_status != NEW.requested_status)
BEGIN
    UPDATE move_intents
       SET state = 'obsoleted',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-obsoleted by user ' || NEW.requested_status
     WHERE job_id = NEW.id AND state = 'open';
    UPDATE placement_intents
       SET state = 'canceled',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-canceled by user ' || NEW.requested_status
     WHERE job_id = NEW.id AND state = 'open';
END;
-- +goose StatementEnd

-- Backfill: resolve currently-leaked intents one-shot so the TUI flips
-- correctly for in-flight jobs (wj2206-shaped rows) without waiting for
-- a fresh attempt to be created. Same semantics as the triggers, applied
-- retroactively.
-- +goose StatementBegin
UPDATE move_intents
   SET state = 'confirmed',
       resolved_at = strftime('%s','now'),
       resolution = COALESCE(resolution, '') || ' [backfill: auto-confirmed by job_attempts on launch ' ||
                    (SELECT launch_id
                       FROM job_attempts ja
                      WHERE ja.job_id = move_intents.job_id
                        AND ja.launch_id IS NOT NULL
                        AND (move_intents.target_launch_id IS NULL OR ja.launch_id = move_intents.target_launch_id)
                      ORDER BY ja.attempt_number DESC
                      LIMIT 1) || ']',
       target_launch_id = COALESCE(
           target_launch_id,
           (SELECT launch_id
              FROM job_attempts ja
             WHERE ja.job_id = move_intents.job_id
               AND ja.launch_id IS NOT NULL
             ORDER BY ja.attempt_number DESC
             LIMIT 1)
       )
 WHERE state = 'open'
   AND EXISTS (
       SELECT 1 FROM job_attempts ja
        WHERE ja.job_id = move_intents.job_id
          AND ja.launch_id IS NOT NULL
          AND (move_intents.target_launch_id IS NULL OR ja.launch_id = move_intents.target_launch_id)
   );
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE placement_intents
   SET state = 'confirmed',
       resolved_at = strftime('%s','now'),
       resolution = COALESCE(resolution, '') || ' [backfill: auto-confirmed by job_attempts]'
 WHERE state = 'open'
   AND EXISTS (
       SELECT 1 FROM job_attempts ja
        WHERE ja.job_id = placement_intents.job_id
          AND ja.launch_id IS NOT NULL
   );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS placement_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS move_intents_auto_obsolete_on_source_attempt_end;
DROP TRIGGER IF EXISTS intents_auto_resolve_on_job_requested_terminal;
-- The backfill UPDATE has no logical inverse; intents that were leaked open
-- and got auto-confirmed on roll-forward will remain confirmed on roll-back.
-- +goose StatementEnd
