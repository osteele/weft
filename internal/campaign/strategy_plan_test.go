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
