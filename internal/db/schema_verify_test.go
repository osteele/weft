package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// setGooseVersionForTest forces the goose-recorded schema version of database
// to v. v <= 0 removes the version table entirely, modelling a pre-goose
// database that owes the baseline migration.
func setGooseVersionForTest(t *testing.T, database *sql.DB, v int) {
	t.Helper()
	if v <= 0 {
		if _, err := database.Exec(`DROP TABLE IF EXISTS goose_db_version`); err != nil {
			t.Fatalf("drop goose_db_version: %v", err)
		}
		return
	}
	if _, err := database.Exec(`DELETE FROM goose_db_version WHERE version_id > ?`, v); err != nil {
		t.Fatalf("lower goose version to %d: %v", v, err)
	}
	if _, err := database.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied)
		 SELECT ?, 1
		 WHERE NOT EXISTS (SELECT 1 FROM goose_db_version WHERE version_id = ?)`,
		v, v,
	); err != nil {
		t.Fatalf("set goose version %d: %v", v, err)
	}
}

func TestVerifySchemaVersion_PassesOnFreshDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if err := verifySchemaVersion(database); err != nil {
		t.Fatalf("verifySchemaVersion on fresh DB: %v", err)
	}
}

func TestVerifySchemaVersion_DetectsBinaryNewerThanDB(t *testing.T) {
	database := SetupTestDB(t)

	// Remove goose's version record: the DB now looks unmigrated.
	setGooseVersionForTest(t, database, 0)

	err := verifySchemaVersion(database)
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch, got %T: %v", err, err)
	}
	if e.DBVersion >= e.BinaryVersion {
		t.Fatalf("got DBVersion=%d BinaryVersion=%d, want DB behind binary", e.DBVersion, e.BinaryVersion)
	}
	if !strings.Contains(err.Error(), "Stop other weft processes") {
		t.Fatalf("error message lacks recovery hint for owed-migration case: %s", err.Error())
	}
}

func TestVerifySchemaVersion_DetectsDBNewerThanBinary(t *testing.T) {
	database := SetupTestDB(t)

	// Record a migration version far beyond what this binary knows.
	setGooseVersionForTest(t, database, 999)

	err := verifySchemaVersion(database)
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch, got %T: %v", err, err)
	}
	if e.DBVersion != 999 || e.DBVersion <= e.BinaryVersion {
		t.Fatalf("got DBVersion=%d BinaryVersion=%d, want DB ahead of binary", e.DBVersion, e.BinaryVersion)
	}
	if !strings.Contains(err.Error(), "Upgrade weft") {
		t.Fatalf("error message lacks upgrade hint for binary-outdated case: %s", err.Error())
	}
}

func TestOpenForReading_RefusesStaleSchema(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	seedCurrentTestDBFile(t, dbFile)
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	// Record a version ahead of this binary; initSchema then has nothing
	// pending, so the post-Open verify must catch the mismatch.
	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	setGooseVersionForTest(t, database, 999)
	database.Close()

	_, err = OpenForReading()
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch from OpenForReading, got %T: %v", err, err)
	}
}

func TestOpenForReading_PreservesOpenNonLockError(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	seedPendingMigrationTestDBFile(t, dbFile)
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	origStartupRepair := startupRepairFn
	startupRepairFn = func(*sql.DB) error {
		return errors.New("startup repair exploded")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	_, err := OpenForReading()
	if err == nil {
		t.Fatal("expected OpenForReading error, got nil")
	}
	if strings.Contains(err.Error(), "database schema is at version") {
		t.Fatalf("OpenForReading hid the real Open error behind schema mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "startup repair exploded") {
		t.Fatalf("OpenForReading error = %v, want startup repair failure", err)
	}
}
