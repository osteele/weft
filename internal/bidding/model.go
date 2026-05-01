// Package bidding implements cost-optimal cloud instance selection using a
// Beta-Binomial survival model. It learns price-reliability curves from
// historical campaign data and selects offers that minimize expected cost
// including retry risk from instance failure.
package bidding

import (
	"math"
	"sort"
	"strconv"
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
//
// Survived/Total are raw counts, used for "do we have enough data" thresholds
// (e.g. minGroupObs) and for human-facing display ("3/5 survived").
//
// WeightedSurvived/WeightedTotal apply exponential time decay so older
// outcomes contribute less to the posterior — they are what the
// Beta-Binomial math actually consumes. This lets a (provider, family) cell
// recover from stale failures without manual intervention: if no new
// observations arrive but old failures keep ageing, the weighted counts
// shrink toward zero and the posterior drifts back toward the prior, so a
// once-banned offer family becomes eligible to be probed again.
type SurvivalStats struct {
	Survived         int
	Total            int
	WeightedSurvived float64
	WeightedTotal    float64
}

// SurvivalModel holds the Beta-Binomial survival model grouped by
// (provider, GPU family, price bucket), with optional per-machine penalty.
//
// Per-provider keying prevents cross-contamination: e.g., RunPod's managed
// hosts have different reliability characteristics than Vast.ai marketplace
// hosts, so a Vast outcome must not influence the posterior for a RunPod
// offer of the same GPU family. Group keys, machine keys, price percentiles,
// and the global blending rate are all scoped to one provider.
type SurvivalModel struct {
	Global           map[cloud.Provider]*SurvivalStats
	Groups           map[string]*SurvivalStats
	MachineStats     map[string]*SurvivalStats
	CountryStats     map[string]*SurvivalStats // keyed by provider|country (e.g. vastai|CN)
	RegionStats      map[string]*SurvivalStats // keyed by provider|data_center (e.g. vastai|Sichuan, CN)
	PricePercentiles map[string][]float64
	PriorStrength    float64 // pseudo-observations from provider reliability
}

const providerKeySep = "|"

// GPUSKU is the survival-model partitioning unit: a (family, VRAM) pair.
// Different VRAM SKUs of the same family (e.g. RTX 4090 24GB vs 48GB) have
// different production volumes, datacenter geographies, and reliability
// profiles, so pooling their survival data hides real differences.
//
// VRAMGB == 0 marks "VRAM unknown" (legacy rows or test fixtures); these
// land in their own SKU bucket so undated outcomes don't contaminate any
// known-VRAM bucket.
type GPUSKU struct {
	Family string
	VRAMGB int
}

func skuFromOutcome(family string, vramGB int) GPUSKU {
	return GPUSKU{Family: family, VRAMGB: vramGB}
}

// String returns the SKU's stringified form used as a map key.
// Format: "RTX_4090@24" or "RTX_4090@?" for unknown VRAM.
func (s GPUSKU) String() string {
	if s.VRAMGB <= 0 {
		return s.Family + "@?"
	}
	return s.Family + "@" + strconv.Itoa(s.VRAMGB)
}

// parseSKU is the inverse of GPUSKU.String. Used by IterateGroups callers
// that need to recover the family/VRAM split for display.
func parseSKU(s string) GPUSKU {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return GPUSKU{Family: s}
	}
	family := s[:at]
	vramStr := s[at+1:]
	if vramStr == "?" {
		return GPUSKU{Family: family}
	}
	vram, err := strconv.Atoi(vramStr)
	if err != nil {
		return GPUSKU{Family: family}
	}
	return GPUSKU{Family: family, VRAMGB: vram}
}

func groupKey(provider cloud.Provider, sku GPUSKU, bucket PriceBucket) string {
	return string(provider) + providerKeySep + sku.String() + ":" + string(bucket)
}

func machineKey(provider cloud.Provider, machineID string) string {
	return string(provider) + providerKeySep + machineID
}

