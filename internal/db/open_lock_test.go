package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestOpen_StartupRepairDatabaseLockedIsDeferred(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "weft-open-lock-*.db")
	if err != nil {
		t.Fatalf("create temp db path: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

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
	tmpFile, err := os.CreateTemp("", "weft-open-repair-*.db")
	if err != nil {
		t.Fatalf("create temp db path: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

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
	tmpFile, err := os.CreateTemp("", "weft-open-readonly-*.db")
	if err != nil {
		t.Fatalf("create temp db path: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

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
	tmpFile, err := os.CreateTemp("", "weft-open-current-*.db")
	if err != nil {
		t.Fatalf("create temp db path: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, testDBTemplate(t), 0o600); err != nil {
		t.Fatalf("seed current schema database: %v", err)
	}

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
	tmpFile, err := os.CreateTemp("", "weft-open-read-current-*.db")
	if err != nil {
		t.Fatalf("create temp db path: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, testDBTemplate(t), 0o600); err != nil {
		t.Fatalf("seed current schema database: %v", err)
	}

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
