package db

import (
	"database/sql"
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
