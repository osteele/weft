package campaign

import (
	"database/sql"
	"math"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
)

// StrategyPlan combines reuse assignments with the best new-instance
// candidate for a strategy. DisplayOffers/DisplayEstimates are aligned with the
// original split groups; ActualEstimates reflect the execution units that will
// actually run (reuse groups + winning new candidate groups).
type StrategyPlan struct {
	Strategy         bidding.SelectionStrategy
	Profile          bidding.ScoreProfile
	DisplayOffers    []GroupOffer
	DisplayEstimates []CostEstimate
	ActualEstimates  []CostEstimate
	NewCandidate     *CandidateResult
	ReuseAssignments []ReuseAssignment
}

func (p StrategyPlan) HasReuse() bool {
	return len(p.ReuseAssignments) > 0
}

func (p StrategyPlan) HasComplexExecution() bool {
	return p.HasReuse() || (p.NewCandidate != nil && p.NewCandidate.Label != "split")
}

type reuseGroupDecision struct {
	groupIndex int
	instance   InstanceCapacity
	estimate   CostEstimate
}

// BuildStrategyPlans builds a reusable/new-instance launch plan for each
// strategy. The split groups are the original launch groups shown in the UI.
func BuildStrategyPlans(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	strategies []bidding.SelectionStrategy,
	minSurvival float64,
) (map[bidding.SelectionStrategy]StrategyPlan, []GroupRawOffers) {
	profiles := make([]bidding.ScoreProfile, 0, len(strategies))
	for _, strategy := range strategies {
		profiles = append(profiles, strategy.Profile())
	}
	profilePlans, splitRaw := BuildProfilePlans(
		database,
		clients,
		splitGroups,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		profiles,
		minSurvival,
	)
	plans := make(map[bidding.SelectionStrategy]StrategyPlan, len(strategies))
	for _, strategy := range strategies {
		plan, ok := profilePlans[strategy.Profile().ID]
		if !ok {
			continue
		}
		plan.Strategy = strategy
		plans[strategy] = plan
	}
	return plans, splitRaw
}

// BuildProfilePlans builds reusable/new-instance launch plans for arbitrary
// score profiles, keyed by profile ID.
func BuildProfilePlans(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profiles []bidding.ScoreProfile,
	minSurvival float64,
) (map[string]StrategyPlan, []GroupRawOffers) {
	plans := make(map[string]StrategyPlan, len(profiles))
	if len(splitGroups) == 0 {
		return plans, nil
	}

	if reusable == nil && database != nil {
		if caps, err := FindReusableInstances(database); err == nil {
			reusable = caps
		}
	}

	splitRaw := make([]GroupRawOffers, len(splitGroups))
	for i, g := range splitGroups {
		splitRaw[i] = GroupRawOffers{Group: g}
	}
	var offerSession *offerSearchSession
	if len(clients) > 0 {
		offerSession = newOfferSearchSession(clients)
		splitRaw = offerSession.fetchGroupRawOffers(splitGroups)
	}

	return buildProfilePlansFromSplitRawWithSession(
		database,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		offerSession,
		profiles,
		minSurvival,
	), splitRaw
}

// BuildProfilePlansFromSplitRaw builds reusable/new-instance launch plans from
// pre-fetched split-group raw offers. This lets callers stage the initial
// offer search separately from the more expensive plan construction work.
func BuildProfilePlansFromSplitRaw(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	splitRaw []GroupRawOffers,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profiles []bidding.ScoreProfile,
	minSurvival float64,
) map[string]StrategyPlan {
	var offerSession *offerSearchSession
	switch {
	case len(clients) > 0:
		offerSession = newOfferSearchSession(clients)
		if len(splitRaw) == len(splitGroups) {
			offerSession.SeedRawOffers(splitRaw)
		} else {
			splitRaw = offerSession.fetchGroupRawOffers(splitGroups)
		}
	case len(splitRaw) != len(splitGroups):
		splitRaw = make([]GroupRawOffers, len(splitGroups))
		for i, g := range splitGroups {
			splitRaw[i] = GroupRawOffers{Group: g}
		}
	}

	return buildProfilePlansFromSplitRawWithSession(
		database,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		offerSession,
		profiles,
		minSurvival,
	)
}

