-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS host_capability_observations (
    host        TEXT    NOT NULL,
    label       TEXT    NOT NULL,
    source      TEXT    NOT NULL,
    observed    INTEGER NOT NULL,
    detail      TEXT,
    observed_at INTEGER NOT NULL,
    PRIMARY KEY (host, label, source)
);
CREATE INDEX IF NOT EXISTS idx_host_capability_observations_host
    ON host_capability_observations(host);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_host_capability_observations_host;
DROP TABLE IF EXISTS host_capability_observations;
-- +goose StatementEnd
