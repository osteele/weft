package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/ids"
)

func TestFormatWatchPlainSnapshotShowsDirectoryTails(t *testing.T) {
	launchedAt := int64(0)
	cloudInstance := &db.Launch{
		ID:                 5,
		Status:             db.LaunchStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32734388",
		GPUSpec:            "A100",
		CostPerHourCents:   200,
		LaunchedAt:         &launchedAt,
	}

	snapshot := watchSystemSnapshot{
		Launches: []*db.Launch{cloudInstance},
		InstanceUpdates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
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
		"Instance " + ids.FormatInstanceID(5) + " — A100 — running",
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

func TestReloadActiveLaunchesFiltersTerminalStatuses(t *testing.T) {
	database := db.SetupTestDB(t)

	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create running launch: %v", err)
	}
	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create failed launch: %v", err)
	}

	launches, err := reloadActiveLaunches(database, []int64{runningID, failedID, 999999})
	if err != nil {
		t.Fatalf("reloadActiveLaunches: %v", err)
	}
	if len(launches) != 1 {
		t.Fatalf("reloadActiveLaunches returned %d launches, want 1", len(launches))
	}
	if launches[0].ID != runningID {
		t.Fatalf("reloadActiveLaunches returned launch %d, want %d", launches[0].ID, runningID)
	}
}

func TestIsActiveCloudLaunchStatus(t *testing.T) {
	tests := map[string]bool{
		db.LaunchStatusLaunching: true,
		db.LaunchStatusRunning:   true,
		db.LaunchStatusGrace:     true,
		db.LaunchStatusFailed:    false,
		db.LaunchStatusCompleted: false,
	}
	for status, expected := range tests {
		if got := isActiveCloudLaunchStatus(status); got != expected {
			t.Fatalf("isActiveCloudLaunchStatus(%q) = %t, want %t", status, got, expected)
		}
	}
}

func TestFormatWatchPlainSnapshot_ShowsCloudDegradedReason(t *testing.T) {
	snapshot := watchSystemSnapshot{
		CloudDegraded: true,
		CloudReason:   degraded.CloudLastKnownRentalsReason(),
	}

	out := formatWatchPlainSnapshot(snapshot, time.Unix(0, 0))
	if !strings.Contains(out, "note: "+degraded.CloudLastKnownRentalsReason()) {
		t.Fatalf("output missing degraded cloud reason, got:\n%s", out)
	}
}

func TestLaunchIDs(t *testing.T) {
	ids := launchIDs([]*db.Launch{{ID: 11}, nil, {ID: 42}})
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 42 {
		t.Fatalf("launchIDs = %v, want [11 42]", ids)
	}
}
