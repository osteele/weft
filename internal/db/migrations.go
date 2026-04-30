package db

import (
	"database/sql"
	"fmt"
)

// migration is a single schema-versioned change. Its position in
// versionedMigrations determines the version it bumps to: the entry at
// index i brings the DB from baseSchemaVersion+i to baseSchemaVersion+i+1.
//
// To add a new schema change, append a new migration entry — there is no
// separate currentSchemaVersion constant to update. This makes the
// "added a migration but forgot to bump the version" bug class
// structurally impossible going forward.
type migration struct {
	// Description is a short label used in pre-migration backup names
	// and migration log lines. Example: "add jobs.foo column".
	Description string
	// Apply runs the migration against a writable handle. Implementations
	// must be idempotent (use addColumnIfMissing, CREATE TABLE IF NOT
	// EXISTS, etc.) so reruns after a partial-failure recovery are safe.
	Apply func(*sql.DB) error
}

// baseSchemaVersion is the highest schema version produced by the legacy
// monolithic initSchema body. DBs created or migrated by binaries from
// before the versionedMigrations refactor report this as user_version when
// they're at the latest state the legacy code knew about. Version numbers
// for new migrations start at baseSchemaVersion+1.
const baseSchemaVersion = 11

// versionedMigrations is the canonical list of schema changes added since
// baseSchemaVersion. Append to this slice to introduce a new migration —
// currentSchemaVersion derives from its length, so the version bumps
// automatically.
//
// Each Apply must be idempotent (see migration.Apply).
var versionedMigrations = []migration{
	// Append new migrations here, e.g.:
	// {
	//     Description: "add jobs.foo column",
	//     Apply: func(db *sql.DB) error {
	//         return addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN foo TEXT`)
	//     },
	// },
	{
		Description: "add move_intents table",
		Apply: func(db *sql.DB) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS move_intents (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					source_attempt_id INTEGER REFERENCES job_attempts(id),
					source_launch_id INTEGER REFERENCES launches(id),
					target_kind TEXT NOT NULL CHECK (target_kind IN ('existing','new')),
					target_launch_id INTEGER REFERENCES launches(id),
					target_offer_provider TEXT,
					target_offer_id TEXT,
					target_gpu_name TEXT,
					state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','confirmed','canceled','obsoleted')),
					created_at INTEGER NOT NULL,
					resolved_at INTEGER,
					resolution TEXT
				)`,
				`CREATE INDEX IF NOT EXISTS idx_move_intents_job ON move_intents(job_id)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_move_intents_open ON move_intents(job_id) WHERE state = 'open'`,
				`CREATE INDEX IF NOT EXISTS idx_move_intents_target_launch ON move_intents(target_launch_id) WHERE state = 'open'`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add placement_intents table",
		Apply: func(db *sql.DB) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS placement_intents (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					operation TEXT NOT NULL,
					state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','confirmed','canceled')),
					created_at INTEGER NOT NULL,
					resolved_at INTEGER,
					resolution TEXT
				)`,
				`CREATE INDEX IF NOT EXISTS idx_placement_intents_job ON placement_intents(job_id)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_placement_intents_open ON placement_intents(job_id) WHERE state = 'open'`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add launches.bootstrap_deadline_unix and launches.agent_ready_at_unix",
		Apply: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE launches ADD COLUMN bootstrap_deadline_unix INTEGER`,
				`ALTER TABLE launches ADD COLUMN agent_ready_at_unix INTEGER`,
			} {
				if err := addColumnIfMissing(db, stmt); err != nil {
					return err
				}
			}
			// Backfill bootstrap_deadline_unix for existing rows so the
			// reconciler can read a single column unconditionally instead
			// of falling back to an inline launched_at + timeout
			// computation. 1200s = 20m matches campaign.BootstrapTerminateTimeout.
			if _, err := db.Exec(`UPDATE launches
				SET bootstrap_deadline_unix = COALESCE(launched_at, created_at) + 1200
				WHERE bootstrap_deadline_unix IS NULL`); err != nil {
				return err
			}
			return nil
		},
	},
}

// currentSchemaVersion is the version this binary expects on disk. Derived
// from versionedMigrations so it advances automatically when a migration
// is appended. This eliminates the "forgot to bump after adding a
// migration" bug class that bit us in commit 2a698ef.
var currentSchemaVersion = baseSchemaVersion + len(versionedMigrations)

// runVersionedMigrations applies every entry in migrations whose target
// version is greater than fromVersion, advancing PRAGMA user_version
// after each one so a partial failure leaves the DB at a well-defined
// intermediate version. The migrations slice is a parameter (rather than
// reading the package global directly) so tests can drive the loop with
// fake migrations.
func runVersionedMigrations(db *sql.DB, fromVersion int, migrations []migration) error {
	for i, m := range migrations {
		targetVersion := baseSchemaVersion + i + 1
		if targetVersion <= fromVersion {
			continue
		}
		if err := m.Apply(db); err != nil {
			return fmt.Errorf("migration v%d (%s): %w", targetVersion, m.Description, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, targetVersion)); err != nil {
			return fmt.Errorf("set user_version=%d: %w", targetVersion, err)
		}
	}
	return nil
}
