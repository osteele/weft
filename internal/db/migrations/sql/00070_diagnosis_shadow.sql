-- Record TypeSafe Jev's classification of each recently failed job next to
-- weft's regex diagnoses, for a measurement-only comparison. One row per job;
-- only successful judgments are stored, so a missing row means "not judged
-- yet", never "judged and found nothing". Nothing reads this table to act.
-- The row is derived data, so it cascades with its job rather than blocking
-- a job cleanup.

-- +goose Up
CREATE TABLE IF NOT EXISTS diagnosis_shadow (
    job_id              INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    attempt_id          INTEGER,
    judged_at           INTEGER NOT NULL,
    model               TEXT,
    display_pattern     TEXT,
    remediation_pattern TEXT,
    fatal_pattern       TEXT,
    jev_category        TEXT,
    jev_confidence      REAL,
    jev_probabilities   TEXT, -- JSON object: category -> probability
    regex_supported     REAL, -- NULL when no display or remediation pattern
    fatal_supported     REAL, -- NULL when no fatal pattern
    latency_ms          INTEGER,
    input_tokens        INTEGER
);

CREATE INDEX IF NOT EXISTS idx_diagnosis_shadow_judged_at
    ON diagnosis_shadow(judged_at);

-- +goose Down
DROP INDEX IF EXISTS idx_diagnosis_shadow_judged_at;
DROP TABLE IF EXISTS diagnosis_shadow;
