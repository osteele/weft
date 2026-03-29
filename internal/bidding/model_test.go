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

func TestBuildSurvivalModel_HalfFailed(t *testing.T) {
	var outcomes []InstanceOutcome
	for i := range 20 {
		reason := "completed"
		if i%2 == 0 {
			reason = "provider_failure"
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
		t.Errorf("expected moderate survival for 50%% failed data, got %.3f", surv)
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

	idx, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyCheap)
	if idx != 1 || best.ProviderID != "b" {
		t.Errorf("nil model should pick cheapest, got idx=%d id=%s", idx, best.ProviderID)
	}
}

func TestBestOffer_PrefersReliableOverCheap(t *testing.T) {
	// Build a model where cheap offers have high failure rate
	var outcomes []InstanceOutcome
	// 20 cheap instances: mostly failed
	for i := range 20 {
		reason := "provider_failure"
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
			reason = "provider_failure"
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

	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyCheap)
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
	// Sparse group with 1 failure should not completely override global rate
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
	// With 1 observation (failed) but strong global rate (90%), should shrink toward global
	if surv < 0.5 {
		t.Errorf("expected shrinkage toward global rate, but survival was too low: %.3f", surv)
	}
}

func TestExpectedWallclockTime(t *testing.T) {
	// Perfect survival: wallclock = job + setup
	wt := ExpectedWallclockTime(2.0, 0.5, 1.0)
	if math.Abs(wt-2.5) > 1e-9 {
		t.Errorf("expected 2.5 for perfect survival, got %.3f", wt)
	}

	// Zero survival: infinite
	wt = ExpectedWallclockTime(2.0, 0.5, 0.0)
	if !math.IsInf(wt, 1) {
		t.Errorf("expected +Inf for zero survival, got %.3f", wt)
	}

	// 50% survival: E[total] = (2+0.5)/0.5 = 5.0
	wt = ExpectedWallclockTime(2.0, 0.5, 0.5)
	if math.Abs(wt-5.0) > 1e-9 {
		t.Errorf("expected 5.0 for 50%% survival, got %.3f", wt)
	}
}

func TestBestOffer_FastStrategy_PrefersHighDLPerf(t *testing.T) {
	model := &SurvivalModel{
		GlobalSurvived:   18,
		GlobalTotal:      20,
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	offers := []cloud.Offer{
		{ProviderID: "cheap-slow", GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0, Reliability: 0.95},
		{ProviderID: "expensive-fast", GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0, Reliability: 0.95},
	}

	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFast)
	if best.ProviderID != "expensive-fast" {
		t.Errorf("fast strategy should prefer high DLPerf, got %s", best.ProviderID)
	}

	// Cost strategy should prefer the cheap one
	_, bestCost := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyCheap)
	if bestCost.ProviderID != "cheap-slow" {
		t.Errorf("cost strategy should prefer cheap offer, got %s", bestCost.ProviderID)
	}
}

func TestBestOffer_FastStrategy_NilModel(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "low-perf", GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0},
		{ProviderID: "high-perf", GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0},
	}

	_, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFast)
	if best.ProviderID != "high-perf" {
		t.Errorf("fast strategy with nil model should pick highest DLPerf, got %s", best.ProviderID)
	}
}

func TestBestOffer_FastestStrategy_IgnoresSurvivalModel(t *testing.T) {
	model := &SurvivalModel{
		GlobalSurvived:   18,
		GlobalTotal:      20,
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	offers := []cloud.Offer{
		{ProviderID: "reliable-slow", GPUName: "RTX 4090", CostPerHour: 0.80, DLPerf: 5.0, Reliability: 0.99},
		{ProviderID: "risky-fast", GPUName: "A100", CostPerHour: 0.30, DLPerf: 20.0, Reliability: 0.50},
	}

	// Fastest should pick highest DLPerf (happy-path time, no survival adjustment)
	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFastest)
	if best.ProviderID != "risky-fast" {
		t.Errorf("fastest strategy should pick highest DLPerf, got %s", best.ProviderID)
	}

	// Fast strategy with same model might prefer the reliable one
	_, bestFast := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFast)
	// Just verify fastest and fast can differ (fastest ignores survival)
	_ = bestFast
}

