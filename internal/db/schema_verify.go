package db

import (
	"database/sql"
	"fmt"
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
				"migration was deferred because the database is held by another process. "+
				"Stop other weft processes and rerun. To find the holder: `lsof %s` "+
				"(look for `weft uj` / `weft autopilot`).",
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

// verifySchemaVersion compares the on-disk PRAGMA user_version with this
// binary's currentSchemaVersion. Returns ErrSchemaMismatch if they differ
// in either direction.
func verifySchemaVersion(database *sql.DB) error {
	v, err := readUserVersion(database)
	if err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v == currentSchemaVersion {
		return nil
	}
	return &ErrSchemaMismatch{
		DBVersion:     v,
		BinaryVersion: currentSchemaVersion,
		DBPath:        dbPath,
	}
}

// readUserVersion returns the on-disk schema version recorded in
// PRAGMA user_version. Cheap; safe on read-only connections.
func readUserVersion(database *sql.DB) (int, error) {
	var v int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
