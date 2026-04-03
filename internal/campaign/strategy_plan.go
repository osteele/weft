package campaign

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
)

var resolvePredictBatch = predictor.ResolvePredictBatch
var estimateJobDurationsDetailed = estimate.EstimateJobDurationsDetailed
var estimateJobDurationsDetailedForReuse = estimate.EstimateJobDurationsDetailed

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

// PlanProgress reports coarse-grained progress while constructing launch plans.
type PlanProgress struct {
	Phase   string
	Lane    string
	Detail  string
	Current int
	Total   int
}

// PlanProgressFunc receives launch-plan progress updates.
type PlanProgressFunc func(PlanProgress)

type rawOfferEvaluation struct {
	raw              []GroupRawOffers
	offerPredictions map[int]map[string]offerRuntimePrediction
}

type groupingEvaluation struct {
	label   string
	groups  []InstanceGroup
	rawEval rawOfferEvaluation
}

type planEvaluator struct {
	database      *sql.DB
	predCfg       *predictor.Config
	overheadModel *estimate.OverheadModel
	survivalModel *bidding.SurvivalModel
	minSurvival   float64
	setupFactory  SetupOverheadFactory

	rawMu        sync.Mutex
	rawEvalCache map[string]map[int]map[string]offerRuntimePrediction

	estimateMu    sync.Mutex
	estimateCache map[string][]CostEstimate

	reuseMu        sync.Mutex
	reuseEstimates map[string]reuseEstimateCacheEntry
}

type reuseEstimateCacheEntry struct {
	estimate CostEstimate
	ok       bool
}

func newPlanEvaluator(
	database *sql.DB,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	minSurvival float64,
) *planEvaluator {
	return &planEvaluator{
		database:       database,
		predCfg:        predCfg,
		overheadModel:  overheadModel,
		survivalModel:  survivalModel,
		minSurvival:    minSurvival,
		setupFactory:   OfferSetupOverheadFactory(database, overheadModel),
		rawEvalCache:   make(map[string]map[int]map[string]offerRuntimePrediction),
		estimateCache:  make(map[string][]CostEstimate),
		reuseEstimates: make(map[string]reuseEstimateCacheEntry),
	}
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
	return BuildProfilePlansWithProgress(
		database,
		clients,
		splitGroups,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		profiles,
		minSurvival,
		nil,
	)
}

func BuildProfilePlansWithProgress(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profiles []bidding.ScoreProfile,
	minSurvival float64,
	onProgress PlanProgressFunc,
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
		onProgress,
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
	return BuildProfilePlansFromSplitRawWithProgress(
		database,
		clients,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		profiles,
		minSurvival,
		nil,
	)
}

func BuildProfilePlansFromSplitRawWithProgress(
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
	onProgress PlanProgressFunc,
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
		onProgress,
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
	onProgress PlanProgressFunc,
) map[string]StrategyPlan {
	plans := make(map[string]StrategyPlan, len(profiles))
	if len(splitGroups) == 0 {
		return plans
	}

	validProfiles := make([]bidding.ScoreProfile, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Valid() {
			validProfiles = append(validProfiles, profile)
		}
	}
	if len(validProfiles) == 0 {
		return plans
	}

	evaluator := newPlanEvaluator(database, predCfg, overheadModel, survivalModel, minSurvival)
	splitEval := evaluator.evaluateRawOffers(splitRaw)

	workerLimit := planProfileWorkerLimit(len(validProfiles))
	sem := make(chan struct{}, workerLimit)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for idx, profile := range validProfiles {
		wg.Add(1)
		go func(idx int, profile bidding.ScoreProfile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			progressLabel := profileProgressLabel(profile.ID, idx+1, len(validProfiles))
			reportPlanProgressForLane(onProgress, "Planning tradeoff profiles", progressLabel, "", idx+1, len(validProfiles))
			plan := buildStrategyPlanForSplitRaw(
				splitGroups,
				splitEval,
				reusable,
				evaluator,
				offerSession,
				profile,
				onProgress,
				progressLabel,
			)

			mu.Lock()
			plans[profile.ID] = plan
			mu.Unlock()
		}(idx, profile)
	}
	wg.Wait()

	return plans
}

func planProfileWorkerLimit(totalProfiles int) int {
	switch {
	case totalProfiles <= 1:
		return 1
	case totalProfiles == 2:
		return 2
	default:
		return 3
	}
}

