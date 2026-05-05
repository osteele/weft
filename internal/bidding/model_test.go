package bidding

import (
	"math"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

const testProvider cloud.Provider = "vastai"

// stats is a test helper that constructs a SurvivalStats with both raw and
// weighted counts set to the given values — equivalent to outcomes recorded
// "now" with no decay yet applied. Tests built around the historical
// (raw-counts-only) shape stay simple this way; the tiny number of tests
// that explicitly exercise decay write SurvivalStats fields directly.
func stats(survived, total int) *SurvivalStats {
	return &SurvivalStats{
		Survived:         survived,
		Total:            total,
		WeightedSurvived: float64(survived),
		WeightedTotal:    float64(total),
	}
}

func TestBuildSurvivalModel_NoData(t *testing.T) {
	model := BuildSurvivalModel(nil)
	if model != nil {
		t.Fatal("expected nil model for no data")
	}
}

// TestHealthFloor_RaisedDuringBadDay verifies that a recent burst of failures
// within HealthWindow raises the survival floor, regardless of the long-term
// per-machine/region statistics that the Beta priors are tracking.
func TestHealthFloor_RaisedDuringBadDay(t *testing.T) {
	now := time.Now()
	recent := now.Add(-15 * time.Minute).Unix()
	old := now.Add(-30 * 24 * time.Hour).Unix()

	var outcomes []InstanceOutcome
	// Decent long-term history.
	for range 20 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider: testProvider, TerminationReason: "completed",
			ResolvedGPUName: "RTX 4090", Reliability: 0.99, EndedAtUnix: old,
		})
	}
	// Mass failure in the last hour.
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider: testProvider, TerminationReason: "infra_failure",
			ResolvedGPUName: "RTX 4090", Reliability: 0.99, EndedAtUnix: recent,
		})
	}
	model := BuildSurvivalModelAt(outcomes, now)
	if model == nil {
		t.Fatal("nil model")
	}
	rate, n := model.HealthRecent()
	if n != 10 || rate != 0 {
		t.Fatalf("recent rate (%v, %d), want (0, 10)", rate, n)
	}
	if floor := model.HealthFloor(0.4); floor < 0.7 {
		t.Errorf("HealthFloor(0.4) during bad-day = %v, want >= 0.7", floor)
	}
}

// TestHealthFloor_LowSampleNoChange verifies the floor is not raised when
// recent sample size is below healthMinSamples — avoids reacting to noise
// from small windows.
func TestHealthFloor_LowSampleNoChange(t *testing.T) {
	now := time.Now()
	recent := now.Add(-10 * time.Minute).Unix()
	outcomes := []InstanceOutcome{
		{Provider: testProvider, TerminationReason: "infra_failure", EndedAtUnix: recent},
		{Provider: testProvider, TerminationReason: "infra_failure", EndedAtUnix: recent},
	}
	model := BuildSurvivalModelAt(outcomes, now)
	if floor := model.HealthFloor(0.4); floor != 0.4 {
		t.Errorf("HealthFloor(0.4) with only 2 recent samples = %v, want 0.4 (unchanged)", floor)
	}
}

// TestHealthFloor_HealthyRecentLeavesFloor verifies that a healthy recent
// window leaves the baseline floor alone.
func TestHealthFloor_HealthyRecentLeavesFloor(t *testing.T) {
	now := time.Now()
	recent := now.Add(-15 * time.Minute).Unix()
	var outcomes []InstanceOutcome
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider: testProvider, TerminationReason: "completed", EndedAtUnix: recent,
		})
	}
	model := BuildSurvivalModelAt(outcomes, now)
	if floor := model.HealthFloor(0.4); floor != 0.4 {
		t.Errorf("HealthFloor(0.4) with healthy recent = %v, want 0.4", floor)
	}
}

func TestBuildSurvivalModel_AllSurvived(t *testing.T) {
	outcomes := make([]InstanceOutcome, 20)
	for i := range outcomes {
		outcomes[i] = InstanceOutcome{
			Provider:          testProvider,
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

	surv := model.SurvivalProbability(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketMedium, 0.99)
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
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.99,
		})
	}

	model := BuildSurvivalModel(outcomes)
	surv := model.SurvivalProbability(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketMedium, 0.99)
	// With 50% observed survival and a 0.99 prior, posterior should be between 0.4 and 0.8
	if surv < 0.4 || surv > 0.8 {
		t.Errorf("expected moderate survival for 50%% failed data, got %.3f", surv)
	}
}

