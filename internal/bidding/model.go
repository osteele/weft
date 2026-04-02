// Package bidding implements cost-optimal cloud instance selection using a
// Beta-Binomial survival model. It learns price-reliability curves from
// historical campaign data and selects offers that minimize expected cost
// including retry risk from instance failure.
package bidding

import (
	"math"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/cloud"
)

// SelectionStrategy controls how BestOffer ranks offers.
type SelectionStrategy string

const (
	// StrategyCheap minimizes expected dollar cost (including retry risk).
	StrategyCheap SelectionStrategy = "cheap"
	// StrategyFast minimizes expected wall-clock time, weighted by survival
	// probability when retry risk is modeled.
	StrategyFast SelectionStrategy = "fast"
	// StrategyFastest minimizes happy-path wall-clock time with minimal cost
	// sensitivity.
	StrategyFastest SelectionStrategy = "fastest"
)

// StrategyWeights defines the cost/time tradeoff for a strategy.
// Both factors use per-hour units (dollars/hr for cost, hours for time)
// so the weights are directly interpretable as exchange rates.
type StrategyWeights struct {
	Cost float64 // weight for expected cost (dollars)
	Time float64 // weight for expected wall-clock time (hours)
}

// ScoreProfile defines a concrete cost/time tradeoff for ranking offers or plans.
// Profiles with UseHappyPathTime=false treat time as survival-adjusted expected
// completion time; happy-path profiles ignore retry effects in the time term.
type ScoreProfile struct {
	ID               string
	Weights_         StrategyWeights
	UseHappyPathTime bool
}

func (p ScoreProfile) Weights() StrategyWeights {
	return p.Weights_
}

func (p ScoreProfile) Valid() bool {
	return p.ID != "" && (p.Weights_.Cost > 0 || p.Weights_.Time > 0)
}

// Weights returns the scoring weights for this strategy.
func (s SelectionStrategy) Weights() StrategyWeights {
	if w, ok := strategyWeights[s]; ok {
		return w
	}
	return strategyWeights[StrategyCheap]
}

// Profile returns the concrete score profile for this strategy.
func (s SelectionStrategy) Profile() ScoreProfile {
	switch s {
	case StrategyFastest:
		return ScoreProfile{
			ID:               string(s),
			Weights_:         strategyWeights[s],
			UseHappyPathTime: true,
		}
	case StrategyFast:
		return ScoreProfile{
			ID:               string(s),
			Weights_:         strategyWeights[s],
			UseHappyPathTime: false,
		}
	case StrategyCheap:
		fallthrough
	default:
		return ScoreProfile{
			ID:               string(StrategyCheap),
			Weights_:         strategyWeights[StrategyCheap],
			UseHappyPathTime: false,
		}
	}
}

// ParetoSamplingProfiles returns an ordered set of score profiles used to
// approximate the cost/time Pareto frontier in the launch TUI.
func ParetoSamplingProfiles() []ScoreProfile {
	cheap := StrategyCheap.Profile()
	fast := StrategyFast.Profile()
	fastest := StrategyFastest.Profile()

	makeExpected := func(id string, timeWeight float64) ScoreProfile {
		return ScoreProfile{
			ID:               id,
			Weights_:         StrategyWeights{Cost: 1.0, Time: timeWeight},
			UseHappyPathTime: false,
		}
	}

	return []ScoreProfile{
		cheap,
		makeExpected("tradeoff-003", 0.03),
		makeExpected("tradeoff-01", 0.1),
		makeExpected("tradeoff-03", 0.3),
		makeExpected("tradeoff-1", 1.0),
		makeExpected("tradeoff-3", 3.0),
		fast,
		makeExpected("tradeoff-30", 30.0),
		fastest,
	}
}

var strategyWeights = map[SelectionStrategy]StrategyWeights{
	StrategyCheap:   {Cost: 1.0, Time: 0.01},
	StrategyFast:    {Cost: 0.1, Time: 1.0},
	StrategyFastest: {Cost: 0.01, Time: 1.0},
}

// PriceBucket classifies an offer's price relative to its GPU family.
type PriceBucket string

const (
	PriceBucketLow     PriceBucket = "low"
	PriceBucketMedium  PriceBucket = "medium"
	PriceBucketHigh    PriceBucket = "high"
	PriceBucketPremium PriceBucket = "premium"
)

// SurvivalStats tracks survived/total instances for a group.
type SurvivalStats struct {
	Survived int
	Total    int
}