func buildProfilePlansFromSplitRawWithSession(
	database *sql.DB,
	splitGroups []InstanceGroup,
	splitRaw []GroupRawOffers,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	offerSession *offerSearchSession,
	profiles []bidding.ScoreProfile,
	minSurvival float64,
) map[string]StrategyPlan {
	plans := make(map[string]StrategyPlan, len(profiles))
	if len(splitGroups) == 0 {
		return plans
	}

	for _, profile := range profiles {
		if !profile.Valid() {
			continue
		}
		plans[profile.ID] = buildStrategyPlanForSplitRaw(
			database,
			splitGroups,
			splitRaw,
			reusable,
			predCfg,
			overheadModel,
			survivalModel,
			offerSession,
			profile,
			minSurvival,
		)
	}

	return plans
}

func buildStrategyPlanForSplitRaw(
	database *sql.DB,
	splitGroups []InstanceGroup,
	splitRaw []GroupRawOffers,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	offerSession *offerSearchSession,
	profile bidding.ScoreProfile,
	minSurvival float64,
) StrategyPlan {
	plan := StrategyPlan{
		Profile:          profile,
		DisplayOffers:    make([]GroupOffer, len(splitGroups)),
		DisplayEstimates: make([]CostEstimate, len(splitGroups)),
	}
	for i, g := range splitGroups {
		plan.DisplayOffers[i] = GroupOffer{Group: g}
		plan.DisplayEstimates[i] = CostEstimate{Group: g, Offer: GroupOffer{Group: g}}
	}

	setupFactory := OfferSetupOverheadFactory(database, overheadModel)
	splitOffers := RankGroupOffersWithProfile(splitRaw, survivalModel, 1.0, setupFactory, profile, minSurvival)
	splitReferenceDLPerf := MedianDLPerfFromRawOffers(splitRaw)
	splitEstimates := EstimateCosts(database, splitOffers, predCfg, overheadModel, nil, survivalModel, splitReferenceDLPerf, nil)

	reuseDecisions := chooseReuseGroups(
		database,
		splitGroups,
		splitEstimates,
		reusable,
		predCfg,
		overheadModel,
		profile,
		splitReferenceDLPerf,
	)

	reused := make(map[int]reuseGroupDecision, len(reuseDecisions))
	for _, decision := range reuseDecisions {
		reused[decision.groupIndex] = decision
		plan.DisplayOffers[decision.groupIndex] = decision.estimate.Offer
		plan.DisplayEstimates[decision.groupIndex] = decision.estimate
		plan.ActualEstimates = append(plan.ActualEstimates, decision.estimate)
		for _, job := range splitGroups[decision.groupIndex].Jobs {
			plan.ReuseAssignments = append(plan.ReuseAssignments, ReuseAssignment{
				Job:      job,
				Instance: decision.instance,
			})
		}
	}

	var remainingGroups []InstanceGroup
	var remainingIdx []int
	for idx, group := range splitGroups {
		if _, ok := reused[idx]; ok {
			continue
		}
		remainingGroups = append(remainingGroups, group)
		remainingIdx = append(remainingIdx, idx)
	}

	if len(remainingGroups) == 0 || offerSession == nil {
		return plan
	}

	candidates := fetchCandidateGroupingsWithSession(offerSession, remainingGroups)
	result := BestCandidateForProfile(database, candidates, predCfg, overheadModel, survivalModel, profile, minSurvival)
	if len(result.Groups) == 0 {
		return plan
	}
	plan.NewCandidate = &result
	plan.ActualEstimates = append(plan.ActualEstimates, result.Estimates...)

	mappedOffers := MapOffersToSplitGroups(remainingGroups, result)
	mappedEstimates := MapEstimatesToSplitGroups(remainingGroups, result)
	for i, originalIdx := range remainingIdx {
		if i < len(mappedOffers) {
			plan.DisplayOffers[originalIdx] = mappedOffers[i]
		}
		if i < len(mappedEstimates) {
			plan.DisplayEstimates[originalIdx] = mappedEstimates[i]
		}
	}

	return plan
}

