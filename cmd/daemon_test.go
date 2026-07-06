package cmd

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/syncorch"
)

func setupDaemonTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "weft-daemon-test-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	_ = tmpFile.Close()

	cleanup := db.SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		_ = os.Remove(tmpFile.Name())
	})

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestCapDaemonWaitForInterruptibles(t *testing.T) {
	database := setupDaemonTestDB(t)
	longWait := time.Minute

	if got := capDaemonWaitForInterruptibles(database, longWait); got != longWait {
		t.Fatalf("wait without interruptibles = %s, want %s", got, longWait)
	}

	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:       db.LaunchStatusRunning,
		Provider:     "vastai",
		InstanceType: cloud.InstanceTypeInterruptible,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if got := capDaemonWaitForInterruptibles(database, longWait); got != daemonInterruptiblePollInterval {
		t.Fatalf("wait with interruptible = %s, want %s", got, daemonInterruptiblePollInterval)
	}
}

func TestDaemonPrePassSyncUsesFastHostTimeout(t *testing.T) {
	if got := daemonPrePassHostTimeout(); got != FastSyncHostTimeout {
		t.Fatalf("daemonPrePassHostTimeout() = %s, want %s", got, FastSyncHostTimeout)
	}
	if daemonPrePassHostTimeout() >= syncorch.NormalHostTimeout {
		t.Fatalf("daemon pre-pass host timeout = %s, must not block autopilot behind full host sync", daemonPrePassHostTimeout())
	}
}
