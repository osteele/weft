package terminal

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestComputeGroupedETA_UsesLiveProgressAndQueue(t *testing.T) {
	now := time.Unix(10_000, 0)
	launchID := int64(7)
	jobs := []*db.Job{
		{
			ID:        1,
			Status:    db.StatusRunning,
			GPUClass:  "A100",
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
	if got.ETACurrent < 60*time.Minute || got.ETACurrent > 90*time.Minute {
		t.Fatalf("ETACurrent = %v, want near 70m", got.ETACurrent)
	}
}

func TestComputeGroupedETA_WithNewInstanceShownWhenQueueLargeEnough(t *testing.T) {
	now := time.Unix(10_000, 0)
	launchID := int64(9)
	jobs := []*db.Job{
		{ID: 10, Status: db.StatusRunning, GPUClass: "A100", LaunchID: &launchID, StartTime: 9_900},
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
