package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/vastai"
)

func TestCheapestOffer(t *testing.T) {
	offers := []vastai.Offer{
		{ID: 1, CostPerHour: 2.50, GPUName: "A100"},
		{ID: 2, CostPerHour: 0.45, GPUName: "RTX_A6000"},
		{ID: 3, CostPerHour: 1.20, GPUName: "RTX_4090"},
	}
	best := cheapestOffer(offers)
	if best.ID != 2 {
		t.Errorf("cheapestOffer returned id=%d, want 2", best.ID)
	}
	if best.CostPerHour != 0.45 {
		t.Errorf("cheapestOffer cost=%f, want 0.45", best.CostPerHour)
	}
}

func TestSortOffersByCost(t *testing.T) {
	offers := []vastai.Offer{
		{ID: 1, CostPerHour: 2.50},
		{ID: 2, CostPerHour: 0.45},
		{ID: 3, CostPerHour: 1.20},
	}
	SortOffersByCost(offers)
	if offers[0].ID != 2 || offers[1].ID != 3 || offers[2].ID != 1 {
		t.Errorf("offers not sorted by cost: got IDs %d,%d,%d", offers[0].ID, offers[1].ID, offers[2].ID)
	}
}

func TestFetchGroupOffersMock(t *testing.T) {
	// Mock client that returns different offers for different GPU classes
	mockClient := &vastai.MockClient{
		SearchOffersFunc: func(constraints vastai.OfferConstraints) ([]vastai.Offer, error) {
			if constraints.GPUClass == "RTX_4090" {
				return []vastai.Offer{
					{ID: 1, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.50},
					{ID: 2, GPUName: "RTX_4090", GPUMemGB: 24, CostPerHour: 0.45},
				}, nil
			}
			if constraints.GPUClass == "A100" {
				return []vastai.Offer{
					{ID: 3, GPUName: "A100", GPUMemGB: 40, CostPerHour: 2.00},
				}, nil
			}
			return nil, nil // No offers
		},
	}

	groups := []InstanceGroup{
		{GPUClass: "RTX_4090", GPUMemGB: 24},
		{GPUClass: "A100", GPUMemGB: 40},
		{GPUClass: "H100", GPUMemGB: 80}, // No offers available
	}

	results := FetchGroupOffers(mockClient, groups)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// RTX_4090 should have the cheapest offer (ID 2)
	if results[0].Offer == nil || results[0].Offer.ID != 2 {
		t.Errorf("group 0: expected offer ID 2, got %v", results[0].Offer)
	}

	// A100 should have the only offer (ID 3)
	if results[1].Offer == nil || results[1].Offer.ID != 3 {
		t.Errorf("group 1: expected offer ID 3, got %v", results[1].Offer)
	}

	// H100 should have no offers
	if results[2].Offer != nil {
		t.Errorf("group 2: expected no offer, got %v", results[2].Offer)
	}
}
