package campaign

import (
	"math"
	"testing"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestCheapestOffer(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "1", CostPerHour: 2.50, GPUName: "A100"},
		{ProviderID: "2", CostPerHour: 0.45, GPUName: "RTX_A6000"},
		{ProviderID: "3", CostPerHour: 1.20, GPUName: "RTX_4090"},
	}
	// nil model falls back to cheapest
	_, best := bidding.BestOffer(nil, offers, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap)
	if best.ProviderID != "2" {
		t.Errorf("BestOffer(nil) returned id=%s, want 2", best.ProviderID)
	}
	if best.CostPerHour != 0.45 {
		t.Errorf("BestOffer(nil) cost=%f, want 0.45", best.CostPerHour)
	}
}

func TestSortOffersByCost(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "1", CostPerHour: 2.50},
		{ProviderID: "2", CostPerHour: 0.45},
		{ProviderID: "3", CostPerHour: 1.20},
	}
	cloud.SortOffersByCost(offers)
	if offers[0].ProviderID != "2" || offers[1].ProviderID != "3" || offers[2].ProviderID != "1" {
		t.Errorf("offers not sorted by cost: got IDs %s,%s,%s", offers[0].ProviderID, offers[1].ProviderID, offers[2].ProviderID)
	}
}

func TestFetchGroupOffersMock(t *testing.T) {
	mockClient := &cloud.MockClient{
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			if constraints.GPUClass == "RTX_4090" {
				return []cloud.Offer{
					{ProviderID: "1", GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.50},
					{ProviderID: "2", GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.45},
				}, nil
			}
			if constraints.GPUClass == "A100" {
				return []cloud.Offer{
					{ProviderID: "3", GPUName: "A100", GPUMemGB: 40, CostPerHour: 2.00},
				}, nil
			}
			return nil, nil
		},
	}

	groups := []InstanceGroup{
		{GPUClass: "RTX_4090", GPUMemGB: 24},
		{GPUClass: "A100", GPUMemGB: 40},
		{GPUClass: "H100", GPUMemGB: 80},
	}

	results := FetchGroupOffers([]cloud.Client{mockClient}, groups, nil, 1.0, nil, bidding.StrategyCheap, 0)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// RTX_4090 should have the cheapest offer (ProviderID "2")
	if results[0].Offer == nil || results[0].Offer.ProviderID != "2" {
		t.Errorf("group 0: expected offer ID 2, got %v", results[0].Offer)
	}

	// A100 should have the only offer (ProviderID "3")
	if results[1].Offer == nil || results[1].Offer.ProviderID != "3" {
		t.Errorf("group 1: expected offer ID 3, got %v", results[1].Offer)
	}

	// H100 should have no offers
	if results[2].Offer != nil {
		t.Errorf("group 2: expected no offer, got %v", results[2].Offer)
	}
}

func TestSearchBestOfferForGroupExcludesFailedOffer(t *testing.T) {
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			if constraints.GPUClass != "RTX_4090" {
				t.Fatalf("GPUClass = %q, want RTX_4090", constraints.GPUClass)
			}
			return []cloud.Offer{
				{ProviderID: "1", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.45},
				{ProviderID: "2", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.50},
			}, nil
		},
	}

	result := SearchBestOfferForGroup(
		[]cloud.Client{mockClient},
		InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, DiskGB: 80},
		nil,
		1.0,
		bidding.ConstantSetup(0.5),
		map[string]struct{}{"vastai:1": {}},
		bidding.StrategyCheap,
		0,
	)
	if result.Err != nil {
		t.Fatalf("SearchBestOfferForGroup: %v", result.Err)
	}
	if result.Offer == nil {
		t.Fatal("expected replacement offer, got nil")
	}
	if result.Offer.ProviderID != "2" {
		t.Fatalf("replacement offer ID = %s, want 2", result.Offer.ProviderID)
	}
}

func TestOfferConstraintsForGroup_ComputeIntensive(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs: []*db.Job{
			{ID: 1, Tags: []string{"compute-intensive"}},
		},
	}
	c := offerConstraintsForGroup(group)
	if c.MinCPUCoresEffective != 16 {
		t.Errorf("MinCPUCoresEffective = %d, want 16", c.MinCPUCoresEffective)
	}
}

func TestOfferConstraintsForGroup_NoComputeIntensive(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs: []*db.Job{
			{ID: 1, Tags: []string{"rental"}},
		},
	}
	c := offerConstraintsForGroup(group)
	if c.MinCPUCoresEffective != 0 {
		t.Errorf("MinCPUCoresEffective = %d, want 0", c.MinCPUCoresEffective)
	}
}

