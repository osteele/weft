package dbwatch

import (
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/config"
)

// The autopilot's run-rate target lives in the config file, and editing it
// produces no database write. A change source that watches only the database
// sleeps through the edit, so the config file has to be a watched target.
func TestTargetsIncludeConfigFile(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	targets := Targets(dbFile)

	configPath := config.ConfigPath()
	if configPath == "" {
		t.Skip("no config path configured in this environment")
	}
	if !IsWatchedFile(configPath, targets) {
		t.Errorf("config file %q is not watched; a budget edit would not wake the autopilot", configPath)
	}
}

func TestTargetsIncludeDatabaseSidecars(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	targets := Targets(dbFile)

	for _, name := range []string{dbFile, dbFile + "-wal", dbFile + "-shm"} {
		if !IsWatchedFile(name, targets) {
			t.Errorf("%q is not watched", name)
		}
	}
	if IsWatchedFile(filepath.Join(filepath.Dir(dbFile), "unrelated.txt"), targets) {
		t.Error("unrelated files must not be watched")
	}
}

func TestTargetsEmptyDBPath(t *testing.T) {
	if targets := Targets(""); targets != nil {
		t.Errorf("Targets(\"\") = %v, want nil", targets)
	}
}
