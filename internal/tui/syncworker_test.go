package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuerunner"
)

// TestSyncWorkerNoJobsNoSyncTime verifies that when there are no jobs to sync,
// no sync time is recorded (since we didn't actually contact the host).
func TestSyncWorkerNoJobsNoSyncTime(t *testing.T) {
	database := db.SetupTestDB(t)
	testHost := "test-host-no-jobs"

	// Clear any existing sync time
	_, _ = database.Exec(`DELETE FROM host_syncs WHERE name = ?`, testHost)

	// Create a sync worker
	worker := NewSyncWorker(database, nil, nil, nil)
	worker.Start()
	defer worker.Stop()

	// DON'T create any jobs - just trigger a sync request
	worker.Request(SyncRequest{Host: testHost, Rate: RateRunning, Priority: true})

	// Wait for the sync to process
	time.Sleep(1 * time.Second)

	// Verify NO sync time was recorded (no jobs = no host contact)
	times, err := db.LoadHostSyncTimes(database)
	if err != nil {
		t.Fatalf("LoadHostSyncTimes: %v", err)
	}

	if _, ok := times[testHost]; ok {
		t.Errorf("Sync time should NOT be recorded when there are no jobs to sync")
	}
}

func TestGetHostSyncRateUsesEffectiveStatus(t *testing.T) {
	queuedRunning := db.StatusRunning
	jobs := []*db.Job{
		{Status: db.StatusQueued, PendingStatus: &queuedRunning, Host: "test-host"},
	}

	if got := GetHostSyncRate(jobs); got != RateRunning {
		t.Fatalf("GetHostSyncRate() = %v, want %v", got, RateRunning)
	}
}

func TestBuildSyncWarning(t *testing.T) {
	tests := []struct {
		name   string
		result SyncResult
		want   string
	}{
		{
			name: "agent deploy failure",
			result: SyncResult{
				QueueRunnerError: "agent deploy failed: version mismatch",
			},
			want: "agent deploy failed: version mismatch",
		},
		{
			name: "queue runner start failure",
			result: SyncResult{
				QueueRunnerError: "queue runner start failed: tmux missing",
			},
			want: "queue runner start failed: tmux missing",
		},
		{
			name: "queue dispatch failure",
			result: SyncResult{
				QueueDispatchError: "job 289 source sync failed",
			},
			want: "queue dispatch failed: job 289 source sync failed",
		},
		{
			name: "multiple failures collapse into one warning",
			result: SyncResult{
				QueueRunnerError:   "agent deploy failed: version mismatch",
				QueueDispatchError: "job 289 source sync failed",
			},
			want: "agent deploy failed: version mismatch; queue dispatch failed: job 289 source sync failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSyncWarning(dbToHostSyncResult(tt.result))
			if got != tt.want {
				t.Fatalf("buildSyncWarning() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleSyncResultStoresAndClearsHostWarning(t *testing.T) {
	database := db.SetupTestDB(t)
	m := NewModel(database)
	m.hosts = []*Host{{Name: "studio"}}

	result := SyncResult{
		Host:        "studio",
		HostWarning: "agent deploy failed: version mismatch",
	}
	m2, _ := m.handleSyncResult(syncResultMsg{result: result})

	if m2.flash.Message != "Sync warning (studio): agent deploy failed: version mismatch" {
		t.Fatalf("flashMessage = %q", m2.flash.Message)
	}
	if !m2.flash.IsError {
		t.Fatal("flashIsError = false, want true")
	}
	if got := m2.hosts[0].SyncWarning; got != result.HostWarning {
		t.Fatalf("host.SyncWarning = %q, want %q", got, result.HostWarning)
	}

	m3, _ := m2.handleSyncResult(syncResultMsg{result: SyncResult{Host: "studio"}})
	if got := m3.hosts[0].SyncWarning; got != "" {
		t.Fatalf("host.SyncWarning = %q, want empty", got)
	}
}

func TestHandleSyncResultDoesNotOverrideSpecificWarningWithQueueStoppedFlash(t *testing.T) {
	database := db.SetupTestDB(t)
	_, err := db.RecordQueued(database, "studio", "/tmp", "echo test", "queued job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	m := NewModel(database)
	m.hosts = []*Host{{Name: "studio"}}

	result := SyncResult{
		Host:        "studio",
		HostWarning: "queue runner start failed: tmux missing",
		QueueStatus: &queuerunner.StatusInfo{
			RunnerActive: false,
		},
	}
	m2, _ := m.handleSyncResult(syncResultMsg{result: result})

	if got := m2.flash.Message; !strings.Contains(got, "Sync warning (studio): queue runner start failed: tmux missing") {
		t.Fatalf("flashMessage = %q", got)
	}
}

func TestEnsureQueueRunnerStartedTUISurfacesAgentDeployFailure(t *testing.T) {
	originalFindHostSpec := findHostSpecFunc
	originalEnsureAgentUpToDate := ensureAgentUpToDateFunc
	t.Cleanup(func() {
		findHostSpecFunc = originalFindHostSpec
		ensureAgentUpToDateFunc = originalEnsureAgentUpToDate
	})

	findHostSpecFunc = func(host string) *inventory.HostSpec {
		return &inventory.HostSpec{Name: host, OS: "linux", Arch: "amd64"}
	}
	ensureAgentUpToDateFunc = func(host string, spec inventory.HostSpec) (bool, error) {
		return false, os.ErrPermission
	}

	started, err := ensureQueueRunnerStartedTUI("studio")
	if err == nil {
		t.Fatal("expected error")
	}
	if started {
		t.Fatal("started = true, want false")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected os.ErrPermission in error chain, got: %v", err)
	}
}

func TestRenderHostDetailPanelShowsSyncWarning(t *testing.T) {
	database := db.SetupTestDB(t)
	m := NewModel(database)
	m.width = 120
	m.height = 40
	m.selectedHostIdx = 0
	m.hosts = []*Host{{
		Name:        "studio",
		Status:      HostStatusOnline,
		SyncWarning: "queue dispatch failed: source sync failed",
	}}

	rendered := m.renderHostDetailPanel(20)
	if !strings.Contains(rendered, "Sync warning: queue dispatch failed: source sync failed") {
		t.Fatalf("rendered host detail missing sync warning: %q", rendered)
	}
}

func dbToHostSyncResult(result SyncResult) ops.HostSyncResult {
	return ops.HostSyncResult{
		QueueDispatchError: result.QueueDispatchError,
		QueueRunnerError:   result.QueueRunnerError,
	}
}
