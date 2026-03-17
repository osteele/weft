package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestFormatWatchPlainSnapshotShowsDirectoryTails(t *testing.T) {
	launchedAt := int64(0)
	cloudInstance := &db.CloudInstance{
		ID:                 5,
		Status:             db.CloudInstanceStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32734388",
		GPUSpec:            "A100",
		CostPerHourCents:   200,
		LaunchedAt:         &launchedAt,
	}

	snapshot := watchSystemSnapshot{
		CloudInstances: []*db.CloudInstance{cloudInstance},
		InstanceUpdates: map[int64]campaign.InstanceUpdate{
			5: {
				CloudInstance: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Project: "EXP-ALPHA", Description: "train model"},
				},
			},
		},
		OnPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval"}}},
		},
		UnplacedJobs: []*db.Job{
			{ID: 123, Status: db.StatusQueued, WorkingDir: "/tmp/project-gamma", Project: "GAMMA", Description: "benchmark"},
		},
	}

	out := formatWatchPlainSnapshot(snapshot, time.Unix(0, 0))
	for _, expected := range []string{
		"RENTAL INSTANCES (1)",
		"Summary:  cost: $0.00  current rate: $2.00/hr",
		"INVENTORY HOSTS (1 active)",
		"UNPLACED JOBS (1)",
		"Instance 5 — A100 — running",
		"Cost: $0.00 (uptime: 0s, rate: $2.00/hr)",
		"  vastai: 32734388",
		"  Jobs: 0/1 resolved",
		"EXP-ALPHA",
		"GAMMA",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "ID 5") {
		t.Fatalf("output should omit redundant provider line ID, got:\n%s", out)
	}
}
