package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestExceedsRetryBudget_FirstRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 45 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  45 * time.Minute,
		NextCostCents:  25,
	}
	if exceeded, _ := exceedsRetryBudget(budget, 1, 10*time.Minute, 50); exceeded {
		t.Fatal("first retry should not exceed budget")
	}
	if exceeded, detail := exceedsRetryBudget(budget, 1, 46*time.Minute, 50); !exceeded {
		t.Fatal("first retry time should exceed budget")
	} else if detail == "" {
		t.Fatal("expected detail for exceeded budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 1, 10*time.Minute, 120); !exceeded {
		t.Fatal("first retry spend should exceed budget")
	}
}

func TestExceedsRetryBudget_SubsequentRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 45 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  30 * time.Minute,
		NextCostCents:  25,
	}
	if exceeded, _ := exceedsRetryBudget(budget, 2, 20*time.Minute, 20); exceeded {
		t.Fatal("subsequent retry should not exceed budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 2, 31*time.Minute, 20); !exceeded {
		t.Fatal("subsequent retry time should exceed budget")
	}
	if exceeded, _ := exceedsRetryBudget(budget, 3, 20*time.Minute, 30); !exceeded {
		t.Fatal("subsequent retry spend should exceed budget")
	}
}

func TestLaunchElapsedAndSpendCents(t *testing.T) {
	start := time.Now().Add(-30 * time.Minute).Unix()
	end := time.Now().Add(-5 * time.Minute).Unix()
	ci := &db.Launch{
		LaunchedAt:         &start,
		EndedAt:            &end,
		CostPerHourCents:   120,
		ActualSpendCents:   0,
		ProviderRunningAt:  nil,
		ProviderInstanceID: "x",
	}
	elapsed, cents := launchElapsedAndSpendCents(ci, time.Now())
	if elapsed < 24*time.Minute || elapsed > 26*time.Minute {
		t.Fatalf("elapsed = %v, want about 25m", elapsed)
	}
	if cents < 49 || cents > 51 {
		t.Fatalf("spend cents = %d, want about 50", cents)
	}

	ci.ActualSpendCents = 77
	_, cents = launchElapsedAndSpendCents(ci, time.Now())
	if cents != 77 {
		t.Fatalf("actual spend should win, got %d", cents)
	}
}

func TestApplyRetryBudgetMultiplier_FirstRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 10 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  20 * time.Minute,
		NextCostCents:  50,
	}
	scaled := applyRetryBudgetMultiplier(budget, 1, 2.0)
	if scaled.FirstTimeLimit != 20*time.Minute {
		t.Fatalf("first time limit = %s, want 20m", scaled.FirstTimeLimit)
	}
	if scaled.FirstCostCents != 200 {
		t.Fatalf("first cost = %d, want 200", scaled.FirstCostCents)
	}
	if scaled.NextTimeLimit != budget.NextTimeLimit || scaled.NextCostCents != budget.NextCostCents {
		t.Fatal("next-tier limits should remain unchanged for first retry scaling")
	}
}

func TestApplyRetryBudgetMultiplier_SubsequentRetry(t *testing.T) {
	budget := RetryBudget{
		FirstTimeLimit: 10 * time.Minute,
		FirstCostCents: 100,
		NextTimeLimit:  15 * time.Minute,
		NextCostCents:  30,
	}
	scaled := applyRetryBudgetMultiplier(budget, 2, 2.0)
	if scaled.NextTimeLimit != 30*time.Minute {
		t.Fatalf("next time limit = %s, want 30m", scaled.NextTimeLimit)
	}
	if scaled.NextCostCents != 60 {
		t.Fatalf("next cost = %d, want 60", scaled.NextCostCents)
	}
	if scaled.FirstTimeLimit != budget.FirstTimeLimit || scaled.FirstCostCents != budget.FirstCostCents {
		t.Fatal("first-tier limits should remain unchanged for subsequent retry scaling")
	}
}

func TestRetryBudgetUsage_NoStartConsumesNoBudget(t *testing.T) {
	now := time.Unix(2_000, 0)
	launchedAt := int64(1_000)
	endedAt := int64(1_900)
	facts := relaunchAttemptFacts{
		LastLaunch: &db.Launch{
			LaunchedAt:       &launchedAt,
			EndedAt:          &endedAt,
			CostPerHourCents: 200,
		},
		LastAttemptStartTime: 0,
		LastAttemptEndTime:   &endedAt,
	}

	elapsed, spend := retryBudgetUsage(facts, now)
	if elapsed != 0 {
		t.Fatalf("elapsed = %v, want 0", elapsed)
	}
	if spend != 0 {
		t.Fatalf("spend = %d, want 0", spend)
	}
}

func TestRetryBudgetUsage_ProratesActualSpendByAttemptElapsed(t *testing.T) {
	now := time.Unix(4_000, 0)
	launchedAt := int64(1_000)
	endedAt := int64(3_000) // 2000s launch elapsed
	attemptStart := int64(2_000)
	attemptEnd := int64(3_000) // 1000s attempt elapsed (50%)
	facts := relaunchAttemptFacts{
		LastLaunch: &db.Launch{
			LaunchedAt:       &launchedAt,
			EndedAt:          &endedAt,
			ActualSpendCents: 300,
		},
		LastAttemptStartTime: attemptStart,
		LastAttemptEndTime:   &attemptEnd,
	}

	elapsed, spend := retryBudgetUsage(facts, now)
	if elapsed != time.Duration(attemptEnd-attemptStart)*time.Second {
		t.Fatalf("elapsed = %v, want %v", elapsed, time.Duration(attemptEnd-attemptStart)*time.Second)
	}
	if spend != 150 {
		t.Fatalf("spend = %d, want 150", spend)
	}
}

func TestRelaunchGroupingJobsTreatsUnplacedPendingPlacementAsQueued(t *testing.T) {
	queued := &db.Job{ID: 1, Status: db.StatusQueued}
	pendingUnplaced := &db.Job{ID: 2, Status: db.StatusPendingPlacement}
	pendingPlaced := &db.Job{ID: 3, Status: db.StatusPendingPlacement, LaunchID: testInt64Ptr(77)}

	grouping := relaunchGroupingJobs([]*db.Job{queued, pendingUnplaced, pendingPlaced})
	if len(grouping) != 3 {
		t.Fatalf("grouping len = %d, want 3", len(grouping))
	}

	if grouping[0] != queued {
		t.Fatal("queued job should be passed through")
	}
	if grouping[1] == pendingUnplaced {
		t.Fatal("pending unplaced job should be copied before normalization")
	}
	if grouping[1].Status != db.StatusQueued {
		t.Fatalf("pending unplaced normalized status = %q, want %q", grouping[1].Status, db.StatusQueued)
	}
	if pendingUnplaced.Status != db.StatusPendingPlacement {
		t.Fatalf("original pending unplaced status mutated to %q", pendingUnplaced.Status)
	}
	if grouping[2] != pendingPlaced {
		t.Fatal("pending placed job should not be rewritten")
	}
}

func testInt64Ptr(v int64) *int64 {
	return &v
}
