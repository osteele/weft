package ops

import (
	"encoding/json"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
)

func TestFetchLaunchHostStatusFromDB_UsesLiveState(t *testing.T) {
	database := db.SetupTestDB(t)

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:             db.LaunchStatusRunning,
		Provider:           "vastai",
		ResolvedGPUName:    "RTX 4090",
		GPUSpec:            "RTX 4090",
		GPUMemGB:           24,
		CostPerHourCents:   99,
		ProviderInstanceID: "abc",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	hb := launchHeartbeatSample{
		Ts:             1_700_000_000,
		GPUUtilPct:     67,
		GPUMemUsedMiB:  10240,
		GPUMemTotalMiB: 24576,
		GPUTempC:       72,
		HostRSSKB:      8 * 1024 * 1024,
		HostMemTotalKB: 16 * 1024 * 1024,
		DiskFreeBytes:  50 * 1024 * 1024 * 1024,
		DiskTotalBytes: 100 * 1024 * 1024 * 1024,
	}
	hbJSON, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       launchID,
		HeartbeatJSON:  string(hbJSON),
		HeartbeatTS:    hb.Ts,
		JobProgressPct: 50,
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	result, err := FetchLaunchHostStatusFromDB(database, db.LaunchHost(launchID))
	if err != nil {
		t.Fatalf("FetchLaunchHostStatusFromDB: %v", err)
	}
	if result == nil || result.Host == nil {
		t.Fatal("expected host status result")
	}

	host := result.Host
	if host.Status != hostinfo.HostStatusOnline {
		t.Fatalf("host status = %v, want online", host.Status)
	}
	if got := host.RAMUtilizationPct(); got != 50 {
		t.Fatalf("RAM utilization pct = %d, want 50", got)
	}
	if host.DiskFree != hb.DiskFreeBytes {
		t.Fatalf("DiskFree = %d, want %d", host.DiskFree, hb.DiskFreeBytes)
	}
	if len(host.GPUs) != 1 {
		t.Fatalf("len(host.GPUs) = %d, want 1", len(host.GPUs))
	}
	if host.GPUs[0].Utilization != hb.GPUUtilPct {
		t.Fatalf("GPU utilization = %d, want %d", host.GPUs[0].Utilization, hb.GPUUtilPct)
	}
}