func TestMachinePenalty_HighFailureRate(t *testing.T) {
	model := &SurvivalModel{
		GlobalSurvived: 18,
		GlobalTotal:    20,
		Groups:         make(map[string]*SurvivalStats),
		MachineStats: map[string]*SurvivalStats{
			"bad-machine":  {Survived: 1, Total: 5}, // 20% survival
			"good-machine": {Survived: 5, Total: 5}, // 100% survival
			"new-machine":  {Survived: 1, Total: 2}, // insufficient data
		},
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	badPenalty := model.MachinePenalty("bad-machine")
	goodPenalty := model.MachinePenalty("good-machine")
	newPenalty := model.MachinePenalty("new-machine")
	unknownPenalty := model.MachinePenalty("unknown")

	if badPenalty >= goodPenalty {
		t.Errorf("bad machine penalty (%.3f) should be less than good machine penalty (%.3f)", badPenalty, goodPenalty)
	}
	if goodPenalty != 1.0 {
		t.Errorf("good machine penalty should be capped at 1.0, got %.3f", goodPenalty)
	}
	if newPenalty != 1.0 {
		t.Errorf("new machine (insufficient data) penalty should be 1.0, got %.3f", newPenalty)
	}
	if unknownPenalty != 1.0 {
		t.Errorf("unknown machine penalty should be 1.0, got %.3f", unknownPenalty)
	}
	if badPenalty < 0.1 || badPenalty > 0.5 {
		t.Errorf("bad machine (20%% survival vs 90%% global) should have penalty ~0.22, got %.3f", badPenalty)
	}
}

func TestOfferSurvival_IncorporatesMachinePenalty(t *testing.T) {
	model := &SurvivalModel{
		GlobalSurvived: 18,
		GlobalTotal:    20,
		Groups:         make(map[string]*SurvivalStats),
		MachineStats: map[string]*SurvivalStats{
			"bad-machine": {Survived: 1, Total: 5},
		},
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	normalOffer := cloud.Offer{GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.95}
	badMachineOffer := cloud.Offer{GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.95, MachineID: "bad-machine"}

	normalSurv := model.OfferSurvival(normalOffer)
	badSurv := model.OfferSurvival(badMachineOffer)

	if badSurv >= normalSurv {
		t.Errorf("bad machine offer survival (%.3f) should be less than normal (%.3f)", badSurv, normalSurv)
	}
}

func TestBestOffer_FastestStrategy_NilModel(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "low-perf", GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0},
		{ProviderID: "high-perf", GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0},
	}

	_, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFastest)
	if best.ProviderID != "high-perf" {
		t.Errorf("fastest strategy with nil model should pick highest DLPerf, got %s", best.ProviderID)
	}
}

func TestFilterOffersBySurvival_NilModel(t *testing.T) {
	offers := []cloud.Offer{{ProviderID: "a", GPUName: "RTX 4090"}}
	passed, rejected := FilterOffersBySurvival(nil, offers, 0.5)
	if len(passed) != 1 || len(rejected) != 0 {
		t.Errorf("nil model should pass all offers through")
	}
}

func TestFilterOffersBySurvival_DisabledByZero(t *testing.T) {
	model := &SurvivalModel{
		GlobalSurvived:   1,
		GlobalTotal:      10,
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}
	offers := []cloud.Offer{{ProviderID: "a", GPUName: "RTX 4090", Reliability: 0.5}}
	passed, rejected := FilterOffersBySurvival(model, offers, 0)
	if len(passed) != 1 || len(rejected) != 0 {
		t.Errorf("minSurvival=0 should pass all offers through")
	}
}

func TestFilterOffersBySurvival_FiltersLowSurvival(t *testing.T) {
	// Build model where RTX 2080 Ti has terrible survival
	var outcomes []InstanceOutcome
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: "infra_failure",
			CostPerHourCents:  7,
			ResolvedGPUName:   "RTX 2080 Ti",
			Reliability:       0.8,
		})
	}
	for range 2 {
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: "completed",
			CostPerHourCents:  7,
			ResolvedGPUName:   "RTX 2080 Ti",
			Reliability:       0.8,
		})
	}
	// RTX 4090 has good survival
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: "completed",
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.95,
		})
	}

	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "bad", GPUName: "RTX 2080 Ti", CostPerHour: 0.07, Reliability: 0.8},
		{ProviderID: "good", GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.95},
	}

	passed, rejected := FilterOffersBySurvival(model, offers, 0.5)

	if len(passed) != 1 || passed[0].ProviderID != "good" {
		t.Errorf("expected only good offer to pass, got %d offers", len(passed))
	}
	if len(rejected) != 1 || rejected[0].GPUFamily != "RTX_2080_Ti" {
		t.Errorf("expected RTX_2080_Ti rejected, got %v", rejected)
	}
	if rejected[0].Count != 1 {
		t.Errorf("expected 1 rejected offer, got %d", rejected[0].Count)
	}
}

func TestFilterOffersBySurvival_AllRejected(t *testing.T) {
	// Model where everything has low survival
	var outcomes []InstanceOutcome
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			TerminationReason: "infra_failure",
			CostPerHourCents:  50,
			ResolvedGPUName:   "RTX 3060",
			Reliability:       0.5,
		})
	}
	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "a", GPUName: "RTX 3060", CostPerHour: 0.50, Reliability: 0.5},
	}

	passed, rejected := FilterOffersBySurvival(model, offers, 0.8)
	if len(passed) != 0 {
		t.Errorf("expected all offers rejected, got %d passed", len(passed))
	}
	if len(rejected) != 1 {
		t.Errorf("expected 1 rejected group, got %d", len(rejected))
	}
}

func TestFilterOffersBySurvival_EmptyOffers(t *testing.T) {
	model := &SurvivalModel{
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}
	passed, rejected := FilterOffersBySurvival(model, nil, 0.5)
	if len(passed) != 0 || len(rejected) != 0 {
		t.Errorf("empty offers should return empty results")
	}
}
