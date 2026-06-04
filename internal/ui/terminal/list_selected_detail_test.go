package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestSelectedJobDetail_InventoryHost(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	job := &db.Job{
		ID:        42,
		Host:      "cool30",
		GPU:       "0,1",
		Project:   "my-proj",
		Command:   "uv run scripts/train.py --epochs 10",
		Status:    db.StatusRunning,
		StartTime: now.Add(-5 * time.Minute).Unix(),
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (Job + Host), got %d: %v", len(lines), lines)
	}
	job0, host1 := lines[0], lines[1]
	for _, want := range []string{"Job: wj42", "elapsed 5m"} {
		if !strings.Contains(job0, want) {
			t.Errorf("expected %q in Job line, got: %s", want, job0)
		}
	}
	if !strings.Contains(host1, "Host: cool30") {
		t.Errorf("expected 'Host: cool30' in Host line, got: %s", host1)
	}
	joined := strings.Join(lines, " | ")
	for _, unwanted := range []string{"project", "my-proj", "train.py", "epochs"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("unexpected %q in detail lines (project/command must be excluded): %s", unwanted, joined)
		}
	}
}

func TestSelectedJobDetail_RentalInstance(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(7)
	job := &db.Job{
		ID:        9,
		Host:      "cloud:vastai:123",
		LaunchID:  &launchID,
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-90 * time.Second).Unix(),
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	if !strings.Contains(lines[0], "Job: wj9") || !strings.Contains(lines[0], "elapsed 1m") {
		t.Errorf("expected 'Job: wj9 · elapsed 1m', got: %s", lines[0])
	}
	for _, want := range []string{"Host:", "wi7", "@ Vast.ai"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("expected %q in Host line, got: %s", want, lines[1])
		}
	}
}

// TestSelectedJobDetail_RentalHostLineOmitsPhase pins a regression: the
// launch "phase" (e.g. running:316) used to appear on the placement line of
// the previous single-line design. The new Host line must NOT carry it;
// phase is internal state that belongs in `weft instance watch`, not on
// every list-TUI frame.
func TestSelectedJobDetail_RentalHostLineOmitsPhase(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(7)
	job := &db.Job{
		ID:        9,
		LaunchID:  &launchID,
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-1 * time.Minute).Unix(),
	}
	ctx := selectedJobContext{
		launchLiveByID: map[int64]*db.LaunchLiveState{
			launchID: {InstancePhase: "running:316"},
		},
	}
	lines := renderSelectedJobDetail(job, ctx, now)
	joined := strings.Join(lines, " | ")
	for _, forbidden := range []string{"phase", "running:316"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("expected %q NOT to appear in footer lines, got: %s", forbidden, joined)
		}
	}
}

// TestSelectedJobDetail_RentalHostShowsLaunchState pins that the Host line
// carries the Launch.Status label (launching / running / grace) right after
// the "id @ provider" head so the user can see whether the rental is alive.
func TestSelectedJobDetail_RentalHostShowsLaunchState(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(55)
	job := &db.Job{
		ID:       3,
		LaunchID: &launchID,
		Tags:     []string{"provider:vastai"},
		Status:   db.StatusRunning,
	}
	for _, tc := range []struct {
		status string
		label  string
	}{
		{db.LaunchStatusLaunching, "launching"},
		{db.LaunchStatusRunning, "running"},
		{db.LaunchStatusGrace, "grace"},
	} {
		ctx := selectedJobContext{
			launchByID: map[int64]*db.Launch{
				launchID: {ID: launchID, Status: tc.status, Provider: "vastai"},
			},
		}
		lines := renderSelectedJobDetail(job, ctx, now)
		if len(lines) != 2 {
			t.Fatalf("status=%s: expected 2 lines, got %v", tc.status, lines)
		}
		host := lines[1]
		if !strings.Contains(host, "wi55 @ Vast.ai") {
			t.Errorf("status=%s: expected 'wi55 @ Vast.ai' head, got: %s", tc.status, host)
		}
		if !strings.Contains(host, " · "+tc.label) {
			t.Errorf("status=%s: expected ' · %s' state segment, got: %s", tc.status, tc.label, host)
		}
	}
}

func TestSelectedJobDetail_Unplaced(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	memGB := 40
	job := &db.Job{
		ID:               5,
		GPUClass:         "ampere+",
		GPUMemGB:         &memGB,
		PlacementReasons: []string{"no capacity"},
		Status:           db.StatusQueued,
		CreatedAt:        now.Add(-2 * time.Hour).Unix(),
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 1 {
		t.Fatalf("unplaced jobs should have no Host line (got %d lines): %v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"Job: wj5", "unplaced", "ampere+", "≥40GB", "blocked: no capacity", "waiting 2h"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in Job line, got: %s", want, line)
		}
	}
	if strings.Contains(line, "queue") {
		t.Errorf("queue name must be excluded, got: %s", line)
	}
}

