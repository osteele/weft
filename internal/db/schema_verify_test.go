package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// jobsTableViews lists the views that reference jobs columns and must be
// dropped before any test ALTER TABLE jobs ... DROP COLUMN. The next Open()
// recreates them via createJobStateViews.
var jobsTableViews = []string{
	"launch_job_membership", "job_status", "job_run_training_examples",
	"training_examples", "job_effective_state", "all_runs",
	"cloud_instance_job_membership",
}

func dropJobsViewsForTest(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, v := range jobsTableViews {
		if _, err := database.Exec(`DROP VIEW IF EXISTS ` + v); err != nil {
			t.Fatalf("drop view %s: %v", v, err)
		}
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
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion-1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	err = verifySchemaVersion(database)
	if err == nil {
		t.Fatal("expected ErrSchemaMismatch, got nil")
	}
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch, got %T: %v", err, err)
	}
	if e.DBVersion != currentSchemaVersion-1 || e.BinaryVersion != currentSchemaVersion {
		t.Fatalf("got DBVersion=%d BinaryVersion=%d", e.DBVersion, e.BinaryVersion)
	}
	if !strings.Contains(err.Error(), "Stop other weft processes") {
		t.Fatalf("error message lacks recovery hint for owed-migration case: %s", err.Error())
	}
}

func TestVerifySchemaVersion_DetectsDBNewerThanBinary(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion+5)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	err = verifySchemaVersion(database)
	if err == nil {
		t.Fatal("expected ErrSchemaMismatch, got nil")
	}
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch, got %T: %v", err, err)
	}
	if e.DBVersion != currentSchemaVersion+5 || e.BinaryVersion != currentSchemaVersion {
		t.Fatalf("got DBVersion=%d BinaryVersion=%d", e.DBVersion, e.BinaryVersion)
	}
	if !strings.Contains(err.Error(), "Upgrade weft") {
		t.Fatalf("error message lacks upgrade hint for binary-outdated case: %s", err.Error())
	}
}

func TestOpenForReading_RefusesStaleSchema(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	// Force user_version *forward* past currentSchemaVersion. initSchema's
	// fast-path then skips migrations (the schema is already "ahead"), so
	// the post-Open verify must catch the mismatch. Rolling backward
	// instead would just make migrations re-run and clear the mismatch
	// before verify gets a chance to look.
	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion+1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	database.Close()

	_, err = OpenForReading()
	if err == nil {
		t.Fatal("expected ErrSchemaMismatch from OpenForReading, got nil")
	}
	var e *ErrSchemaMismatch
	if !errors.As(err, &e) {
		t.Fatalf("expected ErrSchemaMismatch, got %T: %v", err, err)
	}
}

func TestOpenForReading_PreservesOpenNonLockError(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
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
