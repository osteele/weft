package campaign

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

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
	_, best := bidding.BestOffer(nil, offers, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap, 0)
	if best.ProviderID != "2" {
		t.Errorf("BestOffer(nil) returned id=%s, want 2", best.ProviderID)
	}
	if best.CostPerHour != 0.45 {
		t.Errorf("BestOffer(nil) cost=%f, want 0.45", best.CostPerHour)
	}
}

func TestOfferPriceQuantilesCents(t *testing.T) {
	minCents, medianCents, p75Cents := OfferPriceQuantilesCents([]cloud.Offer{
		{CostPerHour: 3.00},
		{CostPerHour: 1.00},
		{CostPerHour: 2.00},
		{CostPerHour: 4.00},
	})
	if minCents != 100 || medianCents != 200 || p75Cents != 300 {
		t.Fatalf("quantiles = (%d, %d, %d), want (100, 200, 300)", minCents, medianCents, p75Cents)
	}
}

func TestRecordOfferAvailabilitySnapshots_RateLimited(t *testing.T) {
	database := db.SetupTestDB(t)
	group := InstanceGroup{GPUClass: "A100", GPUMemGB: 80, DiskGB: 120, NumGPUs: 1}
	raw := []GroupRawOffers{{
		Group: group,
		Offers: []cloud.Offer{
			{Provider: cloud.ProviderVastai, CostPerHour: 1.00},
			{Provider: cloud.ProviderVastai, CostPerHour: 2.00},
			{Provider: cloud.ProviderRunpod, CostPerHour: 3.00},
		},
		ProviderErrors: []string{"runpod: temporary API failure"},
	}}
	ranked := []GroupOffer{{
		Group:       group,
		FilterStats: OfferFilterStats{RawCount: 3, AfterSurvival: 2},
	}}

	RecordOfferAvailabilitySnapshots(database, raw, ranked, 0.95)
	RecordOfferAvailabilitySnapshots(database, raw, ranked, 0.95)

	var count, offers, postFilter, minPrice, medianPrice, p75Price int
	var details string
	if err := database.QueryRow(`
		SELECT COUNT(*), COALESCE(MAX(offer_count), 0), COALESCE(MAX(post_filter_count), 0),
		       COALESCE(MAX(price_min_cents), 0), COALESCE(MAX(price_median_cents), 0),
		       COALESCE(MAX(price_p75_cents), 0), COALESCE(MAX(details_json), '')
		FROM offer_availability_snapshots
		WHERE gpu_class = 'A100' AND gpu_mem_bucket_gb = 80 AND disk_bucket_gb = 120`,
	).Scan(&count, &offers, &postFilter, &minPrice, &medianPrice, &p75Price, &details); err != nil {
		t.Fatalf("query snapshots: %v", err)
	}
	if count != 1 {
		t.Fatalf("snapshot count = %d, want 1 after rate-limited duplicate", count)
	}
	if offers != 3 || postFilter != 2 || minPrice != 100 || medianPrice != 200 || p75Price != 300 {
		t.Fatalf("snapshot values offers=%d post=%d prices=(%d,%d,%d), want offers=3 post=2 prices=(100,200,300)",
			offers, postFilter, minPrice, medianPrice, p75Price)
	}
	for _, want := range []string{
		`"provider_errors":["runpod: temporary API failure"]`,
		`"provider_offer_counts":{"runpod":1,"vastai":2}`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details_json = %s, want to contain %s", details, want)
		}
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

	results := FetchGroupOffers([]cloud.Client{mockClient}, groups, nil, 1.0, nil, bidding.StrategyCheap, 0.95, 0)

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

func TestRankOffer_UsesUnknownFallbackWhenKnownProviderSearchErrors(t *testing.T) {
	vastErr := errors.New("vastai search timed out")
	vast := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return nil, vastErr
		},
	}
	runpod := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "rp1", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", GPUMemGB: 48, CostPerHour: 0.45},
			}, nil
		},
	}
	group := InstanceGroup{
		GPUClass:       "ampere+",
		GPUMemGB:       26,
		MinCUDAVersion: "12.5",
	}

	raw := FetchGroupRawOffers([]cloud.Client{vast, runpod}, []InstanceGroup{group}, 0.85)
	if len(raw) != 1 {
		t.Fatalf("got %d raw results, want 1", len(raw))
	}
	if raw[0].Err != nil {
		t.Fatalf("raw error = %v, want nil because RunPod search succeeded", raw[0].Err)
	}
	ranked := RankGroupOffersWithProfile(raw, nil, 1, nil, bidding.StrategyCheap.Profile(), 0)
	if len(ranked) != 1 {
		t.Fatalf("got %d ranked results, want 1", len(ranked))
	}
	if ranked[0].Offer == nil || ranked[0].Offer.ProviderID != "rp1" {
		t.Fatalf("selected offer = %+v, want RunPod unknown-compatible fallback", ranked[0].Offer)
	}
	if ranked[0].FilterStats.UnknownCompatibility != 1 {
		t.Fatalf("UnknownCompatibility = %d, want 1", ranked[0].FilterStats.UnknownCompatibility)
	}
	if len(ranked[0].FilterStats.ProviderErrors) != 1 ||
		!strings.Contains(ranked[0].FilterStats.ProviderErrors[0], "vastai: "+vastErr.Error()) {
		t.Fatalf("ProviderErrors = %#v, want partial Vast.ai search error", ranked[0].FilterStats.ProviderErrors)
	}
}

func TestOfferSearchSessionCachedOnlyMissNamesMissingSnapshotConstraints(t *testing.T) {
	session := newOfferSearchSessionWithOptions(nil, 0.95, false)
	snapshotErr := fmt.Errorf("%w: no cloud providers available: vastai: vastai CLI not found in PATH", ErrOfferSnapshotUnavailable)
	session.SeedRawOffers([]GroupRawOffers{{
		Group: InstanceGroup{GPUClass: "nvidia", GPUMemGB: 24},
		Err:   snapshotErr,
	}})

	raw := session.fetchGroupRawOffers([]InstanceGroup{{GPUClass: "nvidia", GPUMemGB: 48}})
	if len(raw) != 1 {
		t.Fatalf("got %d raw result(s), want 1", len(raw))
	}
	if !errors.Is(raw[0].Err, ErrOfferSnapshotUnavailable) {
		t.Fatalf("error = %v, want ErrOfferSnapshotUnavailable", raw[0].Err)
	}
	if !strings.Contains(raw[0].Err.Error(), "cached offer snapshot missing") || !strings.Contains(raw[0].Err.Error(), "vram>=48GB") {
		t.Fatalf("error = %v, want missing snapshot constraints", raw[0].Err)
	}
	if strings.Contains(raw[0].Err.Error(), "vastai CLI not found in PATH") {
		t.Fatalf("error = %v, should not attach unrelated snapshot error to derived group miss", raw[0].Err)
	}
}

