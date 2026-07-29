-- launches.host_id and the hosts table are dropped by guarded Go migration
-- v37 because SQLite in this build has no ALTER TABLE DROP COLUMN IF EXISTS,
-- and the migration must be safe when tests replay versions against a schema
-- that no longer has the column.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
