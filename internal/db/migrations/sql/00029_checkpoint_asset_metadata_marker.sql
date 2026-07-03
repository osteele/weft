-- Checkpoint asset metadata is applied by guarded Go migration v28 because
-- SQLite in this build does not support ALTER TABLE ADD COLUMN IF NOT EXISTS,
-- and the migration must be safe when tests replay versions against a schema
-- that already has the columns.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