func TestBuildProfilePlansFromSplitRaw_ReusesCandidateOfferSearchesAcrossProfiles(t *testing.T) {
	splitGroup := InstanceGroup{
		GPUClass: "nvidia",
		GPUMemGB: 80,
		Jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(24)},
			{ID: 2, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(48)},
		},
	}
	splitRaw := []GroupRawOffers{{
		Group: splitGroup,
		Offers: []cloud.Offer{
			{ProviderID: "split-h100", GPUName: "H100", GPUMemGB: 80, CostPerHour: 2.00, DLPerf: 40},
		},
	}}

	var (
		mu           sync.Mutex
		searchCounts = make(map[string]int)
	)
	client := &cloud.MockClient{
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			key := constraintKey(constraints, "")
			mu.Lock()
			searchCounts[key]++
			mu.Unlock()

			memGB := float64(constraints.MinGPUMemGB)
			if memGB == 0 {
				memGB = 24
			}
			return []cloud.Offer{
				{
					ProviderID:  fmt.Sprintf("offer-%s", key),
					GPUName:     fmt.Sprintf("GPU-%dGB", constraints.MinGPUMemGB),
					GPUMemGB:    memGB,
					CostPerHour: 1.00,
					DLPerf:      memGB,
				},
			}, nil
		},
	}

	plans := BuildProfilePlansFromSplitRaw(
		nil,
		[]cloud.Client{client},
		[]InstanceGroup{splitGroup},
		splitRaw,
		nil,
		nil,
		nil,
		nil,
		[]bidding.ScoreProfile{
			bidding.StrategyCheap.Profile(),
			bidding.StrategyFastest.Profile(),
		},
		0.95,
		0,
	)
	if len(plans) != 2 {
		t.Fatalf("expected 2 profile plans, got %d", len(plans))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(searchCounts) != 3 {
		t.Fatalf("expected 3 unique candidate searches (split, packed, parallel), got %d (%v)", len(searchCounts), searchCounts)
	}
	for key, count := range searchCounts {
		if count != 1 {
			t.Fatalf("search %q ran %d times, want 1", key, count)
		}
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
		0.95,
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

func TestSearchTopKOffersForGroup_DistinctOffers(t *testing.T) {
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "1", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.30, Reliability: 0.99, MachineID: "m1"},
				{ProviderID: "2", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.35, Reliability: 0.99, MachineID: "m2"},
				{ProviderID: "3", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.40, Reliability: 0.99, MachineID: "m3"},
			}, nil
		},
	}
	results := SearchTopKOffersForGroup(
		[]cloud.Client{mockClient},
		InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, DiskGB: 80},
		3,
		nil, 1.0, bidding.ConstantSetup(0.5), nil,
		bidding.StrategyCheap, 0.95, 0,
	)
	if len(results) != 3 {
		t.Fatalf("got %d offers, want 3", len(results))
	}
	seen := map[string]bool{}
	for i, r := range results {
		if r.Offer == nil {
			t.Fatalf("offer %d is nil", i)
		}
		if seen[r.Offer.ProviderID] {
			t.Fatalf("duplicate offer %q at position %d", r.Offer.ProviderID, i)
		}
		seen[r.Offer.ProviderID] = true
	}
}

func TestSearchTopKOffersForGroup_ReturnsFewerWhenInventoryShort(t *testing.T) {
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "1", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.30, Reliability: 0.99},
			}, nil
		},
	}
	results := SearchTopKOffersForGroup(
		[]cloud.Client{mockClient},
		InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, DiskGB: 80},
		3,
		nil, 1.0, bidding.ConstantSetup(0.5), nil,
		bidding.StrategyCheap, 0.95, 0,
	)
	if len(results) != 1 {
		t.Fatalf("got %d offers, want 1 (only one available)", len(results))
	}
}

func TestSearchBestOfferForGroup_ProviderFilter(t *testing.T) {
	vastClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "v1", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.40}}, nil
		},
	}
	runpodClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderRunpod,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "r1", Provider: cloud.ProviderRunpod, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.35}}, nil
		},
	}

	result := SearchBestOfferForGroup(
		[]cloud.Client{vastClient, runpodClient},
		InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Provider: "runpod"},
		nil,
		1.0,
		bidding.ConstantSetup(0.5),
		nil,
		bidding.StrategyCheap,
		0.95,
		0,
	)
	if result.Err != nil {
		t.Fatalf("SearchBestOfferForGroup: %v", result.Err)
	}
	if result.Offer == nil {
		t.Fatal("expected offer, got nil")
	}
	if result.Offer.Provider != cloud.ProviderRunpod {
		t.Fatalf("provider = %s, want runpod", result.Offer.Provider)
	}
}

func TestSearchBestOfferForGroup_ProviderUnavailable(t *testing.T) {
	vastClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "v1", Provider: cloud.ProviderVastai, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.40}}, nil
		},
	}

	result := SearchBestOfferForGroup(
		[]cloud.Client{vastClient},
		InstanceGroup{GPUClass: "RTX_4090", GPUMemGB: 24, Provider: "runpod"},
		nil,
		1.0,
		bidding.ConstantSetup(0.5),
		nil,
		bidding.StrategyCheap,
		0.95,
		0,
	)
	if result.Err == nil {
		t.Fatal("expected provider unavailable error")
	}
	if !strings.Contains(result.Err.Error(), "requested provider") {
		t.Fatalf("unexpected error: %v", result.Err)
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
	c := offerConstraintsForGroup(group, 0.95)
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
	c := offerConstraintsForGroup(group, 0.95)
	if c.MinCPUCoresEffective != 0 {
		t.Errorf("MinCPUCoresEffective = %d, want 0", c.MinCPUCoresEffective)
	}
}

func TestOfferConstraintsForGroup_MultiGPUShape(t *testing.T) {
	group := InstanceGroup{
		GPUClass:     "H100",
		NumGPUs:      4,
		GPUMemGB:     80,
		CPUCores:     32,
		Interconnect: "nvlink",
		Jobs:         []*db.Job{{ID: 1, Tags: []string{"rental"}}},
	}
	c := offerConstraintsForGroup(group, 0.95)
	if c.NumGPUs != 4 {
		t.Errorf("NumGPUs = %d, want 4", c.NumGPUs)
	}
	if c.MinCPUCoresEffective != 32 {
		t.Errorf("MinCPUCoresEffective = %d, want 32", c.MinCPUCoresEffective)
	}
	if c.Interconnect != "nvlink" {
		t.Errorf("Interconnect = %q, want nvlink", c.Interconnect)
	}
}

func TestOfferConstraintsForGroup_RunpodCloudType(t *testing.T) {
	group := InstanceGroup{
		GPUClass:        "L4",
		Provider:        "runpod",
		RunpodCloudType: cloud.RunpodCloudTypeSecure,
		GPUMemGB:        20,
		Jobs:            []*db.Job{{ID: 1, Tags: []string{"provider:runpod"}}},
	}
	c := offerConstraintsForGroup(group, 0.95)
	if c.RunpodCloudType != cloud.RunpodCloudTypeSecure {
		t.Fatalf("RunpodCloudType = %q, want secure", c.RunpodCloudType)
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

func TestMapOffersToSplitGroups_MergedCandidate(t *testing.T) {
	// 2 split groups merged into 1 candidate group
	splitGroups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 2}}},
	}
	mergedOffer := &cloud.Offer{ProviderID: "merged-offer", CostPerHour: 0.50}
	result := CandidateResult{
		Label: "merged",
		Groups: []InstanceGroup{
			{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
		},
		Offers: []GroupOffer{
			{
				Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}, {ID: 2}}},
				Offer: mergedOffer,
			},
		},
	}

	mapped := MapOffersToSplitGroups(splitGroups, result)
	if len(mapped) != 2 {
		t.Fatalf("expected 2 mapped offers, got %d", len(mapped))
	}
	// Both split groups should get the merged offer
	if mapped[0].Offer == nil || mapped[0].Offer.ProviderID != "merged-offer" {
		t.Errorf("split group 0: expected merged-offer, got %v", mapped[0].Offer)
	}
	if mapped[1].Offer == nil || mapped[1].Offer.ProviderID != "merged-offer" {
		t.Errorf("split group 1: expected merged-offer, got %v", mapped[1].Offer)
	}
}

func TestMapOffersToSplitGroups_SplitCandidate(t *testing.T) {
	// Split candidate = same grouping, 1:1 mapping
	splitGroups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "H100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 2}}},
	}
	offer1 := &cloud.Offer{ProviderID: "offer-1"}
	offer2 := &cloud.Offer{ProviderID: "offer-2"}
	result := CandidateResult{
		Label:  "split",
		Groups: splitGroups,
		Offers: []GroupOffer{
			{Group: splitGroups[0], Offer: offer1},
			{Group: splitGroups[1], Offer: offer2},
		},
	}

	mapped := MapOffersToSplitGroups(splitGroups, result)
	if len(mapped) != 2 {
		t.Fatalf("expected 2 mapped offers, got %d", len(mapped))
	}
	if mapped[0].Offer.ProviderID != "offer-1" {
		t.Errorf("group 0: expected offer-1, got %s", mapped[0].Offer.ProviderID)
	}
	if mapped[1].Offer.ProviderID != "offer-2" {
		t.Errorf("group 1: expected offer-2, got %s", mapped[1].Offer.ProviderID)
	}
}

