package web

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/hostinfo"
)

func TestBuildHostSummaries_FiltersStaleHosts(t *testing.T) {
	now := time.Now()
	hosts := []*hostinfo.Host{
		{Name: "cool30", Status: hostinfo.HostStatusOnline},
		{Name: "cool100", Status: hostinfo.HostStatusOnline},
		{Name: "lm2", Status: hostinfo.HostStatusOffline},
	}

	tests := []struct {
		name          string
		syncTimes     map[string]time.Time
		wantHostNames []string
	}{
		{
			name: "only recently synced hosts shown",
			syncTimes: map[string]time.Time{
				"cool30":  now.Add(-1 * time.Hour),
				"cool100": now.Add(-24 * time.Hour),
				"lm2":     now.Add(-30 * 24 * time.Hour), // 30 days ago
			},
			wantHostNames: []string{"cool100", "cool30"},
		},
		{
			name: "host with no sync time excluded",
			syncTimes: map[string]time.Time{
				"cool30":  now.Add(-1 * time.Hour),
				"cool100": now.Add(-1 * time.Hour),
			},
			wantHostNames: []string{"cool100", "cool30"},
		},
		{
			name:          "empty sync times shows no hosts",
			syncTimes:     map[string]time.Time{},
			wantHostNames: []string{},
		},
		{
			name: "all hosts recently synced",
			syncTimes: map[string]time.Time{
				"cool30":  now.Add(-1 * time.Hour),
				"cool100": now.Add(-1 * time.Hour),
				"lm2":     now.Add(-1 * time.Hour),
			},
			wantHostNames: []string{"cool100", "cool30", "lm2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summaries := buildHostSummaries(hosts, tt.syncTimes)
			gotNames := make([]string, len(summaries))
			for i, s := range summaries {
				gotNames[i] = s.Name
			}
			if len(gotNames) != len(tt.wantHostNames) {
				t.Errorf("got %d hosts %v, want %d hosts %v", len(gotNames), gotNames, len(tt.wantHostNames), tt.wantHostNames)
				return
			}
			for i, want := range tt.wantHostNames {
				if gotNames[i] != want {
					t.Errorf("host[%d] = %q, want %q", i, gotNames[i], want)
				}
			}
		})
	}
}

func TestMergeLiveGPUs(t *testing.T) {
	apiGPUs := []apiGPU{
		{Name: "A100", Class: "a100", Memory: "80GB", Count: 2},
		{Name: "RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Count: 3},
	}

	liveGPUs := []hostinfo.GPUInfo{
		{Index: 0, Utilization: 85, MemUsed: "45000MiB", MemTotal: "80000MiB", Temperature: 72},
		{Index: 1, Utilization: 30, MemUsed: "12000MiB", MemTotal: "80000MiB", Temperature: 55},
		{Index: 2, Utilization: 0, MemUsed: "100MiB", MemTotal: "11264MiB", Temperature: 40},
		{Index: 3, Utilization: 95, MemUsed: "10000MiB", MemTotal: "11264MiB", Temperature: 78},
		{Index: 4, Utilization: 50, MemUsed: "5000MiB", MemTotal: "11264MiB", Temperature: 60},
	}

	mergeLiveGPUs(apiGPUs, liveGPUs)

	// A100 group: max utilization should be 85 (from GPU 0)
	if apiGPUs[0].Utilization != 85 {
		t.Errorf("A100 utilization: got %d, want 85", apiGPUs[0].Utilization)
	}
	if apiGPUs[0].Temperature != 72 {
		t.Errorf("A100 temperature: got %d, want 72", apiGPUs[0].Temperature)
	}
	if apiGPUs[0].MemUsed == "" {
		t.Error("A100 MemUsed should be populated")
	}

	// RTX 2080 Ti group: max utilization should be 95 (from GPU 3)
	if apiGPUs[1].Utilization != 95 {
		t.Errorf("RTX 2080 Ti utilization: got %d, want 95", apiGPUs[1].Utilization)
	}
	if apiGPUs[1].Temperature != 78 {
		t.Errorf("RTX 2080 Ti temperature: got %d, want 78", apiGPUs[1].Temperature)
	}
}

func TestMergeLiveGPUs_NoLiveData(t *testing.T) {
	apiGPUs := []apiGPU{
		{Name: "A100", Count: 2},
	}

	mergeLiveGPUs(apiGPUs, nil)

	if apiGPUs[0].Utilization != 0 {
		t.Errorf("utilization should be 0 with no live data, got %d", apiGPUs[0].Utilization)
	}
}

func TestBuildHostFilters_IncludesAllHosts(t *testing.T) {
	hosts := []*hostinfo.Host{
		{Name: "cool30"},
		{Name: "cool100"},
		{Name: "lm2"},
	}

	filters := buildHostFilters(hosts)

	// Should have "Synced <2d", "All", plus individual hosts
	if len(filters) != 5 {
		t.Fatalf("got %d filters, want 5", len(filters))
	}
	if filters[0].ID != "recent" {
		t.Errorf("filters[0].ID = %q, want %q", filters[0].ID, "recent")
	}
	if filters[1].ID != "all" {
		t.Errorf("filters[1].ID = %q, want %q", filters[1].ID, "all")
	}
}
