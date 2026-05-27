-- Allow 'upload_stall' in launches.termination_reason. Written by the agent's
-- upload-drain layer (cmd/agent/upload_drain.go) when R2 uploads stall
-- repeatedly with no progress — distinct from 'infra_failure' so the
-- clustered-failures banner can ignore them, and retryable on a fresh
-- instance.
--
-- This value was added to the Go constants in internal/db/cloud_instances.go
-- (TerminationReasonUploadStall) without a matching schema migration, so
-- writes from the reconciler's ActionTerminationIntent path failed the
-- launches_termination_reason_check CHECK constraint, leaving affected
-- instances stuck in status='running' with queued jobs that never re-place.
--
-- Pattern mirrors 00002 (provider_timeout): writable_schema REPLACE on the
-- IN-list, anchored on the previous tail so the surrounding 60-column
-- launches schema text need not be reproduced verbatim.

-- +goose Up
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''provider_timeout''))',
       '''provider_timeout'', ''upload_stall''))'
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
       '''provider_timeout'', ''upload_stall''))',
       '''provider_timeout''))'
   )
 WHERE type = 'table' AND name = 'launches';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