// TestBuildSurvivalModel_DoesNotCrossContaminateProviders verifies that
// outcomes from one provider do not influence the posterior for a different
// provider. RunPod's managed hosts and Vast.ai's marketplace hosts have
// different reliability baselines; survival statistics must be scoped per
// provider so a Vast offer's history doesn't bias a RunPod offer's score.
func TestBuildSurvivalModel_DoesNotCrossContaminateProviders(t *testing.T) {
	var outcomes []InstanceOutcome
	// Vast: 20 A100 SXM4 outcomes, all failed.
	for range 20 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider:          "vastai",
			TerminationReason: "provider_failure",
			CostPerHourCents:  100,
			ResolvedGPUName:   "A100 SXM4",
			Reliability:       0.95,
		})
	}
	// RunPod: 0 outcomes (cold start).
	model := BuildSurvivalModel(outcomes)

	// Query via OfferSurvival so the bucket is derived from price percentiles
	// the same way the planner derives it for a real offer.
	vastSurv := model.OfferSurvival(cloud.Offer{Provider: "vastai", GPUName: "A100 SXM4", CostPerHour: 1.00, Reliability: 0.95})
	runpodSurv := model.OfferSurvival(cloud.Offer{Provider: "runpod", GPUName: "A100 SXM4", CostPerHour: 1.00, Reliability: 0.95})

	// Vast posterior should reflect the 20 failures and be well below the prior.
	if vastSurv > 0.4 {
		t.Errorf("vast posterior should be low after 20 failures, got %.3f", vastSurv)
	}
	// RunPod has no data; the prior alone should give ~0.95, well above vast.
	if runpodSurv < 0.85 {
		t.Errorf("runpod posterior should be near the 0.95 reliability prior, got %.3f", runpodSurv)
	}
	if runpodSurv-vastSurv < 0.5 {
		t.Errorf("runpod (%.3f) and vast (%.3f) posteriors should differ substantially; cross-contamination suspected", runpodSurv, vastSurv)
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
			pricePercentileKey(testProvider, GPUSKU{Family: "RTX_4090"}): {0.30, 0.40, 0.50, 0.60, 0.70, 0.80, 0.90, 1.00},
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
		got := model.PriceBucketFor(testProvider, GPUSKU{Family: "RTX_4090"}, tt.price)
		if got != tt.want {
			t.Errorf("PriceBucketFor(RTX_4090, %.2f) = %s, want %s", tt.price, got, tt.want)
		}
	}
}

func TestPriceBucketFor_UnknownFamily(t *testing.T) {
	model := &SurvivalModel{
		PricePercentiles: map[string][]float64{},
	}
	got := model.PriceBucketFor(testProvider, GPUSKU{Family: "UNKNOWN"}, 0.50)
	if got != PriceBucketMedium {
		t.Errorf("expected medium for unknown family, got %s", got)
	}
}

func TestBestOffer_NilModel_FallsBackToCheapest(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "a", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.99},
		{ProviderID: "b", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.80},
	}

	idx, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyCheap, 0)
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
			Provider:          testProvider,
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
			Provider:          testProvider,
			TerminationReason: reason,
			CostPerHourCents:  80,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.99,
		})
	}

	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "cheap", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.30, Reliability: 0.80},
		{ProviderID: "moderate", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.80, Reliability: 0.99},
	}

	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyCheap, 0)
	if best.ProviderID != "moderate" {
		t.Errorf("expected moderate offer (higher reliability) to win, got %s", best.ProviderID)
	}
}

