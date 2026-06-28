package campaign

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

func TestBuildProfilePlansFromSplitRawWithProgressReportsStages(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
	}
	splitRaw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{ProviderID: "rtx3090", GPUName: "RTX 3090", CostPerHour: 0.30},
		},
	}}

	seen := map[string]bool{}
	plans := BuildProfilePlansFromSplitRawWithProgress(
		nil,
		nil,
		[]InstanceGroup{group},
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		[]bidding.ScoreProfile{bidding.StrategyCheap.Profile()},
		0.95,
		0,
		func(progress PlanProgress) {
			if progress.Phase != "" {
				seen[progress.Phase] = true
			}
		},
	)

	if _, ok := plans[bidding.StrategyCheap.Profile().ID]; !ok {
		t.Fatalf("expected cheap profile plan, got %#v", plans)
	}
	for _, phase := range []string{
		"Estimating raw-offer runtimes",
		"Planning tradeoff profiles",
		"Ranking direct-offer groups",
		"Estimating direct-offer costs",
		"Checking reusable instances",
	} {
		if !seen[phase] {
			t.Fatalf("expected progress phase %q, got %#v", phase, seen)
		}
	}
}

func TestDefaultProfilePlanSpecs_FastestUsesParallelPreferred(t *testing.T) {
	specs := defaultProfilePlanSpecs([]bidding.ScoreProfile{
		bidding.StrategyCheap.Profile(),
		bidding.StrategyFast.Profile(),
		bidding.StrategyFastest.Profile(),
	})
	if len(specs) != 3 {
		t.Fatalf("spec count = %d, want 3", len(specs))
	}
	if specs[0].CandidateMode != CandidatePlanModeFull {
		t.Fatalf("cheap mode = %v, want full", specs[0].CandidateMode)
	}
	if specs[1].CandidateMode != CandidatePlanModeFull {
		t.Fatalf("fast mode = %v, want full", specs[1].CandidateMode)
	}
	if specs[2].CandidateMode != CandidatePlanModeParallelPreferred {
		t.Fatalf("fastest mode = %v, want parallel-preferred", specs[2].CandidateMode)
	}
}

func TestRankOfferWithPredictedRuntime_PreservesFilterStatsAfterCUDAFilter(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "RTX-5090",
		Jobs:     []*db.Job{{ID: 1229}},
	}
	offer := cloud.Offer{
		ProviderID:  "o1",
		GPUName:     "RTX 5090",
		GPUMemGB:    32,
		CostPerHour: 1.0,
	}
	predicted := map[string]offerRuntimePrediction{
		offerPredictionKey(offer): {
			complete:       true,
			feasible:       true,
			totalRunHrs:    1,
			adjustedRunHrs: 1,
		},
	}

	got, ok := rankOfferWithPredictedRuntime(
		group,
		[]cloud.Offer{offer},
		nil,
		bidding.ConstantSetup(0),
		bidding.StrategyCheap.Profile(),
		0,
		predicted,
	)
	if !ok {
		t.Fatal("expected ranking to complete")
	}
	if got.Offer != nil {
		t.Fatalf("expected nil selected offer due to CUDA filter, got %#v", got.Offer)
	}
	if got.FilterStats.RawCount != 1 || got.FilterStats.AfterCUDA != 0 {
		t.Fatalf("unexpected filter stats: %#v", got.FilterStats)
	}
	detail := got.FilterStats.NoOffersDetail("")
	if strings.Contains(detail, "no offers from providers") {
		t.Fatalf("unexpected provider-empty detail after non-empty raw offers: %q", detail)
	}
	if !strings.Contains(detail, "CUDA compatibility") {
		t.Fatalf("expected CUDA compatibility detail, got %q", detail)
	}
}

func TestBuildProfilePlansFromSplitRawWithSession_SplitOnlySkipsExpandedCandidates(t *testing.T) {
	originalFetchCandidates := fetchCandidateGroupingsForPlanning
	originalFetchRaw := fetchGroupRawOffersForPlanning
	t.Cleanup(func() {
		fetchCandidateGroupingsForPlanning = originalFetchCandidates
		fetchGroupRawOffersForPlanning = originalFetchRaw
	})

	fetchCandidateGroupingsForPlanning = func(_ *offerSearchSession, _ []InstanceGroup) []GroupingCandidate {
		t.Fatal("split-only pass should not fetch merged/parallel candidates")
		return nil
	}
	fetchGroupRawOffersForPlanning = func(_ *offerSearchSession, _ []InstanceGroup) []GroupRawOffers {
		t.Fatal("split-only pass should not fetch merged candidates")
		return nil
	}

	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
	}
	splitRaw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", CostPerHour: 0.40},
		},
	}}

	plans := buildProfilePlansFromSplitRawWithSession(
		nil,
		[]InstanceGroup{group},
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		newOfferSearchSession(nil, 0.95),
		[]ProfilePlanSpec{{
			Profile:       bidding.StrategyFast.Profile(),
			CandidateMode: CandidatePlanModeSplitOnly,
		}},
		0,
		nil,
		defaultPlanOptions(),
	)

	plan, ok := plans[bidding.StrategyFast.Profile().ID]
	if !ok {
		t.Fatalf("expected fast plan, got %#v", plans)
	}
	if plan.NewCandidate == nil || plan.NewCandidate.Label != "split" {
		t.Fatalf("expected split-only candidate, got %#v", plan.NewCandidate)
	}
	if len(plan.DisplayOffers) != 1 || plan.DisplayOffers[0].Offer == nil {
		t.Fatalf("expected split offer to be displayed, got %#v", plan.DisplayOffers)
	}
}

