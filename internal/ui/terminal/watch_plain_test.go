package terminal

import (
	"bytes"
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

func TestFlattenSystemSnapshotJobsDedupes(t *testing.T) {
	j1 := &db.Job{ID: 1, Status: "running"}
	j2 := &db.Job{ID: 2, Status: "queued"}
	j3 := &db.Job{ID: 3, Status: "queued"}
	snap := watchSystemSnapshot{
		InstanceUpdates: map[int64]campaign.InstanceUpdate{
			10: {Jobs: []*db.Job{j1, j1}},
			11: {Jobs: []*db.Job{j2}},
		},
		OnPremHosts:  []onPremHostSummary{{Name: "h1", Jobs: []*db.Job{j2}}},
		UnplacedJobs: []*db.Job{j3},
	}
	jobs := flattenSystemSnapshotJobs(snap)
	seen := make(map[int64]bool)
	for _, j := range jobs {
		seen[j.ID] = true
	}
	if len(jobs) != 3 || !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("got %d jobs, ids=%v", len(jobs), seen)
	}
}

func TestEmitTransitionsTextMode(t *testing.T) {
	var buf bytes.Buffer
	opts := WatchPlainOptions{TransitionsOnly: true, Stdout: &buf}
	events := []TransitionEvent{
		{JobID: "wj1", PrevStatus: "running", Status: "completed"},
		{JobID: "wj2", PrevStatus: "queued", Status: "running"},
	}
	anyTerm, err := opts.EmitTransitions(events)
	if err != nil {
		t.Fatal(err)
	}
	if !anyTerm {
		t.Error("completed should mark anyTerminal true")
	}
	out := buf.String()
	if !strings.Contains(out, "wj1") || !strings.Contains(out, "wj2") {
		t.Errorf("missing job IDs in output: %q", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Errorf("want 2 lines, got %d: %q", strings.Count(out, "\n"), out)
	}
}

func TestEmitTransitionsJSONLMode(t *testing.T) {
	var buf bytes.Buffer
	opts := WatchPlainOptions{JSONLines: true, Stdout: &buf}
	events := []TransitionEvent{
		{Type: "transition", JobID: "wj1", Status: "running", PrevStatus: "queued"},
	}
	if _, err := opts.EmitTransitions(events); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("want JSON line, got %q", buf.String())
	}
}

func TestEmitTransitionsSnapshotModeIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	opts := WatchPlainOptions{Stdout: &buf}
	events := []TransitionEvent{{JobID: "wj1", Status: "completed"}}
	anyTerm, err := opts.EmitTransitions(events)
	if err != nil {
		t.Fatal(err)
	}
	if !anyTerm {
		t.Error("anyTerminal should still report regardless of output mode")
	}
	if buf.Len() != 0 {
		t.Errorf("snapshot mode should not write transition lines, got %q", buf.String())
	}
}

func TestEmitSnapshotJSONHasTypeAndNewline(t *testing.T) {
	var buf bytes.Buffer
	opts := WatchPlainOptions{JSONLines: true, Stdout: &buf}
	jobs := []*db.Job{{ID: 1, Status: "running"}}
	if err := opts.EmitSnapshotJSON(jobs, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"snapshot"`) {
		t.Errorf("want type=snapshot, got %q", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("want trailing newline")
	}
}
