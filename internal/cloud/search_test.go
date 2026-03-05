package cloud

import (
	"fmt"
	"testing"
)

func TestSearchAllProviders_MergesOffers(t *testing.T) {
	c1 := &MockClient{
		ProviderVal: ProviderVastai,
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return []Offer{{ProviderID: "1", Provider: ProviderVastai, GPUName: "RTX_4090"}}, nil
		},
	}
	c2 := &MockClient{
		ProviderVal: ProviderRunpod,
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return []Offer{{ProviderID: "a", Provider: ProviderRunpod, GPUName: "RTX_4090"}}, nil
		},
	}

	offers, err := SearchAllProviders([]Client{c1, c2}, OfferConstraints{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(offers) != 2 {
		t.Fatalf("expected 2 offers, got %d", len(offers))
	}
	if offers[0].Provider != ProviderVastai || offers[1].Provider != ProviderRunpod {
		t.Errorf("unexpected providers: %v, %v", offers[0].Provider, offers[1].Provider)
	}
}

func TestSearchAllProviders_PartialFailure(t *testing.T) {
	c1 := &MockClient{
		ProviderVal: ProviderVastai,
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return nil, fmt.Errorf("vastai down")
		},
	}
	c2 := &MockClient{
		ProviderVal: ProviderRunpod,
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return []Offer{{ProviderID: "a", Provider: ProviderRunpod}}, nil
		},
	}

	offers, err := SearchAllProviders([]Client{c1, c2}, OfferConstraints{})
	if err != nil {
		t.Fatalf("unexpected error on partial failure: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(offers))
	}
}

func TestSearchAllProviders_AllFail(t *testing.T) {
	c1 := &MockClient{
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return nil, fmt.Errorf("fail1")
		},
	}
	c2 := &MockClient{
		SearchOffersFunc: func(_ OfferConstraints) ([]Offer, error) {
			return nil, fmt.Errorf("fail2")
		},
	}

	_, err := SearchAllProviders([]Client{c1, c2}, OfferConstraints{})
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
}