func TestBuildProfilePlansFromSplitRawWithSession_ParallelPreferredChoosesParallelWhenLaunchable(t *testing.T) {
	originalFetchRaw := fetchGroupRawOffersForPlanning
	t.Cleanup(func() {
		fetchGroupRawOffersForPlanning = originalFetchRaw
	})

	fetchGroupRawOffersForPlanning = func(_ *offerSearchSession, groups []InstanceGroup) []GroupRawOffers {
		raw := make([]GroupRawOffers, len(groups))
		for i, group := range groups {
			raw[i] = GroupRawOffers{Group: group}
		}
		switch len(groups) {
		case 1:
			// merged candidate
			raw[0].Offers = []cloud.Offer{{ProviderID: "merged", GPUName: "RTX 4090", CostPerHour: 0.30}}
		case 4:
			// parallel candidate (fully launchable). Each sub-group gets a
			// distinct ProviderID + MachineID so the in-pass exclusion
			// (rankGroupOffersFromPredictions) doesn't drop sub-groups for
			// claiming the same machine — running four jobs in parallel
			// requires four distinct instances.
			for i := range raw {
				raw[i].Offers = []cloud.Offer{{
					ProviderID:  fmt.Sprintf("parallel-%d", i),
					MachineID:   fmt.Sprintf("p-%d", i),
					GPUName:     "RTX 4090",
					CostPerHour: 0.60,
				}}
			}
		default:
			t.Fatalf("unexpected candidate size %d", len(groups))
		}
		return raw
	}

	groups := []InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 1, Command: "python a.py"}, {ID: 2, Command: "python b.py"}},
		},
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 3, Command: "python c.py"}, {ID: 4, Command: "python d.py"}},
		},
	}
	splitRaw := []GroupRawOffers{
		{
			Group: groups[0],
			Offers: []cloud.Offer{
				{ProviderID: "split-a", GPUName: "RTX 4090", CostPerHour: 0.20},
			},
		},
		{
			Group: groups[1],
			Offers: []cloud.Offer{
				{ProviderID: "split-b", GPUName: "RTX 4090", CostPerHour: 0.20},
			},
		},
	}

	plans := buildProfilePlansFromSplitRawWithSession(
		nil,
		groups,
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		newOfferSearchSession(nil, 0.95),
		[]ProfilePlanSpec{{
			Profile:       bidding.StrategyFastest.Profile(),
			CandidateMode: CandidatePlanModeParallelPreferred,
		}},
		0,
		nil,
		defaultPlanOptions(),
	)

	plan, ok := plans[bidding.StrategyFastest.Profile().ID]
	if !ok {
		t.Fatalf("expected fastest plan, got %#v", plans)
	}
	if plan.NewCandidate == nil || plan.NewCandidate.Label != "parallel" {
		t.Fatalf("expected parallel candidate, got %#v", plan.NewCandidate)
	}
}

func TestBuildProfilePlansFromSplitRawWithSession_ParallelPreferredFallsBackWhenParallelIncomplete(t *testing.T) {
	originalFetchRaw := fetchGroupRawOffersForPlanning
	t.Cleanup(func() {
		fetchGroupRawOffersForPlanning = originalFetchRaw
	})

	fetchGroupRawOffersForPlanning = func(_ *offerSearchSession, groups []InstanceGroup) []GroupRawOffers {
		raw := make([]GroupRawOffers, len(groups))
		for i, group := range groups {
			raw[i] = GroupRawOffers{Group: group}
		}
		switch len(groups) {
		case 1:
			// merged candidate: valid and cheaper than split, so it should win non-parallel fallback.
			raw[0].Offers = []cloud.Offer{{ProviderID: "merged", GPUName: "RTX 4090", CostPerHour: 0.10}}
		case 4:
			// parallel candidate: one missing offer => incomplete => must not be selected.
			for i := range raw {
				raw[i].Offers = []cloud.Offer{{ProviderID: "parallel", GPUName: "RTX 4090", CostPerHour: 0.60}}
			}
			raw[0].Offers = nil
		default:
			t.Fatalf("unexpected candidate size %d", len(groups))
		}
		return raw
	}

	groups := []InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 1, Command: "python a.py"}, {ID: 2, Command: "python b.py"}},
		},
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 3, Command: "python c.py"}, {ID: 4, Command: "python d.py"}},
		},
	}
	splitRaw := []GroupRawOffers{
		{
			Group: groups[0],
			Offers: []cloud.Offer{
				{ProviderID: "split-a", GPUName: "RTX 4090", CostPerHour: 0.25},
			},
		},
		{
			Group: groups[1],
			Offers: []cloud.Offer{
				{ProviderID: "split-b", GPUName: "RTX 4090", CostPerHour: 0.25},
			},
		},
	}

	plans := buildProfilePlansFromSplitRawWithSession(
		nil,
		groups,
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		newOfferSearchSession(nil, 0.95),
		[]ProfilePlanSpec{{
			Profile:       bidding.StrategyFastest.Profile(),
			CandidateMode: CandidatePlanModeParallelPreferred,
		}},
		0,
		nil,
		defaultPlanOptions(),
	)

	plan, ok := plans[bidding.StrategyFastest.Profile().ID]
	if !ok {
		t.Fatalf("expected fastest plan, got %#v", plans)
	}
	if plan.NewCandidate == nil {
		t.Fatalf("expected fallback non-parallel candidate, got %#v", plan.NewCandidate)
	}
	if plan.NewCandidate.Label == "parallel" {
		t.Fatalf("expected non-parallel fallback candidate, got %#v", plan.NewCandidate)
	}
}

func TestBuildProfilePlansFromSplitRawWithSession_MergedPreferredUsesMergedCandidate(t *testing.T) {
	originalFetchRaw := fetchGroupRawOffersForPlanning
	t.Cleanup(func() {
		fetchGroupRawOffersForPlanning = originalFetchRaw
	})

	fetchGroupRawOffersForPlanning = func(_ *offerSearchSession, groups []InstanceGroup) []GroupRawOffers {
		if len(groups) != 1 {
			t.Fatalf("merged fetch group count = %d, want 1", len(groups))
		}
		return []GroupRawOffers{{
			Group: groups[0],
			Offers: []cloud.Offer{
				{ProviderID: "merged", GPUName: "RTX 4090", CostPerHour: 0.60},
			},
		}}
	}

	groups := []InstanceGroup{
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
		},
		{
			GPUClass: "NVIDIA",
			GPUMemGB: 12,
			Jobs:     []*db.Job{{ID: 2, Command: "python train.py --epochs 1"}},
		},
	}
	splitRaw := []GroupRawOffers{
		{
			Group: groups[0],
			Offers: []cloud.Offer{
				{ProviderID: "split-a", GPUName: "RTX 4090", CostPerHour: 0.40},
			},
		},
		{
			Group: groups[1],
			Offers: []cloud.Offer{
				{ProviderID: "split-b", GPUName: "RTX 4090", CostPerHour: 0.40},
			},
		},
	}

	plans := buildProfilePlansFromSplitRawWithSession(
		nil,
		groups,
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		newOfferSearchSession(nil, 0.95),
		[]ProfilePlanSpec{{
			Profile:       bidding.StrategyCheap.Profile(),
			CandidateMode: CandidatePlanModeMergedPreferred,
		}},
		0,
		nil,
		defaultPlanOptions(),
	)

	plan, ok := plans[bidding.StrategyCheap.Profile().ID]
	if !ok {
		t.Fatalf("expected cheap plan, got %#v", plans)
	}
	if plan.NewCandidate == nil || plan.NewCandidate.Label != "merged" {
		t.Fatalf("expected merged candidate, got %#v", plan.NewCandidate)
	}
	for _, offer := range plan.DisplayOffers {
		if offer.Offer == nil || offer.Offer.ProviderID != "merged" {
			t.Fatalf("expected merged offer to be mapped back to split groups, got %#v", plan.DisplayOffers)
		}
	}
}

