package campaign

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
)

type CampaignEffect struct {
	Tau2       float64
	SigmaW2    float64
	ICC        float64
	NCampaigns int
}

var campaignEffectCache = struct {
	sync.Mutex
	entries map[*sql.DB]campaignEffectCacheEntry
}{
	entries: make(map[*sql.DB]campaignEffectCacheEntry),
}

type campaignEffectCacheEntry struct {
	effect *CampaignEffect
	err    error
	at     time.Time
}

func cachedCampaignEffect(database *sql.DB) (*CampaignEffect, error) {
	if database == nil {
		return nil, nil
	}
	now := time.Now()
	campaignEffectCache.Lock()
	if entry, ok := campaignEffectCache.entries[database]; ok && now.Sub(entry.at) < 5*time.Minute {
		campaignEffectCache.Unlock()
		return entry.effect, entry.err
	}
	campaignEffectCache.Unlock()

	effect, err := EstimateCampaignEffect(database)
	campaignEffectCache.Lock()
	campaignEffectCache.entries[database] = campaignEffectCacheEntry{effect: effect, err: err, at: now}
	campaignEffectCache.Unlock()
	return effect, err
}

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
	if demand, ok := hybridCampaignDemandAtFractile(valid, fractile); ok {
		demandAtFractile = demand
	} else if demand, ok := durationQuantileDemandAtFractile(valid, fractile); ok {
		demandAtFractile = demand
	}
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

func durationQuantileDemandAtFractile(estimates []CostEstimate, fractile float64) (float64, bool) {
	var demandSeconds float64
	used := 0
	for _, est := range estimates {
		common := est.Breakdown.Total.Mean - est.Breakdown.Run.Mean
		if common < 0 {
			common = 0
		}
		demandSeconds += common.Seconds()
		for _, job := range est.Group.Jobs {
			if job == nil {
				continue
			}
			q, ok := est.JobDurationQuantiles[job.ID]
			if !ok || q == nil {
				return 0, false
			}
			seconds, ok := q.Quantile(fractile)
			if !ok || seconds <= 0 {
				return 0, false
			}
			demandSeconds += seconds
			used++
		}
	}
	if used == 0 {
		return 0, false
	}
	return demandSeconds / 3600, true
}

func hybridCampaignDemandAtFractile(estimates []CostEstimate, fractile float64) (float64, bool) {
	effect := firstCampaignEffect(estimates)
	if effect == nil || effect.NCampaigns < 2 || (effect.Tau2 <= 0 && effect.SigmaW2 <= 0) {
		return 0, false
	}

	type jobDist struct {
		meanLog float64
	}
	var jobs []jobDist
	var commonSeconds float64
	for _, est := range estimates {
		common := est.Breakdown.Total.Mean - est.Breakdown.Run.Mean
		if common > 0 {
			commonSeconds += common.Seconds()
		}
		for _, job := range est.Group.Jobs {
			if job == nil {
				continue
			}
			q := est.JobDurationQuantiles[job.ID]
			if q == nil || (q.MeanLog == 0 && q.N <= 0) {
				return 0, false
			}
			jobs = append(jobs, jobDist{meanLog: q.MeanLog})
		}
	}
	if len(jobs) == 0 {
		return 0, false
	}

	const samples = 4096
	demands := make([]float64, samples)
	rng := rand.New(rand.NewSource(1))
	tau := math.Sqrt(math.Max(0, effect.Tau2))
	sigma := math.Sqrt(math.Max(0, effect.SigmaW2))
	for i := range demands {
		shared := rng.NormFloat64() * tau
		total := commonSeconds
		for _, job := range jobs {
			total += math.Exp(job.meanLog + shared + rng.NormFloat64()*sigma)
		}
		demands[i] = total / 3600
	}
	sort.Float64s(demands)
	idx := int(math.Ceil(fractile*float64(len(demands)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(demands) {
		idx = len(demands) - 1
	}
	return demands[idx], true
}

func firstCampaignEffect(estimates []CostEstimate) *CampaignEffect {
	for _, est := range estimates {
		if est.CampaignEffect != nil {
			return est.CampaignEffect
		}
	}
	return nil
}

func EstimateCampaignEffect(database *sql.DB) (*CampaignEffect, error) {
	if database == nil {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT launch_id, duration_s
		FROM training_examples
		WHERE launch_id IS NOT NULL
		  AND duration_s > 0
		  AND (exit_code IS NULL OR exit_code = 0)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byLaunch := make(map[int64][]float64)
	for rows.Next() {
		var launchID int64
		var durationS float64
		if err := rows.Scan(&launchID, &durationS); err != nil {
			return nil, err
		}
		if durationS > 0 {
			byLaunch[launchID] = append(byLaunch[launchID], math.Log(durationS))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	means := make([]float64, 0, len(byLaunch))
	withinVars := make([]float64, 0, len(byLaunch))
	for _, values := range byLaunch {
		if len(values) < 2 {
			continue
		}
		mean := meanFloat64(values)
		means = append(means, mean)
		withinVars = append(withinVars, sampleVariance(values, mean))
	}
	if len(means) < 2 {
		return nil, nil
	}
	sort.Float64s(withinVars)
	sigmaW2 := medianFloat64(withinVars)
	tau2 := sampleVariance(means, meanFloat64(means))
	denom := tau2 + sigmaW2
	icc := 0.0
	if denom > 0 {
		icc = tau2 / denom
	}
	return &CampaignEffect{Tau2: tau2, SigmaW2: sigmaW2, ICC: icc, NCampaigns: len(means)}, nil
}

func meanFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func sampleVariance(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}
	var sum float64
	for _, v := range values {
		d := v - mean
		sum += d * d
	}
	return sum / float64(len(values)-1)
}

func medianFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return (values[mid-1] + values[mid]) / 2
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
