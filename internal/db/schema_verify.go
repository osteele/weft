package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db/migrations"
)

// ErrSchemaMismatch signals an incompatibility between this binary's
// expected schema version and the on-disk database. The direction matters:
//
//   - DBVersion < BinaryVersion: a migration is owed but did not run. The
//     usual cause is that the database was open via the read-only fallback
//     in OpenForReading (typically because another process held the writer
//     lock) so initSchema's migration step never executed.
//
//   - DBVersion > BinaryVersion: this binary is older than the on-disk DB
//     (a newer weft has migrated it). Continuing to use the binary would
//     be unsafe; upgrade weft.
//
// The error message includes recovery instructions for both directions.
type ErrSchemaMismatch struct {
	DBVersion     int
	BinaryVersion int
	DBPath        string
}

func (e *ErrSchemaMismatch) Error() string {
	if e.DBVersion < e.BinaryVersion {
		return fmt.Sprintf(
			"database schema is at version %d but this weft binary expects %d; "+
				"migration was deferred because the database could not be opened writable. "+
				"Stop other weft processes and rerun. If the holder is the daemon, use "+
				"`weft daemon stop` rather than killing the PID directly because launchd may restart it. "+
				"To find the holder: `lsof %s` (look for `weft uj`, `weft autopilot`, or `weft daemon`).",
			e.DBVersion, e.BinaryVersion, e.DBPath,
		)
	}
	return fmt.Sprintf(
		"database schema is at version %d but this weft binary only knows version %d; "+
			"the database was migrated by a newer weft. Upgrade weft to a version that "+
			"supports schema v%d (DB path: %s).",
		e.DBVersion, e.BinaryVersion, e.DBVersion, e.DBPath,
	)
}

// verifySchemaVersion compares the migration version applied to the database
// (tracked by goose) against the highest version this binary knows about.
// Returns ErrSchemaMismatch if they differ in either direction.
func verifySchemaVersion(database *sql.DB) error {
	dbVersion := int(migrations.Version(context.Background(), database))
	target := int(migrations.Target())
	if dbVersion == target {
		return nil
	}
	return &ErrSchemaMismatch{
		DBVersion:     dbVersion,
		BinaryVersion: target,
		DBPath:        dbPath,
	}
}

// ObservedSchemaVersions returns (version this binary expects, version applied
// to the database). Used by schema-drift monitors to compare what the binary
// expects against what the DB reports.
func ObservedSchemaVersions(database *sql.DB) (current int, observed int, err error) {
	return int(migrations.Target()), int(migrations.Version(context.Background(), database)), nil
}