func TestPruneReuseCandidates_LimitsPoolAndKeepsStrongCandidates(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		GPUMemGB: 12,
		Jobs: []*db.Job{{
			ID:     1,
			Inputs: []string{"hf:model-a", "hf:model-b"},
		}},
	}

	working := make([]InstanceCapacity, 0, maxReuseCandidatesPerGroup+8)
	for i := 0; i < maxReuseCandidatesPerGroup+8; i++ {
		working = append(working, InstanceCapacity{
			Instance: &db.Launch{
				ID:              int64(100 + i),
				Status:          db.LaunchStatusRunning,
				GPUClass:        "NVIDIA",
				ResolvedGPUName: "RTX 3090",
				GPUMemGB:        24,
				DLPerf:          5,
			},
			ProvisionedInputs: []string{},
			RunningJobCount:   10 + i,
			DiskFreeGB:        20,
		})
	}
	working = append(working, InstanceCapacity{
		Instance: &db.Launch{
			ID:              7,
			Status:          db.LaunchStatusGrace,
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "RTX 4090",
			GPUMemGB:        24,
			DLPerf:          20,
		},
		ProvisionedInputs: []string{"hf:model-a", "hf:model-b"},
		DiskFreeGB:        200,
	})
	working = append(working, InstanceCapacity{
		Instance: &db.Launch{
			ID:              8,
			Status:          db.LaunchStatusRunning,
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "H100",
			GPUMemGB:        80,
			DLPerf:          40,
		},
		ProvisionedInputs: []string{"hf:model-a"},
		RunningJobCount:   0,
		DiskFreeGB:        200,
	})
	working = append(working, InstanceCapacity{
		Instance: &db.Launch{
			ID:              9,
			Status:          db.LaunchStatusRunning,
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "RTX 2060",
			GPUMemGB:        8,
			DLPerf:          50,
		},
		DiskFreeGB: 200,
	})

	candidates := pruneReuseCandidates(group, working, false)
	if len(candidates) != maxReuseCandidatesPerGroup {
		t.Fatalf("pruned candidate count = %d, want %d", len(candidates), maxReuseCandidatesPerGroup)
	}

	seen := map[int64]bool{}
	for _, candidate := range candidates {
		seen[candidate.cap.Instance.ID] = true
	}
	if !seen[7] {
		t.Fatalf("expected grace candidate to survive pruning, got %#v", candidates)
	}
	if !seen[8] {
		t.Fatalf("expected strong running candidate to survive pruning, got %#v", candidates)
	}
	if seen[9] {
		t.Fatalf("expected incompatible low-memory candidate to be pruned, got %#v", candidates)
	}
}

func TestQuickReuseCompatible_RejectsInstanceBelowMinCUDA(t *testing.T) {
	group := InstanceGroup{
		GPUClass:       "V100",
		GPUMemGB:       22,
		MinCUDAVersion: "12.8",
	}

	if quickReuseCompatible(group, InstanceCapacity{Instance: &db.Launch{
		Status:          db.LaunchStatusRunning,
		GPUClass:        "V100",
		ResolvedGPUName: "Tesla V100",
		GPUMemGB:        32,
		CUDAVersion:     12.2,
	}}) {
		t.Fatal("expected CUDA 12.2 reusable instance to be incompatible with min CUDA 12.8")
	}

	if !quickReuseCompatible(group, InstanceCapacity{Instance: &db.Launch{
		Status:          db.LaunchStatusRunning,
		GPUClass:        "V100",
		ResolvedGPUName: "Tesla V100",
		GPUMemGB:        32,
		CUDAVersion:     12.8,
	}}) {
		t.Fatal("expected CUDA 12.8 reusable instance to be compatible with min CUDA 12.8")
	}
}

func TestQuickReuseCompatible_RejectsRequiredImageMismatch(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{{
			ID:         3451,
			WorkingDir: testSGLangProjectDir(t),
			Command:    "python profile_inference_sglang.py",
			GPUClass:   "nvidia",
		}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			ID:              4231,
			Status:          db.LaunchStatusRunning,
			Provider:        string(cloud.ProviderVastai),
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "RTX 3090",
			GPUMemGB:        24,
			DockerImage:     "nvidia/cuda:12.8.2-devel-ubuntu22.04",
		},
		DiskFreeGB: 100,
	}

	if quickReuseCompatible(group, cap) {
		t.Fatal("quickReuseCompatible accepted SGLang job on CUDA-devel instance")
	}
	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance accepted SGLang job on CUDA-devel instance")
	}
	if !strings.Contains(reason, "image incompatible") {
		t.Fatalf("reason = %q, want image incompatibility", reason)
	}

	cap.Instance.DockerImage = sglangRuntimeImage
	if !quickReuseCompatible(group, cap) {
		t.Fatal("quickReuseCompatible rejected matching SGLang runtime image")
	}
	ok, reason = MatchGroupToInstance(group, cap)
	if !ok {
		t.Fatalf("MatchGroupToInstance rejected matching SGLang runtime image: %s", reason)
	}
}

