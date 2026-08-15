-- +goose Up
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_abandoned
AFTER UPDATE OF abandoned_at ON job_attempts
WHEN NEW.abandoned_at IS NOT NULL AND OLD.abandoned_at IS NULL
BEGIN
    UPDATE move_intents
       SET state = 'obsoleted',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-obsoleted by source_attempt abandoned'
     WHERE source_attempt_id = NEW.id
       AND state = 'open'
       AND (NEW.abandoned_by_intent_id IS NULL OR id != NEW.abandoned_by_intent_id);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS move_intents_auto_obsolete_on_source_attempt_abandoned;
-- +goose StatementEnd