func TestMapOffersToSplitGroups_PreservesFilterStats(t *testing.T) {
	splitGroups := []InstanceGroup{
		{GPUClass: "RTX-5090", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
	}
	result := CandidateResult{
		Label:  "split",
		Groups: splitGroups,
		Offers: []GroupOffer{
			{
				Group: splitGroups[0],
				Offer: &cloud.Offer{ProviderID: "offer-1"},
				FilterStats: OfferFilterStats{
					RawCount:      19,
					AfterVRAM:     19,
					AfterCUDA:     0,
					AfterSurvival: 0,
				},
			},
		},
	}

	mapped := MapOffersToSplitGroups(splitGroups, result)
	if len(mapped) != 1 {
		t.Fatalf("expected 1 mapped offer, got %d", len(mapped))
	}
	if mapped[0].FilterStats.RawCount != 19 || mapped[0].FilterStats.AfterCUDA != 0 {
		t.Fatalf("mapped filter stats were not preserved: %#v", mapped[0].FilterStats)
	}
}

func TestMapOffersToSplitGroups_UnmatchedGroupGetsNilOffer(t *testing.T) {
	// Split group with a job not in any candidate group
	splitGroups := []InstanceGroup{
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 99}}}, // not in candidate
	}
	result := CandidateResult{
		Label: "merged",
		Groups: []InstanceGroup{
			{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{{ID: 1}}},
		},
		Offers: []GroupOffer{
			{Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}}, Offer: &cloud.Offer{ProviderID: "o1"}},
		},
	}

	mapped := MapOffersToSplitGroups(splitGroups, result)
	if mapped[0].Offer == nil {
		t.Error("group 0 should have an offer")
	}
	if mapped[1].Offer != nil {
		t.Error("group 1 (unmatched) should have nil offer")
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

// TestScoreGrouping_FastestPrefersParallel verifies that the fastest strategy
// prefers running N jobs on N instances in parallel over running them
// sequentially on one instance.
func TestScoreGrouping_FastestPrefersParallel(t *testing.T) {
	h200Offer := &cloud.Offer{ProviderID: "h200", CostPerHour: 2.50, GPUName: "H200", DLPerf: 40.0}
	rtx3090Offer := &cloud.Offer{ProviderID: "rtx3090", CostPerHour: 0.15, GPUName: "RTX 3090", DLPerf: 15.0}

	// Grouped: 5 jobs on one H200 instance (sequential)
	grouped := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 20, Jobs: []*db.Job{
				{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5},
			}},
			Offer: h200Offer,
		},
	}

	// Parallel: 5 jobs on 5 separate cheap instances
	parallel := []GroupOffer{
		{Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 8, Jobs: []*db.Job{{ID: 1}}}, Offer: rtx3090Offer},
		{Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 8, Jobs: []*db.Job{{ID: 2}}}, Offer: rtx3090Offer},
		{Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 8, Jobs: []*db.Job{{ID: 3}}}, Offer: rtx3090Offer},
		{Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 8, Jobs: []*db.Job{{ID: 4}}}, Offer: rtx3090Offer},
		{Group: InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 8, Jobs: []*db.Job{{ID: 5}}}, Offer: rtx3090Offer},
	}

	groupedScore := ScoreGrouping(grouped, bidding.StrategyFastest)
	parallelScore := ScoreGrouping(parallel, bidding.StrategyFastest)

	if parallelScore >= groupedScore {
		t.Errorf("fastest should prefer parallel (score=%.3f) over grouped (score=%.3f)",
			parallelScore, groupedScore)
	}

	// Grouped: time = 5 jobs * 1hr + 0.5hr setup = 5.5hr
	// Parallel: time = max(1hr + 0.5hr) = 1.5hr
	// Fastest weights: Cost=0.01, Time=1.0
	// So parallel should win by a large margin
	t.Logf("grouped score=%.3f, parallel score=%.3f (ratio=%.1fx)",
		groupedScore, parallelScore, groupedScore/parallelScore)
}

// TestSummarizeGroupOffers_ParallelInstanceCount verifies that SummarizeGroupOffers
// correctly counts instances when each job is on its own instance.
func TestSummarizeGroupOffers_ParallelInstanceCount(t *testing.T) {
	offer := &cloud.Offer{ProviderID: "1", CostPerHour: 0.15, GPUName: "RTX 3090"}
	offers := []GroupOffer{
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 2}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 3}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 4}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 5}}}, Offer: offer},
	}
	summary := SummarizeGroupOffers(offers)
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
	if summary.NumGPUs != 5 {
		t.Errorf("NumGPUs = %d, want 5", summary.NumGPUs)
	}
}

