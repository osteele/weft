package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestFormatWatchPlainSnapshotShowsDirectoryTails(t *testing.T) {
	cloudInstance := &db.CloudInstance{
		ID:                 5,
		Status:             db.CloudInstanceStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32734388",
		GPUSpec:            "A100",
	}

	snapshot := watchSystemSnapshot{
		CloudInstances: []*db.CloudInstance{cloudInstance},
		InstanceUpdates: map[int64]campaign.InstanceUpdate{
			5: {
				CloudInstance: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Description: "train model"},
				},
			},
		},
		OnPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval"}}},
		},
		UnplacedJobs: []*db.Job{
			{ID: 123, Status: db.StatusQueued, WorkingDir: "/tmp/project-gamma", Description: "benchmark"},
		},
	}

	out := formatWatchPlainSnapshot(snapshot, time.Unix(0, 0))
	for _, expected := range []string{
		"CLOUD INSTANCES (1)",
		"ON-PREM HOSTS (1 active)",
		"UNPLACED JOBS (1)",
		"#5 32734388",
		"project-alpha",
		"project-gamma",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
}
