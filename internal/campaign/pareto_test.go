package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
)

func TestBuildParetoTradeoffOptions_LabelsFrontierEndpointsAndMiddle(t *testing.T) {
	plans := map[string]StrategyPlan{
		"cheap-profile":   tradeoffTestPlan(8*time.Hour, 1.0),
		"fast-profile":    tradeoffTestPlan(4*time.Hour, 2.0),
		"fastest-profile": tradeoffTestPlan(2*time.Hour, 4.0),
		"dominated":       tradeoffTestPlan(6*time.Hour, 3.0),
	}

	options := BuildParetoTradeoffOptions(plans)
	if len(options) != 3 {
		t.Fatalf("expected 3 frontier options, got %d", len(options))
	}
	if options[0].ID != "cheap-profile" || options[0].Label != "cheap" {
		t.Fatalf("first option = %#v, want cheap-profile/cheap", options[0])
	}
	if options[1].ID != "fast-profile" || options[1].Label != "fast" {
		t.Fatalf("middle option = %#v, want fast-profile/fast", options[1])
	}
	if options[2].ID != "fastest-profile" || options[2].Label != "fastest" {
		t.Fatalf("last option = %#v, want fastest-profile/fastest", options[2])
	}
}

func TestBuildParetoTradeoffOptions_LabelsTwoPointFrontier(t *testing.T) {
	plans := map[string]StrategyPlan{
		"cheap-profile": tradeoffTestPlan(8*time.Hour, 1.0),
		"dominated":     tradeoffTestPlan(9*time.Hour, 2.0),
		"fast-profile":  tradeoffTestPlan(2*time.Hour, 4.0),
	}

	options := BuildParetoTradeoffOptions(plans)
	if len(options) != 2 {
		t.Fatalf("expected 2 frontier options, got %d", len(options))
	}
	if options[0].Label != "cheap" || options[1].Label != "fast" {
		t.Fatalf("labels = %#v, want cheap/fast", options)
	}
}

func TestBuildParetoTradeoffOptions_DedupesEquivalentPoints(t *testing.T) {
	plans := map[string]StrategyPlan{
		"a": tradeoffTestPlan(5*time.Hour, 2.0),
		"b": tradeoffTestPlan(5*time.Hour, 2.0),
	}

	options := BuildParetoTradeoffOptions(plans)
	if len(options) != 1 {
		t.Fatalf("expected 1 deduped option, got %d", len(options))
	}
	if options[0].Label != "cheap/fast/fastest" {
		t.Fatalf("label = %q, want cheap/fast/fastest", options[0].Label)
	}
}

func TestBuildParetoTradeoffOptions_PreservesCanonicalStrategyAnchors(t *testing.T) {
	plans := map[string]StrategyPlan{
		bidding.StrategyCheap.Profile().ID:   tradeoffTestPlan(8*time.Hour, 1.0),
		bidding.StrategyFast.Profile().ID:    tradeoffTestPlan(9*time.Hour, 1.1), // dominated by cheap
		bidding.StrategyFastest.Profile().ID: tradeoffTestPlan(2*time.Hour, 4.0),
	}

	options := BuildParetoTradeoffOptions(plans)
	if len(options) != 3 {
		t.Fatalf("expected 3 options (including fast anchor), got %d: %#v", len(options), options)
	}
	labelsByID := map[string]string{}
	for _, option := range options {
		labelsByID[option.ID] = option.Label
	}
	if labelsByID[bidding.StrategyCheap.Profile().ID] != "cheap" {
		t.Fatalf("cheap label = %q, want cheap", labelsByID[bidding.StrategyCheap.Profile().ID])
	}
	if labelsByID[bidding.StrategyFast.Profile().ID] != "fast" {
		t.Fatalf("fast label = %q, want fast", labelsByID[bidding.StrategyFast.Profile().ID])
	}
	if labelsByID[bidding.StrategyFastest.Profile().ID] != "fastest" {
		t.Fatalf("fastest label = %q, want fastest", labelsByID[bidding.StrategyFastest.Profile().ID])
	}
}

func tradeoffTestPlan(total time.Duration, cost float64) StrategyPlan {
	return StrategyPlan{
		ActualEstimates: []CostEstimate{{
			Group:     InstanceGroup{},
			Offer:     GroupOffer{Offer: &cloud.Offer{ProviderID: "o", CostPerHour: 1.0}},
			Breakdown: estimate.Breakdown{Total: estimate.Constant(total)},
			TotalTime: total,
			TotalCost: cost,
		}},
	}
}