func buildStrategyPlanForSplitRaw(
	splitGroups []InstanceGroup,
	splitEval rawOfferEvaluation,
	reusable []InstanceCapacity,
	evaluator *planEvaluator,
	offerSession *offerSearchSession,
	profile bidding.ScoreProfile,
	onProgress PlanProgressFunc,
	progressLabel string,
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

	reportPlanProgressForLane(onProgress, "Ranking direct-offer groups", progressLabel, "", 0, 0)
	splitOffers, splitPredictions := evaluator.selectOffers(splitEval, profile)
	reportPlanProgressForLane(onProgress, "Estimating direct-offer costs", progressLabel, "", 0, 0)
	splitEstimates := evaluator.estimateSelectedOffers(splitOffers, splitPredictions)

	reportPlanProgressForLane(onProgress, "Checking reusable instances", progressLabel, "", 0, 0)
	reuseDecisions := chooseReuseGroups(
		splitGroups,
		splitEstimates,
		reusable,
		evaluator,
		profile,
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

	reportPlanProgressForLane(onProgress, "Fetching merged and parallel candidates", progressLabel, "", 0, 0)
	candidates := fetchCandidateGroupingsWithSession(offerSession, remainingGroups)
	result := bestCandidateForProfileEvaluations(
		evaluator,
		evaluator.evaluateCandidateGroupings(candidates),
		profile,
		onProgress,
		progressLabel,
	)
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

func (e *planEvaluator) evaluateRawOffers(raw []GroupRawOffers) rawOfferEvaluation {
	key := rawOfferEvaluationCacheKey(raw)

	e.rawMu.Lock()
	cached, ok := e.rawEvalCache[key]
	e.rawMu.Unlock()
	if ok {
		return rawOfferEvaluation{raw: raw, offerPredictions: cached}
	}

	predictions := predictOfferRuntimes(raw, e.predCfg)

	e.rawMu.Lock()
	if cached, ok := e.rawEvalCache[key]; ok {
		e.rawMu.Unlock()
		return rawOfferEvaluation{raw: raw, offerPredictions: cached}
	}
	e.rawEvalCache[key] = predictions
	e.rawMu.Unlock()

	return rawOfferEvaluation{raw: raw, offerPredictions: predictions}
}

func (e *planEvaluator) selectOffers(eval rawOfferEvaluation, profile bidding.ScoreProfile) ([]GroupOffer, []offerRuntimePrediction) {
	return rankGroupOffersFromPredictions(eval.raw, eval.offerPredictions, e.survivalModel, e.setupFactory, profile, e.minSurvival)
}

func (e *planEvaluator) estimateSelectedOffers(groupOffers []GroupOffer, selected []offerRuntimePrediction) []CostEstimate {
	key := selectedOfferSetCacheKey(groupOffers)
	if key != "" {
		e.estimateMu.Lock()
		cached, ok := e.estimateCache[key]
		e.estimateMu.Unlock()
		if ok {
			return append([]CostEstimate(nil), cached...)
		}
	}

	estimates := EstimateCostsWithRuntimePredictions(
		e.database,
		groupOffers,
		selected,
		e.predCfg,
		e.overheadModel,
		nil,
		e.survivalModel,
		nil,
	)
	estimates = applySelectedOfferRuntimePredictions(estimates, selected)

	if key != "" {
		e.estimateMu.Lock()
		if _, ok := e.estimateCache[key]; !ok {
			e.estimateCache[key] = append([]CostEstimate(nil), estimates...)
		}
		e.estimateMu.Unlock()
	}

	return estimates
}

func (e *planEvaluator) evaluateCandidateGroupings(candidates []GroupingCandidate) []groupingEvaluation {
	evaluations := make([]groupingEvaluation, len(candidates))
	if len(candidates) == 0 {
		return evaluations
	}

	workerLimit := planProfileWorkerLimit(len(candidates))
	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup

	for idx, cand := range candidates {
		wg.Add(1)
		go func(idx int, cand GroupingCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			evaluations[idx] = groupingEvaluation{
				label:   cand.Label,
				groups:  cand.Groups,
				rawEval: e.evaluateRawOffers(cand.Raw),
			}
		}(idx, cand)
	}
	wg.Wait()
	return evaluations
}

func (e *planEvaluator) estimateReuseGroup(group InstanceGroup, cap InstanceCapacity) (CostEstimate, bool) {
	key := reuseEstimateCacheKey(group, cap)

	e.reuseMu.Lock()
	if cached, ok := e.reuseEstimates[key]; ok {
		e.reuseMu.Unlock()
		return cached.estimate, cached.ok
	}

	est, ok := EstimateReuseGroup(e.database, group, cap, e.predCfg, e.overheadModel)
	e.reuseEstimates[key] = reuseEstimateCacheEntry{estimate: est, ok: ok}
	e.reuseMu.Unlock()
	return est, ok
}

func reuseEstimateCacheKey(group InstanceGroup, cap InstanceCapacity) string {
	jobIDs := make([]string, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil {
			jobIDs = append(jobIDs, "0")
			continue
		}
		jobIDs = append(jobIDs, fmt.Sprintf("%d", job.ID))
	}
	inputs := append([]string(nil), cap.ProvisionedInputs...)
	return strings.Join([]string{
		strings.Join(jobIDs, ","),
		fmt.Sprintf("%d", cap.Instance.ID),
		fmt.Sprintf("%d", cap.DiskFreeGB),
		fmt.Sprintf("%d", cap.RunningJobCount),
		fmt.Sprintf("%d", int64(cap.GraceRemaining/time.Second)),
		strings.Join(inputs, ","),
	}, "|")
}

func rawOfferEvaluationCacheKey(raw []GroupRawOffers) string {
	if len(raw) == 0 {
		return ""
	}

	parts := make([]string, len(raw))
	for i, groupRaw := range raw {
		jobIDs := make([]string, 0, len(groupRaw.Group.Jobs))
		for _, job := range groupRaw.Group.Jobs {
			if job == nil {
				jobIDs = append(jobIDs, "0")
				continue
			}
			jobIDs = append(jobIDs, fmt.Sprintf("%d", job.ID))
		}
		offerKeys := make([]string, len(groupRaw.Offers))
		for j, offer := range groupRaw.Offers {
			offerKeys[j] = offerPredictionKey(offer)
		}
		parts[i] = strings.Join(jobIDs, ",") + "=>" + strings.Join(offerKeys, ",")
	}

	return strings.Join(parts, "||")
}

func selectedOfferSetCacheKey(groupOffers []GroupOffer) string {
	if len(groupOffers) == 0 {
		return ""
	}

	keys := make([]string, len(groupOffers))
	for i, groupOffer := range groupOffers {
		if groupOffer.Offer == nil {
			continue
		}
		keys[i] = groupOffer.Offer.Key()
	}
	return strings.Join(keys, "|")
}

type offerRuntimePrediction struct {
	totalRunHrs          float64
	adjustedRunHrs       float64
	jobEstimates         []estimate.Estimate
	jobDurations         []time.Duration
	adjustedJobDurations []time.Duration
	metadata             []*predictor.RuntimeMetadata
	feasible             bool
	complete             bool
}

func rankGroupOffersForPlanning(
	raw []GroupRawOffers,
	predCfg *predictor.Config,
	survivalModel *bidding.SurvivalModel,
	setupFactory SetupOverheadFactory,
	profile bidding.ScoreProfile,
	minSurvival float64,
) []GroupOffer {
	offers, _ := rankGroupOffersForPlanningWithPredictions(raw, predCfg, survivalModel, setupFactory, profile, minSurvival)
	return offers
}

func rankGroupOffersForPlanningWithPredictions(
	raw []GroupRawOffers,
	predCfg *predictor.Config,
	survivalModel *bidding.SurvivalModel,
	setupFactory SetupOverheadFactory,
	profile bidding.ScoreProfile,
	minSurvival float64,
) ([]GroupOffer, []offerRuntimePrediction) {
	predicted := predictOfferRuntimes(raw, predCfg)
	return rankGroupOffersFromPredictions(raw, predicted, survivalModel, setupFactory, profile, minSurvival)
}

func rankGroupOffersFromPredictions(
	raw []GroupRawOffers,
	predicted map[int]map[string]offerRuntimePrediction,
	survivalModel *bidding.SurvivalModel,
	setupFactory SetupOverheadFactory,
	profile bidding.ScoreProfile,
	minSurvival float64,
) ([]GroupOffer, []offerRuntimePrediction) {
	results := make([]GroupOffer, len(raw))
	selected := make([]offerRuntimePrediction, len(raw))
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}

		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}

		if offerPredictions, ok := predicted[i]; ok {
			if ranked, ok := rankOfferWithPredictedRuntime(
				r.Group,
				r.Offers,
				survivalModel,
				setupOverhead,
				profile,
				minSurvival,
				offerPredictions,
			); ok {
				results[i] = ranked
				if ranked.Offer != nil {
					selected[i] = offerPredictions[offerPredictionKey(*ranked.Offer)]
				}
				continue
			}
		}

		neutral := neutralOfferRuntimePredictions(r.Group, r.Offers)
		results[i], _ = rankOfferWithPredictedRuntime(
			r.Group,
			r.Offers,
			survivalModel,
			setupOverhead,
			profile,
			minSurvival,
			neutral,
		)
		if results[i].Offer != nil {
			selected[i] = neutral[offerPredictionKey(*results[i].Offer)]
		}
	}
	return results, selected
}

