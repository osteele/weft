package bidding

import (
	"math"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestBuildSurvivalModel_NoData(t *testing.T) {
	model := BuildSurvivalModel(nil)
	if model != nil {
		t.Fatal("expected nil model for no data")
	}
}

func TestBuildSurvivalModel_AllSurvived(t *testing.T) {
	outcomes := make([]InstanceOutcome, 20)
	for i := range outcomes {
		outcomes[i] = InstanceOutcome{
			TerminationReason: "completed",
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.99,
		}
	}

	model := BuildSurvivalModel(outcomes)
	if model == nil {
		t.Fatal("expected non-nil model")
	}

	surv := model.SurvivalProbability("RTX_4090", PriceBucketMedium, 0.99)
	if surv < 0.9 {
		t.Errorf("expected high survival for all-survived data, got %.3f", surv)
	}
}

func TestBuildSurvivalModel_HalfPreempted(t *testing.T) {
	var outcomes []InstanceOutcome
	for i := range 20 {
		reason := "completed"
		if i%2 == 0 {
			reason = "preempted"
		}
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: reason,
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.99,
		})
	}

	model := BuildSurvivalModel(outcomes)
	surv := model.SurvivalProbability("RTX_4090", PriceBucketMedium, 0.99)
	// With 50% observed survival and a 0.99 prior, posterior should be between 0.4 and 0.8
	if surv < 0.4 || surv > 0.8 {
		t.Errorf("expected moderate survival for 50%% preempted data, got %.3f", surv)
	}
}

func TestExpectedCost(t *testing.T) {
	// Perfect survival: expected cost = raw cost
	ec := ExpectedCost(1.0, 2.0, 0.5, 1.0)
	if math.Abs(ec-2.0) > 1e-9 {
		t.Errorf("expected 2.0 for perfect survival, got %.3f", ec)
	}

	// Zero survival: infinite
	ec = ExpectedCost(1.0, 2.0, 0.5, 0.0)
	if !math.IsInf(ec, 1) {
		t.Errorf("expected +Inf for zero survival, got %.3f", ec)
	}

	// 50% survival: cost should be roughly 2× raw + retry overhead
	ec = ExpectedCost(1.0, 2.0, 0.5, 0.5)
	// E[cost] = 2.0/0.5 + (0.5/0.5)*0.5*1.0 = 4.0 + 0.5 = 4.5
	if math.Abs(ec-4.5) > 1e-9 {
		t.Errorf("expected 4.5 for 50%% survival, got %.3f", ec)
	}
}

func TestNormalizeGPUFamily(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"GeForce RTX 4090", "RTX_4090"},
		{"NVIDIA A100 80GB PCIe", "A100"},
		{"RTX 3090", "RTX_3090"},
		{"Tesla V100", "V100"},
	}
	for _, tt := range tests {
		got := NormalizeGPUFamily(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeGPUFamily(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPriceBucketFor(t *testing.T) {
	model := &SurvivalModel{
		PricePercentiles: map[string][]float64{
			"RTX_4090": {0.30, 0.40, 0.50, 0.60, 0.70, 0.80, 0.90, 1.00},
		},
	}

	tests := []struct {
		price float64
		want  PriceBucket
	}{
		{0.30, PriceBucketLow},
		{0.50, PriceBucketMedium},
		{0.70, PriceBucketHigh},
		{0.95, PriceBucketPremium},
	}
	for _, tt := range tests {
		got := model.PriceBucketFor("RTX_4090", tt.price)
		if got != tt.want {
			t.Errorf("PriceBucketFor(RTX_4090, %.2f) = %s, want %s", tt.price, got, tt.want)
		}
	}
}

func TestPriceBucketFor_UnknownFamily(t *testing.T) {
	model := &SurvivalModel{
		PricePercentiles: map[string][]float64{},
	}
	got := model.PriceBucketFor("UNKNOWN", 0.50)
	if got != PriceBucketMedium {
		t.Errorf("expected medium for unknown family, got %s", got)
	}
}

func TestBestOffer_NilModel_FallsBackToCheapest(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "a", GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.99},
		{ProviderID: "b", GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.80},
	}

	idx, best := BestOffer(nil, offers, 1.0, 0.5)
	if idx != 1 || best.ProviderID != "b" {
		t.Errorf("nil model should pick cheapest, got idx=%d id=%s", idx, best.ProviderID)
	}
}

func TestBestOffer_PrefersReliableOverCheap(t *testing.T) {
	// Build a model where cheap offers have high preemption
	var outcomes []InstanceOutcome
	// 20 cheap instances: mostly preempted
	for i := range 20 {
		reason := "preempted"
		if i < 2 {
			reason = "completed"
		}
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: reason,
			CostPerHourCents:  30,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.80,
		})
	}
	// 20 moderate instances: mostly survived
	for i := range 20 {
		reason := "completed"
		if i < 2 {
			reason = "preempted"
		}
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: reason,
			CostPerHourCents:  80,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.99,
		})
	}

	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "cheap", GPUName: "RTX 4090", CostPerHour: 0.30, Reliability: 0.80},
		{ProviderID: "moderate", GPUName: "RTX 4090", CostPerHour: 0.80, Reliability: 0.99},
	}

	_, best := BestOffer(model, offers, 2.0, 0.5)
	if best.ProviderID != "moderate" {
		t.Errorf("expected moderate offer (higher reliability) to win, got %s", best.ProviderID)
	}
}

func TestColdStart_ReliabilityPrior(t *testing.T) {
	// Model with no group data at all
	model := &SurvivalModel{
		GlobalSurvived:   0,
		GlobalTotal:      0,
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	// High reliability prior should give high survival
	highSurv := model.SurvivalProbability("RTX_4090", PriceBucketMedium, 0.99)
	lowSurv := model.SurvivalProbability("RTX_4090", PriceBucketMedium, 0.50)

	if highSurv <= lowSurv {
		t.Errorf("expected high reliability prior (%.3f) > low reliability prior (%.3f)", highSurv, lowSurv)
	}
	if highSurv < 0.9 {
		t.Errorf("expected cold start with 0.99 reliability to give ≥0.9 survival, got %.3f", highSurv)
	}
}

func TestHierarchicalShrinkage(t *testing.T) {
	// Sparse group with 1 preemption should not completely override global rate
	model := &SurvivalModel{
		GlobalSurvived: 18,
		GlobalTotal:    20,
		Groups: map[string]*SurvivalStats{
			"RTX_4090:low": {Survived: 0, Total: 1},
		},
		PricePercentiles: map[string][]float64{},
		PriorStrength:    10.0,
	}

	surv := model.SurvivalProbability("RTX_4090", PriceBucketLow, 0.95)
	// With 1 observation (preempted) but strong global rate (90%), should shrink toward global
	if surv < 0.5 {
		t.Errorf("expected shrinkage toward global rate, but survival was too low: %.3f", surv)
	}
}
