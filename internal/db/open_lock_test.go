package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_StartupRepairDatabaseLockedIsDeferred(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "jobs.db")
	seedPendingMigrationTestDBFile(t, tmpPath)

	restorePath := SetDBPath(tmpPath)
	t.Cleanup(restorePath)

	origStartupRepair := startupRepairFn
	startupRepairFn = func(*sql.DB) error {
		return fmt.Errorf("close duplicate open attempts: database is locked (5) (SQLITE_BUSY)")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	database, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	if database == nil {
		t.Fatal("Open() returned nil database")
	}
	database.Close()
}

func TestOpen_StartupRepairNonLockStillFails(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "jobs.db")
	seedPendingMigrationTestDBFile(t, tmpPath)

	restorePath := SetDBPath(tmpPath)
	t.Cleanup(restorePath)

	origStartupRepair := startupRepairFn
	startupRepairFn = func(*sql.DB) error {
		return errors.New("startup repair exploded")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	database, err := Open()
	if err == nil {
		if database != nil {
			database.Close()
		}
		t.Fatal("Open() error = nil, want failure")
	}
	if database != nil {
		database.Close()
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "startup repair: startup repair exploded") {
		t.Fatalf("Open() error = %q, want startup repair failure", got)
	}
}

func TestOpen_StartupRepairReadOnlyIsDeferred(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "jobs.db")
	seedPendingMigrationTestDBFile(t, tmpPath)

	restorePath := SetDBPath(tmpPath)
	t.Cleanup(restorePath)

	origStartupRepair := startupRepairFn
	startupRepairFn = func(*sql.DB) error {
		return fmt.Errorf("close duplicate open attempts: attempt to write a readonly database (8)")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	database, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	if database == nil {
		t.Fatal("Open() returned nil database")
	}
	database.Close()
}

func TestOpen_CurrentSchemaSkipsStartupRepair(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "jobs.db")
	seedCurrentTestDBFile(t, tmpPath)

	restorePath := SetDBPath(tmpPath)
	t.Cleanup(restorePath)

	origStartupRepair := startupRepairFn
	calls := 0
	startupRepairFn = func(*sql.DB) error {
		calls++
		return errors.New("startup repair should not run")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	database, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	database.Close()
	if calls != 0 {
		t.Fatalf("startupRepair calls = %d, want 0", calls)
	}
}

func TestOpenForReading_CurrentSchemaSkipsWritableStartupPath(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "jobs.db")
	seedCurrentTestDBFile(t, tmpPath)

	restorePath := SetDBPath(tmpPath)
	t.Cleanup(restorePath)

	origStartupRepair := startupRepairFn
	calls := 0
	startupRepairFn = func(*sql.DB) error {
		calls++
		return errors.New("startup repair should not run")
	}
	t.Cleanup(func() { startupRepairFn = origStartupRepair })

	database, err := OpenForReading()
	if err != nil {
		t.Fatalf("OpenForReading() error = %v, want nil", err)
	}
	database.Close()
	if calls != 0 {
		t.Fatalf("startupRepair calls = %d, want 0", calls)
	}
}
