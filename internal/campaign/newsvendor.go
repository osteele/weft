package campaign

import (
	"fmt"
	"math"
	"time"

	"github.com/osteele/weft/internal/bidding"
)

// NewsvendorRecommendation summarizes the instance-count decision implied by
// the launch plan and the selected cost/time strategy.
type NewsvendorRecommendation struct {
	Quantity         int
	CriticalFractile float64
	DemandHours      float64
	InstanceHours    float64
	RatePerHour      float64
}

// RecommendInstanceCount applies a newsvendor-style critical fractile to the
// plan's uncertain instance-hour demand. The returned quantity is clamped to
// the candidate groups because launch planning has already chosen the feasible
// offer set; this function makes the implied count visible to users.
func RecommendInstanceCount(estimates []CostEstimate, profile bidding.ScoreProfile) *NewsvendorRecommendation {
	valid := make([]CostEstimate, 0, len(estimates))
	var totalRate float64
	for _, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		valid = append(valid, est)
		totalRate += est.Offer.Offer.CostPerHour
	}
	if len(valid) == 0 || totalRate <= 0 {
		return nil
	}

	weights := profile.Weights()
	timeValue := weights.Time
	costValue := weights.Cost * (totalRate / float64(len(valid)))
	if timeValue <= 0 && costValue <= 0 {
		return nil
	}
	fractile := timeValue / (timeValue + costValue)
	fractile = math.Max(0.05, math.Min(0.95, fractile))

	var demandLower, demandMean, demandUpper float64
	var instanceHours float64
	for _, est := range valid {
		total := est.Breakdown.Total
		if total.Mean <= 0 && est.TotalTime > 0 {
			total.Mean = est.TotalTime
			total.Lower = time.Duration(float64(est.TotalTime) * 0.5)
			total.Upper = time.Duration(float64(est.TotalTime) * 1.5)
		}
		demandLower += total.Lower.Hours()
		demandMean += total.Mean.Hours()
		demandUpper += total.Upper.Hours()
		instanceHours = math.Max(instanceHours, total.Mean.Hours())
	}
	if instanceHours <= 0 || demandMean <= 0 {
		return nil
	}

	demandAtFractile := piecewiseQuantile(demandLower, demandMean, demandUpper, fractile)
	q := int(math.Ceil(demandAtFractile / instanceHours))
	if q < 1 {
		q = 1
	}
	if q > len(valid) {
		q = len(valid)
	}
	return &NewsvendorRecommendation{
		Quantity:         q,
		CriticalFractile: fractile,
		DemandHours:      demandAtFractile,
		InstanceHours:    instanceHours,
		RatePerHour:      totalRate / float64(len(valid)),
	}
}

func piecewiseQuantile(lower, median, upper, p float64) float64 {
	if lower <= 0 {
		lower = median
	}
	if upper <= 0 {
		upper = median
	}
	if p <= 0.5 {
		return lower + (median-lower)*(p/0.5)
	}
	return median + (upper-median)*((p-0.5)/0.5)
}

func FormatNewsvendorRecommendation(r *NewsvendorRecommendation) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf(
		"Newsvendor recommendation: launch %d instance(s) now (critical fractile %.0f%%, demand %.1f instance-hours, ~$%.2f/hr each)",
		r.Quantity,
		r.CriticalFractile*100,
		r.DemandHours,
		r.RatePerHour,
	)
}
