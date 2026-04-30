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
