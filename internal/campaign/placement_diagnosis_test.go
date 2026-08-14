package campaign

import (
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestDiagnosePlacementPreservesGPUIdentityAndFindsRAMCounterfactual(t *testing.T) {
	var mu sync.Mutex
	var searchedClasses []string
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			mu.Lock()
			searchedClasses = append(searchedClasses, constraints.GPUClass)
			mu.Unlock()
			if constraints.MinHostRAMGB > 32 {
				return nil, nil
			}
			return []cloud.Offer{{
				ProviderID: "l4-low-ram", Provider: cloud.ProviderVastai,
				GPUName: "L4", GPUMemGB: 24, RAMGB: 32, CostPerHour: 0.40,
			}}, nil
		},
	}
	group := InstanceGroup{
		GPUClass: "L4", GPUMemGB: 20, CPUMemGB: 64,
		Jobs: []*db.Job{{ID: 1, GPUClass: "L4"}},
	}

	diagnosis := DiagnosePlacement([]cloud.Client{client}, group, nil, PlacementDiagnosisOptions{})
	if diagnosis.Exact.Offer != nil || diagnosis.Exact.RawCount != 0 {
		t.Fatalf("exact probe = %+v, want empty", diagnosis.Exact)
	}
	if len(diagnosis.Counterfactuals) != 1 {
		t.Fatalf("counterfactuals = %+v, want only host RAM", diagnosis.Counterfactuals)
	}
	if got := diagnosis.Counterfactuals[0]; got.Constraint != "host RAM floor" || got.Result.Offer == nil {
		t.Fatalf("counterfactual = %+v, want viable host RAM relaxation", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, gpuClass := range searchedClasses {
		if gpuClass != "L4" {
			t.Fatalf("counterfactual widened GPU class to %q; searches = %v", gpuClass, searchedClasses)
		}
	}
}

func TestDiagnosePlacementSeparatesRateSurvivalTradeoff(t *testing.T) {
	low := cloud.Offer{
		ProviderID: "cheap-fragile", Provider: cloud.ProviderVastai,
		GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 0.40,
		Reliability: 0.5, MachineID: "fragile",
	}
	high := cloud.Offer{
		ProviderID: "costly-reliable", Provider: cloud.ProviderVastai,
		GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 1.00,
		Reliability: 0.99, MachineID: "reliable",
	}
	now := time.Now()
	var outcomes []bidding.InstanceOutcome
	for range 40 {
		outcomes = append(outcomes,
			bidding.InstanceOutcome{
				Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonInfraFailure,
				ResolvedGPUName: low.GPUName, GPUMemGB: 80, CostPerHourCents: 40,
				Reliability: low.Reliability, MachineID: low.MachineID, EndedAtUnix: now.Unix(),
			},
			bidding.InstanceOutcome{
				Provider: cloud.ProviderVastai, TerminationReason: db.TerminationReasonCompleted,
				ResolvedGPUName: high.GPUName, GPUMemGB: 80, CostPerHourCents: 100,
				Reliability: high.Reliability, MachineID: high.MachineID, EndedAtUnix: now.Unix(),
			},
		)
	}
	model := bidding.BuildSurvivalModelAt(outcomes, now)
	minSurvival := (model.OfferSurvival(low) + model.OfferSurvival(high)) / 2
	capCents := 50
	group := InstanceGroup{
		GPUClass: "A100", GPUMemGB: 80,
		Jobs: []*db.Job{{
			ID: 1, GPUClass: "A100",
			CLIResourceOverrides: &db.CLIResourceOverrides{MaxHourlyRateCents: &capCents},
		}},
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{low, high}, nil
		},
	}

	diagnosis := DiagnosePlacement(
		[]cloud.Client{client}, group, model,
		PlacementDiagnosisOptions{MinSurvival: minSurvival},
	)
	if diagnosis.Exact.Offer != nil {
		t.Fatalf("exact offer = %+v, want rate/survival tradeoff to reject both", diagnosis.Exact.Offer)
	}
	got := map[string]string{}
	for _, counterfactual := range diagnosis.Counterfactuals {
		if counterfactual.Result.Offer != nil {
			got[counterfactual.Constraint] = counterfactual.Result.Offer.ProviderID
		}
	}
	if got["hourly-rate cap"] != high.ProviderID {
		t.Fatalf("hourly-rate counterfactual = %q, want %q; all = %+v", got["hourly-rate cap"], high.ProviderID, diagnosis.Counterfactuals)
	}
	if got["survival floor"] != low.ProviderID {
		t.Fatalf("survival counterfactual = %q, want %q; all = %+v", got["survival floor"], low.ProviderID, diagnosis.Counterfactuals)
	}
}