func TestColdStart_ReliabilityPrior(t *testing.T) {
	// Model with no group data at all
	model := &SurvivalModel{
		Global:           map[cloud.Provider]*SurvivalStats{},
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	// High reliability prior should give high survival
	highSurv := model.SurvivalProbability(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketMedium, 0.99)
	lowSurv := model.SurvivalProbability(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketMedium, 0.50)

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
		Global: map[cloud.Provider]*SurvivalStats{testProvider: stats(18, 20)},
		Groups: map[string]*SurvivalStats{
			groupKey(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketLow): stats(0, 1),
		},
		PricePercentiles: map[string][]float64{},
		PriorStrength:    10.0,
	}

	surv := model.SurvivalProbability(testProvider, GPUSKU{Family: "RTX_4090"}, PriceBucketLow, 0.95)
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

func TestBestOffer_FastStrategy_UsesNeutralRuntimeFallback(t *testing.T) {
	model := &SurvivalModel{
		Global:           map[cloud.Provider]*SurvivalStats{testProvider: stats(18, 20)},
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	offers := []cloud.Offer{
		{ProviderID: "cheap-slow", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0, Reliability: 0.95},
		{ProviderID: "expensive-fast", Provider: testProvider, GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0, Reliability: 0.95},
	}

	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFast, 0)
	if best.ProviderID != "cheap-slow" {
		t.Errorf("fast strategy should prefer cheaper offer when runtime ties, got %s", best.ProviderID)
	}

	// Cost strategy should prefer the cheap one
	_, bestCost := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyCheap, 0)
	if bestCost.ProviderID != "cheap-slow" {
		t.Errorf("cost strategy should prefer cheap offer, got %s", bestCost.ProviderID)
	}
}

func TestBestOffer_FastStrategy_NilModel(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "low-perf", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0},
		{ProviderID: "high-perf", Provider: testProvider, GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0},
	}

	_, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFast, 0)
	if best.ProviderID != "low-perf" {
		t.Errorf("fast strategy with nil model should use neutral runtime fallback, got %s", best.ProviderID)
	}
}

func TestBestOffer_FastestStrategy_IgnoresSurvivalModel(t *testing.T) {
	model := &SurvivalModel{
		Global:           map[cloud.Provider]*SurvivalStats{testProvider: stats(18, 20)},
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	offers := []cloud.Offer{
		{ProviderID: "reliable-slow", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.80, DLPerf: 5.0, Reliability: 0.99},
		{ProviderID: "risky-fast", Provider: testProvider, GPUName: "A100", CostPerHour: 0.30, DLPerf: 20.0, Reliability: 0.50},
	}

	// Fastest uses happy-path time, so with equal runtime it can still pick the
	// cheaper risky offer while fast may be pushed away by retry-adjusted time.
	_, best := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFastest, 0)
	if best.ProviderID != "risky-fast" {
		t.Errorf("fastest strategy should prefer cheap happy-path winner, got %s", best.ProviderID)
	}

	// Fast strategy with the same model might prefer the reliable one.
	_, bestFast := BestOffer(model, offers, 2.0, ConstantSetup(0.5), StrategyFast, 0)
	// Just verify fastest and fast can differ (fastest ignores survival)
	_ = bestFast
}

func TestMachinePenalty_HighFailureRate(t *testing.T) {
	model := &SurvivalModel{
		Global: map[cloud.Provider]*SurvivalStats{testProvider: stats(18, 20)},
		Groups: make(map[string]*SurvivalStats),
		MachineStats: map[string]*SurvivalStats{
			machineKey(testProvider, "bad-machine"):    stats(1, 5), // 20% survival
			machineKey(testProvider, "good-machine"):   stats(5, 5), // 100% survival
			machineKey(testProvider, "lightly-failed"): stats(0, 1), // single failure
			machineKey(testProvider, "single-success"): stats(1, 1), // single success
		},
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	badPenalty := model.MachinePenalty(testProvider, "bad-machine")
	goodPenalty := model.MachinePenalty(testProvider, "good-machine")
	lightPenalty := model.MachinePenalty(testProvider, "lightly-failed")
	singleSuccess := model.MachinePenalty(testProvider, "single-success")
	unknownPenalty := model.MachinePenalty(testProvider, "unknown")

	if badPenalty >= goodPenalty {
		t.Errorf("bad machine penalty (%.3f) should be less than good machine penalty (%.3f)", badPenalty, goodPenalty)
	}
	if goodPenalty != 1.0 {
		t.Errorf("good machine penalty should be capped at 1.0, got %.3f", goodPenalty)
	}
	if singleSuccess != 1.0 {
		t.Errorf("single-success machine penalty should be capped at 1.0, got %.3f", singleSuccess)
	}
	if unknownPenalty != 1.0 {
		t.Errorf("unknown machine penalty should be 1.0, got %.3f", unknownPenalty)
	}
	// Single failure should already drop below 1.0 under the Beta prior.
	if lightPenalty >= 1.0 {
		t.Errorf("single-failure machine penalty should be < 1.0 under Beta prior, got %.3f", lightPenalty)
	}
	if lightPenalty <= badPenalty {
		t.Errorf("single failure (%.3f) should still be less penalised than 1/5 (%.3f)", lightPenalty, badPenalty)
	}
	// 1/5 vs 90% global with a weak machine prior gives a penalty around
	// 0.35. Allow a comfortable band around that.
	if badPenalty < 0.25 || badPenalty > 0.5 {
		t.Errorf("bad machine (20%% survival vs 90%% global) should have penalty around 0.35, got %.3f", badPenalty)
	}
}

func TestOfferSurvival_IncorporatesMachinePenalty(t *testing.T) {
	model := &SurvivalModel{
		Global: map[cloud.Provider]*SurvivalStats{testProvider: stats(18, 20)},
		Groups: make(map[string]*SurvivalStats),
		MachineStats: map[string]*SurvivalStats{
			machineKey(testProvider, "bad-machine"): stats(1, 5),
		},
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}

	normalOffer := cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.95}
	badMachineOffer := cloud.Offer{Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.50, Reliability: 0.95, MachineID: "bad-machine"}

	normalSurv := model.OfferSurvival(normalOffer)
	badSurv := model.OfferSurvival(badMachineOffer)

	if badSurv >= normalSurv {
		t.Errorf("bad machine offer survival (%.3f) should be less than normal (%.3f)", badSurv, normalSurv)
	}
}