// TestJob545Through549_FullPipeline exercises the full pipeline for the actual
// job scenario: grouping → offer ranking → strategy selection. Verifies:
// 1. 8GB jobs (546-549) are in a separate group from 20GB jobs
// 2. Fastest strategy doesn't pick H200 for 8GB jobs without runtime evidence
// 3. Parallel candidate splits the 4-job group for parallel execution
func TestJob545Through549_FullPipeline(t *testing.T) {
	// Step 1: Group the jobs
	jobs := []*db.Job{
		{ID: 542, Status: db.StatusQueued, GPUClass: "3090", GPUMemGB: intPtr(20)},
		{ID: 545, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 546, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 547, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 548, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 549, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(8),
			Inputs: []string{"hf:gpt2", "hf-dataset:wikitext"}},
		{ID: 550, Status: db.StatusQueued, GPUClass: "nvidia", GPUMemGB: intPtr(20),
			Inputs: []string{"hf:EleutherAI/pythia-1.4b"}},
	}

	groups := GroupByAffinity(jobs, nil)
	t.Logf("Groups: %d", len(groups))
	for i, g := range groups {
		t.Logf("  group %d: %s ≥%dGB, %d jobs", i, g.GPUClass, g.GPUMemGB, len(g.Jobs))
	}

	// Find the 8GB group
	var group8GB *InstanceGroup
	for i := range groups {
		if groups[i].GPUMemGB == 8 {
			group8GB = &groups[i]
			break
		}
	}
	if group8GB == nil {
		t.Fatal("no group with GPUMemGB=8 found — tier separation failed")
	}
	if len(group8GB.Jobs) != 4 {
		t.Errorf("8GB group has %d jobs, want 4", len(group8GB.Jobs))
	}

	// Step 2: Verify the legacy fallback selector stays neutral on runtime.
	// Even with oversized offers present, the fastest strategy should prefer a
	// cheap adequate GPU when no estimator-backed runtime is available.
	offers := []cloud.Offer{
		{ProviderID: "rtx3090", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.15, DLPerf: 15.0},
		{ProviderID: "rtx4090", GPUName: "RTX 4090", GPUMemGB: 24, CostPerHour: 0.33, DLPerf: 25.0},
		{ProviderID: "h200", GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
	}
	_, best := bidding.BestOffer(nil, offers, 1.0, bidding.ConstantSetup(0.5),
		bidding.StrategyFastest, 0)
	if best.ProviderID == "h200" {
		t.Errorf("fastest strategy picked H200 for 8GB group — neutral runtime fallback failed")
	}
	t.Logf("fastest strategy picked %s ($%.2f/hr) for 8GB group", best.GPUName, best.CostPerHour)

	// Step 3: Verify parallel candidate splits the 4-job group
	parallelGroups := SplitToParallel(groups)
	if len(parallelGroups) <= len(groups) {
		t.Errorf("parallel candidate has %d groups (same as split %d) — splitting failed",
			len(parallelGroups), len(groups))
	}
	// Count 1-job groups that came from the 8GB group.
	var singleJob8GB int
	for _, g := range parallelGroups {
		if g.GPUMemGB == 8 && len(g.Jobs) == 1 {
			singleJob8GB++
		}
	}
	if singleJob8GB != 4 {
		t.Errorf("expected 4 single-job 8GB groups, got %d — parallel split failed", singleJob8GB)
	}

	// Step 3b: Verify offer selection on parallel candidate's 8GB groups
	// avoids H200 under the neutral runtime fallback.
	for _, g := range parallelGroups {
		if g.GPUMemGB != 8 {
			continue
		}
		_, pick := bidding.BestOffer(nil, offers, 1.0, bidding.ConstantSetup(0.5),
			bidding.StrategyFastest, 0)
		if pick.ProviderID == "h200" {
			t.Errorf("parallel 8GB group (job %d): fastest picked H200 — should pick cheaper GPU",
				g.Jobs[0].ID)
		}
	}

	// Step 4: Verify parallel scores better than grouped for fastest
	cheapOffer := &cloud.Offer{ProviderID: "rtx3090", CostPerHour: 0.15, GPUName: "RTX 3090"}
	grouped := []GroupOffer{
		{Group: *group8GB, Offer: cheapOffer},
	}
	parallel := make([]GroupOffer, 4)
	for i := 0; i < 4; i++ {
		parallel[i] = GroupOffer{
			Group: InstanceGroup{GPUMemGB: 8, Jobs: []*db.Job{{ID: int64(546 + i)}}},
			Offer: cheapOffer,
		}
	}
	groupedScore := ScoreGrouping(grouped, bidding.StrategyFastest)
	parallelScore := ScoreGrouping(parallel, bidding.StrategyFastest)
	if parallelScore >= groupedScore {
		t.Errorf("fastest should prefer parallel (%.3f) over grouped (%.3f)", parallelScore, groupedScore)
	}
	t.Logf("grouped score=%.3f, parallel score=%.3f (%.1fx improvement)", groupedScore, parallelScore, groupedScore/parallelScore)
}

func TestOfferConstraints_UsesOnlyMinGPUMemGB(t *testing.T) {
	group := InstanceGroup{
		GPUClass:    "NVIDIA",
		GPUMemGB:    8,
		MaxGPUMemGB: 12,
	}
	c := offerConstraintsForGroup(group, 0.95)
	if c.MaxGPUMemGB != 0 {
		t.Errorf("MaxGPUMemGB should not be passed to search constraints (got %d)", c.MaxGPUMemGB)
	}
	if c.MinGPUMemGB != 8 {
		t.Errorf("MinGPUMemGB = %d, want 8", c.MinGPUMemGB)
	}
}

func TestRankOffer_NoHardVRAMCeiling(t *testing.T) {
	// Legacy MaxGPUMemGB values are not a hard filter. All offers meeting the
	// minimum should be eligible; cheapest wins.
	group := InstanceGroup{
		GPUClass:    "NVIDIA",
		GPUMemGB:    18,
		MaxGPUMemGB: 18,
		Jobs:        []*db.Job{{ID: 1}},
	}
	offers := []cloud.Offer{
		{ProviderID: "small", GPUName: "RTX 2080 Ti", GPUMemGB: 11, CostPerHour: 0.10},
		{ProviderID: "24gb", GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.05},
		{ProviderID: "48gb", GPUName: "A100 PCIE", GPUMemGB: 48, CostPerHour: 0.50},
	}

	got := rankOfferWithProfile(group, offers, nil, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap.Profile(), 0)
	if got.Offer == nil {
		t.Fatal("expected an offer, got nil")
	}
	// 11GB is below minimum (18), so excluded. Both 24GB and 48GB pass the
	// minimum filter; cheapest (24gb at $0.05) wins.
	if got.Offer.ProviderID != "24gb" {
		t.Fatalf("picked offer %q, want %q (cheapest above minimum)", got.Offer.ProviderID, "24gb")
	}
}

func TestRankOffer_FiltersInsufficientGPUCount(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "A100",
		GPUMemGB: 80,
		NumGPUs:  2,
		Jobs:     []*db.Job{{ID: 1}},
	}
	offers := []cloud.Offer{
		{ProviderID: "one-gpu", GPUName: "A100 SXM4", GPUMemGB: 80, NumGPUs: 1, CostPerHour: 0.10},
		{ProviderID: "two-gpu", GPUName: "A100 SXM4", GPUMemGB: 80, NumGPUs: 2, CostPerHour: 0.20},
	}

	got := rankOfferWithProfile(group, offers, nil, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap.Profile(), 0)
	if got.Offer == nil {
		t.Fatal("expected an offer, got nil")
	}
	if got.Offer.ProviderID != "two-gpu" {
		t.Fatalf("picked offer %q, want two-gpu", got.Offer.ProviderID)
	}
}

func TestRankOffer_UsesJobMinSurvivalOverride(t *testing.T) {
	minSurvivalZero := 0.0
	lowSurvivalOffer := cloud.Offer{
		ProviderID:  "low",
		Provider:    cloud.ProviderVastai,
		GPUName:     "A100 PCIE",
		GPUMemGB:    80,
		CostPerHour: 0.10,
		Reliability: 0.5,
		MachineID:   "machine-low",
	}
	now := time.Now()
	outcomes := make([]bidding.InstanceOutcome, 0, 40)
	for range 40 {
		outcomes = append(outcomes, bidding.InstanceOutcome{
			Provider:          cloud.ProviderVastai,
			TerminationReason: db.TerminationReasonInfraFailure,
			ResolvedGPUName:   "A100 PCIE",
			GPUMemGB:          80,
			CostPerHourCents:  10,
			Reliability:       0.5,
			MachineID:         "machine-low",
			EndedAtUnix:       now.Unix(),
		})
	}
	model := bidding.BuildSurvivalModelAt(outcomes, now)

	overridden := InstanceGroup{
		GPUClass: "A100",
		Jobs: []*db.Job{{
			ID: 1,
			CLIResourceOverrides: &db.CLIResourceOverrides{
				MinSurvival: &minSurvivalZero,
			},
		}},
	}
	got := rankOfferWithProfile(overridden, []cloud.Offer{lowSurvivalOffer}, model, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap.Profile(), 0.4)
	if got.Offer == nil || got.Offer.ProviderID != "low" {
		t.Fatalf("override min_survival=0 should allow low survival offer, got %+v", got.Offer)
	}

	mixed := InstanceGroup{
		GPUClass: "A100",
		Jobs: []*db.Job{
			{ID: 1, CLIResourceOverrides: &db.CLIResourceOverrides{MinSurvival: &minSurvivalZero}},
			{ID: 2},
		},
	}
	got = rankOfferWithProfile(mixed, []cloud.Offer{lowSurvivalOffer}, model, 1.0, bidding.ConstantSetup(0.5), bidding.StrategyCheap.Profile(), 0.4)
	if got.Offer != nil {
		t.Fatalf("mixed group should keep default member survival floor, got %+v", got.Offer)
	}
}

func TestSplitToParallel_UsesOnlyPerJobMinimumMemory(t *testing.T) {
	group := InstanceGroup{
		GPUClass:    "NVIDIA",
		GPUMemGB:    20, // supremum
		MaxGPUMemGB: 24, // legacy metadata ignored
		Jobs: []*db.Job{
			{ID: 1, GPUMemGB: intPtr(20)},
			{ID: 2, GPUMemGB: intPtr(8)},
			{ID: 3, GPUMemGB: intPtr(8), GPUMemMaxGB: intPtr(16)},
		},
	}
	result := SplitToParallel([]InstanceGroup{group})
	if len(result) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(result))
	}

	for _, g := range result {
		if g.MaxGPUMemGB != 0 {
			t.Errorf("group with GPUMemGB=%d has MaxGPUMemGB=%d, want 0", g.GPUMemGB, g.MaxGPUMemGB)
		}
	}
}