func neutralOfferRuntimePredictions(group InstanceGroup, offers []cloud.Offer) map[string]offerRuntimePrediction {
	jobCount := len(group.Jobs)
	if jobCount < 1 {
		jobCount = 1
	}
	jobEstimates := make([]estimate.Estimate, jobCount)
	jobDurations := make([]time.Duration, jobCount)
	totalRunHrs := 0.0
	for i := range jobDurations {
		jobEstimates[i] = estimate.DefaultJobDuration
		jobDurations[i] = estimate.DefaultJobDuration.Mean
		totalRunHrs += estimate.DefaultJobDuration.Mean.Hours()
	}

	predicted := make(map[string]offerRuntimePrediction, len(offers))
	for _, offer := range offers {
		predicted[offerPredictionKey(offer)] = offerRuntimePrediction{
			totalRunHrs:          totalRunHrs,
			jobEstimates:         append([]estimate.Estimate(nil), jobEstimates...),
			adjustedRunHrs:       totalRunHrs,
			jobDurations:         append([]time.Duration(nil), jobDurations...),
			adjustedJobDurations: append([]time.Duration(nil), jobDurations...),
			feasible:             true,
			complete:             true,
		}
	}
	return predicted
}

func predictOfferRuntimes(
	raw []GroupRawOffers,
	predCfg *predictor.Config,
) map[int]map[string]offerRuntimePrediction {
	if predCfg == nil || !predCfg.Configured() {
		return nil
	}

	type batchRef struct {
		groupIdx int
		offerKey string
		jobIdx   int
	}
	type runtimeAccum struct {
		jobEstimates []estimate.Estimate
		jobDurations []time.Duration
		metadata     []*predictor.RuntimeMetadata
		count        int
		feasible     bool
	}

	expected := make(map[int]map[string]int)
	refs := make(map[int64]batchRef)
	batchJobs := make([]predictor.BatchJob, 0)
	var nextID int64 = 1

	for groupIdx, groupRaw := range raw {
		if len(groupRaw.Group.Jobs) == 0 {
			continue
		}
		expected[groupIdx] = make(map[string]int)
		for _, offer := range groupRaw.Offers {
			key := offerPredictionKey(offer)
			expected[groupIdx][key] = len(groupRaw.Group.Jobs)
			for jobIdx, job := range groupRaw.Group.Jobs {
				if job == nil || job.Command == "" {
					continue
				}
				batchJobs = append(batchJobs, predictor.BatchJob{
					ID:       nextID,
					Command:  job.Command,
					Project:  job.Project,
					GPUClass: offer.GPUName,
				})
				refs[nextID] = batchRef{groupIdx: groupIdx, offerKey: key, jobIdx: jobIdx}
				nextID++
			}
		}
	}

	if len(batchJobs) == 0 {
		return nil
	}

	results, err := resolvePredictBatch(*predCfg, batchJobs)
	if err != nil || results == nil {
		return nil
	}

	accums := make(map[int]map[string]*runtimeAccum)
	for id, ref := range refs {
		result := results[id]
		if result == nil || result.DurationS == nil {
			continue
		}

		groupAccums := accums[ref.groupIdx]
		if groupAccums == nil {
			groupAccums = make(map[string]*runtimeAccum)
			accums[ref.groupIdx] = groupAccums
		}

		accum := groupAccums[ref.offerKey]
		if accum == nil {
			accum = &runtimeAccum{
				jobEstimates: make([]estimate.Estimate, expected[ref.groupIdx][ref.offerKey]),
				jobDurations: make([]time.Duration, expected[ref.groupIdx][ref.offerKey]),
				metadata:     make([]*predictor.RuntimeMetadata, expected[ref.groupIdx][ref.offerKey]),
				feasible:     true,
			}
			groupAccums[ref.offerKey] = accum
		}

		accum.jobEstimates[ref.jobIdx] = estimate.FromSeconds(
			result.DurationS.Mean,
			result.DurationS.Lower,
			result.DurationS.Upper,
		)
		accum.jobDurations[ref.jobIdx] = time.Duration(result.DurationS.Mean * float64(time.Second))
		accum.metadata[ref.jobIdx] = result.DurationMetadata
		if result.DurationMetadata != nil && result.DurationMetadata.Feasible != nil && !*result.DurationMetadata.Feasible {
			accum.feasible = false
		}
		accum.count++
	}

	predicted := make(map[int]map[string]offerRuntimePrediction)
	for groupIdx, groupAccums := range accums {
		predicted[groupIdx] = make(map[string]offerRuntimePrediction, len(groupAccums))
		for key, accum := range groupAccums {
			complete := accum.count == expected[groupIdx][key]
			totalRunHrs := 0.0
			if complete {
				for _, dur := range accum.jobDurations {
					if dur <= 0 {
						complete = false
						break
					}
					totalRunHrs += dur.Hours()
				}
			}
			predicted[groupIdx][key] = offerRuntimePrediction{
				totalRunHrs:  totalRunHrs,
				jobEstimates: append([]estimate.Estimate(nil), accum.jobEstimates...),
				jobDurations: append([]time.Duration(nil), accum.jobDurations...),
				metadata:     append([]*predictor.RuntimeMetadata(nil), accum.metadata...),
				feasible:     accum.feasible,
				complete:     complete,
			}
		}
	}

	applyRuntimeMetadataAdjustments(predicted)
	return predicted
}

