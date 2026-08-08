-- A termination intent records when provider destruction succeeded. Before
-- v41, a later reconcile pass ignored that timestamp and stamped ended_at at
-- reconcile time, inflating derived uptime and cost by an arbitrary idle gap.
--
-- Repair only terminal launches whose valid intent is explicitly succeeded
-- (or is a legacy marker whose destroy timestamp implies success), and only
-- when ended_at is later than the recorded destroy success. Earlier end times
-- may come from a different lifecycle observation and are outside this repair.

-- +goose Up
-- +goose StatementBegin
UPDATE launches
   SET ended_at = CASE
       WHEN json_valid(termination_intent_json)
       THEN CAST(json_extract(termination_intent_json, '$.destroy_succeeded_at_unix') AS INTEGER)
       ELSE ended_at
   END
 WHERE status IN ('completed', 'failed', 'canceled')
   AND ended_at IS NOT NULL
   AND termination_intent_json IS NOT NULL
   AND json_valid(termination_intent_json)
   AND CASE
       WHEN json_valid(termination_intent_json)
       THEN CAST(json_extract(termination_intent_json, '$.destroy_succeeded_at_unix') AS INTEGER)
       ELSE 0
   END > 0
   AND ended_at > CASE
       WHEN json_valid(termination_intent_json)
       THEN CAST(json_extract(termination_intent_json, '$.destroy_succeeded_at_unix') AS INTEGER)
       ELSE ended_at
   END
   AND (
       CASE
           WHEN json_valid(termination_intent_json)
           THEN json_extract(termination_intent_json, '$.state')
           ELSE NULL
       END = 'succeeded'
       OR (
           COALESCE(CASE
               WHEN json_valid(termination_intent_json)
               THEN json_extract(termination_intent_json, '$.state')
               ELSE NULL
           END, '') = ''
           AND CASE
               WHEN json_valid(termination_intent_json)
               THEN CAST(json_extract(termination_intent_json, '$.destroy_succeeded_at_unix') AS INTEGER)
               ELSE 0
           END > 0
       )
   );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- The historical repair has no inverse.
-- +goose StatementEnd
