package campaign

import (
	"log"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/predictor"
)

// DefaultSetupOverhead is the flat time estimate for instance boot, env setup, and teardown.
const DefaultSetupOverhead = 10 * time.Minute

// DefaultJobDuration is the fallback duration when no prediction is available.
const DefaultJobDuration = 1 * time.Hour

// CostEstimate holds the cost projection for one instance group.
type CostEstimate struct {
	Group         InstanceGroup
	Offer         GroupOffer
	JobDurations  map[int64]time.Duration // job ID → predicted duration (empty if unavailable)
	SetupOverhead time.Duration
	DownloadBytes int64         // total bytes of HF model inputs to download
	DownloadTime  time.Duration // estimated download time from offer bandwidth
	TotalTime     time.Duration
	TotalCost     float64
	HasPrediction bool // false = fell back to DefaultJobDuration
}

// EstimateCosts computes per-group cost estimates using the predictor for duration.
// If predCfg is nil or not configured, falls back to 1hr/job estimates.
func EstimateCosts(groupOffers []GroupOffer, predCfg *predictor.Config) []CostEstimate {
	estimates := make([]CostEstimate, len(groupOffers))

	for i, go_ := range groupOffers {
		est := CostEstimate{
			Group:         go_.Group,
			Offer:         go_,
			SetupOverhead: DefaultSetupOverhead,
			JobDurations:  make(map[int64]time.Duration),
		}

		if go_.Offer == nil {
			estimates[i] = est
			continue
		}

		hasPrediction := false
		var totalJobTime time.Duration

		for _, job := range go_.Group.Jobs {
			dur := predictJobDuration(predCfg, go_.Offer.GPUName, job)
			if dur > 0 {
				est.JobDurations[job.ID] = dur
				totalJobTime += dur
				hasPrediction = true
			} else {
				totalJobTime += DefaultJobDuration
			}
		}

		// Estimate download time from HF model inputs
		if totalBytes, err := dataloc.ResolveInputSizes(go_.Group.AllInputs(), nil); err != nil {
			log.Printf("warning: could not resolve input sizes: %v", err)
		} else if totalBytes > 0 {
			est.DownloadBytes = totalBytes
			if go_.Offer.DownloadBandwidth > 0 {
				bytesPerSec := cloud.MbpsToBytesPerSec(go_.Offer.DownloadBandwidth)
				est.DownloadTime = time.Duration(float64(totalBytes)/bytesPerSec) * time.Second
			}
		}

		est.HasPrediction = hasPrediction
		est.TotalTime = totalJobTime + est.SetupOverhead + est.DownloadTime
		est.TotalCost = est.TotalTime.Hours() * go_.Offer.CostPerHour

		estimates[i] = est
	}

	return estimates
}

// predictJobDuration calls the predictor for a single job.
// Returns 0 if predictor is not configured or prediction fails.
func predictJobDuration(predCfg *predictor.Config, gpuClass string, job *db.Job) time.Duration {
	if predCfg == nil || !predCfg.Configured() {
		return 0
	}

	result, err := predictor.Predict(*predCfg, "", job.Project, gpuClass, job.Command)
	if err != nil || result == nil || result.DurationS == nil {
		return 0
	}

	return time.Duration(result.DurationS.Mean) * time.Second
}

// BudgetMultiplier is the safety factor applied to estimates for budget limits.
const BudgetMultiplier = 10.0

// MinBudgetTime is the minimum time budget to prevent premature kills.
const MinBudgetTime = 8 * time.Hour

// MinBudgetCents is the minimum spend budget ($20) to prevent premature kills.
const MinBudgetCents = 2000

// BudgetFromEstimate derives safety-net budget limits from a cost estimate.
// Returns maxSpendCents and maxTimeSeconds with a generous multiplier,
// floored at minimums to avoid killing jobs we can't estimate well.
func BudgetFromEstimate(est CostEstimate) (maxSpendCents int, maxTimeSeconds int) {
	maxTime := time.Duration(float64(est.TotalTime) * BudgetMultiplier)
	if maxTime < MinBudgetTime {
		maxTime = MinBudgetTime
	}
	maxSpendCents = int(est.TotalCost * BudgetMultiplier * 100)
	if maxSpendCents < MinBudgetCents {
		maxSpendCents = MinBudgetCents
	}
	return maxSpendCents, int(maxTime.Seconds())
}

// TotalEstimatedCostFromEstimates returns the total cost across all estimates.
func TotalEstimatedCostFromEstimates(estimates []CostEstimate) float64 {
	var total float64
	for _, est := range estimates {
		total += est.TotalCost
	}
	return total
}