// SurvivalModel holds the Beta-Binomial survival model grouped by
// (GPU family, price bucket), with optional per-machine penalty.
type SurvivalModel struct {
	GlobalSurvived   int
	GlobalTotal      int
	Groups           map[string]*SurvivalStats // key: "GPU_FAMILY:price_bucket"
	MachineStats     map[string]*SurvivalStats // key: machine ID
	PricePercentiles map[string][]float64      // GPU family → sorted prices seen
	PriorStrength    float64                   // pseudo-observations from Vast.ai reliability
}

// groupKey returns the lookup key for a GPU family and price bucket.
func groupKey(gpuFamily string, bucket PriceBucket) string {
	return gpuFamily + ":" + string(bucket)
}

// SurvivalProbability returns the posterior mean survival probability for a
// given GPU family, price bucket, and Vast.ai reliability score.
//
// Uses a hierarchical Beta-Binomial model:
//   - Prior: Beta(alpha0, beta0) from Vast.ai reliability with PriorStrength pseudo-observations
//   - Likelihood: Binomial(survived, total) from the group
//   - When group has < minGroupObs observations, blends toward the global rate
func (m *SurvivalModel) SurvivalProbability(gpuFamily string, bucket PriceBucket, vastaiReliability float64) float64 {
	if vastaiReliability <= 0 {
		vastaiReliability = 0.95
	}

	// Informative prior from Vast.ai reliability
	alpha0 := m.PriorStrength * vastaiReliability
	beta0 := m.PriorStrength * (1 - vastaiReliability)

	key := groupKey(gpuFamily, bucket)
	gs, ok := m.Groups[key]

	if !ok || gs.Total < minGroupObs {
		// Sparse group: blend toward global rate
		globalRate := 0.95 // default if no global data
		if m.GlobalTotal > 0 {
			globalRate = float64(m.GlobalSurvived) / float64(m.GlobalTotal)
		}

		if !ok || gs == nil || gs.Total == 0 {
			// No group data: use prior blended with global
			alpha := alpha0 + m.PriorStrength*globalRate
			beta := beta0 + m.PriorStrength*(1-globalRate)
			return alpha / (alpha + beta)
		}

		// Some group data but sparse: shrink toward global
		shrinkage := float64(gs.Total) / float64(minGroupObs)
		groupRate := float64(gs.Survived) / float64(gs.Total)
		blended := shrinkage*groupRate + (1-shrinkage)*globalRate

		alpha := alpha0 + m.PriorStrength*blended
		beta := beta0 + m.PriorStrength*(1-blended)
		return alpha / (alpha + beta)
	}

	// Sufficient group data: standard Beta posterior
	alpha := alpha0 + float64(gs.Survived)
	beta := beta0 + float64(gs.Total-gs.Survived)
	return alpha / (alpha + beta)
}

const minGroupObs = 5

// ExpectedCost computes the expected cost of running a job accounting for
// instance failure retries. Uses a geometric retry model:
//
//	E[cost] = (job_hrs * $/hr) / p + (1-p)/p * setup_hrs * $/hr
//
// where p = survival probability.
func ExpectedCost(pricePerHour, jobDurationHrs, setupOverheadHrs, survivalProb float64) float64 {
	if survivalProb <= 0 {
		return math.Inf(1)
	}
	if survivalProb >= 1 {
		return jobDurationHrs * pricePerHour
	}
	jobCost := jobDurationHrs * pricePerHour
	retryCost := (1 - survivalProb) / survivalProb * setupOverheadHrs * pricePerHour
	return jobCost/survivalProb + retryCost
}

// PriceBucketFor classifies a price relative to historical percentiles for the GPU family.
func (m *SurvivalModel) PriceBucketFor(gpuFamily string, pricePerHour float64) PriceBucket {
	prices, ok := m.PricePercentiles[gpuFamily]
	if !ok || len(prices) == 0 {
		return PriceBucketMedium
	}

	// Find the percentile rank
	idx := sort.SearchFloat64s(prices, pricePerHour)
	pct := float64(idx) / float64(len(prices))

	switch {
	case pct < 0.25:
		return PriceBucketLow
	case pct < 0.50:
		return PriceBucketMedium
	case pct < 0.75:
		return PriceBucketHigh
	default:
		return PriceBucketPremium
	}
}

