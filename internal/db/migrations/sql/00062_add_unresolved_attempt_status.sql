-- Allow an open queue-runner attempt to record that its worker is confirmed
-- absent while its execution outcome remains unknown. SQLite cannot alter a
-- CHECK constraint directly, so update the schema text in place using the
-- established constraint-migration pattern.

-- +goose Up
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''paused'', ''draft''',
       '''paused'', ''unresolved'', ''draft'''
   )
 WHERE type = 'table' AND name = 'job_attempts';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd

-- +goose Down
UPDATE job_attempts
   SET status = 'running',
       last_synced_status = CASE
           WHEN last_synced_status = 'unresolved' THEN 'running'
           ELSE last_synced_status
       END
 WHERE status = 'unresolved';
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''paused'', ''unresolved'', ''draft''',
       '''paused'', ''draft'''
   )
 WHERE type = 'table' AND name = 'job_attempts';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