// TestApproximateEstimates_MatchesGroupOfferCount verifies that
// ApproximateEstimates produces one estimate per group with a valid offer.
func TestApproximateEstimates_MatchesGroupOfferCount(t *testing.T) {
	offer := &cloud.Offer{ProviderID: "1", CostPerHour: 0.15, GPUName: "RTX 3090"}
	offers := []GroupOffer{
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 2}}}, Offer: offer},
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 3}}}, Offer: nil}, // no offer
		{Group: InstanceGroup{Jobs: []*db.Job{{ID: 4}}}, Offer: offer},
	}
	estimates := ApproximateEstimates(offers)
	if len(estimates) != 3 {
		t.Errorf("expected 3 estimates (groups with offers), got %d", len(estimates))
	}
	for _, est := range estimates {
		if est.Offer.Offer == nil {
			t.Error("estimate has nil offer — should be filtered")
		}
		if est.TotalCost <= 0 {
			t.Error("estimate TotalCost should be positive")
		}
	}
}

func TestApproximateEstimates_UsesNeutralRuntimeAcrossOffers(t *testing.T) {
	offers := []GroupOffer{
		{
			Group: InstanceGroup{Jobs: []*db.Job{{ID: 1}}},
			Offer: &cloud.Offer{ProviderID: "rtx4090", CostPerHour: 0.33, GPUName: "RTX 4090", DLPerf: 25},
		},
		{
			Group: InstanceGroup{Jobs: []*db.Job{{ID: 2}}},
			Offer: &cloud.Offer{ProviderID: "h200", CostPerHour: 3.23, GPUName: "H200", DLPerf: 40},
		},
	}

	estimates := ApproximateEstimates(offers)
	if len(estimates) != 2 {
		t.Fatalf("expected 2 estimates, got %d", len(estimates))
	}
	if estimates[0].Breakdown.Run.Mean != time.Hour {
		t.Fatalf("first run mean = %v, want 1h", estimates[0].Breakdown.Run.Mean)
	}
	if estimates[1].Breakdown.Run.Mean != time.Hour {
		t.Fatalf("second run mean = %v, want 1h", estimates[1].Breakdown.Run.Mean)
	}
	if estimates[0].TotalTime != estimates[1].TotalTime {
		t.Fatalf("TotalTime differs across offers: %v vs %v", estimates[0].TotalTime, estimates[1].TotalTime)
	}
}

func TestNoOffersDetail_IncludesConstraints(t *testing.T) {
	stats := OfferFilterStats{RawCount: 0}
	got := stats.NoOffersDetail("gpu=ampere+ vram>=40GB")
	want := "no offers from providers for gpu=ampere+ vram>=40GB"
	if got != want {
		t.Fatalf("NoOffersDetail = %q, want %q", got, want)
	}
}

func TestNoOffersDetail_ProviderErrorsMeanUnknownMarket(t *testing.T) {
	stats := OfferFilterStats{
		RawCount:       0,
		ProviderErrors: []string{"vastai: command timed out", "runpod: temporary API failure"},
	}
	got := stats.NoOffersDetail("gpu=A40")
	for _, want := range []string{
		"provider offer fetch unavailable",
		"vastai: command timed out",
		"runpod: temporary API failure",
		"market unknown",
		"Weft will retry",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("NoOffersDetail = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "no offers") {
		t.Fatalf("NoOffersDetail = %q, must not render provider errors as no offers", got)
	}
}

func TestNoOffersDetail_FilterStages(t *testing.T) {
	cases := []struct {
		name string
		s    OfferFilterStats
		want string
	}{
		{"vram", OfferFilterStats{RawCount: 12}, "12 offers found, all filtered by VRAM requirement"},
		{"host ram", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterGPUCount: 8, HostRAMFiltered: 8, AfterHostRAM: 0}, "12 offers found, 8 offers passed VRAM/GPU count but all filtered by host RAM requirement"},
		{"interconnect", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterGPUCount: 8, AfterHostRAM: 6, InterconnectFiltered: 6, AfterInterconnect: 0, InterconnectRequired: "nvlink"}, "12 offers found, 6 offers passed host RAM but all filtered by interconnect requirement (nvlink)"},
		{"cuda", OfferFilterStats{RawCount: 12, AfterVRAM: 8}, "12 offers found, 8 offers passed interconnect/topology filters but all filtered by CUDA compatibility"},
		{"torch arch max", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5, TorchArchMaxCap: "sm_89"}, "12 offers found, 5 offers passed VRAM/CUDA but all filtered by torch arch upper bound (max cap=sm_89)"},
		{"torch arch min", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5, TorchArchMinCap: "7.5"}, "12 offers found, 5 offers passed VRAM/CUDA but all filtered by torch arch lower bound (min cap=7.5)"},
		{"unknown provider compatibility", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5, AfterProvider: 0, UnknownCompatibility: 5, ProviderCompatibilityFiltered: 5}, "12 offers found, 5 offers passed CUDA/image filters but all filtered because the provider did not report required CUDA/driver compatibility"},
		{"forward compat driver", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5, AfterProvider: 5, ForwardCompatFiltered: 5, AfterForward: 0, ForwardCompatExampleGPU: "RTX 3090"}, "12 offers found, 5 offers passed provider compatibility but all filtered by Vast.ai datacenter forward-compat driver guard on consumer GPUs (e.g. RTX 3090)"},
		{"survival", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5}, "12 offers found, 5 offers passed filters but none met survival threshold"},
		{"defensive fallback", OfferFilterStats{RawCount: 12, AfterVRAM: 8, AfterCUDA: 5, AfterSurvival: 5}, "12 offers found, none met all criteria"},
		{"singular vram", OfferFilterStats{RawCount: 1}, "1 offer found, all filtered by VRAM requirement"},
		{"singular cuda", OfferFilterStats{RawCount: 1, AfterVRAM: 1}, "1 offer found, 1 offer passed interconnect/topology filters but all filtered by CUDA compatibility"},
		{"singular torch arch", OfferFilterStats{RawCount: 1, AfterVRAM: 1, AfterCUDA: 1, TorchArchMaxCap: "sm_89"}, "1 offer found, 1 offer passed VRAM/CUDA but all filtered by torch arch upper bound (max cap=sm_89)"},
		{"singular survival", OfferFilterStats{RawCount: 1, AfterVRAM: 1, AfterCUDA: 1}, "1 offer found, 1 offer passed filters but none met survival threshold"},
		{"singular defensive fallback", OfferFilterStats{RawCount: 1, AfterVRAM: 1, AfterCUDA: 1, AfterSurvival: 1}, "1 offer found, none met all criteria"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.NoOffersDetail(""); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoOffersDetail_CUDAIncludesImageAndRequiredVersion(t *testing.T) {
	stats := OfferFilterStats{
		RawCount:         19,
		AfterVRAM:        19,
		AfterCUDA:        0,
		CUDAImage:        "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
		CUDAImageVersion: 12.4,
		CUDAMinRequired:  12.8,
		CUDAExampleGPU:   "RTX 5090",
	}
	got := stats.NoOffersDetail("")
	if !strings.Contains(got, "CUDA compatibility") ||
		!strings.Contains(got, "image=nvidia/cuda:12.4.1-runtime-ubuntu22.04 CUDA 12.4") ||
		!strings.Contains(got, "requires >=12.8") ||
		!strings.Contains(got, "RTX 5090") {
		t.Fatalf("unexpected CUDA detail message: %q", got)
	}
}

func TestRankOffer_KnownCompatibleBeatsUnknownCompatibility(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "12.8"}
	offers := []cloud.Offer{
		{ProviderID: "unknown", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.01},
		{ProviderID: "known", Provider: cloud.ProviderVastai, GPUName: "RTX A6000", CUDAVersion: 12.8, CostPerHour: 5.00},
	}

	got := rankOfferWithProfile(group, offers, nil, 1, bidding.ConstantSetup(0), bidding.StrategyCheap.Profile(), 0)
	if got.Offer == nil {
		t.Fatal("expected an offer")
	}
	if got.Offer.ProviderID != "known" {
		t.Fatalf("selected %q, want known-compatible offer", got.Offer.ProviderID)
	}
	if got.FilterStats.UnknownCompatibility != 1 {
		t.Fatalf("UnknownCompatibility = %d, want 1", got.FilterStats.UnknownCompatibility)
	}
}

