package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestRankGroupOffers_MultiJobGroupUsesTotalDuration(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{
			{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4},
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

	offers := RankGroupOffers(raw, nil, 1.0, setupFactory, bidding.StrategyFastest, 0)
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

func TestBestCandidateForStrategy_UsesEstimatedRuntimeNotPlaceholderGroupingScore(t *testing.T) {
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
		nil,
		nil,
		nil,
		bidding.StrategyFastest,
		0,
	)
	if result.Label != "grouped" {
		t.Fatalf("expected grouped candidate to win, got %s", result.Label)
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
