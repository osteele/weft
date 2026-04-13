package orchestration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestRunGroupedAutoPilotPass_DoesNotRelaunchPlannerBlockedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   nil,
			BlockedReasons: map[int64]string{jobID: "planner: capacity unavailable"},
		}, nil
	}

	relaunchCalls := 0
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchCalls++
		return &campaign.RelaunchResult{}, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if relaunchCalls != 0 {
		t.Fatalf("relaunch calls = %d, want 0", relaunchCalls)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Launched != 0 {
		t.Fatalf("Launched = %d, want 0", result.Launched)
	}
	if got := result.BlockedReasons[jobID]; got != "planner: capacity unavailable" {
		t.Fatalf("blocked reason = %q, want %q", got, "planner: capacity unavailable")
	}
}

func TestRunGroupedAutoPilotPass_FallbackRelaunchesWhenPlannerReturnsNoDecisions(t *testing.T) {
	database := db.SetupTestDB(t)

	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchJobIDs:   nil,
			BlockedReasons: map[int64]string{},
		}, nil
	}

	relaunchCalls := 0
	var gotScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchCalls++
		gotScope = append([]int64(nil), scope...)
		return &campaign.RelaunchResult{}, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if relaunchCalls != 1 {
		t.Fatalf("relaunch calls = %d, want 1", relaunchCalls)
	}

	got := map[int64]bool{}
	for _, id := range gotScope {
		got[id] = true
	}
	if len(got) != 2 || !got[jobA] || !got[jobB] {
		t.Fatalf("relaunch scope = %v, want both jobs [%d %d]", gotScope, jobA, jobB)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
}
