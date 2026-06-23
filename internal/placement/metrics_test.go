package placement

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuerunner"
)

func TestHostMetricsFromHostInfo_Basic(t *testing.T) {
	host := &hostinfo.Host{
		Name:     "test-host",
		CPUs:     16,
		LoadAvg:  "8.0, 4.0, 2.0",
		MemTotal: "64G",
		MemUsed:  "32G",
		GPUs: []hostinfo.GPUInfo{
			{Index: 0, Name: "RTX 3090", Utilization: 75, MemUsed: "12000 MiB", MemTotal: "24576 MiB"},
		},
	}

	queueStatus := &queuerunner.StatusInfo{
		QueuedJobCount: 3,
	}

	m := HostMetricsFromHostInfo(host, queueStatus)

	// CPU: load 8.0 / 16 cores = 50%
	if m.CPUPercent != 50 {
		t.Errorf("CPUPercent = %d, want 50", m.CPUPercent)
	}

	// RAM: 32/64 = 50%
	if m.RAMPercent != 50 {
		t.Errorf("RAMPercent = %d, want 50", m.RAMPercent)
	}

	// GPU: 75% utilization
	if m.GPUPercent != 75 {
		t.Errorf("GPUPercent = %d, want 75", m.GPUPercent)
	}

	// Free RAM: (64-32) GB = 32 GB = 32*1024*1024 KB
	expectedFreeRAMKB := int64(32 * 1024 * 1024)
	if m.FreeRAMKB != expectedFreeRAMKB {
		t.Errorf("FreeRAMKB = %d, want %d", m.FreeRAMKB, expectedFreeRAMKB)
	}

	// GPU free: 24576-12000 = 12576 MiB
	if free, ok := m.GPUDeviceFreeMemMiB["0"]; !ok || free != 12576 {
		t.Errorf("GPUDeviceFreeMemMiB[0] = %d, want 12576", free)
	}

	// Queue depth
	if m.QueueDepth != 3 {
		t.Errorf("QueueDepth = %d, want 3", m.QueueDepth)
	}
}

func TestCollectMetricsReturnsPartialBeforeHungHost(t *testing.T) {
	db := setupTestDB(t)

	originalFetch := collectMetricsFetchHostStatus
	t.Cleanup(func() {
		collectMetricsFetchHostStatus = originalFetch
	})

	collectMetricsFetchHostStatus = func(_ *sql.DB, hostName string, _ string, _ time.Duration) (*ops.HostStatusResult, error) {
		if hostName == "slow-host" {
			time.Sleep(500 * time.Millisecond)
			return &ops.HostStatusResult{
				Host: &hostinfo.Host{Name: hostName, CPUs: 4, LoadAvg: "1.0", MemTotal: "8G", MemUsed: "2G"},
			}, nil
		}
		return &ops.HostStatusResult{
			Host: &hostinfo.Host{Name: hostName, CPUs: 4, LoadAvg: "1.0", MemTotal: "8G", MemUsed: "2G"},
		}, nil
	}

	started := time.Now()
	metrics := CollectMetrics(db, []string{"fast-host", "slow-host"}, 100*time.Millisecond)
	elapsed := time.Since(started)

	if elapsed >= 400*time.Millisecond {
		t.Fatalf("CollectMetrics took %s, want it to return before slow host completes", elapsed)
	}
	if metrics["fast-host"] == nil {
		t.Fatalf("expected fast-host metrics")
	}
	if metrics["slow-host"] != nil {
		t.Fatalf("slow-host metrics should be omitted after timeout")
	}
}

func TestHostMetricsFromHostInfo_NilQueue(t *testing.T) {
	host := &hostinfo.Host{
		Name:     "test-host",
		CPUs:     8,
		LoadAvg:  "4.0",
		MemTotal: "32G",
		MemUsed:  "16G",
	}

	m := HostMetricsFromHostInfo(host, nil)

	if m.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0", m.QueueDepth)
	}
	if m.CPUPercent != 50 {
		t.Errorf("CPUPercent = %d, want 50", m.CPUPercent)
	}
}

func TestHostMetricsFromHostInfo_MultiGPU(t *testing.T) {
	host := &hostinfo.Host{
		Name:     "host-alpha",
		CPUs:     64,
		LoadAvg:  "16.0",
		MemTotal: "256G",
		MemUsed:  "100G",
		GPUs: []hostinfo.GPUInfo{
			{Index: 0, Name: "A100", Utilization: 90, MemUsed: "70000 MiB", MemTotal: "81920 MiB"},
			{Index: 1, Name: "A100", Utilization: 10, MemUsed: "5000 MiB", MemTotal: "81920 MiB"},
		},
	}

	m := HostMetricsFromHostInfo(host, nil)

	// GPU percent should be max across GPUs — 90%
	if m.GPUPercent != 90 {
		t.Errorf("GPUPercent = %d, want 90", m.GPUPercent)
	}

	// Device 0: 81920-70000 = 11920 MiB free
	if free := m.GPUDeviceFreeMemMiB["0"]; free != 11920 {
		t.Errorf("GPUDeviceFreeMemMiB[0] = %d, want 11920", free)
	}

	// Device 1: 81920-5000 = 76920 MiB free
	if free := m.GPUDeviceFreeMemMiB["1"]; free != 76920 {
		t.Errorf("GPUDeviceFreeMemMiB[1] = %d, want 76920", free)
	}
}

func TestHostBetaWinsWhenHostAlphaLoaded(t *testing.T) {
	db := setupTestDB(t)

	// host-alpha is heavily loaded; host-beta is idle
	metrics := map[string]*HostMetrics{
		"host-alpha": {
			GPUPercent: 95,
			CPUPercent: 80,
			QueueDepth: 4,
			GPUDeviceFreeMemMiB: map[string]int64{
				"0": 5 * 1024,  // 5 GB free
				"1": 10 * 1024, // 10 GB free
			},
		},
		"host-beta": {
			GPUPercent: 0,
			CPUPercent: 5,
			QueueDepth: 0,
			GPUDeviceFreeMemMiB: map[string]int64{
				"0": 23 * 1024, // 23 GB free
			},
		},
	}

	// Use nvidia family constraint (both hosts eligible)
	scores := scoreTestHostsWithMetrics(db, Constraints{GPUClass: "nvidia"}, metrics)

	hostBeta := findScore(scores, "host-beta")
	hostAlpha := findScore(scores, "host-alpha")

	if !hostBeta.Eligible || !hostAlpha.Eligible {
		t.Fatalf("both should be eligible: host-beta=%v host-alpha=%v", hostBeta.Eligible, hostAlpha.Eligible)
	}

	if hostBeta.Total <= hostAlpha.Total {
		t.Errorf("host-beta (%.2f) should beat loaded host-alpha (%.2f)\n  host-beta reasons: %v\n  host-alpha reasons: %v",
			hostBeta.Total, hostAlpha.Total, hostBeta.Reasons, hostAlpha.Reasons)
	}
}

func TestParseSizeToMiB(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"80 GiB", 80 * 1024},
		{"24576 MiB", 24576},
		{"12000 MiB", 12000},
		{"", 0},
		{"-", 0},
	}
	for _, tt := range tests {
		got := parseSizeToMiB(tt.input)
		if got != tt.want {
			t.Errorf("parseSizeToMiB(%q) = %.0f, want %.0f", tt.input, got, tt.want)
		}
	}
}
