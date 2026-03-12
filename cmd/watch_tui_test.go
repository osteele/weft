package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestWatchAllModelViewShowsSectionsAndDirectoryTails(t *testing.T) {
	cloudInstance := &db.CloudInstance{
		ID:       5,
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchAllModel{
		width:  120,
		height: 20,
		cloudInstances: []*db.CloudInstance{
			cloudInstance,
		},
		instanceUpdates: map[int64]campaign.InstanceUpdate{
			5: {
				CloudInstance: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Description: "train model"},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval model"},
				},
			},
		},
		unplacedJobs: []*db.Job{
			{ID: 123, Status: db.StatusQueued, WorkingDir: "/tmp/project-gamma", Description: "benchmark", GPUClass: "A100"},
		},
	}

	out := stripANSI(m.View())
	for _, expected := range []string{
		"Cloud Instances (1)",
		"On-Prem Hosts (1 active)",
		"Unplaced Jobs (1)",
		"[u] unplace queued job",
		"project-alpha",
		"project-beta",
		"project-gamma",
		"cool30",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
}