func countryKey(provider cloud.Provider, country string) string {
	return string(provider) + providerKeySep + country
}

func regionKey(provider cloud.Provider, region string) string {
	return string(provider) + providerKeySep + region
}

// parseCountryFromDataCenter extracts the trailing country code from a
// vast.ai-style data_center string like "Sichuan, CN" or ", US". Returns
// empty for unparseable values; the caller uses an empty country to skip
// the country-level adjustment without polluting the country stats.
func parseCountryFromDataCenter(dc string) string {
	idx := strings.LastIndex(dc, ", ")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(dc[idx+2:])
}

func pricePercentileKey(provider cloud.Provider, sku GPUSKU) string {
	return string(provider) + providerKeySep + sku.String()
}

// GroupStat is one row of group-level survival data, used by callers that
// need to display or aggregate the model without parsing internal keys.
type GroupStat struct {
	Provider cloud.Provider
	Family   string // legacy: family alone, identical to SKU.Family
	SKU      GPUSKU
	Bucket   PriceBucket
	*SurvivalStats
}

// MachineStat is one row of per-machine survival data.
type MachineStat struct {
	Provider cloud.Provider
	ID       string
	*SurvivalStats
}

// IterateGroups returns the model's group-level stats with keys decoded.
func (m *SurvivalModel) IterateGroups() []GroupStat {
	out := make([]GroupStat, 0, len(m.Groups))
	for key, stats := range m.Groups {
		barIdx := strings.Index(key, providerKeySep)
		if barIdx < 0 {
			continue
		}
		rest := key[barIdx+1:]
		colonIdx := strings.LastIndex(rest, ":")
		if colonIdx < 0 {
			continue
		}
		sku := parseSKU(rest[:colonIdx])
		out = append(out, GroupStat{
			Provider:      cloud.Provider(key[:barIdx]),
			Family:        sku.Family,
			SKU:           sku,
			Bucket:        PriceBucket(rest[colonIdx+1:]),
			SurvivalStats: stats,
		})
	}
	return out
}

// IterateMachines returns the model's per-machine stats with keys decoded.
func (m *SurvivalModel) IterateMachines() []MachineStat {
	out := make([]MachineStat, 0, len(m.MachineStats))
	for key, stats := range m.MachineStats {
		barIdx := strings.Index(key, providerKeySep)
		if barIdx < 0 {
			continue
		}
		out = append(out, MachineStat{
			Provider:      cloud.Provider(key[:barIdx]),
			ID:            key[barIdx+1:],
			SurvivalStats: stats,
		})
	}
	return out
}

func (m *SurvivalModel) globalRate(provider cloud.Provider) (rate float64, ok bool) {
	gs, has := m.Global[provider]
	if !has || gs.WeightedTotal <= 0 {
		return 0, false
	}
	return gs.WeightedSurvived / gs.WeightedTotal, true
}

