package orchestration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func TestRunGroupedAutoPilotPass_DoesNotRelaunchPlannerBlockedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "blocked job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
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

func TestRunGroupedAutoPilotPass_SkipsComputeIntensiveReuseBelowCPUFloor(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "cpu-heavy", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{db.TagComputeIntensive}); err != nil {
		t.Fatalf("SetJobTags: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		GPUMemGB:         80,
		ResolvedGPUName:  "A100",
		CPUCores:         8,
		CostPerHourCents: 100,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job: job,
				Instance: campaign.InstanceCapacity{
					Instance:   inst,
					DiskFreeGB: 100,
				},
			}},
			BlockedReasons: map[int64]string{},
		}, nil
	}
	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ int64, _ []*db.Job) error {
		submitCalls++
		return nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return &campaign.RelaunchResult{}, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 0 {
		t.Fatalf("submit calls = %d, want 0", submitCalls)
	}
	if result.BlockedReasons[jobID] == "" || !strings.Contains(result.BlockedReasons[jobID], "CPU cores insufficient") {
		t.Fatalf("blocked reason = %q, want CPU floor reason", result.BlockedReasons[jobID])
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
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
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

func TestSelectLaunchGroupsWithinHeadroom_PicksBestFitByJobsPerCost(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1, 2}, CostPerHourCents: 50},
		{JobIDs: []int64{3}, CostPerHourCents: 99},
		{JobIDs: []int64{4}, CostPerHourCents: 110},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if len(accepted) != 1 {
		t.Fatalf("accepted groups = %d, want 1", len(accepted))
	}
	if accepted[0].CostPerHourCents != 50 {
		t.Fatalf("accepted group cost = %d, want 50", accepted[0].CostPerHourCents)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected groups = %d, want 2", len(rejected))
	}
	if used != 50 {
		t.Fatalf("used = %d, want 50", used)
	}
}

func TestSelectLaunchGroupsWithinHeadroom_NoneFit(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1}, CostPerHourCents: 150},
		{JobIDs: []int64{2}, CostPerHourCents: 200},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if len(accepted) != 0 {
		t.Fatalf("accepted groups = %d, want 0", len(accepted))
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected groups = %d, want 2", len(rejected))
	}
	if used != 0 {
		t.Fatalf("used = %d, want 0", used)
	}
}

func TestSelectLaunchGroupsWithinHeadroom_PrefersPriorityGroup(t *testing.T) {
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{1}, CostPerHourCents: 100, Priority: 0},
		{JobIDs: []int64{2}, CostPerHourCents: 100, Priority: 1},
	}

	accepted, rejected, used := selectLaunchGroupsWithinHeadroom(groups, 100)
	if used != 100 {
		t.Fatalf("used = %d, want 100", used)
	}
	if len(accepted) != 1 || accepted[0].JobIDs[0] != 2 {
		t.Fatalf("accepted = %+v, want priority job 2", accepted)
	}
	if len(rejected) != 1 || rejected[0].JobIDs[0] != 1 {
		t.Fatalf("rejected = %+v, want normal job 1", rejected)
	}
}

func TestApplyAcceptedLaunchGroups_UpdatesLegacyLaunchFields(t *testing.T) {
	plan := campaign.AutoPlacementPlan{
		LaunchJobIDs: []int64{1, 2, 3},
		BlockedReasons: map[int64]string{
			1: "old reason",
			2: "old reason",
		},
	}
	groups := []campaign.LaunchGroup{
		{JobIDs: []int64{3, 1}, CostPerHourCents: 70},
	}

	applyAcceptedLaunchGroups(&plan, groups, plan.BlockedReasons)
	if plan.LaunchRateCentsPerHour != 70 {
		t.Fatalf("launch rate = %d, want 70", plan.LaunchRateCentsPerHour)
	}
	if len(plan.LaunchJobIDs) != 2 || plan.LaunchJobIDs[0] != 1 || plan.LaunchJobIDs[1] != 3 {
		t.Fatalf("launch job ids = %v, want [1 3]", plan.LaunchJobIDs)
	}
	if _, exists := plan.BlockedReasons[1]; exists {
		t.Fatalf("blocked reason for accepted job 1 should be cleared, got %q", plan.BlockedReasons[1])
	}
}