func TestBuildProfilePlansFromSplitRaw_CachesSelectedOfferEstimatesAcrossProfiles(t *testing.T) {
	originalResolve := resolvePredictBatch
	originalCostPredict := estimateJobDurationsDetailedForCosts
	t.Cleanup(func() {
		resolvePredictBatch = originalResolve
		estimateJobDurationsDetailedForCosts = originalCostPredict
	})

	resolveCalls := 0
	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		resolveCalls++
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  1800,
					Lower: 1500,
					Upper: 2100,
				},
				DurationMetadata: &predictor.RuntimeMetadata{Source: "empirical", Confidence: 1.0},
			}
		}
		return results, nil
	}

	costPredictCalls := 0
	estimateJobDurationsDetailedForCosts = func(_ *predictor.Config, batchJobs []predictor.BatchJob) map[int64]estimate.DurationPrediction {
		costPredictCalls++
		results := make(map[int64]estimate.DurationPrediction, len(batchJobs))
		for _, job := range batchJobs {
			results[job.ID] = estimate.DurationPrediction{Estimate: estimate.FromSeconds(1800, 1500, 2100)}
		}
		return results
	}

	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1, Project: "demo", Command: "python train.py --epochs 1"}},
	}
	splitRaw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", CostPerHour: 0.40},
		},
	}}

	plans := BuildProfilePlansFromSplitRaw(
		nil,
		nil,
		[]InstanceGroup{group},
		splitRaw,
		nil,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		[]bidding.ScoreProfile{bidding.StrategyCheap.Profile(), bidding.StrategyFast.Profile()},
		0.95,
		0,
	)

	if len(plans) != 2 {
		t.Fatalf("expected 2 profile plans, got %d", len(plans))
	}
	if resolveCalls != 1 {
		t.Fatalf("resolvePredictBatch call count = %d, want 1", resolveCalls)
	}
	if costPredictCalls != 0 {
		t.Fatalf("cost duration estimator call count = %d, want 0", costPredictCalls)
	}
}

func TestRankGroupOffersWithPredictor_MultiJobGroupUsesTotalDuration(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 3600.0
			meta := &predictor.RuntimeMetadata{Source: "empirical", Confidence: 1.0}
			if job.GPUClass == "H100" {
				mean = 600.0
			}
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
				DurationMetadata: meta,
			}
		}
		return results, nil
	}

	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{
			{ID: 1, Command: "python train.py --epochs 1"},
			{ID: 2, Command: "python train.py --epochs 1"},
			{ID: 3, Command: "python train.py --epochs 1"},
			{ID: 4, Command: "python train.py --epochs 1"},
		},
	}
	raw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{ProviderID: "slow", GPUName: "RTX 3090", CostPerHour: 0.20, DLPerf: 10},
			{ProviderID: "fast", GPUName: "H100", CostPerHour: 0.40, DLPerf: 40},
		},
	}}
	setupFactory := func(InstanceGroup) bidding.OfferSetupFunc {
		return func(o cloud.Offer) float64 {
			if o.ProviderID == "slow" {
				return 0.1
			}
			return 2.0
		}
	}

	offers := RankGroupOffersWithPredictor(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		setupFactory,
		bidding.StrategyFastest,
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "fast" {
		t.Fatalf("expected multi-job group to pick fast offer, got %s", offers[0].Offer.ProviderID)
	}
}

func TestRankGroupOffers_MultiJobGroupCountsRepeatedSetupInTimeScore(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{
			{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5},
			{ID: 6}, {ID: 7}, {ID: 8}, {ID: 9}, {ID: 10},
		},
	}
	raw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{ProviderID: "slow", GPUName: "RTX 3090", CostPerHour: 0.30, DLPerf: 10},
			{ProviderID: "fast", GPUName: "L40S", CostPerHour: 0.30, DLPerf: 12},
		},
	}}
	setupFactory := func(InstanceGroup) bidding.OfferSetupFunc {
		return func(o cloud.Offer) float64 {
			if o.ProviderID == "slow" {
				return 0.1
			}
			return 5.0
		}
	}

	offers := RankGroupOffers(raw, nil, 1.0, setupFactory, bidding.StrategyFastest, 0)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "slow" {
		t.Fatalf("expected repeated setup penalty to pick slow offer, got %s", offers[0].Offer.ProviderID)
	}
}

