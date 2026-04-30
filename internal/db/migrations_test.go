package db

import (
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCurrentSchemaVersion_DerivesFromMigrationList(t *testing.T) {
	if currentSchemaVersion != baseSchemaVersion+len(versionedMigrations) {
		t.Fatalf(
			"currentSchemaVersion (%d) is not derived from migration list (base %d + len %d = %d); "+
				"the constant must remain a derived var so adding a migration auto-bumps it",
			currentSchemaVersion, baseSchemaVersion, len(versionedMigrations),
			baseSchemaVersion+len(versionedMigrations),
		)
	}
}

func TestRunVersionedMigrations_SkipsAlreadyApplied(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	var ran atomic.Int32
	migrations := []migration{
		{Description: "first", Apply: func(*sql.DB) error { ran.Add(1); return nil }},
		{Description: "second", Apply: func(*sql.DB) error { ran.Add(1); return nil }},
	}

	// fromVersion at baseSchemaVersion means both migrations are pending.
	if err := runVersionedMigrations(database, baseSchemaVersion, migrations); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("first run applied %d migrations, want 2", got)
	}

	v, err := readUserVersion(database)
	if err != nil {
		t.Fatalf("readUserVersion: %v", err)
	}
	if v != baseSchemaVersion+2 {
		t.Fatalf("user_version after run = %d, want %d", v, baseSchemaVersion+2)
	}

	// Re-running with the same fromVersion+migrations should be a no-op.
	ran.Store(0)
	if err := runVersionedMigrations(database, v, migrations); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := ran.Load(); got != 0 {
		t.Fatalf("rerun applied %d migrations, want 0", got)
	}
}

func TestRunVersionedMigrations_AppliesOnlyNewerThanFromVersion(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	var applied []string
	migrations := []migration{
		{Description: "first", Apply: func(*sql.DB) error { applied = append(applied, "first"); return nil }},
		{Description: "second", Apply: func(*sql.DB) error { applied = append(applied, "second"); return nil }},
		{Description: "third", Apply: func(*sql.DB) error { applied = append(applied, "third"); return nil }},
	}

	// Pretend the DB is already at version baseSchemaVersion+2: "first" and
	// "second" should be skipped, only "third" runs.
	if err := runVersionedMigrations(database, baseSchemaVersion+2, migrations); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(applied) != 1 || applied[0] != "third" {
		t.Fatalf("applied = %v, want [third]", applied)
	}
	v, err := readUserVersion(database)
	if err != nil {
		t.Fatalf("readUserVersion: %v", err)
	}
	if v != baseSchemaVersion+3 {
		t.Fatalf("user_version after run = %d, want %d", v, baseSchemaVersion+3)
	}
}
