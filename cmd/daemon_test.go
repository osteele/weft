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
	"github.com/osteele/weft/internal/orchestration"
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
	workerStopped := make(chan struct{})
	daemonSyncAll = func(_ *sql.DB, _ *config.Config, opts syncorch.SyncOptions) syncorch.SyncResult {
		defer close(workerStopped)
		<-opts.Context.Done()
		return syncorch.SyncResult{AllCompleted: false}
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
	select {
	case <-workerStopped:
	default:
		t.Fatal("daemonSyncAll was still running after runDaemonPrePassSync returned")
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

func TestDaemonSyncCadence(t *testing.T) {
	busy := orchestration.WakeSnapshot{ActiveJobs: 1}
	quiet := orchestration.WakeSnapshot{}
	adaptive := orchestration.AutopilotCooldownIdle

	tests := []struct {
		name             string
		snapshot         orchestration.WakeSnapshot
		wait             time.Duration
		syncIncomplete   bool
		incompleteStreak int
		want             time.Duration
	}{
		{
			name:     "work in flight keeps the adaptive cadence",
			snapshot: busy,
			wait:     adaptive,
			want:     adaptive,
		},
		{
			name:     "quiet system backs off to the quiet interval",
			snapshot: quiet,
			wait:     adaptive,
			want:     daemonQuietSyncInterval,
		},
		{
			// An unreachable host leaves its state unknown. Unknown is a
			// reason to look again soon, not a reason to stand down — the
			// system may only look quiet because we could not see it.
			name:             "first incomplete sync holds the short cadence even when quiet",
			snapshot:         quiet,
			wait:             adaptive,
			syncIncomplete:   true,
			incompleteStreak: 1,
			want:             adaptive,
		},
		{
			name:             "second consecutive incomplete sync backs off",
			snapshot:         quiet,
			wait:             adaptive,
			syncIncomplete:   true,
			incompleteStreak: 2,
			want:             2 * adaptive,
		},
		{
			name:             "third consecutive incomplete sync doubles again",
			snapshot:         quiet,
			wait:             adaptive,
			syncIncomplete:   true,
			incompleteStreak: 3,
			want:             4 * adaptive,
		},
		{
			name:             "escalation caps at the quiet interval",
			snapshot:         quiet,
			wait:             adaptive,
			syncIncomplete:   true,
			incompleteStreak: 6,
			want:             daemonQuietSyncInterval,
		},
		{
			// A completed sync resets the streak: a leftover positive streak
			// must not keep escalating a quiet, synced system.
			name:             "a completed sync resets the streak",
			snapshot:         quiet,
			wait:             adaptive,
			incompleteStreak: 4,
			want:             daemonQuietSyncInterval,
		},
		{
			name:             "work in flight keeps the short cadence despite a long streak",
			snapshot:         busy,
			wait:             adaptive,
			syncIncomplete:   true,
			incompleteStreak: 7,
			want:             adaptive,
		},
		{
			name:     "a cooldown longer than the quiet interval is not shortened",
			snapshot: quiet,
			wait:     2 * daemonQuietSyncInterval,
			want:     2 * daemonQuietSyncInterval,
		},
		{
			name:             "incomplete sync never shortens a cooldown above the quiet interval",
			snapshot:         quiet,
			wait:             2 * daemonQuietSyncInterval,
			syncIncomplete:   true,
			incompleteStreak: 3,
			want:             2 * daemonQuietSyncInterval,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := daemonSyncCadence(tc.snapshot, tc.wait, tc.syncIncomplete, tc.incompleteStreak)
			if got != tc.want {
				t.Errorf("daemonSyncCadence = %s, want %s", got, tc.want)
			}
		})
	}
}

// Every field that makes a snapshot non-quiet must be one the daemon would
// want to sync for. A field missing from Quiet() lets the daemon back off to
// the five-minute cadence while work is in flight.
func TestWakeSnapshotQuietCoversEveryWorkSignal(t *testing.T) {
	tests := []struct {
		name     string
		snapshot orchestration.WakeSnapshot
	}{
		{name: "unplaced job", snapshot: orchestration.WakeSnapshot{UnplacedJobs: 1}},
		{name: "active job", snapshot: orchestration.WakeSnapshot{ActiveJobs: 1}},
		{name: "live launch", snapshot: orchestration.WakeSnapshot{LiveLaunches: 1}},
		{name: "open placement intent", snapshot: orchestration.WakeSnapshot{OpenPlacementIntents: 1}},
		{name: "open move intent", snapshot: orchestration.WakeSnapshot{OpenMoveIntents: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.snapshot.Quiet() {
				t.Errorf("%+v reads as quiet; the daemon would back off while work is in flight", tc.snapshot)
			}
		})
	}
	if !(orchestration.WakeSnapshot{}).Quiet() {
		t.Error("an empty snapshot must read as quiet")
	}
}

// The daemon's per-pass log line reports how long it will sleep, so it must be
// computed from the wait the loop actually applies rather than the autopilot
// cooldown alone.
func TestDaemonEffectiveWait(t *testing.T) {
	tests := []struct {
		name          string
		wait          time.Duration
		quietWait     time.Duration
		timerRunsPass bool
		want          time.Duration
	}{
		{
			name:          "armed timer shorter than the sync cadence wins",
			wait:          orchestration.AutopilotCooldownProgress,
			quietWait:     daemonQuietSyncInterval,
			timerRunsPass: true,
			want:          orchestration.AutopilotCooldownProgress,
		},
		{
			name:          "unarmed timer leaves the sync cadence",
			wait:          orchestration.AutopilotCooldownIdle,
			quietWait:     daemonQuietSyncInterval,
			timerRunsPass: false,
			want:          daemonQuietSyncInterval,
		},
		{
			name:          "sync cadence shorter than the cooldown wins",
			wait:          orchestration.AutopilotCooldownError,
			quietWait:     orchestration.AutopilotCooldownIdle,
			timerRunsPass: true,
			want:          orchestration.AutopilotCooldownIdle,
		},
		{
			name:          "backstop bounds a wait with nothing else armed",
			wait:          0,
			quietWait:     0,
			timerRunsPass: false,
			want:          orchestration.AutopilotQuietBackstop,
		},
		{
			name:          "backstop caps a longer cadence",
			wait:          0,
			quietWait:     2 * orchestration.AutopilotQuietBackstop,
			timerRunsPass: false,
			want:          orchestration.AutopilotQuietBackstop,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := daemonEffectiveWait(tc.wait, tc.quietWait, tc.timerRunsPass)
			if got != tc.want {
				t.Errorf("daemonEffectiveWait(%s, %s, %v) = %s, want %s",
					tc.wait, tc.quietWait, tc.timerRunsPass, got, tc.want)
			}
		})
	}
}

func TestNextIncompleteSyncStreak(t *testing.T) {
	if got := nextIncompleteSyncStreak(0, true); got != 1 {
		t.Errorf("first incomplete sync: streak = %d, want 1", got)
	}
	if got := nextIncompleteSyncStreak(3, true); got != 4 {
		t.Errorf("consecutive incomplete sync: streak = %d, want 4", got)
	}
	if got := nextIncompleteSyncStreak(3, false); got != 0 {
		t.Errorf("completed sync must reset the streak: streak = %d, want 0", got)
	}
}
