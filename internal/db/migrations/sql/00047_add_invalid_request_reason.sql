-- Allow 'invalid_request' in launches.termination_reason. Used for failures
-- where the job's declared inputs are internally inconsistent -- e.g. a --needs
-- artifact that the producer job did not produce. These are neither provider
-- failures nor weft bugs and should be excluded from the survival model.
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
       '''account_credit_exhausted''))',
       '''account_credit_exhausted'', ''invalid_request''))'
   )
 WHERE type = 'table' AND name = 'launches';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''account_credit_exhausted'', ''invalid_request''))',
       '''account_credit_exhausted''))'
   )
 WHERE type = 'table' AND name = 'launches';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