func TestScoreEstimates_UsesTotalJobCompletionTime(t *testing.T) {
	split := []CostEstimate{
		{
			Group:     InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
			Offer:     GroupOffer{Offer: &cloud.Offer{ProviderID: "split-1"}},
			Breakdown: estimate.Breakdown{Run: estimate.Constant(time.Hour)},
			TotalTime: 90 * time.Minute,
		},
		{
			Group:     InstanceGroup{Jobs: []*db.Job{{ID: 2}}},
			Offer:     GroupOffer{Offer: &cloud.Offer{ProviderID: "split-2"}},
			Breakdown: estimate.Breakdown{Run: estimate.Constant(time.Hour)},
			TotalTime: 90 * time.Minute,
		},
	}
	merged := []CostEstimate{{
		Group:        InstanceGroup{Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
		Offer:        GroupOffer{Offer: &cloud.Offer{ProviderID: "merged"}},
		Breakdown:    estimate.Breakdown{Run: estimate.Constant(2 * time.Hour)},
		JobDurations: map[int64]time.Duration{1: time.Hour, 2: time.Hour},
		TotalTime:    150 * time.Minute,
	}}

	splitScore := ScoreEstimates(split, bidding.StrategyFastest)
	mergedScore := ScoreEstimates(merged, bidding.StrategyFastest)

	if splitScore >= mergedScore {
		t.Fatalf("expected split completion score %.2f to beat merged %.2f", splitScore, mergedScore)
	}
}

func TestRankGroupOffersForPlanning_UsesPredictorDurationsToAvoidH200(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 3600.0
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass:    "NVIDIA",
			GPUMemGB:    8,
			MaxGPUMemGB: 12,
			Jobs: []*db.Job{
				{ID: 548, Project: "head-type-ontology", Command: "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 24"},
				{ID: 549, Project: "head-type-ontology", Command: "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 48"},
				{ID: 553, Project: "head-type-ontology", Command: "uv run python scripts/parametric_type_recognition.py --device cuda"},
			},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx3090", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.15, DLPerf: 15.0},
			{ProviderID: "rtx4090", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
			{ProviderID: "h200", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFastest.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx3090" {
		t.Fatalf("expected predictor-backed ranking to avoid H200, got %s", offers[0].Offer.ProviderID)
	}
}

// TestRankGroupOffersForPlanning_NoMachineRaceAcrossGroups is the regression
// for the production bug: nine separate launch groups were all ranking
// offers independently and each picking the same cheapest vastai
// machine_id, so the parallel CreateInstance calls raced on vastai's
// per-machine-serialized create endpoint. Two succeeded, seven returned
// success=false. With the in-pass exclusion the second group must NOT
// pick a machine the first group already claimed — it falls through to
// the next-cheapest available machine, eliminating the race at its source.
func TestRankGroupOffersForPlanning_NoMachineRaceAcrossGroups(t *testing.T) {
	raw := []GroupRawOffers{
		{
			Group: InstanceGroup{
				GPUClass: "NVIDIA",
				GPUMemGB: 10,
				Jobs:     []*db.Job{{ID: 1, Project: "p", Command: "x"}},
			},
			Offers: []cloud.Offer{
				{Provider: "vastai", ProviderID: "39256061", MachineID: "44696", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.12, DLPerf: 10},
				{Provider: "vastai", ProviderID: "39256067", MachineID: "29150", GPUName: "GTX 1060", GPUMemGB: 12, CostPerHour: 0.14, DLPerf: 8},
			},
		},
		{
			Group: InstanceGroup{
				GPUClass: "NVIDIA",
				GPUMemGB: 10,
				Jobs:     []*db.Job{{ID: 2, Project: "p", Command: "x"}},
			},
			Offers: []cloud.Offer{
				{Provider: "vastai", ProviderID: "39256061", MachineID: "44696", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.12, DLPerf: 10},
				{Provider: "vastai", ProviderID: "39256067", MachineID: "29150", GPUName: "GTX 1060", GPUMemGB: 12, CostPerHour: 0.14, DLPerf: 8},
			},
		},
	}
	offers := rankGroupOffersForPlanning(raw, nil, nil, nil, bidding.StrategyCheap.Profile(), 0)
	if len(offers) != 2 || offers[0].Offer == nil || offers[1].Offer == nil {
		t.Fatalf("expected 2 ranked offers, got %#v", offers)
	}
	if offers[0].Offer.MachineID == offers[1].Offer.MachineID {
		t.Fatalf("two groups picked the same machine_id=%q — race not prevented", offers[0].Offer.MachineID)
	}
	if offers[0].Offer.MachineID != "44696" {
		t.Fatalf("first group should pick cheapest machine 44696, got %q", offers[0].Offer.MachineID)
	}
	if offers[1].Offer.MachineID != "29150" {
		t.Fatalf("second group should fall through to next-cheapest machine 29150 (44696 taken), got %q", offers[1].Offer.MachineID)
	}
}

func TestRankGroupOffersForPlanning_DistinctRunpodAllowsGpuTypeResampling(t *testing.T) {
	raw := []GroupRawOffers{
		{
			Group: InstanceGroup{
				GPUClass: "L4",
				GPUMemGB: 24,
				Jobs:     []*db.Job{{ID: 1, Project: "p", Command: "x"}},
			},
			Offers: []cloud.Offer{
				{Provider: cloud.ProviderRunpod, ProviderID: "gpu-l4", GPUName: "L4", GPUMemGB: 24, CostPerHour: 0.40, DLPerf: 10},
			},
		},
		{
			Group: InstanceGroup{
				GPUClass: "L4",
				GPUMemGB: 24,
				Jobs:     []*db.Job{{ID: 2, Project: "p", Command: "x"}},
			},
			Offers: []cloud.Offer{
				{Provider: cloud.ProviderRunpod, ProviderID: "gpu-l4", GPUName: "L4", GPUMemGB: 24, CostPerHour: 0.40, DLPerf: 10},
			},
		},
	}
	offers, _ := rankGroupOffersFromPredictionsWithMachineExclusions(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		nil,
		nil,
		true,
	)
	if len(offers) != 2 || offers[0].Offer == nil || offers[1].Offer == nil {
		t.Fatalf("expected two RunPod GPU-type samples under distinct mode, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "gpu-l4" || offers[1].Offer.ProviderID != "gpu-l4" {
		t.Fatalf("provider IDs = %q/%q, want repeated gpu-l4", offers[0].Offer.ProviderID, offers[1].Offer.ProviderID)
	}
}

func TestRankGroupOffersForPlanning_InitialMachineExclusion(t *testing.T) {
	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			GPUMemGB: 10,
			Jobs:     []*db.Job{{ID: 1, Project: "p", Command: "x"}},
		},
		Offers: []cloud.Offer{
			{Provider: cloud.ProviderVastai, ProviderID: "covered-offer", MachineID: "covered", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.10, DLPerf: 10},
			{Provider: cloud.ProviderVastai, ProviderID: "fresh-offer", MachineID: "fresh", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.20, DLPerf: 10},
		},
	}}
	offers, _ := rankGroupOffersFromPredictionsWithMachineExclusions(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		map[string]struct{}{"vastai/covered": {}},
		nil,
		false,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.MachineID != "fresh" {
		t.Fatalf("selected machine = %q, want fresh", offers[0].Offer.MachineID)
	}

	offers, _ = rankGroupOffersFromPredictionsWithMachineExclusions(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		map[string]struct{}{"vastai/covered": {}, "vastai/fresh": {}},
		nil,
		false,
	)
	if len(offers) != 1 || !errors.Is(offers[0].Err, ErrDistinctMachinesExhausted) {
		t.Fatalf("expected distinct-machine exhaustion, got %#v", offers)
	}
}

func TestRankGroupOffersForPlanning_MachineAffinity(t *testing.T) {
	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			GPUMemGB: 10,
			Jobs:     []*db.Job{{ID: 1, Project: "p", Command: "x"}},
		},
		Offers: []cloud.Offer{
			{Provider: cloud.ProviderVastai, ProviderID: "cheap-offer", MachineID: "other", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.10, DLPerf: 10},
			{Provider: cloud.ProviderVastai, ProviderID: "target-offer", MachineID: "target", GPUName: "RTX A4000", GPUMemGB: 16, CostPerHour: 0.20, DLPerf: 10},
		},
	}}
	offers, _ := rankGroupOffersFromPredictionsWithMachineExclusions(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		nil,
		map[string]struct{}{"vastai/target": {}},
		false,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.MachineID != "target" {
		t.Fatalf("selected machine = %q, want target", offers[0].Offer.MachineID)
	}

	offers, _ = rankGroupOffersFromPredictionsWithMachineExclusions(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
		nil,
		map[string]struct{}{"vastai/missing": {}},
		false,
	)
	if len(offers) != 1 || !errors.Is(offers[0].Err, ErrMachineAffinityUnsatisfied) {
		t.Fatalf("expected machine-affinity exhaustion, got %#v", offers)
	}
}