func TestNoRentalHeadroomReason_IncludesMatchDiagnostic(t *testing.T) {
	mem := 80
	job := &db.Job{GPUClass: "A100", GPUMemGB: &mem}
	caps := []campaign.InstanceCapacity{{
		Instance: &db.Launch{
			GPUClass: "A100",
			GPUMemGB: 40,
		},
	}}

	reason := noRentalHeadroomReason(job, caps, nil)
	if !strings.Contains(reason, "no rental headroom; running instances couldn't accept this job") {
		t.Fatalf("reason = %q, want base message", reason)
	}
	if !strings.Contains(reason, "GPU memory insufficient") {
		t.Fatalf("reason = %q, want MatchJobToInstance diagnostic", reason)
	}
}

func TestRunGroupedAutoPilotPass_NoSubsetFitsTriggersPreferReuseRetry(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		ResolvedGPUName:  "A100",
		GPUMemGB:         80,
		CostPerHourCents: 100,
		DiskGB:           200,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups:           []campaign.LaunchGroup{{JobIDs: []int64{jobID}, CostPerHourCents: 150}},
			LaunchJobIDs:           []int64{jobID},
			LaunchRateCentsPerHour: 150,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	retryCalled := false
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		retryCalled = true
		if !options.PreferReuse {
			t.Fatalf("retry options PreferReuse = false, want true")
		}
		return campaign.AutoPlacementPlan{
			BlockedReasons: map[int64]string{},
		}, nil
	}

	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch should not be called when no subset fits and retry does not produce reuse")
		return nil, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if !retryCalled {
		t.Fatalf("expected PreferReuse retry call")
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if got := result.BlockedReasons[jobID]; !strings.Contains(got, "no subset fits") {
		t.Fatalf("blocked reason = %q, want no-subset-fits marker", got)
	}
}

func TestRunGroupedAutoPilotPass_RunRateAllFitLeavesLaunchScope(t *testing.T) {
	database := db.SetupTestDB(t)
	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 10.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups: []campaign.LaunchGroup{
				{JobIDs: []int64{jobA}, CostPerHourCents: 120},
				{JobIDs: []int64{jobB}, CostPerHourCents: 180},
			},
			LaunchJobIDs:           []int64{jobA, jobB},
			LaunchRateCentsPerHour: 300,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	var gotScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		gotScope = append([]int64(nil), scope...)
		return &campaign.RelaunchResult{}, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
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

func TestRunGroupedAutoPilotPass_RunRateNoneFitBlocksAll(t *testing.T) {
	database := db.SetupTestDB(t)
	jobA, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_a.py", "job a", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobA: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train_b.py", "job b", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU jobB: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups: []campaign.LaunchGroup{
				{JobIDs: []int64{jobA}, CostPerHourCents: 120},
				{JobIDs: []int64{jobB}, CostPerHourCents: 150},
			},
			LaunchJobIDs:           []int64{jobA, jobB},
			LaunchRateCentsPerHour: 270,
			BlockedReasons:         map[int64]string{},
		}, nil
	}
	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity, _ campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{BlockedReasons: map[int64]string{}}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		t.Fatalf("autoPilotRelaunch should not run when no subset fits")
		return nil, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if got := result.BlockedReasons[jobA]; !strings.Contains(got, "no subset fits") {
		t.Fatalf("blocked reason A = %q, want no-subset-fits marker", got)
	}
	if got := result.BlockedReasons[jobB]; !strings.Contains(got, "no subset fits") {
		t.Fatalf("blocked reason B = %q, want no-subset-fits marker", got)
	}
}