// SurvivalProbability returns the posterior mean survival probability for a
// given provider, GPU family, price bucket, and provider reliability score.
//
// Uses a hierarchical Beta-Binomial model:
//   - Prior: Beta(alpha0, beta0) from provider reliability with PriorStrength pseudo-observations
//   - Likelihood: Binomial(survived, total) from the (provider, family, bucket) group,
//     weighted by recency (older outcomes contribute less; see SurvivalDecayHalfLife)
//   - When the group has effectively few observations (weighted total < minGroupObs),
//     blends toward the per-provider global rate
func (m *SurvivalModel) SurvivalProbability(provider cloud.Provider, sku GPUSKU, bucket PriceBucket, providerReliability float64) float64 {
	if providerReliability <= 0 {
		providerReliability = 0.95
	}

	// Informative prior from provider reliability
	alpha0 := m.PriorStrength * providerReliability
	beta0 := m.PriorStrength * (1 - providerReliability)

	key := groupKey(provider, sku, bucket)
	gs, ok := m.Groups[key]

	if !ok || gs.WeightedTotal < float64(minGroupObs) {
		// Sparse group: blend toward the provider's global rate (not a
		// cross-provider rate — RunPod and Vast have different baselines).
		globalRate := 0.95
		if rate, has := m.globalRate(provider); has {
			globalRate = rate
		}

		if !ok || gs == nil || gs.WeightedTotal <= 0 {
			// No group data: use prior blended with provider global
			alpha := alpha0 + m.PriorStrength*globalRate
			beta := beta0 + m.PriorStrength*(1-globalRate)
			return alpha / (alpha + beta)
		}

		// Some group data but sparse: shrink toward provider global
		shrinkage := gs.WeightedTotal / float64(minGroupObs)
		groupRate := gs.WeightedSurvived / gs.WeightedTotal
		blended := shrinkage*groupRate + (1-shrinkage)*globalRate

		alpha := alpha0 + m.PriorStrength*blended
		beta := beta0 + m.PriorStrength*(1-blended)
		return alpha / (alpha + beta)
	}

	// Sufficient group data: standard Beta posterior on weighted counts
	alpha := alpha0 + gs.WeightedSurvived
	beta := beta0 + (gs.WeightedTotal - gs.WeightedSurvived)
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

// PriceBucketFor classifies a price relative to historical percentiles for the
// (provider, SKU) pair. Provider matters because RunPod and Vast have
// different price ranges for the same GPU family; SKU matters because the
// 24GB and 48GB variants of the same family run at very different price
// points and shouldn't be pooled.
func (m *SurvivalModel) PriceBucketFor(provider cloud.Provider, sku GPUSKU, pricePerHour float64) PriceBucket {
	prices, ok := m.PricePercentiles[pricePercentileKey(provider, sku)]
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

// MinMachineObs is the display threshold for per-machine penalty reporting:
// machines with fewer observations are hidden from `weft campaign survival`
// even if their posterior penalty differs from 1.0. The penalty itself is
// computed from any non-zero number of observations via a Beta posterior.
const MinMachineObs = 3

// MachinePriorStrength is the pseudo-observation count of the Beta prior
// used by MachinePenalty. Smaller than the group prior (defaultPriorStrength)
// so machine-level signal moves the posterior faster: with a single failure
// observation, the penalty already drops noticeably below 1.0, and a
// machine with 0/N successes is penalised more aggressively than the old
// hard ratio could express without overconfidence.
const MachinePriorStrength = 2.0

// MachinePenalty returns a multiplicative penalty for a specific (provider, machine).
// Returns 1.0 (no penalty) if the machine has no observations or if no
// machine stats are available. With observations, the posterior survival
// rate is computed under a Beta prior centred on the provider's global
// survival rate (MachinePriorStrength pseudo-observations), then divided by
// the global rate and clamped to [0, 1]. Per-provider scoping prevents a
// Vast machine from being compared to RunPod's baseline.
func (m *SurvivalModel) MachinePenalty(provider cloud.Provider, machineID string) float64 {
	if m.MachineStats == nil || machineID == "" {
		return 1.0
	}
	ms, ok := m.MachineStats[machineKey(provider, machineID)]
	if !ok || ms.WeightedTotal <= 0 {
		return 1.0
	}
	globalRate := 0.95
	if rate, has := m.globalRate(provider); has {
		globalRate = rate
	}
	if globalRate <= 0 {
		return 1.0
	}
	// Beta(alpha0, beta0) prior centred on the provider's global rate, then
	// updated with the recency-weighted (survived, failed) counts. The
	// posterior mean is alpha / (alpha + beta).
	alpha0 := MachinePriorStrength * globalRate
	beta0 := MachinePriorStrength * (1 - globalRate)
	alpha := alpha0 + ms.WeightedSurvived
	beta := beta0 + (ms.WeightedTotal - ms.WeightedSurvived)
	posterior := alpha / (alpha + beta)
	penalty := posterior / globalRate
	if penalty > 1.0 {
		penalty = 1.0
	}
	return penalty
}

// OfferSurvival returns the survival probability for a specific offer.
//
// The result is the SKU-level Beta posterior (group survival) multiplied
// by a single geographic adjustment chosen from the most specific level
// that has signal: machine > region > country. The adjustment is the
// posterior at that level divided by the next-coarser level's posterior
// (the parent rate), clamped to ≤ 1.0.
//
// Choosing one level instead of stacking all three avoids double-counting
// the same observations: in a perfectly nested hierarchy (every machine
// belongs to one region, every region to one country), the same failures
// that drag the country posterior down also drag the region and machine
// posteriors down. Multiplying all three would compound the same evidence.
//
// Falling back through the levels means RunPod offers (no machine_id) get
// a regional adjustment, vast.ai offers in newly-seen datacenters fall
// back to the country adjustment, and offers in entirely-new countries
// fall back to the SKU-only base.
func (m *SurvivalModel) OfferSurvival(o cloud.Offer) float64 {
	sku := offerSKU(o)
	bucket := m.PriceBucketFor(o.Provider, sku, o.CostPerHour)
	base := m.SurvivalProbability(o.Provider, sku, bucket, o.Reliability)
	return base * m.geoAdjustment(o)
}

// geoAdjustment selects the most specific (machine | region | country)
// level that has observations and returns its conditional adjustment
// against the next-coarser parent's posterior. Returns 1.0 when no
// geographic signal is available at any level.
func (m *SurvivalModel) geoAdjustment(o cloud.Offer) float64 {
	globalRate := 0.95
	if r, ok := m.globalRate(o.Provider); ok {
		globalRate = r
	}
	country := parseCountryFromDataCenter(o.DataCenter)

	// Walk the hierarchy from coarse to fine, tracking the rate at each
	// level that has signal. The deepest level with signal is what gets
	// applied; its parentRate is whatever rate held at the next-coarser
	// level (data or, fallback, the global).
	parentRate := globalRate
	chosen := (*SurvivalStats)(nil)
	chosenParent := globalRate

	if country != "" {
		if s, ok := m.CountryStats[countryKey(o.Provider, country)]; ok && s.WeightedTotal > 0 {
			r := levelRate(s, parentRate)
			chosen, chosenParent = s, parentRate
			parentRate = r
		}
	}
	if o.DataCenter != "" {
		if s, ok := m.RegionStats[regionKey(o.Provider, o.DataCenter)]; ok && s.WeightedTotal > 0 {
			r := levelRate(s, parentRate)
			chosen, chosenParent = s, parentRate
			parentRate = r
		}
	}
	if o.MachineID != "" {
		if s, ok := m.MachineStats[machineKey(o.Provider, o.MachineID)]; ok && s.WeightedTotal > 0 {
			chosen, chosenParent = s, parentRate
		}
	}

	if chosen == nil || chosenParent <= 0 {
		return 1.0
	}
	posterior := levelRate(chosen, chosenParent)
	adj := posterior / chosenParent
	if adj > 1.0 {
		adj = 1.0
	}
	return adj
}

// levelRate returns the Beta posterior survival rate for a single
// geographic level, anchored to the parent rate as the prior centre.
func levelRate(s *SurvivalStats, parentRate float64) float64 {
	if parentRate <= 0 {
		parentRate = 0.95
	}
	alpha0 := MachinePriorStrength * parentRate
	beta0 := MachinePriorStrength * (1 - parentRate)
	alpha := alpha0 + s.WeightedSurvived
	beta := beta0 + (s.WeightedTotal - s.WeightedSurvived)
	return alpha / (alpha + beta)
}

// offerSKU extracts the (family, VRAM) key for an offer. Falls back to
// VRAM=0 ("?" bucket) when GPUMemGB isn't reported by the provider, which
// avoids contaminating known-VRAM cells with unknowns.
func offerSKU(o cloud.Offer) GPUSKU {
	return GPUSKU{
		Family: NormalizeGPUFamily(o.GPUName),
		VRAMGB: int(math.Round(o.GPUMemGB)),
	}
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
