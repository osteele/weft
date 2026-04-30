package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshot_ProducesValidStandaloneCopy(t *testing.T) {
	src := setupBackupTestDB(t)
	defer os.Remove(dbPath)

	if _, err := src.Exec(`CREATE TABLE x (a INTEGER PRIMARY KEY, b TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := src.Exec(`INSERT INTO x (a, b) VALUES (1, 'hello'), (2, 'world')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := Snapshot(src, dest); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Snapshot file must exist and be readable as an independent SQLite DB.
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		t.Fatalf("snapshot file missing or empty: %v size=%d", err, info.Size())
	}
	copy, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer copy.Close()
	var n int
	if err := copy.QueryRow(`SELECT COUNT(*) FROM x`).Scan(&n); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if n != 2 {
		t.Fatalf("snapshot row count = %d, want 2", n)
	}
}

func TestSnapshot_RefusesExistingDestination(t *testing.T) {
	src := setupBackupTestDB(t)
	defer os.Remove(dbPath)

	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(dest, []byte("existing"), 0o644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	err := Snapshot(src, dest)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got %v", err)
	}
}

func TestPruneSnapshots_KeepsNewestN(t *testing.T) {
	tmp := t.TempDir()
	dbFile := filepath.Join(tmp, "jobs.db")
	if err := os.WriteFile(dbFile, []byte{}, 0o644); err != nil {
		t.Fatalf("touch dbFile: %v", err)
	}
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	// Create 5 snapshot files with staggered mtimes.
	dir := SnapshotDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("jobs.db.snapshot-%d.db", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		mtime := now.Add(time.Duration(-i) * time.Hour)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	deleted, err := PruneSnapshots(2)
	if err != nil {
		t.Fatalf("PruneSnapshots: %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted = %d, want 3", len(deleted))
	}
	// Two newest (i=0, i=1) must remain; i=2..4 must be gone.
	remaining, err := ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2", len(remaining))
	}
	for _, p := range remaining {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, "-0.db") && !strings.HasSuffix(base, "-1.db") {
			t.Errorf("kept unexpected snapshot: %s", base)
		}
	}
}

func TestOpen_TakesMigrationBackupOnUpgrade(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	// First Open creates the schema and writes user_version=current.
	database, err := Open()
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Drop a column and roll user_version back to simulate "old DB".
	dropJobsViewsForTest(t, database)
	if _, err := database.Exec(`ALTER TABLE jobs DROP COLUMN max_compute_cap`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion-1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	database.Close()

	// Snapshot directory must be empty before second Open.
	if entries, _ := os.ReadDir(SnapshotDir()); len(entries) != 0 {
		t.Fatalf("snapshot dir not empty before upgrade: %d entries", len(entries))
	}

	// Second Open must take a backup before running migrations.
	database, err = Open()
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer database.Close()

	snaps, err := ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 pre-migration snapshot, got %d: %v", len(snaps), snaps)
	}
	if !strings.Contains(filepath.Base(snaps[0]), migrationBackupPrefix) {
		t.Errorf("snapshot name missing pre-migration prefix: %s", snaps[0])
	}
}

func TestOpen_NoMigrationBackupOnFreshDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := SetDBPath(dbFile)
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	snaps, _ := ListSnapshots()
	if len(snaps) != 0 {
		t.Fatalf("expected no snapshots on fresh DB; got %d: %v", len(snaps), snaps)
	}
}

func setupBackupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "weft-snap-*.db")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(cleanup)

	database, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)", tmpFile.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}