func applyRuntimeMetadataAdjustments(predicted map[int]map[string]offerRuntimePrediction) {
	for groupIdx, offerPredictions := range predicted {
		jobCount := 0
		for _, pred := range offerPredictions {
			if len(pred.jobDurations) > jobCount {
				jobCount = len(pred.jobDurations)
			}
		}
		if jobCount == 0 {
			continue
		}

		neutralByJob := make([]time.Duration, jobCount)
		for jobIdx := 0; jobIdx < jobCount; jobIdx++ {
			var samples []time.Duration
			var trustedSamples []time.Duration
			for _, pred := range offerPredictions {
				if !pred.complete || !pred.feasible || jobIdx >= len(pred.jobDurations) {
					continue
				}
				if pred.jobDurations[jobIdx] <= 0 {
					continue
				}
				samples = append(samples, pred.jobDurations[jobIdx])
				if runtimePredictionConfidence(metadataAt(pred.metadata, jobIdx)) >= 0.5 {
					trustedSamples = append(trustedSamples, pred.jobDurations[jobIdx])
				}
			}
			switch {
			case len(trustedSamples) > 0:
				neutralByJob[jobIdx] = minDuration(trustedSamples)
			case len(samples) == 0:
				neutralByJob[jobIdx] = estimate.DefaultJobDuration.Mean
			default:
				neutralByJob[jobIdx] = medianDuration(samples)
			}
		}

		for key, pred := range offerPredictions {
			offerPredictions[key] = adjustRuntimePrediction(pred, neutralByJob)
		}
		predicted[groupIdx] = offerPredictions
	}
}

