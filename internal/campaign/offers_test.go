package campaign

import (
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