func TestFilterOffersByProviderCompatibility_DropsUnknownWhenKnownExists(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "12.8"}
	offers := []cloud.Offer{
		{ProviderID: "rp1", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.50},
		{ProviderID: "va1", Provider: cloud.ProviderVastai, GPUName: "RTX A6000", CUDAVersion: 12.8, CostPerHour: 2.00},
		{ProviderID: "rp2", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.40},
	}
	got, filtered, unknown := filterOffersByProviderCompatibility(group, offers)
	if filtered != 2 {
		t.Fatalf("filtered = %d, want 2 unknown-compatible offers filtered", filtered)
	}
	if unknown != 2 {
		t.Fatalf("unknown count = %d, want 2 (both RunPod offers dropped)", unknown)
	}
	if len(got) != 1 || got[0].ProviderID != "va1" {
		t.Fatalf("filtered offers = %+v, want only va1", got)
	}
}

func TestFilterOffersByProviderCompatibility_KeepsUnknownWhenNoKnown(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "12.8"}
	offers := []cloud.Offer{
		{ProviderID: "rp1", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.50},
		{ProviderID: "rp2", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.40},
	}
	got, filtered, unknown := filterOffersByProviderCompatibility(group, offers)
	if filtered != 0 {
		t.Fatalf("filtered = %d, want 0 when unknown offers are the only fallback", filtered)
	}
	if unknown != 2 {
		t.Fatalf("unknown = %d, want 2", unknown)
	}
	if len(got) != 2 {
		t.Fatalf("filtered offers = %d, want both unknown-compatible fallback offers", len(got))
	}
}

func TestFilterOffersByProviderCompatibility_KeepsUnknownForPinnedRunPodProbe(t *testing.T) {
	group := InstanceGroup{Provider: "runpod", MinCUDAVersion: "12.8", MinDriverVersion: 570}
	offers := []cloud.Offer{
		{ProviderID: "rp1", Provider: cloud.ProviderRunpod, GPUName: "L4", CostPerHour: 0.50},
		{ProviderID: "rp2", Provider: cloud.ProviderRunpod, GPUName: "L4", CostPerHour: 0.40},
	}
	got, filtered, unknown := filterOffersByProviderCompatibility(group, offers)
	if filtered != 0 {
		t.Fatalf("filtered = %d, want 0 for explicitly pinned RunPod probes", filtered)
	}
	if unknown != 2 {
		t.Fatalf("unknown = %d, want 2", unknown)
	}
	if len(got) != 2 {
		t.Fatalf("filtered offers = %d, want both RunPod offers kept", len(got))
	}
}