func TestRunGroupedAutoPilotPass_ReuseFallbackExecutesAssignmentsWithoutLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         "A100",
		ResolvedGPUName:  "A100",
		GPUMemGB:         80,
		CostPerHourCents: 100,
		DiskGB:           200,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 1.0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreConfig := config.SetConfigPathsForTesting(cfgPath, filepath.Join(cfgDir, "config.yaml"))
	defer restoreConfig()

	originalBuildPlan := autoPilotBuildPlan
	originalBuildPlanWithOptions := autoPilotBuildPlanWithOptions
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotBuildPlanWithOptions = originalBuildPlanWithOptions
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, _ []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		return campaign.AutoPlacementPlan{
			LaunchGroups:           []campaign.LaunchGroup{{JobIDs: []int64{jobID}, CostPerHourCents: 150}},
			LaunchJobIDs:           []int64{jobID},
			LaunchRateCentsPerHour: 150,
			BlockedReasons:         map[int64]string{},
		}, nil
	}

	autoPilotBuildPlanWithOptions = func(_ *sql.DB, _ *config.Config, jobs []*db.Job, capacities []campaign.InstanceCapacity, options campaign.PlanOptions) (campaign.AutoPlacementPlan, error) {
		if !options.PreferReuse {
			t.Fatalf("retry options PreferReuse = false, want true")
		}
		if len(jobs) == 0 || len(capacities) == 0 || capacities[0].Instance == nil {
			t.Fatalf("expected jobs/capacities in retry")
		}
		return campaign.AutoPlacementPlan{
			ReuseAssignments: []campaign.ReuseAssignment{{
				Job:      jobs[0],
				Instance: capacities[0],
			}},
			BlockedReasons: map[int64]string{},
		}, nil
	}

	submitCalls := 0
	autoPilotSubmitJobsToInstance = func(_ context.Context, _ *sql.DB, _ *r2.Client, instanceID int64, jobs []*db.Job) error {
		submitCalls++
		if len(jobs) != 1 || jobs[0] == nil || jobs[0].ID != jobID {
			t.Fatalf("submitted jobs = %#v, want [%d]", jobs, jobID)
		}
		if instanceID == 0 {
			t.Fatalf("instanceID = 0, want running instance id")
		}
		return nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		if len(scope) != 0 {
			t.Fatalf("expected no launch scope after reuse fallback, got %v", scope)
		}
		return &campaign.RelaunchResult{}, nil
	}

	result, err := RunGroupedAutoPilotPass(context.Background(), database, nil)
	if err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}
	if submitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1", submitCalls)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Placed != 1 {
		t.Fatalf("Placed = %d, want 1", result.Placed)
	}
	if result.Launched != 0 {
		t.Fatalf("Launched = %d, want 0", result.Launched)
	}
}

func TestMergeRelaunchReasonsIntoBlockedReasons_PrefersPerJobOverPerInstance(t *testing.T) {
	// Three sibling jobs (PFT, ASIDE, AIR eval) all came from the same prior
	// failed cloud instance, so they share one failedInstanceID. Each has its
	// own waiting-on-producer reason; recordNotReplacedReason aggregated those
	// under the single instance key as "multiple reasons (...)". The per-job
	// JobReasons map carries the accurate per-job reason. The merge must
	// surface each job's own reason rather than the aggregated one.
	const failedInstanceID int64 = 9001
	pftReason := `waiting for "output/exp_053_pft_d256/ise_masked_lossweighted.pt" from wj1602 (queued)`
	asideReason := `waiting for "output/exp_053_aside/ise_masked_lossweighted.pt" from wj1603 (queued)`
	airReason := `waiting for "output/exp_053_air/ise_masked_lossweighted.pt" from wj1604 (queued)`
	combined := "multiple reasons (" + pftReason + "; " + asideReason + "; " + airReason + ")"

	result := &campaign.RelaunchResult{
		NotReplacedReasons: map[int64]string{failedInstanceID: combined},
		JobReasons: map[int64]string{
			1605: pftReason,
			1606: asideReason,
			1607: airReason,
		},
	}
	failedInstanceByJob := map[int64]int64{
		1605: failedInstanceID,
		1606: failedInstanceID,
		1607: failedInstanceID,
	}

	blockedReasons := map[int64]string{}
	mergeRelaunchReasonsIntoBlockedReasons(blockedReasons, result, failedInstanceByJob)

	for jobID, want := range map[int64]string{
		1605: pftReason,
		1606: asideReason,
		1607: airReason,
	} {
		got := blockedReasons[jobID]
		if got != want {
			t.Errorf("blockedReasons[%d] = %q, want %q", jobID, got, want)
		}
		if strings.HasPrefix(got, "multiple reasons (") {
			t.Errorf("blockedReasons[%d] should not be the aggregated string, got %q", jobID, got)
		}
	}
}