func chooseReuseGroups(
	database *sql.DB,
	splitGroups []InstanceGroup,
	splitEstimates []CostEstimate,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	profile bidding.ScoreProfile,
	referenceDLPerf float64,
) []reuseGroupDecision {
	if len(reusable) == 0 || len(splitGroups) == 0 {
		return nil
	}

	working := cloneInstanceCapacities(reusable)
	var decisions []reuseGroupDecision
	for idx, group := range splitGroups {
		newScore := math.Inf(1)
		if idx < len(splitEstimates) {
			newScore = ScoreEstimatesWithProfile([]CostEstimate{splitEstimates[idx]}, profile)
		}

		bestScore := math.Inf(1)
		bestIdx := -1
		var bestEstimate CostEstimate
		for i, cap := range working {
			est, ok := EstimateReuseGroup(database, group, cap, predCfg, overheadModel, referenceDLPerf)
			if !ok {
				continue
			}
			score := ScoreEstimatesWithProfile([]CostEstimate{est}, profile)
			if score < bestScore {
				bestScore = score
				bestIdx = i
				bestEstimate = est
			}
		}

		if bestIdx >= 0 && bestScore < newScore {
			decisions = append(decisions, reuseGroupDecision{
				groupIndex: idx,
				instance:   working[bestIdx],
				estimate:   bestEstimate,
			})
			consumeReuseGroup(&working[bestIdx], group)
		}
	}

	return decisions
}

func cloneInstanceCapacities(instances []InstanceCapacity) []InstanceCapacity {
	cloned := make([]InstanceCapacity, len(instances))
	for i, cap := range instances {
		cloned[i] = cap
		cloned[i].ProvisionedInputs = append([]string(nil), cap.ProvisionedInputs...)
	}
	return cloned
}

func consumeReuseGroup(cap *InstanceCapacity, group InstanceGroup) {
	if cap == nil {
		return
	}
	for _, job := range group.Jobs {
		consumeReuseJob(cap, job)
	}
}

// MatchGroupToInstance reports whether an instance can run every job in the
// group sequentially, updating a temporary copy of the instance capacity while
// checking disk and cached inputs.
func MatchGroupToInstance(group InstanceGroup, cap InstanceCapacity) (bool, string) {
	temp := cap
	temp.ProvisionedInputs = append([]string(nil), cap.ProvisionedInputs...)
	for _, job := range group.Jobs {
		ok, reason := MatchJobToInstance(job, temp)
		if !ok {
			return false, reason
		}
		consumeReuseJob(&temp, job)
	}
	return true, ""
}

