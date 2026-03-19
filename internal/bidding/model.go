// Package bidding implements cost-optimal cloud instance selection using a
// Beta-Binomial survival model. It learns price-reliability curves from
// historical campaign data and selects offers that minimize expected cost
// including retry risk from preemption.
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
	// StrategyFast minimizes expected wall-clock time (prefers higher DLPerf,
	// weighted by survival probability).
	StrategyFast SelectionStrategy = "fast"
	// StrategyFastest picks the highest raw DLPerf, ignoring the survival model.
	StrategyFastest SelectionStrategy = "fastest"
)

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
// (GPU family, price bucket).
type SurvivalModel struct {
	GlobalSurvived   int
	GlobalTotal      int
	Groups           map[string]*SurvivalStats // key: "GPU_FAMILY:price_bucket"
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
// preemption retries. Uses a geometric retry model:
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
// scaling duration by DLPerf relative to the median, and accounting for
// preemption retries using a geometric retry model:
//
//	scaledJobHrs = jobDurationHrs × (medianDLPerf / dlPerf)
//	E[wallclock] = scaledJobHrs / p + (1-p)/p × setupOverheadHrs
func ExpectedWallclockTime(dlPerf, medianDLPerf, jobDurationHrs, setupOverheadHrs, survivalProb float64) float64 {
	if dlPerf <= 0 || medianDLPerf <= 0 {
		return math.Inf(1)
	}
	scaledHrs := jobDurationHrs * (medianDLPerf / dlPerf)
	if survivalProb <= 0 {
		return math.Inf(1)
	}
	if survivalProb >= 1 {
		return scaledHrs
	}
	return scaledHrs/survivalProb + (1-survivalProb)/survivalProb*setupOverheadHrs
}

// BestOffer selects the best offer according to the given strategy.
// StrategyCheap minimizes expected dollar cost. StrategyFast minimizes expected
// wall-clock time (survival-weighted). StrategyFastest picks the highest raw
// DLPerf, ignoring the survival model entirely.
// If the model is nil, cheap falls back to lowest price and fast/fastest to highest DLPerf.
func BestOffer(model *SurvivalModel, offers []cloud.Offer, jobDurationHrs, setupOverheadHrs float64, strategy SelectionStrategy) (int, cloud.Offer) {
	if len(offers) == 0 {
		return -1, cloud.Offer{}
	}

	// Fastest always ignores the survival model.
	if strategy == StrategyFastest {
		return bestOfferByScore(offers, func(o cloud.Offer) float64 { return -o.DLPerf })
	}

	if model == nil {
		if strategy == StrategyFast {
			return bestOfferByScore(offers, func(o cloud.Offer) float64 { return -o.DLPerf })
		}
		return bestOfferByScore(offers, func(o cloud.Offer) float64 { return o.CostPerHour })
	}

	var medianDLPerf float64
	if strategy == StrategyFast {
		medianDLPerf = medianOfferDLPerf(offers)
	}

	return bestOfferByScore(offers, func(o cloud.Offer) float64 {
		surv := model.OfferSurvival(o)
		if strategy == StrategyFast {
			return ExpectedWallclockTime(o.DLPerf, medianDLPerf, jobDurationHrs, setupOverheadHrs, surv)
		}
		return ExpectedCost(o.CostPerHour, jobDurationHrs, setupOverheadHrs, surv)
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

// medianOfferDLPerf returns the median DLPerf across offers.
func medianOfferDLPerf(offers []cloud.Offer) float64 {
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

// OfferSurvival returns the survival probability for a specific offer.
func (m *SurvivalModel) OfferSurvival(o cloud.Offer) float64 {
	gpuFamily := NormalizeGPUFamily(o.GPUName)
	bucket := m.PriceBucketFor(gpuFamily, o.CostPerHour)
	return m.SurvivalProbability(gpuFamily, bucket, o.Reliability)
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
