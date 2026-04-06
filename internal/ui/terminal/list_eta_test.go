package terminal

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func float64Ptr(v float64) *float64 { return &v }

func TestComputeGroupedETA_UsesLiveProgressAndQueue(t *testing.T) {
	now := time.Unix(10_000, 0)
	launchID := int64(7)
	jobs := []*db.Job{
		{
			ID:        1,
			Status:    db.StatusRunning,
			GPUClass:  "A100",
			Host:      db.LaunchHost(launchID),
			LaunchID:  &launchID,
			StartTime: 9_400,
		},
		{
			ID:       2,
			Status:   db.StatusQueued,
			GPUClass: "A100",
		},
	}
	live := map[int64]*db.LaunchLiveState{
		launchID: {LaunchID: launchID, JobProgressID: 1, JobProgressPct: 50},
	}

	got := computeGroupedETA(jobs, live, now)
	if !got.HasQueued {
		t.Fatalf("HasQueued = false, want true")
	}
	if got.ETACurrent <= 0 {
		t.Fatalf("ETACurrent = %v, want > 0", got.ETACurrent)
	}
	if got.ETACurrent < 110*time.Minute || got.ETACurrent > 150*time.Minute {
		t.Fatalf("ETACurrent = %v, want around 2h (running remainder + one queued job)", got.ETACurrent)
	}
}

func TestComputeGroupedETA_WithNewInstanceShownWhenQueueLargeEnough(t *testing.T) {
	now := time.Unix(10_000, 0)
	launchID := int64(9)
	jobs := []*db.Job{
		{ID: 10, Status: db.StatusRunning, GPUClass: "A100", Host: db.LaunchHost(launchID), LaunchID: &launchID, StartTime: 9_900},
		{ID: 1, Status: db.StatusQueued, GPUClass: "A100"},
		{ID: 2, Status: db.StatusQueued, GPUClass: "A100"},
	}

	got := computeGroupedETA(jobs, nil, now)
	if got.ETAWithNewInst <= 0 {
		t.Fatalf("ETAWithNewInst = %v, want > 0", got.ETAWithNewInst)
	}
	if got.ETAWithNewInst >= got.ETACurrent {
		t.Fatalf("ETAWithNewInst = %v, ETACurrent = %v, want improved ETA", got.ETAWithNewInst, got.ETACurrent)
	}
}

func TestFormatETAApprox(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{in: 30 * time.Second, want: "<1m"},
		{in: 14 * time.Minute, want: "~14m"},
		{in: 80 * time.Minute, want: "~1h 20m"},
		{in: 26 * time.Hour, want: ">24h"},
	}
	for _, tc := range cases {
		if got := formatETAApprox(tc.in); got != tc.want {
			t.Fatalf("formatETAApprox(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatETALine_ShowsNewInstanceWithoutEstimateWhenNotBetter(t *testing.T) {
	line := formatETALine(etaResult{
		ETACurrent:     5 * time.Hour,
		ETAWithNewInst: 5*time.Hour + 4*time.Minute,
		HasQueued:      true,
	})
	if line != "ETA: ~5h  ·  with +1 instance" {
		t.Fatalf("formatETALine() = %q, want %q", line, "ETA: ~5h  ·  with +1 instance")
	}
}

func TestFormatETALine_ShowsEstimateWhenBetter(t *testing.T) {
	line := formatETALine(etaResult{
		ETACurrent:     5 * time.Hour,
		ETAWithNewInst: 4*time.Hour + 40*time.Minute,
		HasQueued:      true,
	})
	if line != "ETA: ~5h  ·  with +1 instance: ~4h 40m" {
		t.Fatalf("formatETALine() = %q, want %q", line, "ETA: ~5h  ·  with +1 instance: ~4h 40m")
	}
}

func TestEstimateRunningJobRemaining_UsesPlacementPriorWhenNoProgress(t *testing.T) {
	now := time.Unix(10_000, 0)
	job := &db.Job{
		ID:        1,
		Status:    db.StatusRunning,
		Host:      "cool30",
		StartTime: 9_100, // 15 minutes elapsed
		PlacementMeta: &db.PlacementMeta{
			PredictedDurationS: float64Ptr(2 * 60 * 60),
		},
	}

	got, ok := estimateRunningJobRemaining(job, nil, now)
	if !ok {
		t.Fatalf("estimateRunningJobRemaining() ok = false, want true")
	}
	if got < 45*time.Minute || got > etaMaxRunningRemainder {
		t.Fatalf("estimateRunningJobRemaining() = %v, want plausible prior-based remainder", got)
	}
}

func TestEstimateRunningJobRemaining_FreshProgressWeightedMoreThanStale(t *testing.T) {
	now := time.Unix(20_000, 0)
	launchID := int64(7)
	job := &db.Job{
		ID:        1,
		Status:    db.StatusRunning,
		Host:      db.LaunchHost(launchID),
		StartTime: 19_400, // 10 minutes elapsed
		LaunchID:  &launchID,
		PlacementMeta: &db.PlacementMeta{
			PredictedDurationS: float64Ptr(3 * 60 * 60),
		},
	}

	freshMap := map[int64]*db.LaunchLiveState{
		launchID: {LaunchID: launchID, JobProgressID: 1, JobProgressPct: 50, UpdatedAt: now.Unix()},
	}
	staleMap := map[int64]*db.LaunchLiveState{
		launchID: {LaunchID: launchID, JobProgressID: 1, JobProgressPct: 50, UpdatedAt: now.Add(-10 * time.Minute).Unix()},
	}

	fresh, okFresh := estimateRunningJobRemaining(job, freshMap, now)
	stale, okStale := estimateRunningJobRemaining(job, staleMap, now)
	if !okFresh || !okStale {
		t.Fatalf("estimateRunningJobRemaining() expected both estimates, got fresh=%v stale=%v", okFresh, okStale)
	}
	if fresh >= stale {
		t.Fatalf("fresh ETA = %v, stale ETA = %v, want fresh < stale", fresh, stale)
	}
}