// ExpectedWallclockTime computes the expected wall-clock time for a job,
// accounting for instance failure retries using a geometric retry model:
//
//	E[wallclock] = (jobDurationHrs + setupOverheadHrs) / p
//
// Each attempt costs setup + run time. With survival probability p, the
// expected number of attempts is 1/p. The caller is responsible for any
// runtime scaling.
func ExpectedWallclockTime(jobDurationHrs, setupOverheadHrs, survivalProb float64) float64 {
	if survivalProb <= 0 {
		return math.Inf(1)
	}
	if survivalProb >= 1 {
		return jobDurationHrs + setupOverheadHrs
	}
	return (jobDurationHrs + setupOverheadHrs) / survivalProb
}

// OfferSetupFunc returns the estimated setup overhead in hours for a given offer.
// This includes startup, SSH setup, provision (model downloads), and job setup.
type OfferSetupFunc func(cloud.Offer) float64

// ConstantSetup returns an OfferSetupFunc that returns the same value for all offers.
func ConstantSetup(hrs float64) OfferSetupFunc {
	return func(_ cloud.Offer) float64 { return hrs }
}

func aggregateJobCompletionTimeHrs(totalRunHrs, setupOverheadHrs float64, jobCount int) float64 {
	if jobCount <= 1 {
		return totalRunHrs + setupOverheadHrs
	}
	return float64(jobCount)*setupOverheadHrs + totalRunHrs*float64(jobCount+1)/2
}

// BestOffer selects the best offer for a single job according to the given strategy.
func BestOffer(model *SurvivalModel, offers []cloud.Offer, jobDurationHrs float64, setupOverhead OfferSetupFunc, strategy SelectionStrategy, maxGPUMemGB int) (int, cloud.Offer) {
	return BestOfferForJobGroupWithProfile(model, offers, jobDurationHrs, 1, setupOverhead, strategy.Profile(), maxGPUMemGB)
}

// BestOfferForJobGroup selects the best offer for a sequential group of jobs.
//
// All strategies use a unified weighted score combining expected cost and
// aggregate job completion time, with strategy-specific weights controlling the
// tradeoff. The time term is the sum of each placed job's completion time from
// group start, so it includes shared setup plus waiting for earlier jobs on the
// same instance to finish.
//
// If the model is nil, survival probability defaults to 1.0 (no retry risk).
//
// This is a legacy fallback selector. It does not infer GPU speed differences
// from DLPerf or VRAM tiers; callers that have estimator-backed runtimes should
// use those directly before reaching this layer.
func BestOfferForJobGroup(model *SurvivalModel, offers []cloud.Offer, totalRunHrs float64, jobCount int, setupOverhead OfferSetupFunc, strategy SelectionStrategy, maxGPUMemGB int) (int, cloud.Offer) {
	return BestOfferForJobGroupWithProfile(model, offers, totalRunHrs, jobCount, setupOverhead, strategy.Profile(), maxGPUMemGB)
}

// BestOfferForJobGroupWithProfile selects the best offer for a sequential group
// of jobs using an explicit score profile.
func BestOfferForJobGroupWithProfile(model *SurvivalModel, offers []cloud.Offer, totalRunHrs float64, jobCount int, setupOverhead OfferSetupFunc, profile ScoreProfile, maxGPUMemGB int) (int, cloud.Offer) {
	if len(offers) == 0 {
		return -1, cloud.Offer{}
	}
	if jobCount < 1 {
		jobCount = 1
	}

	w := profile.Weights()

	return bestOfferByScore(offers, func(o cloud.Offer) float64 {
		setup := setupOverhead(o)

		surv := 1.0
		if model != nil {
			surv = model.OfferSurvival(o)
		}

		runHrs := totalRunHrs

		cost := ExpectedCost(o.CostPerHour, runHrs, setup, surv)
		completionHrs := aggregateJobCompletionTimeHrs(runHrs, setup, jobCount)

		var completionScore float64
		if profile.UseHappyPathTime {
			// Happy-path completion only — no survival adjustment.
			completionScore = completionHrs
		} else {
			completionScore = completionHrs / surv
		}

		return w.Cost*cost + w.Time*completionScore
	})
}

// bestOfferByScore returns the offer with the lowest score value.
func bestOfferByScore(offers []cloud.Offer, score func(cloud.Offer) float64) (int, cloud.Offer) {
	bestIdx := 0
	bestScore := score(offers[0])
	for i, o := range offers[1:] {
		s := score(o)
		if s < bestScore {
			bestScore = s
			bestIdx = i + 1
		}
	}
	return bestIdx, offers[bestIdx]
}