func TestScoreGrouping_CheapPrefersMerged(t *testing.T) {
	// With survival < 1, merging saves retry overhead (fewer instances to fail).
	// Merged: 1 group with 2 jobs, pays setup once per retry
	mergedOffers := []GroupOffer{
		{
			Group:        InstanceGroup{Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
			Offer:        &cloud.Offer{CostPerHour: 0.50},
			SurvivalProb: 0.8,
		},
	}
	// Split: 2 groups, each pays setup independently, each can fail
	splitOffers := []GroupOffer{
		{
			Group:        InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
			Offer:        &cloud.Offer{CostPerHour: 0.50},
			SurvivalProb: 0.8,
		},
		{
			Group:        InstanceGroup{Jobs: []*db.Job{{ID: 2}}},
			Offer:        &cloud.Offer{CostPerHour: 0.50},
			SurvivalProb: 0.8,
		},
	}

	mergedScore := ScoreGrouping(mergedOffers, bidding.StrategyCheap)
	splitScore := ScoreGrouping(splitOffers, bidding.StrategyCheap)

	if mergedScore >= splitScore {
		t.Errorf("cheap strategy should prefer merged (score=%.3f) over split (score=%.3f)", mergedScore, splitScore)
	}
}

func TestScoreGrouping_FastestPrefersSplit(t *testing.T) {
	// Split: 2 groups run in parallel, max time = 1 job
	splitOffers := []GroupOffer{
		{
			Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
			Offer: &cloud.Offer{CostPerHour: 0.50},
		},
		{
			Group: InstanceGroup{Jobs: []*db.Job{{ID: 2}}},
			Offer: &cloud.Offer{CostPerHour: 0.50},
		},
	}
	// Merged: 1 group with 2 jobs sequential, time = 2 jobs
	mergedOffers := []GroupOffer{
		{
			Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
			Offer: &cloud.Offer{CostPerHour: 0.50},
		},
	}

	splitScore := ScoreGrouping(splitOffers, bidding.StrategyFastest)
	mergedScore := ScoreGrouping(mergedOffers, bidding.StrategyFastest)

	if splitScore >= mergedScore {
		t.Errorf("fastest strategy should prefer split (score=%.3f) over merged (score=%.3f)", splitScore, mergedScore)
	}
}

func TestBuildReuseCandidate_MatchesCompatibleJobs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(80)},
	}
	instances := []InstanceCapacity{
		{
			Instance: &db.Launch{
				ID: 100, Status: db.LaunchStatusGrace,
				GPUClass: "nvidia", GPUMemGB: 24,
				ResolvedGPUName: "RTX 4090",
				Provider:        "vastai", ProviderInstanceID: "v100",
			},
		},
	}

	cand := BuildReuseCandidate(jobs, instances)
	if cand == nil {
		t.Fatal("expected reuse candidate, got nil")
	}
	if cand.Label != "reuse" {
		t.Errorf("expected label 'reuse', got %q", cand.Label)
	}
	// Only job 1 (20GB) fits on 24GB instance; job 2 (80GB) doesn't
	if len(cand.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(cand.Groups))
	}
	if len(cand.Groups[0].Jobs) != 1 || cand.Groups[0].Jobs[0].ID != 1 {
		t.Errorf("expected job 1 in reuse group, got %v", cand.Groups[0].Jobs)
	}
	// Synthetic offer should have zero cost
	if cand.Raw[0].Offers[0].CostPerHour != 0 {
		t.Errorf("expected zero cost for reuse offer, got %f", cand.Raw[0].Offers[0].CostPerHour)
	}
}

func TestBuildReuseCandidate_NilWhenNoMatch(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "h100", GPUMemGB: intPtr(80)},
	}
	instances := []InstanceCapacity{
		{
			Instance: &db.Launch{
				ID: 100, Status: db.LaunchStatusGrace,
				GPUClass: "nvidia", GPUMemGB: 24,
				ResolvedGPUName: "RTX 4090",
			},
		},
	}

	cand := BuildReuseCandidate(jobs, instances)
	if cand != nil {
		t.Errorf("expected nil when no jobs match, got %v", cand)
	}
}

func TestBuildReuseCandidate_NilWhenNoInstances(t *testing.T) {
	jobs := []*db.Job{{ID: 1, Status: db.StatusQueued}}
	cand := BuildReuseCandidate(jobs, nil)
	if cand != nil {
		t.Errorf("expected nil for empty instances, got %v", cand)
	}
}

func TestScoreGrouping_NoOfferReturnsInf(t *testing.T) {
	offers := []GroupOffer{
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}}, Offer: nil},
	}
	score := ScoreGrouping(offers, bidding.StrategyCheap)
	if score != math.Inf(1) {
		t.Errorf("expected +Inf for nil offer, got %.3f", score)
	}
}