func TestOfferCompatibilityStatus_CUDAChain(t *testing.T) {
	// The offer filter funnels through compat.ValidateCUDAChain: the group's
	// merged CUDA floor and image toolkit are checked against the offer's
	// driver-supported CUDA with one shared semantics.
	tests := []struct {
		name  string
		group InstanceGroup
		offer cloud.Offer
		want  CompatibilityStatus
	}{
		{
			name:  "driver below floor is incompatible (wb32/wb36 class)",
			group: InstanceGroup{MinCUDAVersion: "12.8"},
			offer: cloud.Offer{Provider: cloud.ProviderVastai, CUDAVersion: 12.0},
			want:  CompatibilityIncompatible,
		},
		{
			name:  "driver at floor is known compatible",
			group: InstanceGroup{MinCUDAVersion: "12.8"},
			offer: cloud.Offer{Provider: cloud.ProviderVastai, CUDAVersion: 12.8},
			want:  CompatibilityKnown,
		},
		{
			name: "image toolkit below floor is not by itself incompatible (wb30 class)",
			group: InstanceGroup{
				MinCUDAVersion: "12.5",
				Image:          "nvidia/cuda:12.4.1-devel-ubuntu22.04",
			},
			offer: cloud.Offer{Provider: cloud.ProviderVastai, CUDAVersion: 12.8},
			want:  CompatibilityKnown,
		},
		{
			name: "cross-major image toolkit above driver CUDA is incompatible",
			group: InstanceGroup{
				MinDriverVersion: 525,
				Image:            "nvidia/cuda:13.0.0-devel-ubuntu22.04",
			},
			offer: cloud.Offer{Provider: cloud.ProviderVastai, CUDAVersion: 12.8},
			want:  CompatibilityIncompatible,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := offerCompatibilityStatus(tt.group, tt.offer); got != tt.want {
				t.Fatalf("offerCompatibilityStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterOffersByProviderCompatibility_KeepsUnknownWhenNotRequired(t *testing.T) {
	group := InstanceGroup{}
	offers := []cloud.Offer{
		{ProviderID: "rp1", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.50},
	}
	got, filtered, unknown := filterOffersByProviderCompatibility(group, offers)
	if filtered != 0 || unknown != 0 {
		t.Fatalf("filtered=%d unknown=%d, want 0,0 when compatibility is not required", filtered, unknown)
	}
	if len(got) != 1 || got[0].ProviderID != "rp1" {
		t.Fatalf("filtered offers = %+v, want original RunPod offer", got)
	}
}

func TestFilterOffersByForwardCompatDriver_DropsVastDatacenterConsumerGPU(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "13.0"}
	offers := []cloud.Offer{
		{ProviderID: "bad", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", DatacenterDriver: true},
		{ProviderID: "a100", Provider: cloud.ProviderVastai, GPUName: "A100 PCIE", DatacenterDriver: true},
		{ProviderID: "consumer-local", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", DatacenterDriver: false},
		{ProviderID: "runpod", Provider: cloud.ProviderRunpod, GPUName: "RTX 3090", DatacenterDriver: true},
	}
	got, filtered, exampleGPU := filterOffersByForwardCompatDriver(group, offers)
	if filtered != 1 {
		t.Fatalf("filtered = %d, want 1", filtered)
	}
	if exampleGPU != "RTX 3090" {
		t.Fatalf("exampleGPU = %q, want RTX 3090", exampleGPU)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d offers, want 3: %+v", len(got), got)
	}
	for _, offer := range got {
		if offer.ProviderID == "bad" {
			t.Fatalf("datacenter-driver RTX offer was not filtered: %+v", got)
		}
	}
}

func TestFilterOffersByForwardCompatDriver_KeepsOffersWhenCompatibilityNotRequired(t *testing.T) {
	group := InstanceGroup{}
	offers := []cloud.Offer{
		{ProviderID: "bad", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", DatacenterDriver: true},
	}
	got, filtered, exampleGPU := filterOffersByForwardCompatDriver(group, offers)
	if filtered != 0 || exampleGPU != "" {
		t.Fatalf("filtered=%d example=%q, want no filtering without provider compatibility requirement", filtered, exampleGPU)
	}
	if len(got) != 1 || got[0].ProviderID != "bad" {
		t.Fatalf("kept offers = %+v, want original offer", got)
	}
}

func TestConsumerNVIDIAGPU(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"RTX 3090", true},
		{"NVIDIA GeForce RTX 4090", true},
		{"RTX_5090", true},
		{"GTX 1080 Ti", true},
		{"RTX A6000", false},
		{"RTX 6000 Ada", false},
		{"A100 PCIE", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := consumerNVIDIAGPU(tt.name); got != tt.want {
				t.Fatalf("consumerNVIDIAGPU(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRankOffer_UsesUnknownCompatibilityFallback(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "12.8"}
	offers := []cloud.Offer{
		{ProviderID: "unknown", Provider: cloud.ProviderRunpod, GPUName: "RTX A6000", CostPerHour: 0.01},
	}

	got := rankOfferWithProfile(group, offers, nil, 1, bidding.ConstantSetup(0), bidding.StrategyCheap.Profile(), 0)
	if got.Offer == nil || got.Offer.ProviderID != "unknown" {
		t.Fatalf("selected %+v, want unknown-compatible fallback offer", got.Offer)
	}
	if got.FilterStats.UnknownCompatibility != 1 {
		t.Fatalf("UnknownCompatibility = %d, want 1", got.FilterStats.UnknownCompatibility)
	}
	if got.FilterStats.ProviderCompatibilityFiltered != 0 {
		t.Fatalf("ProviderCompatibilityFiltered = %d, want 0 for fallback offer", got.FilterStats.ProviderCompatibilityFiltered)
	}
}

func TestRankOffer_FiltersForwardCompatDatacenterConsumerGPU(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "13.0"}
	offers := []cloud.Offer{
		{ProviderID: "bad", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", GPUMemGB: 24, CUDAVersion: 13.0, DatacenterDriver: true, CostPerHour: 0.01},
	}

	got := rankOfferWithProfile(group, offers, nil, 1, bidding.ConstantSetup(0), bidding.StrategyCheap.Profile(), 0)
	if got.Offer != nil {
		t.Fatalf("selected forward-compat consumer offer: %#v", got.Offer)
	}
	if got.FilterStats.ForwardCompatFiltered != 1 || got.FilterStats.AfterForward != 0 {
		t.Fatalf("forward filter stats = %+v, want one filtered and zero after", got.FilterStats)
	}
	if got.FilterStats.ForwardCompatExampleGPU != "RTX 3090" {
		t.Fatalf("ForwardCompatExampleGPU = %q, want RTX 3090", got.FilterStats.ForwardCompatExampleGPU)
	}
}

func TestRankOffer_FiltersKnownIncompatibleCUDA(t *testing.T) {
	group := InstanceGroup{MinCUDAVersion: "12.8"}
	offers := []cloud.Offer{
		{ProviderID: "old", Provider: cloud.ProviderVastai, GPUName: "RTX A6000", CUDAVersion: 12.4, CostPerHour: 0.01},
	}

	got := rankOfferWithProfile(group, offers, nil, 1, bidding.ConstantSetup(0), bidding.StrategyCheap.Profile(), 0)
	if got.Offer != nil {
		t.Fatalf("selected known-incompatible offer: %#v", got.Offer)
	}
	if got.FilterStats.ProviderCompatibilityFiltered != 1 {
		t.Fatalf("ProviderCompatibilityFiltered = %d, want 1", got.FilterStats.ProviderCompatibilityFiltered)
	}
}

func TestFormatOfferConstraints(t *testing.T) {
	c := cloud.OfferConstraints{
		GPUClass:       "ampere+",
		MinGPUMemGB:    40,
		MinDiskGB:      60,
		MinReliability: 0.95,
	}
	got := FormatOfferConstraints(c)
	want := "gpu=ampere+ vram>=40GB disk>=60GB reliability>=0.95"
	if got != want {
		t.Fatalf("FormatOfferConstraints = %q, want %q", got, want)
	}
}

func TestFilterOffersByTorchArch(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "a100", GPUName: "A100"},
		{ProviderID: "h100", GPUName: "H100"},
		{ProviderID: "b200", GPUName: "B200"},
		{ProviderID: "rtxpro", GPUName: "RTX PRO 4500 Blackwell"},
		{ProviderID: "unknown", GPUName: "weird-future-gpu"},
	}
	// Cap at 9.0 (Hopper): A100 and H100 pass; B200/RTX PRO Blackwell
	// rejected by model lookup; unknown GPU rejected because fail-closed
	// under any bound (see wj2365 regression on 2026-06-02).
	got, filtered, exGPU, exCap := filterOffersByTorchArch(offers, "", "9.0")
	if filtered != 3 {
		t.Errorf("filtered = %d, want 3", filtered)
	}
	if exGPU == "" || exCap == "" {
		t.Errorf("expected example GPU and cap to be set, got %q %q", exGPU, exCap)
	}
	keepIDs := map[string]bool{}
	for _, o := range got {
		keepIDs[o.ProviderID] = true
	}
	if !keepIDs["a100"] || !keepIDs["h100"] {
		t.Errorf("expected a100, h100 to survive; got %+v", keepIDs)
	}
	if keepIDs["b200"] || keepIDs["rtxpro"] || keepIDs["unknown"] {
		t.Errorf("expected b200, rtxpro, unknown to be filtered; got %+v", keepIDs)
	}
}

// TestFilterOffersByTorchArch_RejectsBlackwellForTorch26 is the wj2365
// regression on 2026-06-02: torch 2.6 maps to maxCap "9.0" and the job had
// gpu-arch-max sm_9.0, but the autopilot launched onto RunPod RTX PRO 4500
// Blackwell instances three times. Two failure modes stacked:
//  1. the GPU catalog didn't list "rtxpro4500" / "rtxpro5000", and the
//     generation-name fallback in ComputeCapForGPU needs the literal
//     "blackwell" in the GPU name. RunPod's offer.GPUName is the terse
//     displayName ("RTX PRO 4500"), so ComputeCapForGPU returned "".
//  2. filterOffersByTorchArch was fail-open on maxCap-only when gpuCap == "",
//     so an unknown card sailed through. Both are now closed.
func TestFilterOffersByTorchArch_RejectsBlackwellForTorch26(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "rtxpro4500", GPUName: "RTX PRO 4500"},
		{ProviderID: "rtxpro5000", GPUName: "RTX PRO 5000"},
		{ProviderID: "rtxpro6000ws", GPUName: "RTX PRO 6000 WS"},
		{ProviderID: "a6000", GPUName: "RTX A6000"},
		{ProviderID: "4000ada", GPUName: "RTX 4000 Ada"},
	}
	got, filtered, _, _ := filterOffersByTorchArch(offers, "", "9.0")
	if filtered != 3 {
		t.Errorf("filtered = %d, want 3 (RTX PRO 4500/5000/6000 WS all sm_12.0)", filtered)
	}
	keepIDs := map[string]bool{}
	for _, o := range got {
		keepIDs[o.ProviderID] = true
	}
	if keepIDs["rtxpro4500"] || keepIDs["rtxpro5000"] || keepIDs["rtxpro6000ws"] {
		t.Errorf("expected RTX PRO Blackwell variants rejected by sm_9.0 cap; got %+v", keepIDs)
	}
	if !keepIDs["a6000"] || !keepIDs["4000ada"] {
		t.Errorf("expected Ampere/Ada offers to survive sm_9.0 cap; got %+v", keepIDs)
	}
}

func TestFilterOffersByTorchArch_NoBound(t *testing.T) {
	offers := []cloud.Offer{{ProviderID: "x", GPUName: "RTX 5090"}}
	got, filtered, _, _ := filterOffersByTorchArch(offers, "", "")
	if filtered != 0 || len(got) != 1 {
		t.Errorf("unbounded filter dropped offers: filtered=%d remaining=%d", filtered, len(got))
	}
}

func TestFilterOffersByTorchArch_MinBound(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "v100", GPUName: "Tesla V100"},
		{ProviderID: "t4", GPUName: "Tesla T4"},
		{ProviderID: "a100", GPUName: "A100"},
		{ProviderID: "unknown", GPUName: "weird-future-gpu"},
	}
	// minCap=7.5: V100 (7.0) below floor → rejected; unknown rejected because
	// fail-closed under a min-cap (see EXP-179 regression).
	got, filtered, exGPU, exCap := filterOffersByTorchArch(offers, "7.5", "")
	if filtered != 2 {
		t.Errorf("filtered = %d, want 2", filtered)
	}
	if exGPU == "" || exCap == "" {
		t.Errorf("expected example GPU and cap to be set, got %q %q", exGPU, exCap)
	}
	keepIDs := map[string]bool{}
	for _, o := range got {
		keepIDs[o.ProviderID] = true
	}
	if keepIDs["v100"] {
		t.Errorf("expected v100 to be filtered; got %+v", keepIDs)
	}
	if keepIDs["unknown"] {
		t.Errorf("expected unknown GPU to be filtered (fail-closed under min-cap); got %+v", keepIDs)
	}
	if !keepIDs["t4"] || !keepIDs["a100"] {
		t.Errorf("expected t4, a100 to survive; got %+v", keepIDs)
	}
}

// TestFilterOffersByTorchArch_RejectsPascalForTorch210 is the EXP-179 wj2240
// regression: torch 2.10 requires sm_75+, Vast.ai offered GTX 1080 Tis (sm_61),
// weft's catalog didn't recognize them, ComputeCapForGPU returned "" and the
// filter let them through. Job dispatched, then failed with
// cudaErrorNoKernelImageForDevice on first CUDA allocation.
func TestFilterOffersByTorchArch_RejectsPascalForTorch210(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "1080ti", GPUName: "GTX 1080 Ti"},
		{ProviderID: "p100", GPUName: "Tesla P100"},
		{ProviderID: "m40", GPUName: "Tesla M40"},
		{ProviderID: "3090", GPUName: "RTX 3090"},
		{ProviderID: "a5000", GPUName: "RTX A5000"},
	}
	// torch 2.10 on cu128 → MinComputeCap "7.5".
	got, filtered, _, _ := filterOffersByTorchArch(offers, "7.5", "")
	if filtered != 3 {
		t.Errorf("filtered = %d, want 3 (Pascal 1080 Ti + Pascal P100 + Maxwell M40)", filtered)
	}
	keepIDs := map[string]bool{}
	for _, o := range got {
		keepIDs[o.ProviderID] = true
	}
	if keepIDs["1080ti"] || keepIDs["p100"] || keepIDs["m40"] {
		t.Errorf("expected Pascal/Maxwell offers rejected by sm_75 floor; got %+v", keepIDs)
	}
	if !keepIDs["3090"] || !keepIDs["a5000"] {
		t.Errorf("expected RTX 3090 and A5000 (Ampere, sm_86) to survive; got %+v", keepIDs)
	}
}