// MedianOfferDLPerf returns the median DLPerf across offers.
// Returns 1.0 if no offers have DLPerf data.
func MedianOfferDLPerf(offers []cloud.Offer) float64 {
	perfs := make([]float64, 0, len(offers))
	for _, o := range offers {
		if o.DLPerf > 0 {
			perfs = append(perfs, o.DLPerf)
		}
	}
	if len(perfs) == 0 {
		return 1.0
	}
	sort.Float64s(perfs)
	n := len(perfs)
	if n%2 == 0 {
		return (perfs[n/2-1] + perfs[n/2]) / 2
	}
	return perfs[n/2]
}

// MinMachineObs is the minimum number of observations before per-machine
// penalty takes effect. Below this threshold, MachinePenalty returns 1.0.
const MinMachineObs = 3

// MachinePenalty returns a multiplicative penalty for a specific machine.
// Returns 1.0 (no penalty) if the machine has fewer than minMachineObs observations
// or if no machine stats are available.
func (m *SurvivalModel) MachinePenalty(machineID string) float64 {
	if m.MachineStats == nil || machineID == "" {
		return 1.0
	}
	ms, ok := m.MachineStats[machineID]
	if !ok || ms.Total < MinMachineObs {
		return 1.0
	}
	machineRate := float64(ms.Survived) / float64(ms.Total)
	globalRate := 0.95
	if m.GlobalTotal > 0 {
		globalRate = float64(m.GlobalSurvived) / float64(m.GlobalTotal)
	}
	if globalRate <= 0 {
		return 1.0
	}
	// Penalty is the ratio of machine survival to global survival,
	// clamped to [0, 1] so good machines don't get a bonus.
	penalty := machineRate / globalRate
	if penalty > 1.0 {
		penalty = 1.0
	}
	return penalty
}

// OfferSurvival returns the survival probability for a specific offer,
// incorporating per-machine penalty when available.
func (m *SurvivalModel) OfferSurvival(o cloud.Offer) float64 {
	gpuFamily := NormalizeGPUFamily(o.GPUName)
	bucket := m.PriceBucketFor(gpuFamily, o.CostPerHour)
	groupSurvival := m.SurvivalProbability(gpuFamily, bucket, o.Reliability)
	return groupSurvival * m.MachinePenalty(o.MachineID)
}

// RejectedGroup summarizes offers rejected by FilterOffersBySurvival.
type RejectedGroup struct {
	GPUFamily    string
	Count        int
	SurvivalProb float64 // minimum survival probability seen in the group
}

// FilterOffersBySurvival removes offers whose survival probability is below
// minSurvival. Returns the passing offers and a summary of rejected groups.
// If model is nil or minSurvival <= 0, all offers pass through unchanged.
func FilterOffersBySurvival(model *SurvivalModel, offers []cloud.Offer, minSurvival float64) ([]cloud.Offer, []RejectedGroup) {
	if model == nil || minSurvival <= 0 || len(offers) == 0 {
		return offers, nil
	}

	var passed []cloud.Offer
	rejected := make(map[string]*RejectedGroup) // keyed by GPU family

	for _, o := range offers {
		surv := model.OfferSurvival(o)
		if surv >= minSurvival {
			passed = append(passed, o)
			continue
		}
		fam := NormalizeGPUFamily(o.GPUName)
		rg, ok := rejected[fam]
		if !ok {
			rg = &RejectedGroup{GPUFamily: fam, SurvivalProb: surv}
			rejected[fam] = rg
		}
		rg.Count++
		if surv < rg.SurvivalProb {
			rg.SurvivalProb = surv
		}
	}

	if len(rejected) == 0 {
		return offers, nil
	}

	groups := make([]RejectedGroup, 0, len(rejected))
	for _, rg := range rejected {
		groups = append(groups, *rg)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].GPUFamily < groups[j].GPUFamily })
	return passed, groups
}

// NormalizeGPUFamily canonicalizes GPU names into family identifiers.
// e.g., "GeForce RTX 4090" → "RTX_4090", "NVIDIA A100 80GB PCIe" → "A100"
func NormalizeGPUFamily(name string) string {
	name = strings.TrimSpace(name)

	// Remove common prefixes
	for _, prefix := range []string{"NVIDIA ", "GeForce ", "Tesla "} {
		name = strings.TrimPrefix(name, prefix)
	}

	// Remove memory/interface suffixes
	for _, suffix := range []string{" PCIe", " SXM", " NVLink", " 80GB", " 40GB", " 24GB", " 16GB", " 12GB", " 8GB"} {
		name = strings.TrimSuffix(name, suffix)
	}

	// Replace spaces with underscores
	name = strings.ReplaceAll(name, " ", "_")

	return name
}