func TestRankGroupOffersForPlanning_FiltersOffersAboveMaxComputeCap(t *testing.T) {
	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass:      "NVIDIA",
			GPUMemGB:      82,
			MaxComputeCap: "8.0",
			Jobs:          []*db.Job{{ID: 1888, Project: "markov-attention", Command: "uv run python -u scripts/exp177_ols_init_for_lora.py"}},
		},
		Offers: []cloud.Offer{
			{ProviderID: "ada", GPUName: "RTX 4090", GPUMemGB: 96, CostPerHour: 0.10, DLPerf: 60.0},
			{ProviderID: "a100", GPUName: "A100", GPUMemGB: 94, CostPerHour: 0.80, DLPerf: 35.0},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyCheap.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "a100" {
		t.Fatalf("expected sm_8.0-compatible offer, got %s", offers[0].Offer.ProviderID)
	}
	if offers[0].FilterStats.TorchArchExampleGPU != "RTX 4090" {
		t.Fatalf("expected Ada offer to be reported as arch-filtered, got %#v", offers[0].FilterStats)
	}
}

func TestRankGroupOffersForPlanning_UsesPredictorDurationsToAvoidH200ForPythiaScaling(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 2200.0
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
				MaxGPUMemMiB: &predictor.Prediction{
					Mean:  9600.0,
					Lower: 8192.0,
					Upper: 11556.7,
				},
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass:    "NVIDIA",
			GPUMemGB:    20,
			MaxGPUMemGB: 24,
			Jobs: []*db.Job{
				{ID: 550, Project: "head-type-ontology", Command: "uv run python scripts/pythia_scaling.py --device cuda"},
			},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
			{ProviderID: "h200", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFast.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx4090" {
		t.Fatalf("expected predictor-backed ranking to avoid H200 for pythia scaling, got %s", offers[0].Offer.ProviderID)
	}
}

func TestRankGroupOffersForPlanning_UsesPredictorDurationsToAvoidH200ForBenchmarkJob(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 800.0
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "GPU",
			Jobs: []*db.Job{
				{ID: 552, Project: "llm-performance-models", Command: "rm -rf ~/.cache/llm-performance-models/ && uv sync && uv run llm-perf benchmark --ablation --cross-model && uv run llm-perf benchmark-inference"},
			},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
			{ProviderID: "h200nvl", GPUName: "H200 NVL", GPUMemGB: 141, CostPerHour: 2.53, DLPerf: 40.0},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFast.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx4090" {
		t.Fatalf("expected predictor-backed ranking to avoid H200 NVL for benchmark job, got %s", offers[0].Offer.ProviderID)
	}
}

