-- Allow 'account_credit_exhausted' in launches.termination_reason. Used to
-- mark instances destroyed by the provider for non-payment, so they don't
-- poison the survival/bidding model with what looks like generic provider
-- failures. Today the only writer is `weft instance mark-credit-exhausted`,
-- which the operator runs after a credit-exhaustion incident; the runtime
-- destruction case has no programmatic signal in the provider response.
-- CreateInstance-time credit failures still write `provider_failure` plus a
-- substring-matched detail; routing those through this constant requires the
-- call-site audit described in docs/planning/ROADMAP.md § "Structured
-- termination reasons for credit exhaustion".
--
-- Pattern mirrors 00002 / 00008: writable_schema REPLACE on the IN-list,
-- anchored on the previous tail so the surrounding 60-column launches schema
-- text need not be reproduced verbatim.

-- +goose Up
-- +goose StatementBegin
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
   SET sql = REPLACE(
       sql,
       '''upload_stall''))',
       '''upload_stall'', ''account_credit_exhausted''))'
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
       '''upload_stall'', ''account_credit_exhausted''))',
       '''upload_stall''))'
   )
 WHERE type = 'table' AND name = 'launches';
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
