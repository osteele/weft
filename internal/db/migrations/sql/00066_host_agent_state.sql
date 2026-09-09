-- Cache independently observed inventory-agent deployment and runtime identity.

-- +goose Up
CREATE TABLE IF NOT EXISTS host_agent_state (
    host                    TEXT PRIMARY KEY,
    deployed_version        TEXT,
    deployed_observed_at    INTEGER,
    running_version         TEXT,
    queue_protocol_version  INTEGER,
    running_observed_at     INTEGER
);

-- +goose Down
DROP TABLE IF EXISTS host_agent_state;
