package web

import (
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/hostinfo"
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
