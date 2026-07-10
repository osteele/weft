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

func TestReleaseDaemonAutopilotClaimClearsStoppedDaemonPID(t *testing.T) {
	database := setupDaemonTestDB(t)
	const daemonPID = 4242
	claimed, paused, existing, err := db.TryClaimAutopilotPass(database, daemonPID, "daemon/4242", "host", time.Minute, db.BinaryIdentity{})
	if err != nil {
		t.Fatalf("TryClaimAutopilotPass: %v", err)
	}
	if !claimed || paused || existing != nil {
		t.Fatalf("claim = %v paused = %v existing = %+v, want fresh claim", claimed, paused, existing)
	}

	if err := releaseDaemonAutopilotClaim(daemonPID, "daemon stopped"); err != nil {
		t.Fatalf("releaseDaemonAutopilotClaim: %v", err)
	}

	state, err := db.LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state.ActiveRunnerPID != 0 || !state.PassStartedAt.IsZero() || !state.LastHeartbeat.IsZero() {
		t.Fatalf("active autopilot state = %+v, want cleared", state)
	}
	if state.LastPassSummary != "daemon stopped" {
		t.Fatalf("last pass summary = %q, want daemon stopped", state.LastPassSummary)
	}
}

func TestReleaseDaemonAutopilotClaimDoesNotClearOtherPID(t *testing.T) {
	database := setupDaemonTestDB(t)
	const daemonPID = 4242
	claimed, _, _, err := db.TryClaimAutopilotPass(database, daemonPID, "daemon/4242", "host", time.Minute, db.BinaryIdentity{})
	if err != nil {
		t.Fatalf("TryClaimAutopilotPass: %v", err)
	}
	if !claimed {
		t.Fatal("expected fresh autopilot claim")
	}

	if err := releaseDaemonAutopilotClaim(9999, "daemon stopped"); err != nil {
		t.Fatalf("releaseDaemonAutopilotClaim: %v", err)
	}

	state, err := db.LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state.ActiveRunnerPID != daemonPID {
		t.Fatalf("active runner pid = %d, want %d", state.ActiveRunnerPID, daemonPID)
	}
}