// EstimateReuseGroup estimates the completion time and shared-cost impact of
// running a group on an existing instance.
func EstimateReuseGroup(
	database *sql.DB,
	group InstanceGroup,
	cap InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	referenceDLPerf float64,
) (CostEstimate, bool) {
	if ok, _ := MatchGroupToInstance(group, cap); !ok {
		return CostEstimate{}, false
	}

	inst := cap.Instance
	offer := reuseSyntheticOffer(cap)
	groupOffer := GroupOffer{
		Group:        group,
		Offer:        &offer,
		SurvivalProb: placement.DefaultReuseSurvival,
	}

	waitEst := estimate.Estimate{}
	if cap.RunningJobCount > 0 {
		waitSecs := float64(cap.RunningJobCount) * 30 * 60
		waitEst = estimate.FromSeconds(waitSecs, waitSecs*0.5, waitSecs*1.5)
	}

	incrementalInputs := subtractInputs(group.AllInputs(), cap.ProvisionedInputs)
	downloadBytes := int64(0)
	if totalBytes, err := dataloc.ResolveInputSizes(incrementalInputs, nil); err == nil {
		downloadBytes = totalBytes
	}

	ctx := estimate.InstanceContext{
		DataCenter:      inst.DataCenter,
		DLPerf:          inst.DLPerf,
		InetDownMbps:    inst.InetDownMbps,
		InetUpMbps:      inst.InetUpMbps,
		DownloadedBytes: downloadBytes,
	}

	bw := effectiveDownloadBandwidth(database, &offer, cloud.MbpsToBytesPerSec(inst.InetDownMbps))
	provision := estimate.EstimateProvision(estimate.ProvisionInput{
		ModelDownloadBytes:   downloadBytes,
		BandwidthBytesPerSec: bw,
	})
	jobSetup := estimate.EstimateJobSetup(overheadModel, ctx)
	upload := estimate.EstimateUpload(overheadModel, ctx)

	var runEst estimate.Estimate
	jobDurations := make(map[int64]time.Duration, len(group.Jobs))
	gpuLabel := offer.GPUName
	if gpuLabel == "" {
		gpuLabel = inst.GPUClass
	}
	for _, job := range group.Jobs {
		pred, _ := estimate.EstimateJobDuration(predCfg, gpuLabel, job)
		runEst = runEst.Add(pred)
		jobDurations[job.ID] = pred.Mean
	}
	if referenceDLPerf > 0 && inst.DLPerf > 0 {
		ratio := referenceDLPerf / inst.DLPerf
		runEst = runEst.Scale(ratio)
		for id, dur := range jobDurations {
			jobDurations[id] = time.Duration(float64(dur) * ratio)
		}
	}

	total := waitEst.Add(provision).Add(jobSetup).Add(runEst).Add(upload)
	costRate := offer.CostPerHour
	totalCost := total.Mean.Hours() * costRate
	setupHrs := waitEst.Mean.Hours() + provision.Mean.Hours() + jobSetup.Mean.Hours() + upload.Mean.Hours()

	est := CostEstimate{
		Group:        group,
		Offer:        groupOffer,
		Breakdown:    estimate.Breakdown{Provision: provision, JobSetup: jobSetup, Run: runEst, Upload: upload, Total: total},
		JobDurations: jobDurations,
		SetupOverhead: waitEst.Mean +
			provision.Mean +
			jobSetup.Mean +
			upload.Mean,
		DownloadBytes:    downloadBytes,
		DownloadTime:     estimate.TransferTime(downloadBytes, bw).Mean,
		TotalTime:        total.Mean,
		TotalCost:        totalCost,
		SurvivalProb:     placement.DefaultReuseSurvival,
		RiskAdjustedCost: bidding.ExpectedCost(costRate, runEst.Mean.Hours(), setupHrs, placement.DefaultReuseSurvival),
	}
	if costRate == 0 {
		est.RiskAdjustedCost = 0
	}
	return est, true
}

func reuseSyntheticOffer(cap InstanceCapacity) cloud.Offer {
	inst := cap.Instance
	costPerHour := 0.0
	if inst.Status != db.LaunchStatusGrace {
		costPerHour = float64(inst.CostPerHourCents) / 100.0
	}
	gpuName := inst.ResolvedGPUName
	if gpuName == "" {
		gpuName = inst.GPUClass
	}
	providerID := inst.ProviderInstanceID
	if providerID == "" {
		providerID = gpuName
	}
	return cloud.Offer{
		ProviderID:        providerID,
		Provider:          cloud.Provider(inst.Provider),
		GPUName:           gpuName,
		GPUMemGB:          float64(inst.GPUMemGB),
		CostPerHour:       costPerHour,
		DLPerf:            inst.DLPerf,
		Reliability:       inst.Reliability,
		DownloadBandwidth: inst.InetDownMbps,
		UploadBandwidth:   inst.InetUpMbps,
		DataCenter:        inst.DataCenter,
		MachineID:         inst.MachineID,
	}
}

func estimateCommonCompletionTime(est CostEstimate) time.Duration {
	total := est.TotalTime
	if total == 0 {
		total = est.Breakdown.Total.Mean
	}
	common := total - est.Breakdown.Run.Mean - est.Breakdown.Upload.Mean
	if common < 0 {
		return 0
	}
	return common
}

