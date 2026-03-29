package campaign

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/transferbw"
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
	SurvivalProb     float64 // 0-1, probability of completing without provider-side failure
	RiskAdjustedCost float64 // expected cost including retry overhead
}

// EstimateProgressFunc reports progress during estimation.
// phase describes what is happening, resolved/total track items.
type EstimateProgressFunc func(phase string, resolved, total int)

// EstimateCosts computes per-group cost estimates using the predictor for duration.
// If predCfg is nil or not configured, falls back to 1hr/job estimates.
// If r2Client is non-nil, UV manifests are fetched to estimate cold uv sync costs.
// If survivalModel is non-nil, computes survival probability and risk-adjusted cost.
// If referenceDLPerf > 0, run durations are scaled by referenceDLPerf/offerDLPerf
// to account for GPU performance differences across strategies.
func EstimateCosts(database *sql.DB, groupOffers []GroupOffer, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, r2Client *r2.Client, survivalModel *bidding.SurvivalModel, referenceDLPerf float64, onProgress EstimateProgressFunc) []CostEstimate {
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
			slog.Warn("could not resolve input sizes", "component", "cost", "error", err)
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

		staticBW := cloud.MbpsToBytesPerSec(go_.Offer.DownloadBandwidth)
		bytesPerSec := effectiveDownloadBandwidth(database, go_.Offer, staticBW)
		provision := estimate.EstimateProvision(estimate.ProvisionInput{
			ModelDownloadBytes:   downloadBytes,
			UVSyncBytes:          uvSyncBytes,
			BandwidthBytesPerSec: bytesPerSec,
		})

		ctx.DownloadedBytes = downloadBytes
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

		// Scale run durations by GPU performance when a reference is available.
		// The predictor returns similar estimates regardless of GPU class, so
		// we scale by DLPerf ratio to differentiate cheap vs fast GPUs.
		if referenceDLPerf > 0 && go_.Offer.DLPerf > 0 {
			ratio := referenceDLPerf / go_.Offer.DLPerf
			runEst = runEst.Scale(ratio)
			for id, dur := range est.JobDurations {
				est.JobDurations[id] = time.Duration(float64(dur) * ratio)
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

// OfferSetupOverhead builds an OfferSetupFunc that computes per-offer setup
// overhead using the overhead model and learned download bandwidth. If database
// or overheadModel is nil, returns a constant 0.5h fallback.
func OfferSetupOverhead(database *sql.DB, overheadModel *estimate.OverheadModel) bidding.OfferSetupFunc {
	if overheadModel == nil {
		return bidding.ConstantSetup(0.5)
	}
	return func(o cloud.Offer) float64 {
		ctx := estimate.InstanceContext{
			DataCenter:   o.DataCenter,
			DLPerf:       o.DLPerf,
			InetDownMbps: o.DownloadBandwidth,
			InetUpMbps:   o.UploadBandwidth,
		}
		startup := estimate.EstimateStartupWithModel(string(o.Provider), overheadModel, ctx)
		sshSetup := estimate.EstimateSSHSetup(overheadModel, ctx)
		jobSetup := estimate.EstimateJobSetup(overheadModel, ctx)
		return (startup.Mean + sshSetup.Mean + jobSetup.Mean).Hours()
	}
}

// effectiveDownloadBandwidth returns the learned HF download bandwidth for the
// offer's datacenter if available (≥2 observations), otherwise the static
// offer bandwidth.
func effectiveDownloadBandwidth(database *sql.DB, offer *cloud.Offer, staticBW float64) float64 {
	if database == nil || offer == nil || offer.DataCenter == "" {
		return staticBW
	}
	src := transferbw.HFEndpoint()
	dst := transferbw.CloudEndpoint(string(offer.Provider), offer.DataCenter, "")
	return transferbw.EffectiveBandwidth(database, src.Key(), dst.Key(), staticBW)
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

// CostEstimateSummary holds aggregated time and cost ranges from launch estimates,
// for display in the watch header.
type CostEstimateSummary struct {
	TimeMean, TimeLower, TimeUpper time.Duration
	CostMean, CostLower, CostUpper float64
}

// FormatLine returns a display line like "Estimate: time: ~45m (30m–1h)  cost: ~$1.50 ($0.80–$2.20)".
func (s *CostEstimateSummary) FormatLine() string {
	timeEst := estimate.Estimate{Mean: s.TimeMean, Lower: s.TimeLower, Upper: s.TimeUpper}
	timePart := formatDurationWithBounds(timeEst)

	costPart := fmt.Sprintf("~$%.2f", s.CostMean)
	if s.CostUpper-s.CostLower >= 0.01 && s.CostLower != s.CostMean {
		costPart = fmt.Sprintf("~$%.2f ($%.2f–$%.2f)", s.CostMean, s.CostLower, s.CostUpper)
	}

	return "Estimate: time: " + timePart + "  cost: " + costPart
}

// StrategySummaryRow holds aggregated cost/time for one strategy, for comparison display.
type StrategySummaryRow struct {
	Label     string            // display label, e.g. "cheap" or "fast/fastest"
	Active    bool              // true if the active strategy is in this row
	Disclosed bool              // true if detail rows should be shown below this row
	NumGPUs   int               // GPU groups with valid offers
	MaxTime   estimate.Estimate // max across groups (wall-clock parallel)
	TotalRate float64           // sum of $/hr across groups
	TotalCost float64           // sum of mean costs across groups
	CostLower float64           // sum of lower cost bounds
	CostUpper float64           // sum of upper cost bounds
	Loading   bool              // estimates not yet available
}

// SummarizeForComparison aggregates estimates into a StrategySummaryRow using
// max-time semantics (groups run in parallel) and sum-cost (total spend).
// Selection-aware: scales per group using selectedPerGroup.
// Returns nil if no valid offers.
func SummarizeForComparison(estimates []CostEstimate, selectedPerGroup []int) *StrategySummaryRow {
	row := &StrategySummaryRow{}
	hasAny := false
	for i, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true
		row.NumGPUs++

		totalJobs := len(est.Group.Jobs)
		_, scale := selectionScale(i, totalJobs, selectedPerGroup)

		scaledTime := est.Breakdown.Total.Scale(scale)
		scaledCost := est.TotalCost * scale
		lowerCost := scaledTime.Lower.Hours() * est.Offer.Offer.CostPerHour
		upperCost := scaledTime.Upper.Hours() * est.Offer.Offer.CostPerHour

		// Max time (wall-clock — groups run in parallel)
		if scaledTime.Mean > row.MaxTime.Mean {
			row.MaxTime.Mean = scaledTime.Mean
		}
		if scaledTime.Lower > row.MaxTime.Lower {
			row.MaxTime.Lower = scaledTime.Lower
		}
		if scaledTime.Upper > row.MaxTime.Upper {
			row.MaxTime.Upper = scaledTime.Upper
		}

		// Sum cost and rate
		row.TotalRate += est.Offer.Offer.CostPerHour
		row.TotalCost += scaledCost
		row.CostLower += lowerCost
		row.CostUpper += upperCost
	}
	if !hasAny {
		return nil
	}
	return row
}

// SummarizeEstimates aggregates cost estimates into a summary with time and cost ranges.
// Returns nil if estimates is empty or has no valid offers.
func SummarizeEstimates(estimates []CostEstimate) *CostEstimateSummary {
	var s CostEstimateSummary
	hasAny := false
	for _, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true
		total := est.Breakdown.Total
		s.TimeMean += total.Mean
		s.TimeLower += total.Lower
		s.TimeUpper += total.Upper
		s.CostMean += est.TotalCost
		s.CostLower += total.Lower.Hours() * est.Offer.Offer.CostPerHour
		s.CostUpper += total.Upper.Hours() * est.Offer.Offer.CostPerHour
	}
	if !hasAny {
		return nil
	}
	return &s
}
