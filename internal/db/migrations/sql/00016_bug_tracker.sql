-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS bugs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    title TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'bug',
    scope TEXT NOT NULL DEFAULT 'infrastructure',
    likelihood TEXT NOT NULL DEFAULT 'unknown',
    severity TEXT NOT NULL DEFAULT 'notice',
    fingerprint TEXT NOT NULL,
    job_id INTEGER REFERENCES jobs(id),
    host TEXT,
    summary TEXT,
    detail TEXT,
    occurrences INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    closed_at INTEGER,
    close_reason TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bugs_open_fingerprint
    ON bugs(fingerprint)
    WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_bugs_status_updated
    ON bugs(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS bug_notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    bug_id INTEGER NOT NULL REFERENCES bugs(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bug_notes_bug_created
    ON bug_notes(bug_id, created_at ASC, id ASC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bug_notes;
DROP INDEX IF EXISTS idx_bugs_status_updated;
DROP INDEX IF EXISTS idx_bugs_open_fingerprint;
DROP TABLE IF EXISTS bugs;
-- +goose StatementEnd