func estimateTotalJobCompletionHours(est CostEstimate) float64 {
	jobCount := len(est.Group.Jobs)
	if jobCount == 0 {
		return est.TotalTime.Hours()
	}

	common := estimateCommonCompletionTime(est)
	total := time.Duration(jobCount) * common

	if len(est.JobDurations) > 0 {
		runFallback := time.Duration(0)
		if jobCount > 0 {
			runFallback = time.Duration(float64(est.Breakdown.Run.Mean) / float64(jobCount))
		}
		cumulativeRun := time.Duration(0)
		for _, job := range est.Group.Jobs {
			dur, ok := est.JobDurations[job.ID]
			if !ok {
				dur = runFallback
			}
			cumulativeRun += dur
			total += cumulativeRun
		}
		return total.Hours()
	}

	runFactor := float64(jobCount+1) / 2
	return total.Hours() + est.Breakdown.Run.Mean.Hours()*runFactor
}

// ScoreEstimates evaluates execution units using strategy weights: total spend
// is summed, while the time term is the total completion time across all placed
// jobs, including shared setup and waiting behind earlier jobs on the same
// instance.
func ScoreEstimates(estimates []CostEstimate, strategy bidding.SelectionStrategy) float64 {
	return ScoreEstimatesWithProfile(estimates, strategy.Profile())
}

func ScoreEstimatesWithProfile(estimates []CostEstimate, profile bidding.ScoreProfile) float64 {
	w := profile.Weights()
	totalCost := 0.0
	totalCompletionTime := 0.0
	hasAny := false

	for _, est := range estimates {
		if est.Offer.Offer == nil {
			return math.Inf(1)
		}
		hasAny = true

		cost := est.TotalCost
		if est.RiskAdjustedCost > 0 {
			cost = est.RiskAdjustedCost
		}

		timeHours := estimateTotalJobCompletionHours(est)
		if !profile.UseHappyPathTime && est.SurvivalProb > 0 && est.SurvivalProb < 1 {
			timeHours /= est.SurvivalProb
		}

		totalCost += cost
		totalCompletionTime += timeHours
	}

	if !hasAny {
		return math.Inf(1)
	}
	return w.Cost*totalCost + w.Time*totalCompletionTime
}

func medianDLPerfFromCandidates(candidates []GroupingCandidate) float64 {
	var all []GroupRawOffers
	for _, cand := range candidates {
		all = append(all, cand.Raw...)
	}
	return MedianDLPerfFromRawOffers(all)
}

// BestCandidateForStrategy selects the candidate grouping with the lowest
// estimate-based weighted score for the given strategy.
func BestCandidateForStrategy(
	database *sql.DB,
	candidates []GroupingCandidate,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	strategy bidding.SelectionStrategy,
	minSurvival float64,
) CandidateResult {
	return BestCandidateForProfile(database, candidates, predCfg, overheadModel, survivalModel, strategy.Profile(), minSurvival)
}

// BestCandidateForProfile selects the candidate grouping with the lowest
// estimate-based weighted score for the given score profile.
func BestCandidateForProfile(
	database *sql.DB,
	candidates []GroupingCandidate,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profile bidding.ScoreProfile,
	minSurvival float64,
) CandidateResult {
	bestScore := math.Inf(1)
	var best CandidateResult
	if len(candidates) == 0 {
		return best
	}

	setupFactory := OfferSetupOverheadFactory(database, overheadModel)
	referenceDLPerf := medianDLPerfFromCandidates(candidates)
	for i, cand := range candidates {
		offers := RankGroupOffersWithProfile(cand.Raw, survivalModel, 1.0, setupFactory, profile, minSurvival)
		estimates := EstimateCosts(database, offers, predCfg, overheadModel, nil, survivalModel, referenceDLPerf, nil)
		score := ScoreEstimatesWithProfile(estimates, profile)
		if i == 0 || score < bestScore {
			bestScore = score
			best = CandidateResult{
				CandidateIdx: i,
				Label:        cand.Label,
				Groups:       cand.Groups,
				Offers:       offers,
				Estimates:    estimates,
			}
		}
	}
	return best
}