func TestMergeRelaunchReasonsIntoBlockedReasons_FallsBackToPerInstance(t *testing.T) {
	// When a job has no per-job entry but its predecessor instance does, the
	// per-instance reason fills in as a fallback.
	result := &campaign.RelaunchResult{
		NotReplacedReasons: map[int64]string{42: "no offers available"},
		JobReasons:         map[int64]string{},
	}
	failedInstanceByJob := map[int64]int64{100: 42}

	blockedReasons := map[int64]string{}
	mergeRelaunchReasonsIntoBlockedReasons(blockedReasons, result, failedInstanceByJob)

	if got, want := blockedReasons[100], "no offers available"; got != want {
		t.Errorf("blockedReasons[100] = %q, want %q", got, want)
	}
}

func TestRunGroupedAutoPilotPass_ExcludesJobsWithOpenMoveIntent(t *testing.T) {
	// Regression: when the user has manually moved a job, an open
	// MoveIntent prevents the autopilot from racing the move flow and
	// reusing some other existing instance for the same job. See
	// specs/job-move.allium § AutopilotIgnoresMovingJobs.
	database := db.SetupTestDB(t)

	moving, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU moving: %v", err)
	}
	other, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python b.py", "other", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU other: %v", err)
	}

	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID: moving, TargetKind: db.MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	var seenUnplaced []int64
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, unplaced []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		for _, j := range unplaced {
			if j != nil {
				seenUnplaced = append(seenUnplaced, j.ID)
			}
		}
		return campaign.AutoPlacementPlan{}, nil
	}

	var relaunchScope []int64
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, scope []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		relaunchScope = append([]int64(nil), scope...)
		return &campaign.RelaunchResult{}, nil
	}

	if _, err := RunGroupedAutoPilotPass(context.Background(), database, nil); err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}

	for _, id := range seenUnplaced {
		if id == moving {
			t.Errorf("planner saw moving job %d in unplaced set", moving)
		}
	}
	if len(seenUnplaced) == 0 || seenUnplaced[0] != other {
		t.Errorf("planner saw %v, expected only [%d]", seenUnplaced, other)
	}
	for _, id := range relaunchScope {
		if id == moving {
			t.Errorf("relaunch scope included moving job %d", moving)
		}
	}
}

func TestRunGroupedAutoPilotPass_ExcludesJobsWithOpenPlacementIntent(t *testing.T) {
	// Regression: a job with an open PlacementIntent (e.g. inside an
	// in-flight RelaunchOrphanedJobs or bulk move) is invisible to the
	// autopilot's planner and relaunch scope. See specs/job-move.allium
	// § AutopilotIgnoresMovingJobs (intent-inclusive).
	database := db.SetupTestDB(t)

	placing, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "placing", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU placing: %v", err)
	}
	other, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python b.py", "other", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU other: %v", err)
	}
	if _, err := db.CreatePlacementIntent(database, placing, "test"); err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}

	originalBuildPlan := autoPilotBuildPlan
	originalRelaunch := autoPilotRelaunch
	originalSubmit := autoPilotSubmitJobsToInstance
	t.Cleanup(func() {
		autoPilotBuildPlan = originalBuildPlan
		autoPilotRelaunch = originalRelaunch
		autoPilotSubmitJobsToInstance = originalSubmit
	})

	var seenUnplaced []int64
	autoPilotBuildPlan = func(_ *sql.DB, _ *config.Config, unplaced []*db.Job, _ []campaign.InstanceCapacity) (campaign.AutoPlacementPlan, error) {
		for _, j := range unplaced {
			if j != nil {
				seenUnplaced = append(seenUnplaced, j.ID)
			}
		}
		return campaign.AutoPlacementPlan{}, nil
	}
	autoPilotRelaunch = func(_ *sql.DB, _ *config.Config, _ int, _ map[int64]float64, _ []int64, _ string, _ bool, _ bool) (*campaign.RelaunchResult, error) {
		return &campaign.RelaunchResult{}, nil
	}

	if _, err := RunGroupedAutoPilotPass(context.Background(), database, nil); err != nil {
		t.Fatalf("RunGroupedAutoPilotPass: %v", err)
	}

	for _, id := range seenUnplaced {
		if id == placing {
			t.Errorf("planner saw placing job %d in unplaced set", placing)
		}
	}
	if len(seenUnplaced) == 0 || seenUnplaced[0] != other {
		t.Errorf("planner saw %v, expected only [%d]", seenUnplaced, other)
	}
}
