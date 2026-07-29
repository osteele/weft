-- external_job_bindings.sync_warning / sync_warning_at are applied by guarded
-- Go migration v35 because SQLite in this build does not support
-- ALTER TABLE ADD COLUMN IF NOT EXISTS, and the migration must be safe when
-- tests replay versions against a schema that already has the columns.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
