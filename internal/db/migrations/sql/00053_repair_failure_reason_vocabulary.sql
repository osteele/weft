-- +goose Up
-- failure_reason is a closed vocabulary; see specs/job-lifecycle.allium.
-- Legacy rows hold prose written before the write-side sanitizer existed.
--
-- The exact-match rewrite below is safe on its own. The general case needs the
-- Go vocabulary, so repairFailureReasons in internal/db/db.go performs it —
-- and startupRepair only runs when a migration is applied, which is what this
-- file exists to guarantee.
UPDATE job_attempts
SET error_message = CASE
        WHEN TRIM(COALESCE(error_message, '')) = '' THEN failure_reason
        ELSE error_message
    END,
    failure_reason = 'infra_r2_results_not_synced'
WHERE failure_reason = 'launch completed but job results were not synced from R2';

-- +goose Down
-- The original prose is preserved in error_message, but it cannot be told apart
-- from a diagnosis written there by any other path, so this is not reversed.
SELECT 1;