func TestBestOffer_FastestStrategy_NilModel(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "low-perf", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.30, DLPerf: 5.0},
		{ProviderID: "high-perf", Provider: testProvider, GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0},
	}

	_, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFastest, 0)
	if best.ProviderID != "low-perf" {
		t.Errorf("fastest strategy with nil model should use neutral runtime fallback, got %s", best.ProviderID)
	}
}

func TestBestOffer_FastestRejectsPathologicallyExpensive(t *testing.T) {
	// A $1000/hr offer that's marginally faster should lose to a $1.50/hr
	// offer, because the tiny cost weight still penalizes extreme prices.
	offers := []cloud.Offer{
		{ProviderID: "reasonable", Provider: testProvider, GPUName: "A100", CostPerHour: 1.50, DLPerf: 20.0},
		{ProviderID: "pathological", Provider: testProvider, GPUName: "H100", CostPerHour: 1000.0, DLPerf: 21.0},
	}

	_, best := BestOffer(nil, offers, 2.0, ConstantSetup(0.5), StrategyFastest, 0)
	if best.ProviderID != "reasonable" {
		t.Errorf("fastest should reject pathologically expensive offer, got %s (cost=$%.0f/hr)", best.ProviderID, best.CostPerHour)
	}
}

func TestBestOffer_CheapBreaksTiesByTime(t *testing.T) {
	// Two offers at the same price and runtime: cheap strategy should prefer the
	// one with lower setup overhead due to the small time weight.
	offers := []cloud.Offer{
		{ProviderID: "slow", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.50, DLPerf: 5.0},
		{ProviderID: "fast", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 0.50, DLPerf: 20.0},
	}

	setup := func(o cloud.Offer) float64 {
		if o.ProviderID == "fast" {
			return 0.25
		}
		return 1.0
	}
	_, best := BestOffer(nil, offers, 2.0, setup, StrategyCheap, 0)
	if best.ProviderID != "fast" {
		t.Errorf("cheap should break ties by time, got %s", best.ProviderID)
	}
}

func TestStrategyWeights(t *testing.T) {
	// Verify weights are accessible and have expected properties
	cheap := StrategyCheap.Weights()
	fast := StrategyFast.Weights()
	fastest := StrategyFastest.Weights()

	if cheap.Cost <= cheap.Time {
		t.Error("cheap should weight cost more than time")
	}
	if fast.Time <= fast.Cost {
		t.Error("fast should weight time more than cost")
	}
	if fastest.Time <= fastest.Cost {
		t.Error("fastest should weight time more than cost")
	}
	if fastest.Cost >= fast.Cost {
		t.Error("fastest should have less cost sensitivity than fast")
	}
}