func adjustRuntimePrediction(pred offerRuntimePrediction, neutralByJob []time.Duration) offerRuntimePrediction {
	if !pred.complete || !pred.feasible {
		return pred
	}

	adjusted := make([]time.Duration, len(pred.jobDurations))
	totalAdjustedHrs := 0.0
	for jobIdx, dur := range pred.jobDurations {
		neutral := estimate.DefaultJobDuration.Mean
		if jobIdx < len(neutralByJob) && neutralByJob[jobIdx] > 0 {
			neutral = neutralByJob[jobIdx]
		}
		confidence := runtimePredictionConfidence(nil)
		if jobIdx < len(pred.metadata) {
			confidence = runtimePredictionConfidence(pred.metadata[jobIdx])
		}
		adjusted[jobIdx] = blendDuration(neutral, dur, confidence)
		totalAdjustedHrs += adjusted[jobIdx].Hours()
	}
	pred.adjustedJobDurations = adjusted
	pred.adjustedRunHrs = totalAdjustedHrs
	return pred
}

func metadataAt(metadata []*predictor.RuntimeMetadata, jobIdx int) *predictor.RuntimeMetadata {
	if jobIdx < 0 || jobIdx >= len(metadata) {
		return nil
	}
	return metadata[jobIdx]
}

func runtimePredictionConfidence(metadata *predictor.RuntimeMetadata) float64 {
	if metadata == nil {
		return 0.25
	}
	confidence := metadata.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	multiplier := 0.35
	switch metadata.Source {
	case "empirical":
		return 1.0
	case "learned+analytical":
		multiplier = 1.0
	case "learned":
		multiplier = 0.5
	}
	confidence *= multiplier

	if metadata.Bottleneck == "memory_capacity" {
		switch {
		case metadata.Feasible != nil && !*metadata.Feasible:
			confidence = max(confidence, 0.95)
		case metadata.BenefitsFromAdditionalVRAM != nil && *metadata.BenefitsFromAdditionalVRAM:
			confidence = max(confidence, 0.8)
		default:
			confidence = max(confidence, 0.7)
		}
		return clamp01(confidence)
	}

	if metadata.Bottleneck == "unknown" && metadata.Source != "empirical" {
		confidence *= 0.5
		if metadata.BenefitsFromAdditionalVRAM != nil && !*metadata.BenefitsFromAdditionalVRAM {
			confidence *= oversizedVRAMConfidenceFactor(metadata.MemoryHeadroomMiB)
		}
	}

	return clamp01(confidence)
}

func minDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	best := durations[0]
	for _, dur := range durations[1:] {
		if dur < best {
			best = dur
		}
	}
	return best
}

func oversizedVRAMConfidenceFactor(headroomMiB float64) float64 {
	switch {
	case headroomMiB >= 64*1024:
		return 0.1
	case headroomMiB >= 32*1024:
		return 0.2
	case headroomMiB >= 16*1024:
		return 0.35
	case headroomMiB >= 8*1024:
		return 0.5
	default:
		return 0.75
	}
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func blendDuration(neutral, predicted time.Duration, confidence float64) time.Duration {
	if confidence <= 0 {
		return neutral
	}
	if confidence >= 1 {
		return predicted
	}
	neutralSeconds := neutral.Seconds()
	predictedSeconds := predicted.Seconds()
	return time.Duration((neutralSeconds + confidence*(predictedSeconds-neutralSeconds)) * float64(time.Second))
}

func medianDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	values := append([]time.Duration(nil), durations...)
	slices.Sort(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return time.Duration((values[mid-1] + values[mid]) / 2)
}

func rankOfferWithPredictedRuntime(
	group InstanceGroup,
	offers []cloud.Offer,
	survivalModel *bidding.SurvivalModel,
	setupOverhead bidding.OfferSetupFunc,
	profile bidding.ScoreProfile,
	minSurvival float64,
	predicted map[string]offerRuntimePrediction,
) (GroupOffer, bool) {
	result := GroupOffer{Group: group}
	if len(offers) == 0 {
		return result, true
	}

	offers, _ = filterOffersByCUDACompat(offers, group.Image)
	if len(offers) == 0 {
		return result, true
	}

	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	if len(filtered) == 0 {
		return result, true
	}

	w := profile.Weights()
	bestScore := math.Inf(1)
	var best cloud.Offer
	found := false

	for _, offer := range filtered {
		runtime, ok := predicted[offerPredictionKey(offer)]
		if !ok || !runtime.complete || !runtime.feasible {
			continue
		}

		setup := setupOverhead(offer)
		surv := 1.0
		if survivalModel != nil {
			surv = survivalModel.OfferSurvival(offer)
		}

		runHrs := runtime.totalRunHrs
		jobDurations := runtime.jobDurations
		if runtime.adjustedRunHrs > 0 {
			runHrs = runtime.adjustedRunHrs
		}
		if len(runtime.adjustedJobDurations) > 0 {
			jobDurations = runtime.adjustedJobDurations
		}

		cost := bidding.ExpectedCost(offer.CostPerHour, runHrs, setup, surv)
		completionHrs := predictedCompletionHours(jobDurations, setup)
		if !profile.UseHappyPathTime && surv > 0 && surv < 1 {
			completionHrs /= surv
		}

		score := w.Cost*cost + w.Time*completionHrs
		if !found || score < bestScore {
			bestScore = score
			best = offer
			result.SurvivalProb = surv
			found = true
		}
	}

	if !found {
		return GroupOffer{}, false
	}

	result.Offer = &best
	return result, true
}

func predictedCompletionHours(jobDurations []time.Duration, setupHrs float64) float64 {
	total := float64(len(jobDurations)) * setupHrs
	cumulative := 0.0
	for _, dur := range jobDurations {
		cumulative += dur.Hours()
		total += cumulative
	}
	return total
}

func offerPredictionKey(offer cloud.Offer) string {
	return fmt.Sprintf(
		"%s|%s|%s|%.6f|%.6f|%.6f|%d",
		offer.Provider,
		offer.ProviderID,
		offer.GPUName,
		offer.GPUMemGB,
		offer.CostPerHour,
		offer.DLPerf,
		offer.NumGPUs,
	)
}

func chooseReuseGroups(
	splitGroups []InstanceGroup,
	splitEstimates []CostEstimate,
	reusable []InstanceCapacity,
	evaluator *planEvaluator,
	profile bidding.ScoreProfile,
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
			est, ok := evaluator.estimateReuseGroup(group, cap)
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
	jobRuntimeMetadata := make(map[int64]predictor.RuntimeMetadata, len(group.Jobs))
	jobDurationList := make([]time.Duration, 0, len(group.Jobs))
	jobMetadataList := make([]*predictor.RuntimeMetadata, 0, len(group.Jobs))
	runtimePred := offerRuntimePrediction{feasible: true, complete: true}
	gpuLabel := offer.GPUName
	if gpuLabel == "" {
		gpuLabel = inst.GPUClass
	}
	reusePredictions := estimateReuseDurationsDetailed(predCfg, gpuLabel, group.Jobs)
	for idx, job := range group.Jobs {
		pred := estimate.DurationPrediction{Estimate: estimate.DefaultJobDuration}
		if got, ok := reusePredictions[int64(idx+1)]; ok {
			pred = got
		}
		runEst = runEst.Add(pred.Estimate)
		jobDurations[job.ID] = pred.Estimate.Mean
		jobDurationList = append(jobDurationList, pred.Estimate.Mean)
		if pred.Estimate.Mean <= 0 {
			runtimePred.complete = false
		}
		if pred.Metadata != nil {
			jobRuntimeMetadata[job.ID] = *pred.Metadata
			if pred.Metadata.Feasible != nil && !*pred.Metadata.Feasible {
				runtimePred.feasible = false
			}
		}
		jobMetadataList = append(jobMetadataList, pred.Metadata)
	}
	runtimePred.jobDurations = jobDurationList
	runtimePred.metadata = jobMetadataList
	runtimePred.totalRunHrs = runEst.Mean.Hours()
	runtimePred = adjustRuntimePrediction(runtimePred, makeDefaultNeutralDurations(len(group.Jobs)))
	if !runtimePred.feasible {
		return CostEstimate{}, false
	}

	total := waitEst.Add(provision).Add(jobSetup).Add(runEst).Add(upload)
	costRate := offer.CostPerHour
	totalCost := total.Mean.Hours() * costRate
	setupHrs := waitEst.Mean.Hours() + provision.Mean.Hours() + jobSetup.Mean.Hours() + upload.Mean.Hours()

	est := CostEstimate{
		Group:              group,
		Offer:              groupOffer,
		Breakdown:          estimate.Breakdown{Provision: provision, JobSetup: jobSetup, Run: runEst, Upload: upload, Total: total},
		JobDurations:       jobDurations,
		JobRuntimeMetadata: jobRuntimeMetadata,
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
	est = applySelectedOfferRuntimePrediction(est, runtimePred)
	return est, true
}

func estimateReuseDurationsDetailed(predCfg *predictor.Config, gpuLabel string, jobs []*db.Job) map[int64]estimate.DurationPrediction {
	if predCfg == nil || !predCfg.Configured() || len(jobs) == 0 {
		return nil
	}

	batchJobs := make([]predictor.BatchJob, 0, len(jobs))
	for idx, job := range jobs {
		if job == nil || job.Command == "" {
			continue
		}
		batchJobs = append(batchJobs, predictor.BatchJob{
			ID:       int64(idx + 1),
			Command:  job.Command,
			Project:  job.Project,
			GPUClass: gpuLabel,
		})
	}
	if len(batchJobs) == 0 {
		return nil
	}
	return estimateJobDurationsDetailedForReuse(predCfg, batchJobs)
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
	return BestCandidateForProfileWithProgress(database, candidates, predCfg, overheadModel, survivalModel, profile, minSurvival, nil, "")
}

func BestCandidateForProfileWithProgress(
	database *sql.DB,
	candidates []GroupingCandidate,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profile bidding.ScoreProfile,
	minSurvival float64,
	onProgress PlanProgressFunc,
	progressLabel string,
) CandidateResult {
	evaluator := newPlanEvaluator(database, predCfg, overheadModel, survivalModel, minSurvival)
	return bestCandidateForProfileEvaluations(
		evaluator,
		evaluator.evaluateCandidateGroupings(candidates),
		profile,
		onProgress,
		progressLabel,
	)
}

func bestCandidateForProfileEvaluations(
	evaluator *planEvaluator,
	candidates []groupingEvaluation,
	profile bidding.ScoreProfile,
	onProgress PlanProgressFunc,
	progressLabel string,
) CandidateResult {
	bestScore := math.Inf(1)
	var best CandidateResult
	if len(candidates) == 0 {
		return best
	}

	for i, cand := range candidates {
		reportPlanProgressForLane(onProgress, "Scoring candidate groupings", progressLabel, cand.label, i+1, len(candidates))
		offers, selectedPredictions := evaluator.selectOffers(cand.rawEval, profile)
		estimates := evaluator.estimateSelectedOffers(offers, selectedPredictions)
		score := ScoreEstimatesWithProfile(estimates, profile)
		if i == 0 || score < bestScore {
			bestScore = score
			best = CandidateResult{
				CandidateIdx: i,
				Label:        cand.label,
				Groups:       cand.groups,
				Offers:       offers,
				Estimates:    estimates,
			}
		}
	}
	return best
}

func reportPlanProgress(onProgress PlanProgressFunc, phase, detail string, current, total int) {
	reportPlanProgressForLane(onProgress, phase, "", detail, current, total)
}

func reportPlanProgressForLane(onProgress PlanProgressFunc, phase, lane, detail string, current, total int) {
	if onProgress == nil {
		return
	}
	onProgress(PlanProgress{
		Phase:   phase,
		Lane:    lane,
		Detail:  detail,
		Current: current,
		Total:   total,
	})
}

func profileProgressLabel(profileID string, current, total int) string {
	if total <= 0 {
		return profileID
	}
	return fmt.Sprintf("%s (%d/%d)", profileID, current, total)
}

func applySelectedOfferRuntimePredictions(estimates []CostEstimate, selected []offerRuntimePrediction) []CostEstimate {
	if len(estimates) == 0 || len(selected) == 0 {
		return estimates
	}
	adjusted := append([]CostEstimate(nil), estimates...)
	for i := range adjusted {
		if i >= len(selected) {
			break
		}
		adjusted[i] = applySelectedOfferRuntimePrediction(adjusted[i], selected[i])
	}
	return adjusted
}

func applySelectedOfferRuntimePrediction(est CostEstimate, pred offerRuntimePrediction) CostEstimate {
	if est.Offer.Offer == nil || !pred.complete || !pred.feasible || len(est.Group.Jobs) == 0 {
		return est
	}

	jobDurations := pred.jobDurations
	runHours := pred.totalRunHrs
	if len(pred.adjustedJobDurations) == len(est.Group.Jobs) {
		jobDurations = pred.adjustedJobDurations
	}
	if pred.adjustedRunHrs > 0 {
		runHours = pred.adjustedRunHrs
	}
	if runHours <= 0 || len(jobDurations) != len(est.Group.Jobs) {
		return est
	}

	est.JobDurations = make(map[int64]time.Duration, len(est.Group.Jobs))
	for idx, job := range est.Group.Jobs {
		if job == nil {
			continue
		}
		est.JobDurations[job.ID] = jobDurations[idx]
	}

	runMean := time.Duration(runHours * float64(time.Hour))
	oldRun := est.Breakdown.Run
	est.Breakdown.Run = rescaleEstimateMean(oldRun, runMean)
	delta := est.Breakdown.Run.Mean - oldRun.Mean
	est.Breakdown.Total = shiftEstimate(est.Breakdown.Total, delta)
	est.TotalTime = est.Breakdown.Total.Mean

	costRate := est.Offer.Offer.CostPerHour
	est.TotalCost = est.TotalTime.Hours() * costRate
	if costRate == 0 {
		est.RiskAdjustedCost = 0
	} else if est.SurvivalProb > 0 {
		est.RiskAdjustedCost = bidding.ExpectedCost(costRate, est.Breakdown.Run.Mean.Hours(), est.SetupOverhead.Hours(), est.SurvivalProb)
	}

	return est
}

func rescaleEstimateMean(base estimate.Estimate, mean time.Duration) estimate.Estimate {
	if mean <= 0 {
		return estimate.Estimate{}
	}
	if base.Mean <= 0 {
		return estimate.Constant(mean)
	}

	factor := mean.Seconds() / base.Mean.Seconds()
	lower := scaleDuration(base.Lower, factor)
	upper := scaleDuration(base.Upper, factor)
	if lower > mean {
		lower = mean
	}
	if upper < mean {
		upper = mean
	}
	return estimate.Estimate{
		Mean:  mean,
		Lower: lower,
		Upper: upper,
	}
}

func scaleDuration(duration time.Duration, factor float64) time.Duration {
	if factor <= 0 || duration <= 0 {
		return 0
	}
	return time.Duration(float64(duration) * factor)
}

func shiftEstimate(base estimate.Estimate, delta time.Duration) estimate.Estimate {
	shift := func(value time.Duration) time.Duration {
		value += delta
		if value < 0 {
			return 0
		}
		return value
	}
	return estimate.Estimate{
		Mean:  shift(base.Mean),
		Lower: shift(base.Lower),
		Upper: shift(base.Upper),
	}
}

func makeDefaultNeutralDurations(jobCount int) []time.Duration {
	if jobCount < 0 {
		jobCount = 0
	}
	neutral := make([]time.Duration, jobCount)
	for i := range neutral {
		neutral[i] = estimate.DefaultJobDuration.Mean
	}
	return neutral
}
