-- Price authorization state for rental launch prompts.
--
-- jobs.price_authorized_up_to_cents is the per-job override used by
-- `weft job authorize-price`. price_authorizations stores class-level sticky
-- overrides keyed by GPU class and memory bucket.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE jobs ADD COLUMN price_authorized_up_to_cents INTEGER;

CREATE TABLE price_authorizations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    gpu_class    TEXT    NOT NULL,
    gpu_mem_gb   INTEGER NOT NULL,
    up_to_cents  INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    created_by   TEXT,
    note         TEXT,
    UNIQUE(gpu_class, gpu_mem_gb)
);

CREATE INDEX idx_price_authorizations_class
    ON price_authorizations(gpu_class, gpu_mem_gb);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_price_authorizations_class;
DROP TABLE IF EXISTS price_authorizations;
ALTER TABLE jobs DROP COLUMN price_authorized_up_to_cents;
-- +goose StatementEnd
