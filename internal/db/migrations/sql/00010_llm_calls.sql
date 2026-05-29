-- llm_calls records each call weft makes to an LLM provider for billable
-- features (narration, compaction, AI-assist, description generation). It
-- is the canonical source for `weft dashboard`'s Usage tab and the LLM
-- section of the Cost tab.
--
-- Only Anthropic is wired up at the time of this migration; OpenRouter is
-- left as a follow-up but the schema is provider-agnostic so the same table
-- holds OpenRouter rows once that wiring is added.
--
-- The cost_micros column stores cost in micro-dollars (1e-6 USD) so we can
-- represent very small per-call amounts (a single haiku call may cost <$0.01)
-- without floating-point fuzz, and aggregate cleanly across thousands of
-- calls.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS llm_calls (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts_unix                  INTEGER NOT NULL,
    provider                 TEXT NOT NULL,             -- 'anthropic' | 'openrouter' | ...
    model                    TEXT NOT NULL,             -- e.g. 'claude-opus-4-7'
    feature                  TEXT NOT NULL,             -- 'narrate' | 'compact' | 'describe' | ...
    input_tokens             INTEGER NOT NULL DEFAULT 0,
    output_tokens            INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
    latency_ms               INTEGER NOT NULL DEFAULT 0,
    cost_micros              INTEGER NOT NULL DEFAULT 0,-- 1 USD = 1_000_000 micros
    job_id                   INTEGER,                   -- optional: associated weft job
    error                    TEXT                       -- non-empty if the call failed and we still recorded it
);
CREATE INDEX IF NOT EXISTS idx_llm_calls_ts ON llm_calls(ts_unix DESC);
CREATE INDEX IF NOT EXISTS idx_llm_calls_provider_ts ON llm_calls(provider, ts_unix DESC);
CREATE INDEX IF NOT EXISTS idx_llm_calls_feature_ts ON llm_calls(feature, ts_unix DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_llm_calls_feature_ts;
DROP INDEX IF EXISTS idx_llm_calls_provider_ts;
DROP INDEX IF EXISTS idx_llm_calls_ts;
DROP TABLE IF EXISTS llm_calls;
-- +goose StatementEnd