func TestRankGroupOffersWithPredictor_RealWorkloadShapesAvoidOversizedH200AcrossStrategies(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	feasible := true
	noExtraVRAM := false
	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			result := &predictor.Result{
				DurationS: &predictor.Prediction{},
			}

			switch {
			case job.Project == "head-type-ontology" && job.Command == "uv run python scripts/pythia_scaling.py --device cuda":
				result.DurationS = &predictor.Prediction{Mean: 1200, Lower: 1080, Upper: 1320}
				result.DurationMetadata = &predictor.RuntimeMetadata{
					Source:                     "learned+analytical",
					Confidence:                 0.9,
					Feasible:                   &feasible,
					Bottleneck:                 "compute",
					MemoryHeadroomMiB:          4 * 1024,
					BenefitsFromAdditionalVRAM: &noExtraVRAM,
				}
				if job.GPUClass == "H200" {
					result.DurationS = &predictor.Prediction{Mean: 1020, Lower: 918, Upper: 1122}
					result.DurationMetadata = &predictor.RuntimeMetadata{
						Source:                     "learned",
						Confidence:                 0.15,
						Feasible:                   &feasible,
						Bottleneck:                 "unknown",
						MemoryHeadroomMiB:          110 * 1024,
						BenefitsFromAdditionalVRAM: &noExtraVRAM,
					}
				}
			case job.Project == "head-type-ontology" && (job.Command == "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 24" || job.Command == "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 48" || job.Command == "uv run python scripts/parametric_type_recognition.py --device cuda"):
				result.DurationS = &predictor.Prediction{Mean: 840, Lower: 756, Upper: 924}
				result.DurationMetadata = &predictor.RuntimeMetadata{
					Source:                     "learned+analytical",
					Confidence:                 0.9,
					Feasible:                   &feasible,
					Bottleneck:                 "compute",
					MemoryHeadroomMiB:          10 * 1024,
					BenefitsFromAdditionalVRAM: &noExtraVRAM,
				}
				if job.GPUClass == "H200" {
					result.DurationS = &predictor.Prediction{Mean: 720, Lower: 648, Upper: 792}
					result.DurationMetadata = &predictor.RuntimeMetadata{
						Source:                     "learned",
						Confidence:                 0.15,
						Feasible:                   &feasible,
						Bottleneck:                 "unknown",
						MemoryHeadroomMiB:          120 * 1024,
						BenefitsFromAdditionalVRAM: &noExtraVRAM,
					}
				}
			case job.Project == "llm-performance-models" && job.Command == "rm -rf ~/.cache/llm-performance-models/ && uv sync && uv run llm-perf benchmark --ablation --cross-model && uv run llm-perf benchmark-inference":
				result.DurationS = &predictor.Prediction{Mean: 1680, Lower: 1512, Upper: 1848}
				result.DurationMetadata = &predictor.RuntimeMetadata{
					Source:     "empirical",
					Confidence: 1.0,
					Feasible:   &feasible,
				}
				if job.GPUClass == "H200 NVL" {
					result.DurationS = &predictor.Prediction{Mean: 1440, Lower: 1296, Upper: 1584}
					result.DurationMetadata = &predictor.RuntimeMetadata{
						Source:                     "learned",
						Confidence:                 0.15,
						Feasible:                   &feasible,
						Bottleneck:                 "unknown",
						MemoryHeadroomMiB:          118 * 1024,
						BenefitsFromAdditionalVRAM: &noExtraVRAM,
					}
				}
			default:
				t.Fatalf("unexpected batch job: %#v", job)
			}

			results[job.ID] = result
		}
		return results, nil
	}

	raw := []GroupRawOffers{
		{
			Group: InstanceGroup{
				GPUClass:    "NVIDIA",
				GPUMemGB:    20,
				MaxGPUMemGB: 24,
				Jobs: []*db.Job{
					{ID: 550, Project: "head-type-ontology", Command: "uv run python scripts/pythia_scaling.py --device cuda"},
				},
			},
			Offers: []cloud.Offer{
				{ProviderID: "rtx4090-20", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
				{ProviderID: "h200-20", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
			},
		},
		{
			Group: InstanceGroup{
				GPUClass:    "NVIDIA",
				GPUMemGB:    8,
				MaxGPUMemGB: 12,
				Jobs: []*db.Job{
					{ID: 548, Project: "head-type-ontology", Command: "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 24"},
					{ID: 549, Project: "head-type-ontology", Command: "uv run python scripts/head_count_regularization_sweep.py --device cuda --head-counts 48"},
					{ID: 553, Project: "head-type-ontology", Command: "uv run python scripts/parametric_type_recognition.py --device cuda"},
				},
			},
			Offers: []cloud.Offer{
				{ProviderID: "rtx3090-8", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.15, DLPerf: 15.0},
				{ProviderID: "rtx4090-8", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
				{ProviderID: "h200-8", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
			},
		},
		{
			Group: InstanceGroup{
				GPUClass: "GPU",
				Jobs: []*db.Job{
					{ID: 552, Project: "llm-performance-models", Command: "rm -rf ~/.cache/llm-performance-models/ && uv sync && uv run llm-perf benchmark --ablation --cross-model && uv run llm-perf benchmark-inference"},
				},
			},
			Offers: []cloud.Offer{
				{ProviderID: "rtx4090-generic", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
				{ProviderID: "h200nvl-generic", GPUName: "H200 NVL", GPUMemGB: 141, CostPerHour: 2.53, DLPerf: 40.0},
			},
		},
	}

	expected := map[bidding.SelectionStrategy][]string{
		bidding.StrategyCheap:   {"RTX 4090", "RTX 3090", "RTX 4090"},
		bidding.StrategyFast:    {"RTX 4090", "RTX 3090", "RTX 4090"},
		bidding.StrategyFastest: {"RTX 4090", "RTX 3090", "RTX 4090"},
	}

	for _, strategy := range []bidding.SelectionStrategy{
		bidding.StrategyCheap,
		bidding.StrategyFast,
		bidding.StrategyFastest,
	} {
		offers := RankGroupOffersWithPredictor(
			raw,
			&predictor.Config{ProjectPath: "/tmp/job-estimator"},
			nil,
			nil,
			strategy,
			0,
		)
		if len(offers) != len(raw) {
			t.Fatalf("%s: expected %d ranked groups, got %d", strategy, len(raw), len(offers))
		}
		for i, wantGPU := range expected[strategy] {
			if offers[i].Offer == nil {
				t.Fatalf("%s group %d: expected ranked offer, got %#v", strategy, i, offers[i])
			}
			if offers[i].Offer.GPUName != wantGPU {
				t.Fatalf("%s group %d: selected GPU %q, want %q", strategy, i, offers[i].Offer.GPUName, wantGPU)
			}
		}
	}
}

func TestRankGroupOffersForPlanning_ShrinksLowConfidenceRuntimeDeltas(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 7200.0
			meta := &predictor.RuntimeMetadata{Source: "learned+analytical", Confidence: 1.0}
			if job.GPUClass == "H200" {
				mean = 6480.0
				meta = &predictor.RuntimeMetadata{Source: "learned", Confidence: 0.1}
			}
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
				DurationMetadata: meta,
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx3090", GPUName: "RTX 3090", CostPerHour: 0.30},
			{ProviderID: "h200", GPUName: "H200", CostPerHour: 1.00},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFast.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx3090" {
		t.Fatalf("expected low-confidence learned speedup to be shrunk away, got %s", offers[0].Offer.ProviderID)
	}
}

func TestRankGroupOffersForPlanning_UnknownOversizedVRAMStaysConservative(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	neutral := false
	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 3600.0
			meta := &predictor.RuntimeMetadata{
				Source:                     "learned",
				Confidence:                 0.7,
				Bottleneck:                 "unknown",
				MemoryHeadroomMiB:          12 * 1024,
				BenefitsFromAdditionalVRAM: &neutral,
			}
			if job.GPUClass == "H200" {
				mean = 3000.0
				meta.MemoryHeadroomMiB = 128 * 1024
			}
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
				DurationMetadata: meta,
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", CostPerHour: 0.30},
			{ProviderID: "h200", GPUName: "H200", CostPerHour: 1.00},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFast.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx4090" {
		t.Fatalf("expected unknown oversized-VRAM speedup to be shrunk away, got %s", offers[0].Offer.ProviderID)
	}
}

func TestRankGroupOffersForPlanning_MemoryCapacityCanJustifyLargerGPU(t *testing.T) {
	original := resolvePredictBatch
	t.Cleanup(func() { resolvePredictBatch = original })

	helps := true
	neutral := false
	resolvePredictBatch = func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 3600.0
			meta := &predictor.RuntimeMetadata{
				Source:                     "learned",
				Confidence:                 0.7,
				Bottleneck:                 "unknown",
				MemoryHeadroomMiB:          10 * 1024,
				BenefitsFromAdditionalVRAM: &neutral,
			}
			if job.GPUClass == "H200" {
				mean = 1200.0
				meta = &predictor.RuntimeMetadata{
					Source:                     "learned",
					Confidence:                 0.7,
					Bottleneck:                 "memory_capacity",
					MemoryHeadroomMiB:          96 * 1024,
					BenefitsFromAdditionalVRAM: &helps,
				}
			}
			results[job.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{
					Mean:  mean,
					Lower: mean * 0.9,
					Upper: mean * 1.1,
				},
				DurationMetadata: meta,
			}
		}
		return results, nil
	}

	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", CostPerHour: 0.30},
			{ProviderID: "h200", GPUName: "H200", CostPerHour: 1.00},
		},
	}}

	offers := rankGroupOffersForPlanning(
		raw,
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFast.Profile(),
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "h200" {
		t.Fatalf("expected explicit memory-capacity signal to preserve larger-GPU win, got %s", offers[0].Offer.ProviderID)
	}
}

