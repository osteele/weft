package campaign

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	Group              InstanceGroup
	Offer              GroupOffer
	Breakdown          estimate.Breakdown
	JobDurations       map[int64]time.Duration // job ID → predicted duration (empty if unavailable)
	JobRuntimeMetadata map[int64]predictor.RuntimeMetadata
	SetupOverhead      time.Duration
	DownloadBytes      int64         // total bytes of HF model inputs to download
	DownloadTime       time.Duration // estimated download time from offer bandwidth
	UVSyncBytes        int64         // estimated cold uv sync download bytes
	TotalTime          time.Duration
	TotalCost          float64

	// Survival model fields (zero values if no model available)
	SurvivalProb     float64 // 0-1, probability of completing without provider-side failure
	RiskAdjustedCost float64 // expected cost including retry overhead
}

// EstimateProgressFunc reports progress during estimation.
// phase describes what is happening, resolved/total track items.
type EstimateProgressFunc func(phase string, resolved, total int)

// EstimateCosts computes per-group cost estimates using predictor-backed
// durations when available. Without predictions, feasible GPUs default to the
// same runtime baseline instead of local DLPerf scaling.
func EstimateCosts(database *sql.DB, groupOffers []GroupOffer, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, r2Client *r2.Client, survivalModel *bidding.SurvivalModel, onProgress EstimateProgressFunc) []CostEstimate {
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
	var allPredictions map[int64]estimate.DurationPrediction

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
		allPredictions = estimateJobDurationsDetailed(predCfg, allBatchJobs)
	}()

	wg.Wait()
	jobsDone := 0

	for i, go_ := range groupOffers {
		est := CostEstimate{
			Group:              go_.Group,
			Offer:              go_,
			JobDurations:       make(map[int64]time.Duration),
			JobRuntimeMetadata: make(map[int64]predictor.RuntimeMetadata),
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
				est.JobDurations[job.ID] = pred.Estimate.Mean
				runEst = runEst.Add(pred.Estimate)
				if pred.Metadata != nil {
					est.JobRuntimeMetadata[job.ID] = *pred.Metadata
				}
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

// OfferSetupOverheadFactory builds a SetupOverheadFactory that creates per-offer
// setup functions using the overhead model, learned download bandwidth, and
// per-group download sizes (resolved from declared inputs). Returns nil if
// overheadModel is nil (callers fall back to a constant 0.5h).
func OfferSetupOverheadFactory(database *sql.DB, overheadModel *estimate.OverheadModel) SetupOverheadFactory {
	if overheadModel == nil {
		return nil
	}
	var downloadBytesCache sync.Map
	return func(group InstanceGroup) bidding.OfferSetupFunc {
		key := groupInputCacheKey(group.AllInputs())
		downloadBytes := int64(0)
		if cached, ok := downloadBytesCache.Load(key); ok {
			downloadBytes = cached.(int64)
		} else {
			if totalBytes, err := dataloc.ResolveInputSizes(group.AllInputs(), nil); err != nil {
				slog.Warn("could not resolve input sizes for setup estimate", "component", "cost", "error", err)
			} else {
				downloadBytes = totalBytes
			}
			downloadBytesCache.Store(key, downloadBytes)
		}
		return offerSetupFunc(database, overheadModel, downloadBytes)
	}
}

func groupInputCacheKey(inputs []string) string {
	if len(inputs) == 0 {
		return ""
	}
	sorted := append([]string(nil), inputs...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x1f")
}

// offerSetupFunc builds an OfferSetupFunc that estimates total setup overhead
// (startup + SSH + job setup + provision) in hours for a given offer.
func offerSetupFunc(database *sql.DB, overheadModel *estimate.OverheadModel, downloadBytes int64) bidding.OfferSetupFunc {
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
		total := startup.Mean + sshSetup.Mean + jobSetup.Mean
		if downloadBytes > 0 {
			bw := effectiveDownloadBandwidth(database, &o, cloud.MbpsToBytesPerSec(o.DownloadBandwidth))
			provision := estimate.TransferTime(downloadBytes, bw)
			total += provision.Mean
		}
		return total.Hours()
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

// CollectGroupInputs gathers all unique HF inputs across groups.
// Used to pre-warm the HF model size cache before offers are available.
func CollectGroupInputs(groups []InstanceGroup) []string {
	seen := make(map[string]bool)
	var inputs []string
	for _, g := range groups {
		for _, input := range g.AllInputs() {
			if !seen[input] {
				seen[input] = true
				inputs = append(inputs, input)
			}
		}
	}
	return inputs
}

// collectAllInputs gathers unique inputs from groups that have a selected offer.
func collectAllInputs(groupOffers []GroupOffer) []string {
	var groups []InstanceGroup
	for _, go_ := range groupOffers {
		if go_.Offer != nil {
			groups = append(groups, go_.Group)
		}
	}
	return CollectGroupInputs(groups)
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
	MaxTime   estimate.Estimate // primary time metric shown in the comparison UI
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

		totalJobs := len(est.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)
		if selected == 0 {
			continue
		}

		hasAny = true
		row.NumGPUs++

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

func completionTimeEstimate(est CostEstimate, selectedJobs int) estimate.Estimate {
	totalJobs := len(est.Group.Jobs)
	if totalJobs <= 0 {
		if est.Breakdown.Total.Zero() && est.TotalTime > 0 {
			return estimate.Constant(est.TotalTime)
		}
		return est.Breakdown.Total
	}
	if selectedJobs <= 0 {
		return estimate.Estimate{}
	}
	if selectedJobs > totalJobs {
		selectedJobs = totalJobs
	}

	commonMean := estimateCommonCompletionTime(est)
	commonLower := est.Breakdown.Total.Lower - est.Breakdown.Run.Lower - est.Breakdown.Upload.Lower
	commonUpper := est.Breakdown.Total.Upper - est.Breakdown.Run.Upper - est.Breakdown.Upload.Upper
	if commonLower < 0 {
		commonLower = 0
	}
	if commonUpper < 0 {
		commonUpper = 0
	}

	result := estimate.Estimate{
		Mean:  time.Duration(selectedJobs) * commonMean,
		Lower: time.Duration(selectedJobs) * commonLower,
		Upper: time.Duration(selectedJobs) * commonUpper,
	}

	if len(est.JobDurations) > 0 {
		runLowerScale := 1.0
		runUpperScale := 1.0
		if est.Breakdown.Run.Mean > 0 {
			runLowerScale = float64(est.Breakdown.Run.Lower) / float64(est.Breakdown.Run.Mean)
			runUpperScale = float64(est.Breakdown.Run.Upper) / float64(est.Breakdown.Run.Mean)
		}

		runFallback := time.Duration(0)
		if totalJobs > 0 {
			runFallback = time.Duration(float64(est.Breakdown.Run.Mean) / float64(totalJobs))
		}

		var cumulativeMean, cumulativeLower, cumulativeUpper time.Duration
		for i := 0; i < selectedJobs && i < len(est.Group.Jobs); i++ {
			job := est.Group.Jobs[i]
			durMean, ok := est.JobDurations[job.ID]
			if !ok {
				durMean = runFallback
			}
			durLower := time.Duration(float64(durMean) * runLowerScale)
			durUpper := time.Duration(float64(durMean) * runUpperScale)

			cumulativeMean += durMean
			cumulativeLower += durLower
			cumulativeUpper += durUpper
			result.Mean += cumulativeMean
			result.Lower += cumulativeLower
			result.Upper += cumulativeUpper
		}
		return result
	}

	runFactor := float64(selectedJobs+1) / 2
	result.Mean += time.Duration(float64(est.Breakdown.Run.Mean) * runFactor)
	result.Lower += time.Duration(float64(est.Breakdown.Run.Lower) * runFactor)
	result.Upper += time.Duration(float64(est.Breakdown.Run.Upper) * runFactor)
	return result
}

// SummarizeTradeoffComparison aggregates estimates into a comparison row using
// total job completion time for the time term, including shared setup once per
// selected job and waiting behind earlier jobs on the same instance.
func SummarizeTradeoffComparison(estimates []CostEstimate, selectedPerGroup []int) *StrategySummaryRow {
	row := &StrategySummaryRow{}
	hasAny := false
	for i, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}

		totalJobs := len(est.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)
		if selected == 0 {
			continue
		}

		hasAny = true
		row.NumGPUs++
		row.MaxTime = row.MaxTime.Add(completionTimeEstimate(est, selected))

		scaledTime := est.Breakdown.Total.Scale(scale)
		scaledCost := est.TotalCost * scale
		lowerCost := scaledTime.Lower.Hours() * est.Offer.Offer.CostPerHour
		upperCost := scaledTime.Upper.Hours() * est.Offer.Offer.CostPerHour

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

// ApproximateEstimates builds lightweight CostEstimate entries from ranked
// group offers, using 1hr per job + 0.5hr setup as baseline. Used for display
// when the winning candidate differs from the split groups and detailed
// predictor-based estimates aren't available.
func ApproximateEstimates(offers []GroupOffer) []CostEstimate {
	estimates := make([]CostEstimate, 0, len(offers))
	for _, o := range offers {
		if o.Offer == nil {
			continue
		}
		numJobs := len(o.Group.Jobs)
		if numJobs < 1 {
			numJobs = 1
		}
		setupHrs := 0.5
		runHrs := float64(numJobs)
		totalHrs := runHrs + setupHrs
		totalTime := time.Duration(totalHrs * float64(time.Hour))
		totalCost := totalHrs * o.Offer.CostPerHour
		riskAdjustedCost := totalCost
		if o.SurvivalProb > 0 {
			riskAdjustedCost = bidding.ExpectedCost(o.Offer.CostPerHour, runHrs, setupHrs, o.SurvivalProb)
		}

		estimates = append(estimates, CostEstimate{
			Group: o.Group,
			Offer: o,
			Breakdown: estimate.Breakdown{
				Run: estimate.Estimate{
					Mean:  time.Duration(runHrs * float64(time.Hour)),
					Lower: time.Duration(runHrs * float64(time.Hour) * 0.5),
					Upper: time.Duration(runHrs * float64(time.Hour) * 2.0),
				},
				Total: estimate.Estimate{
					Mean:  totalTime,
					Lower: time.Duration(float64(totalTime) * 0.5),
					Upper: time.Duration(float64(totalTime) * 2.0),
				},
			},
			TotalTime:        totalTime,
			TotalCost:        totalCost,
			SurvivalProb:     o.SurvivalProb,
			RiskAdjustedCost: riskAdjustedCost,
		})
	}
	return estimates
}

// SummarizeGroupOffers builds an approximate StrategySummaryRow directly from
// ranked group offers. This is used when detailed estimates aren't available
// for the winning candidate's groups (e.g., when the parallel candidate wins
// and its groups differ from the split groups). Uses 1hr per job + 0.5hr setup
// as baseline estimates.
func SummarizeGroupOffers(offers []GroupOffer) *StrategySummaryRow {
	return SummarizeTradeoffExecutionEstimates(ApproximateEstimates(offers))
}

// SummarizeExecutionEstimates aggregates execution-unit estimates using
// max-time semantics (parallel groups) and sum-cost semantics.
func SummarizeExecutionEstimates(estimates []CostEstimate) *StrategySummaryRow {
	row := &StrategySummaryRow{}
	hasAny := false
	for _, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true
		row.NumGPUs++

		total := est.Breakdown.Total
		if total.Mean > row.MaxTime.Mean {
			row.MaxTime.Mean = total.Mean
		}
		if total.Lower > row.MaxTime.Lower {
			row.MaxTime.Lower = total.Lower
		}
		if total.Upper > row.MaxTime.Upper {
			row.MaxTime.Upper = total.Upper
		}

		row.TotalRate += est.Offer.Offer.CostPerHour
		row.TotalCost += est.TotalCost
		row.CostLower += total.Lower.Hours() * est.Offer.Offer.CostPerHour
		row.CostUpper += total.Upper.Hours() * est.Offer.Offer.CostPerHour
	}
	if !hasAny {
		return nil
	}
	return row
}

// SummarizeTradeoffExecutionEstimates aggregates execution-unit estimates using
// total job completion time for the time term and sum-cost semantics.
func SummarizeTradeoffExecutionEstimates(estimates []CostEstimate) *StrategySummaryRow {
	row := &StrategySummaryRow{}
	hasAny := false
	for _, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true
		row.NumGPUs++
		row.MaxTime = row.MaxTime.Add(completionTimeEstimate(est, len(est.Group.Jobs)))

		total := est.Breakdown.Total
		row.TotalRate += est.Offer.Offer.CostPerHour
		row.TotalCost += est.TotalCost
		row.CostLower += total.Lower.Hours() * est.Offer.Offer.CostPerHour
		row.CostUpper += total.Upper.Hours() * est.Offer.Offer.CostPerHour
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
