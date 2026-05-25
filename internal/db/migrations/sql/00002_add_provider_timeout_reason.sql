-- Allow 'provider_timeout' in launches.termination_reason. Used for failures
-- where weft's CLI call to the cloud provider exceeded its per-command
-- deadline (cloud.ErrProviderCommandTimeout) — distinct from a real
-- infra_failure so the clustered-failures banner can ignore them.
--
-- SQLite has no ALTER TABLE ... DROP/ADD CONSTRAINT, but a CHECK-constraint
-- change does not affect on-disk row content, so the writable_schema path
-- documented at https://www.sqlite.org/lang_altertable.html#otheralter is the
-- recommended in-place approach. We use REPLACE() with a unique anchor at
-- the end of the existing IN(...) list so the surrounding 60-column launches
-- schema text need not be reproduced verbatim here.

-- +goose Up
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''weft_bug''))',
       '''weft_bug'', ''provider_timeout''))'
   )
 WHERE type = 'table' AND name = 'launches';
-- writable_schema = RESET (SQLite 3.42+) reloads and re-parses the schema in
-- the current connection so the new CHECK clause takes effect immediately,
-- not just on the next reopen. Without this, the connection that ran the
-- migration retains the old compiled CHECK and rejects 'provider_timeout'
-- writes until restart.
PRAGMA writable_schema = RESET;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''weft_bug'', ''provider_timeout''))',
       '''weft_bug''))'
   )
 WHERE type = 'table' AND name = 'launches';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