func TestSelectedJobDetail_UnplacedHidesResetReasonWithLiveBlocker(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	memGB := 34
	job := &db.Job{
		ID:                 1571,
		GPUClass:           "ampere+",
		GPUMemGB:           &memGB,
		Status:             db.StatusQueued,
		CreatedAt:          now.Add(-3 * time.Minute).Unix(),
		QueueBlockedReason: `waiting for "output/model.pt" from wj1570 (running)`,
		PlacementReasons:   []string{"cloud instance 1778 unavailable; job reset to unplaced queue"},
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	line := lines[0]
	if want := `waiting for "output/model.pt" from wj1570 (running)`; !strings.Contains(line, want) {
		t.Errorf("expected %q in line, got: %s", want, line)
	}
	if strings.Contains(line, "cloud instance 1778 unavailable") {
		t.Errorf("unexpected reset reason in line: %s", line)
	}
}

func TestSelectedJobDetail_UnplacedDeduplicatesBlockedReasons(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	job := &db.Job{
		ID:                 7,
		Status:             db.StatusQueued,
		CreatedAt:          now.Add(-time.Minute).Unix(),
		QueueBlockedReason: "no offers",
		PlacementReasons:   []string{"no offers"},
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if got := strings.Count(lines[0], "no offers"); got != 1 {
		t.Errorf("expected reason once, got %d copies in: %s", got, lines[0])
	}
}

func TestSelectedJobDetail_Failed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	exit := 137
	job := &db.Job{
		ID:            3,
		Host:          "cool30",
		Status:        db.StatusFailed,
		ExitCode:      &exit,
		FailureReason: "oom",
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	for _, want := range []string{"Job: wj3", "exit 137", "oom"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("expected %q in Job line, got: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "Host: cool30") {
		t.Errorf("expected 'Host: cool30' in Host line, got: %s", lines[1])
	}
}

func TestSelectedJobDetail_NilJob(t *testing.T) {
	if lines := renderSelectedJobDetail(nil, selectedJobContext{}, time.Now()); lines != nil {
		t.Fatalf("expected nil for nil job, got %v", lines)
	}
}

func TestSelectedJobDetail_RentalQueuedWaitingForSibling(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	launchID := int64(1236)
	job := &db.Job{
		ID:       1268,
		LaunchID: &launchID,
		Tags:     []string{"provider:vastai"},
		Status:   db.StatusQueued,
		QueuedAt: now.Add(-8 * time.Minute).Unix(),
	}
	ctx := selectedJobContext{
		launchLiveByID: map[int64]*db.LaunchLiveState{
			launchID: {JobProgressID: 1267},
		},
	}
	lines := renderSelectedJobDetail(job, ctx, now)
	if len(lines) == 0 {
		t.Fatalf("expected detail lines, got none")
	}
	if !strings.Contains(lines[0], "waiting for wj1267") {
		t.Fatalf("expected 'waiting for wj1267' in Job line, got: %s", lines[0])
	}
}

func TestSelectedJobDetail_InventoryQueuedWaitingForSibling(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	job := &db.Job{
		ID:       200,
		Host:     "cool30",
		Status:   db.StatusQueued,
		QueuedAt: now.Add(-3 * time.Minute).Unix(),
	}
	running := &db.Job{
		ID:        199,
		Host:      "cool30",
		Status:    db.StatusRunning,
		StartTime: now.Add(-30 * time.Minute).Unix(),
	}
	ctx := selectedJobContext{siblingJobs: []*db.Job{job, running}}
	lines := renderSelectedJobDetail(job, ctx, now)
	if !strings.Contains(lines[0], "waiting for wj199") {
		t.Fatalf("expected 'waiting for wj199' in Job line, got: %s", lines[0])
	}
}

func TestSelectedJobDetail_WaitingForOnlyWhenQueued(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	launchID := int64(5)
	runningJob := &db.Job{
		ID:        100,
		LaunchID:  &launchID,
		Status:    db.StatusRunning,
		StartTime: now.Add(-1 * time.Minute).Unix(),
	}
	// A different running job reported via JobProgressID must not make
	// the *running* job render as "waiting for".
	ctx := selectedJobContext{
		launchLiveByID: map[int64]*db.LaunchLiveState{
			launchID: {JobProgressID: 999},
		},
	}
	lines := renderSelectedJobDetail(runningJob, ctx, now)
	joined := strings.Join(lines, " | ")
	if strings.Contains(joined, "waiting for") {
		t.Fatalf("non-queued job must not show 'waiting for', got: %s", joined)
	}
}

func TestSelectedJobDetail_RentalHostEnriched(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	launchID := int64(1236)
	launchedAt := now.Add(-1 * time.Hour).Unix()
	launch := &db.Launch{
		ID:               launchID,
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX 3090",
		GPUMemGB:         24,
		NumGPUs:          1,
		CostPerHourCents: 15,
		LaunchedAt:       &launchedAt,
	}
	job := &db.Job{
		ID:        1267,
		LaunchID:  &launchID,
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-30 * time.Minute).Unix(),
	}
	ctx := selectedJobContext{
		launchByID: map[int64]*db.Launch{launchID: launch},
	}
	lines := renderSelectedJobDetail(job, ctx, now)
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	host := lines[1]
	for _, want := range []string{"Host: wi1236", "RTX 3090 24GB", "Vast.ai", "$0.15/hr", "uptime 1h", "$0.15"} {
		if !strings.Contains(host, want) {
			t.Errorf("expected %q in Host line, got: %s", want, host)
		}
	}
}

func TestSelectedJobDetail_RentalRunningShowsElapsedCost(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	launchID := int64(1935)
	launchedAt := now.Add(-42 * time.Minute).Unix()
	job := &db.Job{
		ID:        1935,
		LaunchID:  &launchID,
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-30 * time.Minute).Unix(),
	}
	ctx := selectedJobContext{
		launchByID: map[int64]*db.Launch{
			launchID: {
				ID:               launchID,
				Status:           db.LaunchStatusRunning,
				Provider:         "vastai",
				CostPerHourCents: 20,
				LaunchedAt:       &launchedAt,
			},
		},
	}

	lines := renderSelectedJobDetail(job, ctx, now)
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	for _, want := range []string{"Job: wj1935", "elapsed 30m", "cost $0.10"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("expected %q in Job line, got: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "$0.14") {
		t.Errorf("expected host line to keep instance uptime cost, got: %s", lines[1])
	}
}

func TestSelectedJobDetail_CompletedShowsRunCostAndExit(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	launchID := int64(2427)
	endTime := now.Add(-4 * time.Minute).Unix()
	exit := 0
	cost := 0.15
	job := &db.Job{
		ID:        2427,
		LaunchID:  &launchID,
		Status:    db.StatusCompleted,
		StartTime: endTime - int64(37*time.Minute/time.Second),
		EndTime:   &endTime,
		ExitCode:  &exit,
		Cost:      &cost,
	}

	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) == 0 {
		t.Fatalf("expected detail lines, got none")
	}
	for _, want := range []string{"Job: wj2427", "ran 37m", "finished 4m ago", "cost $0.15", "exit 0"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("expected %q in Job line, got: %s", want, lines[0])
		}
	}
}

