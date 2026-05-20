package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

// TestOpen_RestoresJobMetadataColumnOnUpgrade is a regression test for a
// wedged-schema bug: jobs.job_metadata — which the job_status view selects via
// COALESCE(la.job_metadata, j.job_metadata) — was added only in the legacy
// initSchema body with no matching versioned migration, so currentSchemaVersion
// never bumped. Existing DBs at the prior version fast-pathed past initSchema
// and never received the column, while startupRepair still recreated the
// job_status view that references it, wedging `weft jobs list` with
// "no such column: j.job_metadata". The column is now also a versioned
// migration, so the version bump routes existing DBs through the upgrade path.
func TestOpen_RestoresJobMetadataColumnOnUpgrade(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Simulate a DB that predates the job_metadata column, reporting the
	// schema version that shipped just before it was added. SQLite re-validates
	// dependent views on DROP COLUMN, so drop them first; the reopen below
	// recreates them.
	for _, v := range []string{"training_examples", "job_run_training_examples", "job_status", "launch_job_membership"} {
		if _, err := database.Exec(`DROP VIEW IF EXISTS ` + v); err != nil {
			t.Fatalf("drop view %s: %v", v, err)
		}
	}
	if _, err := database.Exec(`ALTER TABLE jobs DROP COLUMN job_metadata`); err != nil {
		t.Fatalf("drop job_metadata column: %v", err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion-1)); err != nil {
		t.Fatalf("rewind user_version: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must run the upgrade path and re-add jobs.job_metadata so the
	// job_status view startupRepair recreates is queryable.
	database, err = Open()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer database.Close()

	var meta sql.NullString
	if err := database.QueryRow(`SELECT job_metadata FROM job_status LIMIT 1`).Scan(&meta); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query job_status after reopen: %v", err)
	}
}

func TestEnsurePlacementRowInvariants_ReplacesLegacyNonUniqueIntentIndexes(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 6101, "echo one", "/tmp", StatusQueued)

	if _, err := database.Exec(`DROP INDEX IF EXISTS idx_move_intents_open`); err != nil {
		t.Fatalf("drop move index: %v", err)
	}
	if _, err := database.Exec(`CREATE INDEX idx_move_intents_open ON move_intents(job_id) WHERE state = 'open'`); err != nil {
		t.Fatalf("create legacy move index: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO move_intents (job_id, target_kind, state, created_at)
		VALUES (6101, 'new', 'open', 100),
		       (6101, 'new', 'open', 200)`); err != nil {
		t.Fatalf("insert duplicate move intents: %v", err)
	}

	if err := ensurePlacementRowInvariants(database); err != nil {
		t.Fatalf("ensurePlacementRowInvariants: %v", err)
	}

	var openCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM move_intents WHERE job_id = 6101 AND state = 'open'`).Scan(&openCount); err != nil {
		t.Fatalf("count open move intents: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("open move intents = %d, want 1", openCount)
	}
	_, err := database.Exec(`
		INSERT INTO move_intents (job_id, target_kind, state, created_at)
		VALUES (6101, 'new', 'open', 300)`)
	if err == nil {
		t.Fatal("second open move intent insert succeeded; want unique constraint failure")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second open move intent error = %v, want UNIQUE constraint", err)
	}
}

func TestEnsurePlacementRowInvariants_EnforcesOneOpenAttemptPerJob(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 6102, "echo one", "/tmp", StatusQueued)
	if _, err := database.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, status, queued_at)
		VALUES (6102, 2, '', 'queued', 200)`); err == nil {
		t.Fatal("second open attempt insert succeeded; want unique constraint failure")
	}
}

func TestEnsurePlacementRowInvariants_BlocksInventoryAndLaunchOnSameAttempt(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 6103, "echo one", "/tmp", StatusQueued)
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = 100 WHERE job_id = 6103`); err != nil {
		t.Fatalf("close initial attempt: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	_, err = database.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at)
		VALUES (6103, 2, 'cool30', ?, 'queued', 200)`, launchID)
	if err == nil {
		t.Fatal("mixed inventory host and launch insert succeeded; want trigger failure")
	}
	if !strings.Contains(err.Error(), "inventory host and launch") {
		t.Fatalf("mixed placement error = %v", err)
	}

	_, err = database.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at, end_time)
		VALUES (6103, 2, ?, ?, 'queued', 200, 201)`, LaunchHost(launchID), launchID)
	if err != nil {
		t.Fatalf("synthetic launch host should remain compatible: %v", err)
	}
}
