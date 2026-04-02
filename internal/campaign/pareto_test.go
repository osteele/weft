package campaign

import (
	"testing"
	"time"

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