func TestSelectedJobDetail_CompletedFallsBackToLaunchRateCost(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	launchID := int64(2428)
	endTime := now.Add(-1 * time.Minute).Unix()
	exit := 0
	job := &db.Job{
		ID:        2428,
		LaunchID:  &launchID,
		Status:    db.StatusCompleted,
		StartTime: endTime - int64(30*time.Minute/time.Second),
		EndTime:   &endTime,
		ExitCode:  &exit,
	}
	ctx := selectedJobContext{
		launchByID: map[int64]*db.Launch{
			launchID: {
				ID:               launchID,
				CostPerHourCents: 20,
			},
		},
	}

	lines := renderSelectedJobDetail(job, ctx, now)
	if len(lines) == 0 {
		t.Fatalf("expected detail lines, got none")
	}
	if !strings.Contains(lines[0], "cost $0.10") {
		t.Fatalf("expected fallback cost from launch rate, got: %s", lines[0])
	}
}

func TestSelectedJobDetail_RentalHostFallbackWhenLaunchMissing(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	launchID := int64(77)
	job := &db.Job{
		ID:        1,
		LaunchID:  &launchID,
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-1 * time.Minute).Unix(),
	}
	// launchByID is empty — simulate race during reload.
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	host := lines[1]
	if host != "Host: wi77 @ Vast.ai" {
		t.Fatalf("expected fallback 'Host: wi77 @ Vast.ai', got: %s", host)
	}
	for _, unwanted := range []string{"$", "uptime", "/hr"} {
		if strings.Contains(host, unwanted) {
			t.Errorf("unexpected enrichment %q in fallback line, got: %s", unwanted, host)
		}
	}
}

func TestSelectedJobDetail_InventoryLastSeenWhenStale(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	job := &db.Job{
		ID:        10,
		Host:      "cool30",
		Status:    db.StatusRunning,
		StartTime: now.Add(-1 * time.Minute).Unix(),
	}
	// Fresh (under threshold): no suffix.
	fresh := selectedJobContext{hostInfoByName: map[string]*db.CachedHostInfo{
		"cool30": {Name: "cool30", LastUpdated: now.Add(-4 * time.Minute).Unix()},
	}}
	if got := renderHostFooterLine(job, fresh, now); got != "Host: cool30" {
		t.Errorf("fresh host info must not add 'last seen', got: %s", got)
	}
	// Stale (over threshold): suffix present.
	stale := selectedJobContext{hostInfoByName: map[string]*db.CachedHostInfo{
		"cool30": {Name: "cool30", LastUpdated: now.Add(-10 * time.Minute).Unix()},
	}}
	got := renderHostFooterLine(job, stale, now)
	if !strings.Contains(got, "Host: cool30") || !strings.Contains(got, "last seen") {
		t.Errorf("stale host info must show 'last seen', got: %s", got)
	}
}
