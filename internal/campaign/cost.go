package campaign

import (
	"log"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
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
	UVSyncBytes   int64         // estimated cold uv sync download bytes
	TotalTime     time.Duration
	TotalCost     float64

	// Survival model fields (zero values if no model available)
	SurvivalProb     float64 // 0-1, probability of completing without preemption
	RiskAdjustedCost float64 // expected cost including retry overhead
}

// EstimateProgressFunc reports progress during estimation.
// phase describes what is happening, resolved/total track items.
type EstimateProgressFunc func(phase string, resolved, total int)

// EstimateCosts computes per-group cost estimates using the predictor for duration.
// If predCfg is nil or not configured, falls back to 1hr/job estimates.
// If r2Client is non-nil, UV manifests are fetched to estimate cold uv sync costs.
// If survivalModel is non-nil, computes survival probability and risk-adjusted cost.
func EstimateCosts(groupOffers []GroupOffer, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, r2Client *r2.Client, survivalModel *bidding.SurvivalModel, onProgress EstimateProgressFunc) []CostEstimate {
	estimates := make([]CostEstimate, len(groupOffers))

	// Collect inputs for all three estimation steps (fast, in-memory)
	allInputs := collectAllInputs(groupOffers)

	seenDirs := make(map[string]bool)
	var uniqueDirs []string
	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		for _, dir := range go_.Group.SourceDirs() {
			if !seenDirs[dir] {
				seenDirs[dir] = true
				uniqueDirs = append(uniqueDirs, dir)
			}
		}
	}

	var allBatchJobs []predictor.BatchJob
	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		for _, job := range go_.Group.Jobs {
			allBatchJobs = append(allBatchJobs, predictor.BatchJob{
				ID:       job.ID,
				Command:  job.Command,
				Project:  job.Project,
				GPUClass: go_.Offer.GPUName,
			})
		}
	}

	// Run three independent I/O-bound steps concurrently
	var wg sync.WaitGroup
	var allManifests map[string]*estimate.UVManifestRef
	var allPredictions map[int64]estimate.Estimate

	// Step 1: HF model sizes (network calls to HuggingFace API)
	var modelProgress func(resolved, total int)
	if onProgress != nil {
		modelProgress = func(resolved, total int) {
			onProgress("Resolving model sizes", resolved, total)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		dataloc.PrefetchInputSizes(allInputs, modelProgress)
	}()

	// Step 2: UV manifests (lockfile hash + R2 fetch)
	wg.Add(1)
	go func() {
		defer wg.Done()
		allLockfileHashes := estimate.LockfileHash(uniqueDirs)
		allManifests = estimate.FetchUVManifests(r2Client, allLockfileHashes, "linux-amd64")
	}()

	// Step 3: Job duration predictions (subprocess call to ML predictor)
	wg.Add(1)
	go func() {
		defer wg.Done()
		allPredictions = estimate.EstimateJobDurations(predCfg, allBatchJobs)
	}()

	wg.Wait()
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

		ctx := estimate.InstanceContext{
			DataCenter:   go_.Offer.DataCenter,
			DLPerf:       go_.Offer.DLPerf,
			InetDownMbps: go_.Offer.DownloadBandwidth,
			InetUpMbps:   go_.Offer.UploadBandwidth,
		}

		startup := estimate.EstimateStartupWithModel(string(go_.Offer.Provider), overheadModel, ctx)
		sshSetup := estimate.EstimateSSHSetup(overheadModel, ctx)

		var downloadBytes int64
		if totalBytes, err := dataloc.ResolveInputSizes(go_.Group.AllInputs(), nil); err != nil {
			log.Printf("warning: could not resolve input sizes: %v", err)
		} else {
			downloadBytes = totalBytes
		}

		// Compute UV sync bytes for this group's source dirs
		var uvSyncBytes int64
		if allManifests != nil {
			groupManifests := make(map[string]*estimate.UVManifestRef)
			for _, dir := range go_.Group.SourceDirs() {
				if m, ok := allManifests[dir]; ok {
					groupManifests[dir] = m
				}
			}
			uvSyncBytes = estimate.EstimateUVSyncBytes(groupManifests)
		}

		bytesPerSec := cloud.MbpsToBytesPerSec(go_.Offer.DownloadBandwidth)
		provision := estimate.EstimateProvision(estimate.ProvisionInput{
			ModelDownloadBytes:   downloadBytes,
			UVSyncBytes:          uvSyncBytes,
			BandwidthBytesPerSec: bytesPerSec,
		})

		jobSetup := estimate.EstimateJobSetup(overheadModel, ctx)

		var runEst estimate.Estimate
		for _, job := range go_.Group.Jobs {
			if pred, ok := allPredictions[job.ID]; ok {
				est.JobDurations[job.ID] = pred.Mean
				runEst = runEst.Add(pred)
			} else {
				runEst = runEst.Add(estimate.DefaultJobDuration)
			}
			jobsDone++
			if onProgress != nil {
				onProgress("Estimating durations", jobsDone, len(allBatchJobs))
			}
		}

		upload := estimate.EstimateUpload(overheadModel, ctx)

		total := startup.Add(sshSetup).Add(provision).Add(jobSetup).Add(runEst).Add(upload)
		bd := estimate.Breakdown{
			Startup:   startup,
			SSHSetup:  sshSetup,
			JobSetup:  jobSetup,
			Provision: provision,
			Run:       runEst,
			Upload:    upload,
			Total:     total,
		}

		est.Breakdown = bd
		est.SetupOverhead = startup.Mean + sshSetup.Mean + provision.Mean + jobSetup.Mean
		est.DownloadBytes = downloadBytes
		est.UVSyncBytes = uvSyncBytes
		est.DownloadTime = estimate.TransferTime(downloadBytes, bytesPerSec).Mean
		est.TotalTime = total.Mean
		est.TotalCost = total.Mean.Hours() * go_.Offer.CostPerHour

		// Compute survival and risk-adjusted cost when model is available
		if go_.SurvivalProb > 0 {
			est.SurvivalProb = go_.SurvivalProb
			setupHrs := est.SetupOverhead.Hours()
			jobHrs := runEst.Mean.Hours()
			est.RiskAdjustedCost = bidding.ExpectedCost(go_.Offer.CostPerHour, jobHrs, setupHrs, go_.SurvivalProb)
		} else if survivalModel != nil && go_.Offer != nil {
			est.SurvivalProb = survivalModel.OfferSurvival(*go_.Offer)
			setupHrs := est.SetupOverhead.Hours()
			jobHrs := runEst.Mean.Hours()
			est.RiskAdjustedCost = bidding.ExpectedCost(go_.Offer.CostPerHour, jobHrs, setupHrs, est.SurvivalProb)
		}

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
