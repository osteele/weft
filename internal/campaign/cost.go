package campaign

import (
	"log"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

// CostEstimate holds the cost projection for one instance group.
type CostEstimate struct {
	Group         InstanceGroup
	Offer         GroupOffer
	Breakdown     estimate.Breakdown
	JobDurations  map[int64]time.Duration // job ID → predicted duration (empty if unavailable)
	SetupOverhead time.Duration
	DownloadBytes int64         // total bytes of HF model inputs to download
	DownloadTime  time.Duration // estimated download time from offer bandwidth
	TotalTime     time.Duration
	TotalCost     float64
	HasPrediction bool // false = fell back to default job duration
}

// EstimateProgressFunc reports progress during estimation.
// phase describes what is happening, resolved/total track items.
type EstimateProgressFunc func(phase string, resolved, total int)

// EstimateCosts computes per-group cost estimates using the predictor for duration.
// If predCfg is nil or not configured, falls back to 1hr/job estimates.
func EstimateCosts(groupOffers []GroupOffer, predCfg *predictor.Config, onProgress EstimateProgressFunc) []CostEstimate {
	estimates := make([]CostEstimate, len(groupOffers))

	// Prefetch all unique model sizes in parallel to avoid sequential HF API calls
	var modelProgress func(resolved, total int)
	if onProgress != nil {
		modelProgress = func(resolved, total int) {
			onProgress("Resolving model sizes", resolved, total)
		}
	}
	dataloc.PrefetchInputSizes(collectAllInputs(groupOffers), modelProgress)

	// Count total jobs for progress
	totalJobs := 0
	for _, go_ := range groupOffers {
		if go_.Offer != nil {
			totalJobs += len(go_.Group.Jobs)
		}
	}
	jobsDone := 0

	for i, go_ := range groupOffers {
		est := CostEstimate{
			Group:        go_.Group,
			Offer:        go_,
			JobDurations: make(map[int64]time.Duration),
		}

		if go_.Offer == nil {
			estimates[i] = est
			continue
		}

		// Startup phase
		startup := estimate.EstimateStartup(string(go_.Offer.Provider))

		// Provision phase: workdir sync + model download
		var downloadBytes int64
		if totalBytes, err := dataloc.ResolveInputSizes(go_.Group.AllInputs(), nil); err != nil {
			log.Printf("warning: could not resolve input sizes: %v", err)
		} else {
			downloadBytes = totalBytes
		}

		bytesPerSec := cloud.MbpsToBytesPerSec(go_.Offer.DownloadBandwidth)
		provision := estimate.EstimateProvision(estimate.ProvisionInput{
			ModelDownloadBytes:   downloadBytes,
			BandwidthBytesPerSec: bytesPerSec,
		})

		// Run phase: sum per-job estimates
		hasPrediction := false
		var runEst estimate.Estimate
		for _, job := range go_.Group.Jobs {
			jobEst, predicted := estimate.EstimateJobDuration(predCfg, go_.Offer.GPUName, job)
			if predicted {
				est.JobDurations[job.ID] = jobEst.Mean
				hasPrediction = true
			}
			runEst = runEst.Add(jobEst)
			jobsDone++
			if onProgress != nil {
				onProgress("Estimating durations", jobsDone, totalJobs)
			}
		}

		// Build breakdown
		total := startup.Add(provision).Add(runEst)
		bd := estimate.Breakdown{
			Startup:          startup,
			Provision:        provision,
			Run:              runEst,
			Total:            total,
			HasRunPrediction: hasPrediction,
		}

		est.Breakdown = bd
		est.HasPrediction = hasPrediction
		est.SetupOverhead = startup.Mean + provision.Mean
		est.DownloadBytes = downloadBytes
		est.DownloadTime = estimate.TransferTime(downloadBytes, bytesPerSec).Mean
		est.TotalTime = total.Mean
		est.TotalCost = total.Mean.Hours() * go_.Offer.CostPerHour

		estimates[i] = est
	}

	return estimates
}

// collectAllInputs gathers all unique inputs across all groups.
func collectAllInputs(groupOffers []GroupOffer) []string {
	seen := make(map[string]bool)
	var inputs []string
	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		for _, input := range go_.Group.AllInputs() {
			if !seen[input] {
				seen[input] = true
				inputs = append(inputs, input)
			}
		}
	}
	return inputs
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