// TestFilterOffersByTorchArch_UnknownGPUFailClosedOnMaxOnly verifies that
// unknown GPUs are rejected under a max-only cap. The catalog gets updated
// lazily, so "unknown to weft" empirically biases newer-than-the-cap-allows
// (e.g. wj2365 / RTX PRO 4500 Blackwell on 2026-06-02), not older. Both
// bounds now fail closed on unknown GPUs.
func TestFilterOffersByTorchArch_UnknownGPUFailClosedOnMaxOnly(t *testing.T) {
	offers := []cloud.Offer{{ProviderID: "unknown", GPUName: "weird-future-gpu"}}
	got, filtered, exGPU, exCap := filterOffersByTorchArch(offers, "", "9.0")
	if filtered != 1 || len(got) != 0 {
		t.Errorf("max-only filter should fail closed on unknown GPU; filtered=%d remaining=%d", filtered, len(got))
	}
	if exGPU != "weird-future-gpu" || exCap != "unknown" {
		t.Errorf("expected diagnostic example to be unknown GPU; got %q %q", exGPU, exCap)
	}
}

func TestSnapshotOnDemandRefCentsUsesComparableGPUName(t *testing.T) {
	group := InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 42}
	chosen := cloud.Offer{
		Provider:     cloud.ProviderVastai,
		InstanceType: cloud.InstanceTypeInterruptible,
		GPUName:      "RTX 5880Ada",
		NumGPUs:      1,
		GPUMemGB:     48,
		CostPerHour:  0.469,
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(c cloud.OfferConstraints) ([]cloud.Offer, error) {
			if c.InstanceType != cloud.InstanceTypeOnDemand {
				t.Fatalf("InstanceType = %q, want on-demand", c.InstanceType)
			}
			return []cloud.Offer{
				{Provider: cloud.ProviderVastai, InstanceType: cloud.InstanceTypeOnDemand, GPUName: "RTX 4090", NumGPUs: 1, GPUMemGB: 48, CostPerHour: 0.38},
				{Provider: cloud.ProviderVastai, InstanceType: cloud.InstanceTypeOnDemand, GPUName: "RTX 5880 Ada", NumGPUs: 1, GPUMemGB: 48, CostPerHour: 0.57},
				{Provider: cloud.ProviderVastai, InstanceType: cloud.InstanceTypeOnDemand, GPUName: "RTX 5880Ada", NumGPUs: 1, GPUMemGB: 48, CostPerHour: 0.61},
			}, nil
		},
	}

	got := snapshotOnDemandRefCents(client, group, chosen)
	if got == nil || *got != 57 {
		t.Fatalf("snapshotOnDemandRefCents = %v, want 57", got)
	}
}

func TestSnapshotOnDemandRefCentsNeverBelowInitialBid(t *testing.T) {
	group := InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 42}
	chosen := cloud.Offer{
		Provider:     cloud.ProviderVastai,
		InstanceType: cloud.InstanceTypeInterruptible,
		GPUName:      "RTX 5880Ada",
		NumGPUs:      1,
		GPUMemGB:     48,
		CostPerHour:  0.469,
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{Provider: cloud.ProviderVastai, InstanceType: cloud.InstanceTypeOnDemand, GPUName: "RTX 5880Ada", NumGPUs: 1, GPUMemGB: 48, CostPerHour: 0.466},
			}, nil
		},
	}

	got := snapshotOnDemandRefCents(client, group, chosen)
	if got == nil || *got != 47 {
		t.Fatalf("snapshotOnDemandRefCents = %v, want 47", got)
	}
}

func TestSnapshotOnDemandRefCentsIgnoresBroadClassWithoutComparableGPU(t *testing.T) {
	group := InstanceGroup{GPUClass: "NVIDIA", GPUMemGB: 42}
	chosen := cloud.Offer{
		Provider:     cloud.ProviderVastai,
		InstanceType: cloud.InstanceTypeInterruptible,
		GPUName:      "RTX 5880Ada",
		NumGPUs:      1,
		GPUMemGB:     48,
		CostPerHour:  0.469,
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{Provider: cloud.ProviderVastai, InstanceType: cloud.InstanceTypeOnDemand, GPUName: "RTX 4090", NumGPUs: 1, GPUMemGB: 48, CostPerHour: 0.38},
			}, nil
		},
	}

	if got := snapshotOnDemandRefCents(client, group, chosen); got != nil {
		t.Fatalf("snapshotOnDemandRefCents = %d, want nil without comparable GPU", *got)
	}
}

func TestOfferConstraintsForGroup_PreemptibleOptsIntoInterruptible(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "RTX_4090",
		GPUMemGB: 24,
		Jobs:     []*db.Job{{ID: 1, Tags: []string{db.TagInterruptible}}},
	}
	c := offerConstraintsForGroup(group, 0.95)
	if c.InstanceType != cloud.InstanceTypeInterruptible {
		t.Errorf("InstanceType = %q, want %q", c.InstanceType, cloud.InstanceTypeInterruptible)
	}
}

// TestOfferConstraintsForGroup_BidLossEscalationForcesOnDemand verifies the
// bid-loss escalation gate: a group flagged EscalateToOnDemand searches
// on-demand offers even though it has preemptible jobs. The escalation only
// suppresses the interruptible opt-in — it never sets an instance type on
// groups without preemptible jobs.
func TestOfferConstraintsForGroup_BidLossEscalationForcesOnDemand(t *testing.T) {
	group := InstanceGroup{
		GPUClass:           "RTX_4090",
		GPUMemGB:           24,
		EscalateToOnDemand: true,
		Jobs:               []*db.Job{{ID: 1, Tags: []string{db.TagInterruptible}}},
	}
	c := offerConstraintsForGroup(group, 0.95)
	if c.InstanceType == cloud.InstanceTypeInterruptible {
		t.Errorf("InstanceType = %q, want escalated group not to opt into interruptible", c.InstanceType)
	}

	// A non-preemptible group with the flag set (shouldn't happen, but must
	// stay harmless) still gets no instance type.
	group.Jobs = []*db.Job{{ID: 1}}
	c = offerConstraintsForGroup(group, 0.95)
	if c.InstanceType != "" {
		t.Errorf("InstanceType = %q, want empty for non-preemptible group", c.InstanceType)
	}
}