func TestApplySelectedOfferRuntimePredictions_UsesAdjustedDurations(t *testing.T) {
	estimates := []CostEstimate{{
		Group:         InstanceGroup{Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
		Offer:         GroupOffer{Offer: &cloud.Offer{ProviderID: "offer-1", CostPerHour: 2.0}},
		Breakdown:     estimate.Breakdown{Startup: estimate.Constant(10 * time.Minute), Run: estimate.Constant(2 * time.Hour), Total: estimate.Constant(130 * time.Minute)},
		JobDurations:  map[int64]time.Duration{1: time.Hour, 2: time.Hour},
		SetupOverhead: 10 * time.Minute,
		TotalTime:     130 * time.Minute,
		TotalCost:     (130 * time.Minute).Hours() * 2.0,
		SurvivalProb:  1.0,
	}}
	selected := []offerRuntimePrediction{{
		totalRunHrs:          2.0,
		adjustedRunHrs:       1.5,
		jobDurations:         []time.Duration{time.Hour, time.Hour},
		adjustedJobDurations: []time.Duration{30 * time.Minute, time.Hour},
		feasible:             true,
		complete:             true,
	}}

	adjusted := applySelectedOfferRuntimePredictions(estimates, selected)
	if len(adjusted) != 1 {
		t.Fatalf("expected one adjusted estimate, got %d", len(adjusted))
	}
	est := adjusted[0]
	if est.JobDurations[1] != 30*time.Minute {
		t.Fatalf("job 1 duration = %v, want 30m", est.JobDurations[1])
	}
	if est.JobDurations[2] != time.Hour {
		t.Fatalf("job 2 duration = %v, want 1h", est.JobDurations[2])
	}
	if est.Breakdown.Run.Mean != 90*time.Minute {
		t.Fatalf("Run.Mean = %v, want 1h30m", est.Breakdown.Run.Mean)
	}
	if est.TotalTime != 100*time.Minute {
		t.Fatalf("TotalTime = %v, want 1h40m", est.TotalTime)
	}
	wantCost := (100 * time.Minute).Hours() * 2.0
	if est.TotalCost != wantCost {
		t.Fatalf("TotalCost = %.2f, want %.2f", est.TotalCost, wantCost)
	}
}

func TestRankGroupOffersWithPredictor_UsesNeutralFallbackWithoutPredictions(t *testing.T) {
	raw := []GroupRawOffers{{
		Group: InstanceGroup{
			GPUClass: "NVIDIA",
			GPUMemGB: 8,
			Jobs:     []*db.Job{{ID: 548}},
		},
		Offers: []cloud.Offer{
			{ProviderID: "rtx4090", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25},
			{ProviderID: "h200", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40},
		},
	}}

	offers := RankGroupOffersWithPredictor(
		raw,
		nil,
		nil,
		nil,
		bidding.StrategyFastest,
		0,
	)
	if len(offers) != 1 || offers[0].Offer == nil {
		t.Fatalf("expected ranked offer, got %#v", offers)
	}
	if offers[0].Offer.ProviderID != "rtx4090" {
		t.Fatalf("expected neutral fallback to prefer cheaper adequate offer, got %s", offers[0].Offer.ProviderID)
	}
}

func TestBuildStrategyPlans_FastestPrefersNewInstanceOverBackloggedRunningReuse(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1}},
	}
	reusable := []InstanceCapacity{{
		Instance: &db.Launch{
			ID:                 42,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "reuse-42",
			GPUClass:           "NVIDIA",
			ResolvedGPUName:    "RTX 3090",
			GPUMemGB:           24,
			CostPerHourCents:   20,
			DLPerf:             10,
		},
		RunningJobCount: 6,
		DiskFreeGB:      100,
	}}
	client := &cloud.MockClient{
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "new", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.50, DLPerf: 10},
			}, nil
		},
	}

	plans, _ := BuildStrategyPlans(
		nil,
		[]cloud.Client{client},
		[]InstanceGroup{group},
		reusable,
		nil,
		nil,
		nil,
		[]bidding.SelectionStrategy{bidding.StrategyFastest},
		0,
	)
	plan := plans[bidding.StrategyFastest]
	if len(plan.ReuseAssignments) != 0 {
		t.Fatalf("expected no reuse assignments, got %d", len(plan.ReuseAssignments))
	}
	if plan.NewCandidate == nil || len(plan.NewCandidate.Offers) != 1 || plan.NewCandidate.Offers[0].Offer == nil {
		t.Fatalf("expected new candidate offer, got %#v", plan.NewCandidate)
	}
	if plan.NewCandidate.Offers[0].Offer.ProviderID != "new" {
		t.Fatalf("expected new offer to win, got %s", plan.NewCandidate.Offers[0].Offer.ProviderID)
	}
}

func TestBuildStrategyPlans_CheapPrefersGraceReuse(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1}},
	}
	reusable := []InstanceCapacity{{
		Instance: &db.Launch{
			ID:                 7,
			Status:             db.LaunchStatusGrace,
			Provider:           "vastai",
			ProviderInstanceID: "reuse-7",
			GPUClass:           "NVIDIA",
			ResolvedGPUName:    "RTX 4090",
			GPUMemGB:           24,
			DLPerf:             20,
		},
		DiskFreeGB: 100,
	}}

	plans, _ := BuildStrategyPlans(
		nil,
		nil,
		[]InstanceGroup{group},
		reusable,
		nil,
		nil,
		nil,
		[]bidding.SelectionStrategy{bidding.StrategyCheap},
		0,
	)
	plan := plans[bidding.StrategyCheap]
	if len(plan.ReuseAssignments) != 1 {
		t.Fatalf("expected 1 reuse assignment, got %d", len(plan.ReuseAssignments))
	}
	if plan.NewCandidate != nil {
		t.Fatalf("expected no new candidate when grace reuse wins, got %#v", plan.NewCandidate)
	}
	if len(plan.DisplayOffers) != 1 || plan.DisplayOffers[0].Offer == nil {
		t.Fatalf("expected display offer for reused group, got %#v", plan.DisplayOffers)
	}
}

func TestDefaultPlanOptions_PreferReuseDisabled(t *testing.T) {
	opts := defaultPlanOptions()
	if opts.PreferReuse {
		t.Fatalf("default plan options unexpectedly enable PreferReuse")
	}
}

func TestReuseHeuristicScore_PreferReuseDisablesBusyPenalty(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		GPUMemGB: 24,
		Jobs:     []*db.Job{{ID: 1}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass: "NVIDIA",
			GPUMemGB: 24,
			DLPerf:   10,
		},
		RunningJobCount: 3,
		DiskFreeGB:      100,
	}

	withPenalty := reuseHeuristicScore(group, nil, cap, false)
	withoutPenalty := reuseHeuristicScore(group, nil, cap, true)
	if withoutPenalty-withPenalty != 60 {
		t.Fatalf("score delta = %v, want 60", withoutPenalty-withPenalty)
	}
}