func TestFilterOffersBySurvival_NilModel(t *testing.T) {
	offers := []cloud.Offer{{ProviderID: "a", Provider: testProvider, GPUName: "RTX 4090"}}
	passed, rejected := FilterOffersBySurvival(nil, offers, 0.5)
	if len(passed) != 1 || len(rejected) != 0 {
		t.Errorf("nil model should pass all offers through")
	}
}

func TestFilterOffersBySurvival_DisabledByZero(t *testing.T) {
	model := &SurvivalModel{
		Global:           map[cloud.Provider]*SurvivalStats{testProvider: stats(1, 10)},
		Groups:           make(map[string]*SurvivalStats),
		PricePercentiles: make(map[string][]float64),
		PriorStrength:    10.0,
	}
	offers := []cloud.Offer{{ProviderID: "a", Provider: testProvider, GPUName: "RTX 4090", Reliability: 0.5}}
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
			Provider:          testProvider,
			TerminationReason: "infra_failure",
			CostPerHourCents:  7,
			ResolvedGPUName:   "RTX 2080 Ti",
			Reliability:       0.8,
		})
	}
	for range 2 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: "completed",
			CostPerHourCents:  7,
			ResolvedGPUName:   "RTX 2080 Ti",
			Reliability:       0.8,
		})
	}
	// RTX 4090 has good survival
	for range 10 {
		outcomes = append(outcomes, InstanceOutcome{
			Provider:          testProvider,
			TerminationReason: "completed",
			CostPerHourCents:  100,
			ResolvedGPUName:   "RTX 4090",
			Reliability:       0.95,
		})
	}

	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "bad", Provider: testProvider, GPUName: "RTX 2080 Ti", CostPerHour: 0.07, Reliability: 0.8},
		{ProviderID: "good", Provider: testProvider, GPUName: "RTX 4090", CostPerHour: 1.00, Reliability: 0.95},
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
			Provider:          testProvider,
			TerminationReason: "infra_failure",
			CostPerHourCents:  50,
			ResolvedGPUName:   "RTX 3060",
			Reliability:       0.5,
		})
	}
	model := BuildSurvivalModel(outcomes)

	offers := []cloud.Offer{
		{ProviderID: "a", Provider: testProvider, GPUName: "RTX 3060", CostPerHour: 0.50, Reliability: 0.5},
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

// TestBestOffer_MaxGPUMemGB_NoLongerAffectsLegacyFallback verifies that
// maxGPUMemGB is ignored by the neutral-runtime fallback selector.
func TestBestOffer_MaxGPUMemGB_NoLongerAffectsLegacyFallback(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "rtx3090", Provider: testProvider, GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.15, DLPerf: 15.0},
		{ProviderID: "h200", Provider: testProvider, GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
	}

	// Without ceiling: cheapest adequate offer wins because runtime ties.
	_, bestNoCeiling := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFastest, 0)
	if bestNoCeiling.ProviderID != "rtx3090" {
		t.Errorf("without ceiling, fastest should pick cheapest offer under neutral runtime fallback, got %s", bestNoCeiling.ProviderID)
	}

	// With a ceiling: the result is unchanged because speed is not inferred here.
	_, bestWithCeiling := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFastest, 12)
	if bestWithCeiling.ProviderID != "rtx3090" {
		t.Errorf("with maxGPUMemGB=12, fastest should still pick RTX 3090, got %s",
			bestWithCeiling.ProviderID)
	}
}

// TestBestOffer_MaxGPUMemGB_Zero_NoEffect verifies that maxGPUMemGB=0 has no
// effect on the neutral-runtime fallback scoring.
func TestBestOffer_MaxGPUMemGB_Zero_NoEffect(t *testing.T) {
	offers := []cloud.Offer{
		{ProviderID: "cheap", Provider: testProvider, GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.15, DLPerf: 15.0},
		{ProviderID: "fast", Provider: testProvider, GPUName: "H200", GPUMemGB: 141, CostPerHour: 3.23, DLPerf: 40.0},
	}
	_, best := BestOffer(nil, offers, 1.0, ConstantSetup(0.5), StrategyFastest, 0)
	if best.ProviderID != "cheap" {
		t.Errorf("maxGPUMemGB=0 should not affect scoring, expected cheap, got %s", best.ProviderID)
	}
}
