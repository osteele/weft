package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

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

func TestBestCandidateForStrategy_UsesPredictorRuntimeAcrossGroupingCandidates(t *testing.T) {
	originalPredictBatch := resolvePredictBatch
	originalEstimateJobDurationsDetailed := estimateJobDurationsDetailed
	t.Cleanup(func() {
		resolvePredictBatch = originalPredictBatch
		estimateJobDurationsDetailed = originalEstimateJobDurationsDetailed
	})

	stubPredict := func(_ predictor.Config, jobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		results := make(map[int64]*predictor.Result, len(jobs))
		for _, job := range jobs {
			mean := 7200.0
			if job.GPUClass == "H200" {
				mean = 300.0
			}
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
	resolvePredictBatch = stubPredict
	estimateJobDurationsDetailed = func(_ *predictor.Config, jobs []predictor.BatchJob) map[int64]estimate.DurationPrediction {
		results, err := stubPredict(predictor.Config{}, jobs)
		if err != nil {
			return nil
		}
		estimates := make(map[int64]estimate.DurationPrediction, len(results))
		for id, result := range results {
			if result == nil || result.DurationS == nil {
				continue
			}
			estimates[id] = estimate.DurationPrediction{
				Estimate: estimate.FromSeconds(result.DurationS.Mean, result.DurationS.Lower, result.DurationS.Upper),
				Metadata: result.DurationMetadata,
			}
		}
		return estimates
	}

	grouped := GroupingCandidate{
		Label: "grouped",
		Groups: []InstanceGroup{{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4},
			},
		}},
		Raw: []GroupRawOffers{{
			Group: InstanceGroup{
				GPUClass: "NVIDIA",
				Jobs:     []*db.Job{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}},
			},
			Offers: []cloud.Offer{
				{ProviderID: "h200", GPUName: "H200", CostPerHour: 1.50, DLPerf: 40},
			},
		}},
	}

	parallelGroups := []InstanceGroup{
		{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 2}}},
		{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 3}}},
		{GPUClass: "NVIDIA", Jobs: []*db.Job{{ID: 4}}},
	}
	parallelRaw := make([]GroupRawOffers, len(parallelGroups))
	for i, g := range parallelGroups {
		parallelRaw[i] = GroupRawOffers{
			Group:  g,
			Offers: []cloud.Offer{{ProviderID: "rtx3090", GPUName: "RTX 3090", CostPerHour: 0.20, DLPerf: 5}},
		}
	}
	parallel := GroupingCandidate{
		Label:  "parallel",
		Groups: parallelGroups,
		Raw:    parallelRaw,
	}

	result := BestCandidateForStrategy(
		nil,
		[]GroupingCandidate{grouped, parallel},
		&predictor.Config{ProjectPath: "/tmp/job-estimator"},
		nil,
		nil,
		bidding.StrategyFastest,
		0,
	)
	if result.Label != "grouped" {
		t.Fatalf("expected grouped candidate to win, got %s", result.Label)
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
				mean = 2400.0
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
