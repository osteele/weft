-- named_assets maps a stable, user-chosen name to a content-addressed blob
-- in R2 under the assets/<sha256> key. Each name has exactly one current
-- version; republishing under the same name overwrites the row and the
-- previous content_hash is left in R2 (eviction is a future concern).
--
-- target_path is the workspace-relative path the file is staged into at
-- consumer-job launch time (recorded when the asset is published, derived
-- from the local file's project-relative path).
--
-- source_job_id is non-NULL when the asset was minted from an existing job
-- artifact (`weft data publish --from <job-id>:<path>`), preserving the
-- provenance link without duplicating bytes.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS named_assets (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    content_hash  TEXT    NOT NULL,
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    target_path   TEXT    NOT NULL,
    source_job_id INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_named_assets_hash ON named_assets(content_hash);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_named_assets_hash;
DROP TABLE IF EXISTS named_assets;
-- +goose StatementEnd
