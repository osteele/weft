package cmd

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
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

func TestRunDaemonPrePassSyncTimesOut(t *testing.T) {
	oldSyncAll := daemonSyncAll
	oldGrace := daemonPrePassSyncGrace
	t.Cleanup(func() {
		daemonSyncAll = oldSyncAll
		daemonPrePassSyncGrace = oldGrace
	})

	daemonPrePassSyncGrace = 0
	daemonSyncAll = func(*sql.DB, *config.Config, syncorch.SyncOptions) syncorch.SyncResult {
		time.Sleep(time.Minute)
		return syncorch.SyncResult{AllCompleted: true}
	}

	start := time.Now()
	result := runDaemonPrePassSync(context.Background(), nil, nil, syncorch.SyncOptions{
		HostTimeout:  10 * time.Millisecond,
		CloudMode:    syncorch.CloudBounded,
		CloudTimeout: 10 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("runDaemonPrePassSync took %s, want bounded return", elapsed)
	}
	if result.AllCompleted {
		t.Fatal("AllCompleted = true, want false after timeout")
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "pre-pass sync exceeded") {
		t.Fatalf("Warnings = %#v, want pre-pass timeout warning", result.Warnings)
	}
}
