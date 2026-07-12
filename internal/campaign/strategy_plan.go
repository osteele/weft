package campaign

import (
	"database/sql"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
)

var resolvePredictBatch = predictor.ResolvePredictBatch
var estimateJobDurationsDetailed = estimate.EstimateJobDurationsDetailed
var estimateJobDurationsDetailedForReuse = estimate.EstimateJobDurationsDetailed
var fetchCandidateGroupingsForPlanning = fetchCandidateGroupingsWithSession
var fetchGroupRawOffersForPlanning = func(session *offerSearchSession, groups []InstanceGroup) []GroupRawOffers {
	return session.fetchGroupRawOffers(groups)
}

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

// PlanOptions configures scoring behavior for launch planning.
type PlanOptions struct {
	OpportunityCostWeight  float64
	PreferReuse            bool
	DistinctMachines       bool
	InitialClaimedMachines map[string]struct{}
	MachineAffinity        map[string]struct{}
	RawOffers              []GroupRawOffers
	CachedOffersOnly       bool
	MinReliability         float64
}

func defaultPlanOptions() PlanOptions {
	return PlanOptions{OpportunityCostWeight: 1.0, MinReliability: cloud.DefaultMinReliability}
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

// CandidatePlanMode controls which candidate grouping families are explored
// for one profile during plan construction.
type CandidatePlanMode int

const (
	CandidatePlanModeFull CandidatePlanMode = iota
	CandidatePlanModeSplitOnly
	CandidatePlanModeMergedPreferred
	CandidatePlanModeParallelPreferred
)

func candidatePlanModeLabel(mode CandidatePlanMode) string {
	switch mode {
	case CandidatePlanModeFull:
		return "full"
	case CandidatePlanModeSplitOnly:
		return "split_only"
	case CandidatePlanModeMergedPreferred:
		return "merged_preferred"
	case CandidatePlanModeParallelPreferred:
		return "parallel_preferred"
	default:
		return fmt.Sprintf("unknown(%d)", mode)
	}
}

// ProfilePlanSpec pairs a score profile with the candidate search mode to use
// when constructing that profile's launch plan.
type ProfilePlanSpec struct {
	Profile       bidding.ScoreProfile
	CandidateMode CandidatePlanMode
}

type rawOfferEvaluation struct {
	raw              []GroupRawOffers
	offerPredictions map[int]map[string]offerRuntimePrediction
}

type groupingEvaluation struct {
	label             string
	groups            []InstanceGroup
	rawEval           rawOfferEvaluation
	overlapSavedHours float64
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

	reusePredictionMu sync.Mutex
	reusePredictions  map[string]map[int64]estimate.DurationPrediction

	reuseInputMu    sync.Mutex
	reuseInputBytes map[string]int64

	opportunityCostWeight  float64
	preferReuse            bool
	distinctMachines       bool
	initialClaimedMachines map[string]struct{}
	machineAffinity        map[string]struct{}
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
	options PlanOptions,
) *planEvaluator {
	if options.OpportunityCostWeight <= 0 {
		options.OpportunityCostWeight = defaultPlanOptions().OpportunityCostWeight
	}
	return &planEvaluator{
		database:               database,
		predCfg:                predCfg,
		overheadModel:          overheadModel,
		survivalModel:          survivalModel,
		minSurvival:            minSurvival,
		setupFactory:           OfferSetupOverheadFactory(database, overheadModel),
		rawEvalCache:           make(map[string]map[int]map[string]offerRuntimePrediction),
		estimateCache:          make(map[string][]CostEstimate),
		reuseEstimates:         make(map[string]reuseEstimateCacheEntry),
		reusePredictions:       make(map[string]map[int64]estimate.DurationPrediction),
		reuseInputBytes:        make(map[string]int64),
		opportunityCostWeight:  options.OpportunityCostWeight,
		preferReuse:            options.PreferReuse,
		distinctMachines:       options.DistinctMachines,
		initialClaimedMachines: cloneStringSet(options.InitialClaimedMachines),
		machineAffinity:        cloneStringSet(options.MachineAffinity),
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
		0.95,
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
	minReliability float64,
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
		minReliability,
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
	minReliability float64,
	minSurvival float64,
	onProgress PlanProgressFunc,
) (map[string]StrategyPlan, []GroupRawOffers) {
	return BuildProfilePlansWithProgressAndOptions(
		database,
		clients,
		splitGroups,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		profiles,
		minReliability,
		minSurvival,
		onProgress,
		defaultPlanOptions(),
	)
}

func BuildProfilePlansWithProgressAndOptions(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	profiles []bidding.ScoreProfile,
	minReliability float64,
	minSurvival float64,
	onProgress PlanProgressFunc,
	options PlanOptions,
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
	if options.MinReliability == 0 {
		options.MinReliability = minReliability
	}

	splitRaw := make([]GroupRawOffers, len(splitGroups))
	for i, g := range splitGroups {
		splitRaw[i] = GroupRawOffers{Group: g}
	}
	var offerSession *offerSearchSession
	if len(clients) > 0 {
		offerSession = newOfferSearchSession(clients, minReliability)
		splitRaw = offerSession.fetchGroupRawOffers(splitGroups)
	}
	splitRaw = applyPlanningOfferBlocks(database, splitRaw)

	return buildProfilePlansFromSplitRawWithSession(
		database,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		offerSession,
		defaultProfilePlanSpecs(profiles),
		minSurvival,
		onProgress,
		options,
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
	minReliability float64,
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
		minReliability,
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
	minReliability float64,
	minSurvival float64,
	onProgress PlanProgressFunc,
) map[string]StrategyPlan {
	return BuildProfilePlansFromSplitRawWithPlanSpecsAndOptions(
		database,
		clients,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		defaultProfilePlanSpecs(profiles),
		minReliability,
		minSurvival,
		onProgress,
		defaultPlanOptions(),
	)
}

// BuildProfilePlansFromSplitRawWithPlanSpecs builds reusable/new-instance
// launch plans from pre-fetched split-group raw offers using explicit
// per-profile candidate search modes.
func BuildProfilePlansFromSplitRawWithPlanSpecs(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	splitRaw []GroupRawOffers,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	specs []ProfilePlanSpec,
	minReliability float64,
	minSurvival float64,
	onProgress PlanProgressFunc,
) map[string]StrategyPlan {
	return BuildProfilePlansFromSplitRawWithPlanSpecsAndOptions(
		database,
		clients,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		specs,
		minReliability,
		minSurvival,
		onProgress,
		defaultPlanOptions(),
	)
}

// BuildProfilePlansFromSplitRawWithPlanSpecsAndOptions builds reusable/new-instance
// launch plans from pre-fetched split-group raw offers using explicit
// per-profile candidate search modes and score options.
func BuildProfilePlansFromSplitRawWithPlanSpecsAndOptions(
	database *sql.DB,
	clients []cloud.Client,
	splitGroups []InstanceGroup,
	splitRaw []GroupRawOffers,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	specs []ProfilePlanSpec,
	minReliability float64,
	minSurvival float64,
	onProgress PlanProgressFunc,
	options PlanOptions,
) map[string]StrategyPlan {
	if options.MinReliability == 0 {
		options.MinReliability = minReliability
	}
	var offerSession *offerSearchSession
	switch {
	case len(clients) > 0 || options.CachedOffersOnly:
		offerSession = newOfferSearchSessionWithOptions(clients, minReliability, !options.CachedOffersOnly)
		offerSession.SeedRawOffers(splitRaw)
		if len(splitRaw) == len(splitGroups) {
			splitRaw = applyPlanningOfferBlocks(database, splitRaw)
		} else {
			splitRaw = offerSession.fetchGroupRawOffers(splitGroups)
			splitRaw = applyPlanningOfferBlocks(database, splitRaw)
		}
	case len(splitRaw) != len(splitGroups):
		splitRaw = make([]GroupRawOffers, len(splitGroups))
		for i, g := range splitGroups {
			splitRaw[i] = GroupRawOffers{Group: g}
		}
	}
	splitRaw = applyPlanningOfferBlocks(database, splitRaw)

	return buildProfilePlansFromSplitRawWithSession(
		database,
		splitGroups,
		splitRaw,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		offerSession,
		specs,
		minSurvival,
		onProgress,
		options,
	)
}

func applyPlanningOfferBlocks(database *sql.DB, raw []GroupRawOffers) []GroupRawOffers {
	raw = applyImagePrestartFailureBlocks(database, raw)
	return applyTorchPreflightMachineBlocks(database, raw)
}

func applyTorchPreflightMachineBlocks(database *sql.DB, raw []GroupRawOffers) []GroupRawOffers {
	if database == nil || len(raw) == 0 {
		return raw
	}
	out := make([]GroupRawOffers, len(raw))
	copy(out, raw)
	for i := range out {
		if out[i].Err != nil || len(out[i].Offers) == 0 || len(out[i].Group.Jobs) == 0 {
			continue
		}
		blocked := map[string]struct{}{}
		for _, job := range out[i].Group.Jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			machines, err := db.FailedTorchPreflightMachineIDs(database, job.ID)
			if err != nil {
				continue
			}
			for key := range machines {
				blocked[key] = struct{}{}
			}
		}
		if len(blocked) == 0 {
			continue
		}
		filtered := out[i].Offers[:0]
		for _, offer := range out[i].Offers {
			if key := offerMachineClaimKey(offer); key != "" {
				if _, skip := blocked[key]; skip {
					continue
				}
			}
			filtered = append(filtered, offer)
		}
		if len(out[i].Offers) > 0 && len(filtered) == 0 {
			out[i].Offers = nil
			out[i].Err = ErrTorchPreflightMachinesExhausted
			continue
		}
		out[i].Offers = filtered
	}
	return out
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
	specs []ProfilePlanSpec,
	minSurvival float64,
	onProgress PlanProgressFunc,
	options PlanOptions,
) map[string]StrategyPlan {
	totalStart := time.Now()
	splitGroups, splitRaw, specs = shapeDistinctMachinePlan(splitGroups, splitRaw, specs, options)
	plans := make(map[string]StrategyPlan, len(specs))
	if len(splitGroups) == 0 {
		return plans
	}
	rawFetchStart := time.Now()
	if len(splitRaw) != len(splitGroups) {
		if offerSession != nil {
			splitRaw = offerSession.fetchGroupRawOffers(splitGroups)
		} else {
			splitRaw = make([]GroupRawOffers, len(splitGroups))
			for i, g := range splitGroups {
				splitRaw[i] = GroupRawOffers{Group: g}
			}
		}
	}
	rawFetchDur := time.Since(rawFetchStart)

	validSpecs := make([]ProfilePlanSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Profile.Valid() {
			validSpecs = append(validSpecs, spec)
		}
	}
	if len(validSpecs) == 0 {
		return plans
	}

	evaluator := newPlanEvaluator(database, predCfg, overheadModel, survivalModel, minSurvival, options)
	reusable = filterReusableByMachineAffinity(reusable, options.MachineAffinity)
	reportPlanProgress(onProgress, "Estimating raw-offer runtimes", fmt.Sprintf("%d direct offer(s)", countRawOffers(splitRaw)), 0, 0)
	rawEvalStart := time.Now()
	splitEval := evaluator.evaluateRawOffers(splitRaw)
	rawEvalDur := time.Since(rawEvalStart)

	workerLimit := planProfileWorkerLimit(len(validSpecs))
	telemetryOffers, _ := evaluator.selectOffers(splitEval, validSpecs[0].Profile)
	RecordOfferAvailabilitySnapshots(database, splitRaw, telemetryOffers, options.MinReliability)

	sem := make(chan struct{}, workerLimit)
	var mu sync.Mutex
	var wg sync.WaitGroup

	profileStart := time.Now()
	for idx, spec := range validSpecs {
		wg.Add(1)
		go func(idx int, spec ProfilePlanSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			progressLabel := profileProgressLabel(spec.Profile.ID, idx+1, len(validSpecs))
			reportPlanProgressForLane(onProgress, "Planning tradeoff profiles", progressLabel, "", idx+1, len(validSpecs))
			plan := buildStrategyPlanForSplitRaw(
				splitGroups,
				splitEval,
				reusable,
				evaluator,
				offerSession,
				spec.Profile,
				spec.CandidateMode,
				onProgress,
				progressLabel,
			)

			mu.Lock()
			plans[spec.Profile.ID] = plan
			mu.Unlock()
		}(idx, spec)
	}
	wg.Wait()
	profileDur := time.Since(profileStart)
	oplog.Log("campaign.planner.timing",
		oplog.WithDetailf(
			"groups=%d raw_offers=%d reusable=%d profiles=%d raw_fetch=%s raw_eval=%s profiles_wall=%s total=%s",
			len(splitGroups),
			countRawOffers(splitRaw),
			len(reusable),
			len(validSpecs),
			rawFetchDur.Truncate(time.Millisecond),
			rawEvalDur.Truncate(time.Millisecond),
			profileDur.Truncate(time.Millisecond),
			time.Since(totalStart).Truncate(time.Millisecond),
		))

	return plans
}

func shapeDistinctMachinePlan(groups []InstanceGroup, raw []GroupRawOffers, specs []ProfilePlanSpec, options PlanOptions) ([]InstanceGroup, []GroupRawOffers, []ProfilePlanSpec) {
	if !options.DistinctMachines {
		return groups, raw, specs
	}
	if !allGroupsSingleJob(groups) {
		groups = SplitToParallel(groups)
		raw = nil
	}
	return groups, raw, forceSplitOnlyProfileSpecs(specs)
}

func allGroupsSingleJob(groups []InstanceGroup) bool {
	for _, group := range groups {
		if len(group.Jobs) > 1 {
			return false
		}
	}
	return true
}

func forceSplitOnlyProfileSpecs(specs []ProfilePlanSpec) []ProfilePlanSpec {
	if len(specs) == 0 {
		return specs
	}
	out := slices.Clone(specs)
	for i := range out {
		out[i].CandidateMode = CandidatePlanModeSplitOnly
	}
	return out
}

func defaultProfilePlanSpecs(profiles []bidding.ScoreProfile) []ProfilePlanSpec {
	specs := make([]ProfilePlanSpec, 0, len(profiles))
	for _, profile := range profiles {
		mode := CandidatePlanModeFull
		if profile.ID == bidding.StrategyFastest.Profile().ID {
			mode = CandidatePlanModeParallelPreferred
		}
		specs = append(specs, ProfilePlanSpec{Profile: profile, CandidateMode: mode})
	}
	return specs
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
	candidateMode CandidatePlanMode,
	onProgress PlanProgressFunc,
	progressLabel string,
) StrategyPlan {
	totalStart := time.Now()
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
	directSelectStart := time.Now()
	splitOffers, splitPredictions := evaluator.selectOffers(splitEval, profile)
	directSelectDur := time.Since(directSelectStart)
	reportPlanProgressForLane(onProgress, "Estimating direct-offer costs", progressLabel, "", 0, 0)
	directEstimateStart := time.Now()
	splitEstimates := evaluator.estimateSelectedOffers(splitOffers, splitPredictions)
	directEstimateDur := time.Since(directEstimateStart)

	reportPlanProgressForLane(onProgress, "Checking reusable instances", progressLabel, "", 0, 0)
	reuseStart := time.Now()
	reuseDecisions := chooseReuseGroups(
		splitGroups,
		splitEstimates,
		reusable,
		evaluator,
		profile,
		evaluator.preferReuse,
		evaluator.opportunityCostWeight,
		onProgress,
		progressLabel,
	)
	reuseDur := time.Since(reuseStart)
	var mergedFetchDur time.Duration
	var mergedEvalDur time.Duration
	var mergedScoreDur time.Duration
	var parallelFetchDur time.Duration
	var parallelEvalDur time.Duration
	var parallelScoreDur time.Duration
	var candidateFetchDur time.Duration
	var candidateEvalDur time.Duration
	var candidateSelectDur time.Duration
	var splitCandidateDur time.Duration

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
	logProfileTiming := func() {
		oplog.Log("campaign.planner.profile_timing",
			oplog.WithDetailf(
				"profile=%s mode=%s groups=%d remaining=%d reuse=%d direct_select=%s direct_estimate=%s reuse_eval=%s split_score=%s merged_fetch=%s merged_eval=%s merged_score=%s parallel_fetch=%s parallel_eval=%s parallel_score=%s candidate_fetch=%s candidate_eval=%s candidate_select=%s total=%s",
				profile.ID,
				candidatePlanModeLabel(candidateMode),
				len(splitGroups),
				len(remainingGroups),
				len(reuseDecisions),
				directSelectDur.Truncate(time.Millisecond),
				directEstimateDur.Truncate(time.Millisecond),
				reuseDur.Truncate(time.Millisecond),
				splitCandidateDur.Truncate(time.Millisecond),
				mergedFetchDur.Truncate(time.Millisecond),
				mergedEvalDur.Truncate(time.Millisecond),
				mergedScoreDur.Truncate(time.Millisecond),
				parallelFetchDur.Truncate(time.Millisecond),
				parallelEvalDur.Truncate(time.Millisecond),
				parallelScoreDur.Truncate(time.Millisecond),
				candidateFetchDur.Truncate(time.Millisecond),
				candidateEvalDur.Truncate(time.Millisecond),
				candidateSelectDur.Truncate(time.Millisecond),
				time.Since(totalStart).Truncate(time.Millisecond),
			))
	}

	if len(remainingGroups) == 0 {
		logProfileTiming()
		return plan
	}

	remainingSplitEval := subsetRawOfferEvaluation(splitEval, remainingIdx)
	splitCandidateStart := time.Now()
	splitResult := evaluateSingleCandidateForProfile(
		evaluator,
		groupingEvaluation{
			label:   "split",
			groups:  remainingGroups,
			rawEval: remainingSplitEval,
		},
		profile,
		onProgress,
		progressLabel,
	)
	splitCandidateDur = time.Since(splitCandidateStart)

	var result CandidateResult
	switch candidateMode {
	case CandidatePlanModeSplitOnly:
		result = splitResult
	case CandidatePlanModeMergedPreferred:
		result = splitResult
		if offerSession != nil {
			mergedGroups := MergeCompatibleGroups(remainingGroups)
			if len(mergedGroups) != len(remainingGroups) {
				reportPlanProgressForLane(onProgress, "Fetching merged candidate", progressLabel, "", 0, 0)
				mergedFetchStart := time.Now()
				mergedRaw := applyPlanningOfferBlocks(evaluator.database, fetchGroupRawOffersForPlanning(offerSession, mergedGroups))
				mergedFetchDur += time.Since(mergedFetchStart)
				mergedEvalStart := time.Now()
				mergedEval := evaluator.evaluateRawOffers(mergedRaw)
				mergedEvalDur += time.Since(mergedEvalStart)
				mergedScoreStart := time.Now()
				mergedResult := evaluateSingleCandidateForProfile(
					evaluator,
					groupingEvaluation{
						label:   "merged",
						groups:  mergedGroups,
						rawEval: mergedEval,
					},
					profile,
					onProgress,
					progressLabel,
				)
				mergedScoreDur += time.Since(mergedScoreStart)
				if candidateResultHasOffers(mergedResult) {
					result = mergedResult
				}
			}
		}
	case CandidatePlanModeParallelPreferred:
		nonParallelCandidates := []groupingEvaluation{
			{
				label:   "split",
				groups:  remainingGroups,
				rawEval: remainingSplitEval,
			},
		}
		if offerSession != nil {
			mergedGroups := MergeCompatibleGroups(remainingGroups)
			if len(mergedGroups) != len(remainingGroups) {
				reportPlanProgressForLane(onProgress, "Fetching merged candidate", progressLabel, "", 0, 0)
				mergedFetchStart := time.Now()
				mergedRaw := applyPlanningOfferBlocks(evaluator.database, fetchGroupRawOffersForPlanning(offerSession, mergedGroups))
				mergedFetchDur += time.Since(mergedFetchStart)
				mergedEvalStart := time.Now()
				mergedEval := evaluator.evaluateRawOffers(mergedRaw)
				mergedEvalDur += time.Since(mergedEvalStart)
				nonParallelCandidates = append(nonParallelCandidates, groupingEvaluation{
					label:   "merged",
					groups:  mergedGroups,
					rawEval: mergedEval,
				})
			}
		}
		candidateSelectStart := time.Now()
		result = bestCandidateForProfileEvaluations(
			evaluator,
			nonParallelCandidates,
			profile,
			onProgress,
			progressLabel,
		)
		candidateSelectDur += time.Since(candidateSelectStart)
		if offerSession != nil {
			parallelGroups := SplitToParallel(remainingGroups)
			if len(parallelGroups) > 0 {
				reportPlanProgressForLane(onProgress, "Fetching parallel candidate", progressLabel, "", 0, 0)
				parallelFetchStart := time.Now()
				parallelRaw := applyPlanningOfferBlocks(evaluator.database, fetchGroupRawOffersForPlanning(offerSession, parallelGroups))
				parallelFetchDur += time.Since(parallelFetchStart)
				parallelEvalStart := time.Now()
				parallelEval := evaluator.evaluateRawOffers(parallelRaw)
				parallelEvalDur += time.Since(parallelEvalStart)
				parallelScoreStart := time.Now()
				parallelResult := evaluateSingleCandidateForProfile(
					evaluator,
					groupingEvaluation{
						label:   "parallel",
						groups:  parallelGroups,
						rawEval: parallelEval,
					},
					profile,
					onProgress,
					progressLabel,
				)
				parallelScoreDur += time.Since(parallelScoreStart)
				if candidateResultHasOffers(parallelResult) {
					result = parallelResult
				}
			}
		}
	default:
		if offerSession == nil {
			result = splitResult
			break
		}
		reportPlanProgressForLane(onProgress, "Fetching merged and parallel candidates", progressLabel, "", 0, 0)
		candidateFetchStart := time.Now()
		candidates := fetchCandidateGroupingsForPlanning(offerSession, remainingGroups)
		candidateFetchDur += time.Since(candidateFetchStart)
		candidateEvalStart := time.Now()
		evaluations := evaluator.evaluateCandidateGroupings(candidates)
		candidateEvalDur += time.Since(candidateEvalStart)
		candidateSelectStart := time.Now()
		result = bestCandidateForProfileEvaluations(
			evaluator,
			evaluations,
			profile,
			onProgress,
			progressLabel,
		)
		candidateSelectDur += time.Since(candidateSelectStart)
	}
	if len(result.Groups) == 0 {
		logProfileTiming()
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

	logProfileTiming()
	return plan
}

func subsetRawOfferEvaluation(eval rawOfferEvaluation, indexes []int) rawOfferEvaluation {
	subsetRaw := make([]GroupRawOffers, 0, len(indexes))
	subsetPredictions := make(map[int]map[string]offerRuntimePrediction, len(indexes))
	for subsetIdx, originalIdx := range indexes {
		if originalIdx < 0 || originalIdx >= len(eval.raw) {
			continue
		}
		subsetRaw = append(subsetRaw, eval.raw[originalIdx])
		if offerPredictions, ok := eval.offerPredictions[originalIdx]; ok {
			subsetPredictions[subsetIdx] = offerPredictions
		}
	}
	return rawOfferEvaluation{raw: subsetRaw, offerPredictions: subsetPredictions}
}

func evaluateSingleCandidateForProfile(
	evaluator *planEvaluator,
	candidate groupingEvaluation,
	profile bidding.ScoreProfile,
	onProgress PlanProgressFunc,
	progressLabel string,
) CandidateResult {
	if len(candidate.groups) == 0 {
		return CandidateResult{}
	}
	reportPlanProgressForLane(onProgress, "Scoring candidate groupings", progressLabel, candidate.label, 1, 1)
	offers, selectedPredictions := evaluator.selectOffers(candidate.rawEval, profile)
	estimates := evaluator.estimateSelectedOffers(offers, selectedPredictions)
	return CandidateResult{
		CandidateIdx: 0,
		Label:        candidate.label,
		Groups:       candidate.groups,
		Offers:       offers,
		Estimates:    estimates,
	}
}

func candidateResultHasOffers(result CandidateResult) bool {
	if len(result.Groups) == 0 || len(result.Offers) != len(result.Groups) {
		return false
	}
	for _, offer := range result.Offers {
		if offer.Offer == nil || offer.Err != nil {
			return false
		}
	}
	return true
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
	return rankGroupOffersFromPredictionsWithMachineExclusions(eval.raw, eval.offerPredictions, e.survivalModel, e.setupFactory, profile, e.minSurvival, e.initialClaimedMachines, e.machineAffinity, e.distinctMachines)
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
				label:             cand.Label,
				groups:            cand.Groups,
				rawEval:           e.evaluateRawOffers(applyPlanningOfferBlocks(e.database, cand.Raw)),
				overlapSavedHours: assetOverlapSavedHours(cand.Groups),
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

	est, ok := estimateReuseGroupWithSharedData(reuseEstimateContext{evaluator: e}, group, cap)
	e.reuseEstimates[key] = reuseEstimateCacheEntry{estimate: est, ok: ok}
	e.reuseMu.Unlock()
	return est, ok
}

func (e *planEvaluator) reuseDurationPredictions(group InstanceGroup, gpuLabel string) map[int64]estimate.DurationPrediction {
	key := reusePredictionCacheKey(group, gpuLabel)

	e.reusePredictionMu.Lock()
	if cached, ok := e.reusePredictions[key]; ok {
		e.reusePredictionMu.Unlock()
		return cached
	}
	predictions := estimateReuseDurationsDetailed(e.predCfg, gpuLabel, group.Jobs)
	e.reusePredictions[key] = predictions
	e.reusePredictionMu.Unlock()
	return predictions
}

func (e *planEvaluator) reuseDownloadBytes(inputs []string) int64 {
	key := reuseInputsCacheKey(inputs)

	e.reuseInputMu.Lock()
	if cached, ok := e.reuseInputBytes[key]; ok {
		e.reuseInputMu.Unlock()
		return cached
	}
	var totalBytes int64
	if len(inputs) > 0 {
		totalBytes, _, _ = dataloc.ResolveInputSizes(inputs, nil)
	}
	e.reuseInputBytes[key] = totalBytes
	e.reuseInputMu.Unlock()
	return totalBytes
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

func reusePredictionCacheKey(group InstanceGroup, gpuLabel string) string {
	jobIDs := make([]string, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil {
			jobIDs = append(jobIDs, "0")
			continue
		}
		jobIDs = append(jobIDs, fmt.Sprintf("%d", job.ID))
	}
	return gpuLabel + "|" + strings.Join(jobIDs, ",")
}

func reuseInputsCacheKey(inputs []string) string {
	if len(inputs) == 0 {
		return ""
	}
	cloned := append([]string(nil), inputs...)
	slices.Sort(cloned)
	return strings.Join(cloned, ",")
}

func countRawOffers(raw []GroupRawOffers) int {
	total := 0
	for _, groupRaw := range raw {
		total += len(groupRaw.Offers)
	}
	return total
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
	jobQuantiles         []*predictor.QuantilePrediction
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
	return rankGroupOffersFromPredictionsWithMachineExclusions(raw, predicted, survivalModel, setupFactory, profile, minSurvival, nil, nil, false)
}

func rankGroupOffersFromPredictionsWithMachineExclusions(
	raw []GroupRawOffers,
	predicted map[int]map[string]offerRuntimePrediction,
	survivalModel *bidding.SurvivalModel,
	setupFactory SetupOverheadFactory,
	profile bidding.ScoreProfile,
	minSurvival float64,
	initialClaimedMachines map[string]struct{},
	machineAffinity map[string]struct{},
	distinctMachines bool,
) ([]GroupOffer, []offerRuntimePrediction) {
	results := make([]GroupOffer, len(raw))
	selected := make([]offerRuntimePrediction, len(raw))
	// Track machines (and specific offer IDs) already claimed by earlier
	// groups in this ranking pass. Vast.ai serializes create-instance per
	// machine — when two groups in the same tick both pick the cheapest
	// RTX A4000, only one create succeeds and the rest get success=false
	// (verified in production: 9 groups, machine_id=44696, 2 successes + 7
	// rejections). Filtering each group's offer pool against earlier
	// selections eliminates the race at its source. The exclusion is
	// in-pass only — global cross-pass ordering is the autopilot's job.
	claimedMachines := cloneStringSet(initialClaimedMachines)
	claimedOffers := make(map[string]struct{})
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}

		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}
		affinityOffers := filterOffersByMachineAffinity(r.Offers, machineAffinity)
		if len(r.Offers) > 0 && len(affinityOffers) == 0 && len(machineAffinity) > 0 {
			results[i] = GroupOffer{Group: r.Group, Err: ErrMachineAffinityUnsatisfied}
			continue
		}
		availableOffers := filterOffersByClaim(affinityOffers, claimedMachines, claimedOffers, distinctMachines)
		if len(r.Offers) > 0 && len(availableOffers) == 0 && len(claimedMachines) > 0 {
			results[i] = GroupOffer{Group: r.Group, Err: ErrDistinctMachinesExhausted}
			continue
		}

		if offerPredictions, ok := predicted[i]; ok {
			if ranked, ok := rankOfferWithPredictedRuntime(
				r.Group,
				availableOffers,
				survivalModel,
				setupOverhead,
				profile,
				minSurvival,
				offerPredictions,
			); ok {
				results[i] = ranked
				attachProviderErrors(&results[i], r.ProviderErrors)
				if ranked.Offer != nil {
					selected[i] = offerPredictions[offerPredictionKey(*ranked.Offer)]
					recordOfferClaim(*ranked.Offer, claimedMachines, claimedOffers, distinctMachines)
				}
				continue
			}
		}

		neutral := neutralOfferRuntimePredictions(r.Group, availableOffers)
		results[i], _ = rankOfferWithPredictedRuntime(
			r.Group,
			availableOffers,
			survivalModel,
			setupOverhead,
			profile,
			minSurvival,
			neutral,
		)
		attachProviderErrors(&results[i], r.ProviderErrors)
		if results[i].Offer != nil {
			selected[i] = neutral[offerPredictionKey(*results[i].Offer)]
			recordOfferClaim(*results[i].Offer, claimedMachines, claimedOffers, distinctMachines)
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
		jobQuantiles []*predictor.QuantilePrediction
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
					ID:                nextID,
					Command:           job.Command,
					Project:           job.Project,
					GPUClass:          offer.GPUName,
					WorkingDir:        job.WorkingDir,
					DurationQuantiles: estimate.DecisionDurationQuantiles(),
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
				jobQuantiles: make([]*predictor.QuantilePrediction, expected[ref.groupIdx][ref.offerKey]),
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
		accum.jobQuantiles[ref.jobIdx] = result.DurationQuantiles
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
				jobQuantiles: append([]*predictor.QuantilePrediction(nil), accum.jobQuantiles...),
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
	stats := OfferFilterStats{RawCount: len(offers)}
	minSurvival = db.RequestedMinSurvivalForJobs(group.Jobs, minSurvival)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result, true
	}
	// Runtime-ranking path receives provider-filtered offers; treat all as
	// VRAM-compatible for staged diagnostics parity with rankOfferWithProfile.
	stats.AfterVRAM = len(offers)

	offers, cudaFiltered, imageCUDA, minRequiredCUDA, exampleGPU, imageRef := filterOffersByCUDACompat(offers, group.Image)
	if cudaFiltered > 0 {
		stats.CUDAImage = imageRef
		stats.CUDAImageVersion = imageCUDA
		stats.CUDAMinRequired = minRequiredCUDA
		stats.CUDAExampleGPU = exampleGPU
	}
	stats.AfterCUDA = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result, true
	}
	offers, providerCompatFiltered, providerCompatUnknown := filterOffersByProviderCompatibility(group, offers)
	stats.UnknownCompatibility = providerCompatUnknown
	stats.ProviderCompatibilityFiltered = providerCompatFiltered
	stats.AfterProvider = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result, true
	}

	if group.MinComputeCap != "" || group.MaxComputeCap != "" {
		archOffers, archFiltered, exampleGPU, exampleCap := filterOffersByTorchArch(offers, group.MinComputeCap, group.MaxComputeCap)
		stats.TorchArchMinCap = group.MinComputeCap
		stats.TorchArchMaxCap = group.MaxComputeCap
		if archFiltered > 0 {
			stats.TorchArchExampleGPU = exampleGPU
			stats.TorchArchExampleCap = exampleCap
		}
		offers = archOffers
	}
	stats.AfterTorchArch = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result, true
	}

	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	stats.AfterSurvival = len(filtered)
	if len(filtered) == 0 {
		result.FilterStats = stats
		return result, true
	}

	w := profile.Weights()
	bestScore := math.Inf(1)
	var best cloud.Offer
	found := false
	alternatives := make([]RankedOfferAlternative, 0, len(filtered))

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

		score := w.Cost*cost + w.Time*completionHrs + compatibilityScorePenalty(group, offer)
		alternatives = append(alternatives, RankedOfferAlternative{
			Offer:         offer,
			Score:         score,
			CompletionHrs: completionHrs,
			Cost:          cost,
			Survival:      surv,
			Compatibility: offerCompatibilityStatus(group, offer),
		})
		if !found || score < bestScore {
			bestScore = score
			best = offer
			result.SurvivalProb = surv
			found = true
		}
	}

	if !found {
		result.FilterStats = stats
		return result, false
	}

	result.Offer = &best
	result.FilterStats = stats
	sort.Slice(alternatives, func(i, j int) bool {
		return alternatives[i].Score < alternatives[j].Score
	})
	for i := range alternatives {
		alternatives[i].Rank = i + 1
		alternatives[i].Selected = offerPredictionKey(alternatives[i].Offer) == offerPredictionKey(best)
	}
	result.Alternatives = alternatives
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
	preferReuse bool,
	opportunityWeight float64,
	onProgress PlanProgressFunc,
	progressLabel string,
) []reuseGroupDecision {
	if len(reusable) == 0 || len(splitGroups) == 0 {
		return nil
	}

	working := cloneInstanceCapacities(reusable)
	var decisions []reuseGroupDecision
	for idx, group := range splitGroups {
		candidates := pruneReuseCandidates(group, working, preferReuse)
		reportPlanProgressForLane(
			onProgress,
			"Checking reusable instances",
			progressLabel,
			fmt.Sprintf("group %d/%d across %d reusable instance(s), scoring top %d", idx+1, len(splitGroups), len(working), len(candidates)),
			idx+1,
			len(splitGroups),
		)

		newScore := math.Inf(1)
		if idx < len(splitEstimates) {
			newScore = ScoreEstimatesWithProfile([]CostEstimate{splitEstimates[idx]}, profile)
		}

		type reuseScore struct {
			idx   int
			score float64
			est   CostEstimate
			ok    bool
		}
		bestScore := math.Inf(1)
		bestIdx := -1
		var bestEstimate CostEstimate
		results := make(chan reuseScore, len(candidates))
		workerLimit := reuseEstimateWorkerLimit(len(candidates))
		sem := make(chan struct{}, workerLimit)
		var wg sync.WaitGroup
		for _, candidate := range candidates {
			wg.Add(1)
			go func(candidate reuseCandidate) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				est, ok := evaluator.estimateReuseGroup(group, candidate.cap)
				if !ok {
					results <- reuseScore{idx: candidate.idx}
					return
				}
				results <- reuseScore{
					idx:   candidate.idx,
					score: ScoreEstimatesWithProfile([]CostEstimate{est}, profile),
					est:   est,
					ok:    true,
				}
			}(candidate)
		}
		wg.Wait()
		close(results)
		for result := range results {
			if !result.ok {
				continue
			}
			penalty := reuseOpportunityPenaltyUSD(
				group,
				working[result.idx],
				splitGroups,
				working,
				opportunityWeight,
			)
			if penalty > 0 {
				result.est.TotalCost += penalty
				if result.est.RiskAdjustedCost > 0 {
					result.est.RiskAdjustedCost += penalty
				} else {
					result.est.RiskAdjustedCost = result.est.TotalCost
				}
				result.score = ScoreEstimatesWithProfile([]CostEstimate{result.est}, profile)
			}
			if result.score < bestScore {
				bestScore = result.score
				bestIdx = result.idx
				bestEstimate = result.est
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

const maxReuseCandidatesPerGroup = 12

type reuseCandidate struct {
	idx   int
	cap   InstanceCapacity
	score float64
}

func pruneReuseCandidates(group InstanceGroup, working []InstanceCapacity, preferReuse bool) []reuseCandidate {
	if len(working) == 0 {
		return nil
	}

	groupInputs := group.AllInputs()
	candidates := make([]reuseCandidate, 0, len(working))
	for idx, cap := range working {
		if !quickReuseCompatible(group, cap) {
			continue
		}
		candidates = append(candidates, reuseCandidate{
			idx:   idx,
			cap:   cap,
			score: reuseHeuristicScore(group, groupInputs, cap, preferReuse),
		})
	}
	if len(candidates) <= maxReuseCandidatesPerGroup {
		return candidates
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	return append([]reuseCandidate(nil), candidates[:maxReuseCandidatesPerGroup]...)
}

func quickReuseCompatible(group InstanceGroup, cap InstanceCapacity) bool {
	inst := cap.Instance
	if inst == nil {
		return false
	}
	if cap.GraceRemaining > 0 && cap.GraceRemaining < MinGraceRemaining {
		return false
	}
	constraints := placementConstraintsFromGroup(group)
	if constraints.GPUMemGB > 0 && inst.GPUMemGB <= 0 {
		constraints.GPUMemGB = 0
	}
	if ok, _ := matchInstanceTargetEligibility(constraints, inst, group.NumGPUs); !ok {
		return false
	}
	for _, job := range group.Jobs {
		if ok, _ := matchJobRequiredImage(job, inst); !ok {
			return false
		}
	}
	if group.HasComputeIntensiveJob() {
		floor := computeCPUCoresFloor()
		if inst.CPUCores <= 0 || inst.CPUCores < floor {
			return false
		}
	}
	return true
}

func placementConstraintsFromGroup(group InstanceGroup) placement.Constraints {
	return placement.Constraints{
		GPUClass:         group.GPUClass,
		Provider:         group.Provider,
		NumGPUs:          group.NumGPUs,
		GPUMemGB:         group.GPUMemGB,
		CPUCores:         group.CPUCores,
		CPUMemGB:         group.CPUMemGB,
		Interconnect:     group.Interconnect,
		MaxComputeCap:    group.MaxComputeCap,
		MinComputeCap:    group.MinComputeCap,
		MinCUDAVersion:   group.MinCUDAVersion,
		MinDriverVersion: group.MinDriverVersion,
	}
}

func reuseHeuristicScore(group InstanceGroup, groupInputs []string, cap InstanceCapacity, preferReuse bool) float64 {
	inst := cap.Instance
	if inst == nil {
		return math.Inf(-1)
	}

	score := 0.0
	if inst.Status == db.LaunchStatusGrace {
		score += 1000
	}
	score += float64(countOverlap(groupInputs, cap.ProvisionedInputs)) * 25
	if !preferReuse {
		score -= float64(cap.RunningJobCount) * 20
	}
	score += inst.DLPerf * 2
	if inst.GPUMemGB > 0 && group.GPUMemGB > 0 {
		headroom := inst.GPUMemGB - group.GPUMemGB
		if headroom < 0 {
			return math.Inf(-1)
		}
		score -= float64(headroom) * 0.5
	}
	if cap.DiskFreeGB > 0 {
		score += math.Min(float64(cap.DiskFreeGB), 200) * 0.05
	}
	return score
}

func reuseOpportunityPenaltyUSD(
	group InstanceGroup,
	cap InstanceCapacity,
	allGroups []InstanceGroup,
	working []InstanceCapacity,
	weight float64,
) float64 {
	if weight <= 0 || cap.Instance == nil || len(allGroups) == 0 {
		return 0
	}

	pressure := 0.0
	for _, other := range allGroups {
		if len(other.Jobs) == 0 {
			continue
		}
		// Penalize taking capacity from jobs that are at least as demanding.
		if other.GPUMemGB < group.GPUMemGB {
			continue
		}
		if !quickReuseCompatible(other, cap) {
			continue
		}
		compatibleSupply := 0
		for _, candidate := range working {
			if quickReuseCompatible(other, candidate) {
				compatibleSupply++
			}
		}
		if compatibleSupply == 0 {
			continue
		}
		demand := float64(len(other.Jobs))
		if demand < 1 {
			demand = 1
		}
		pressure += demand / float64(compatibleSupply)
	}
	if pressure == 0 {
		return 0
	}

	basePrice := float64(cap.Instance.CostPerHourCents) / 100.0
	if basePrice <= 0 {
		// Grace instances are "free" now but still consume scarce capacity.
		basePrice = 0.02 * float64(max(1, cap.Instance.GPUMemGB))
	}
	groupMem := max(1, group.GPUMemGB)
	headroom := float64(max(0, cap.Instance.GPUMemGB-groupMem)) / float64(groupMem)
	memFactor := 1.0 + headroom

	return weight * pressure * basePrice * memFactor
}

func reuseEstimateWorkerLimit(total int) int {
	if total <= 1 {
		return 1
	}
	limit := runtime.NumCPU()
	if limit < 2 {
		limit = 2
	}
	if limit > 8 {
		limit = 8
	}
	if total < limit {
		return total
	}
	return limit
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
	if len(group.VastCapAdd) > 0 {
		// Reuse candidates do not currently persist cap-add metadata. Avoid routing
		// capability-constrained groups to instances with unknown launch flags.
		return false, "requires fresh launch with vast-cap-add"
	}
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
	return estimateReuseGroupWithSharedData(reuseEstimateContext{
		database:      database,
		predCfg:       predCfg,
		overheadModel: overheadModel,
	}, group, cap)
}

type reuseEstimateContext struct {
	database      *sql.DB
	predCfg       *predictor.Config
	overheadModel *estimate.OverheadModel
	evaluator     *planEvaluator
}

func (c reuseEstimateContext) predictions(group InstanceGroup, gpuLabel string) map[int64]estimate.DurationPrediction {
	if c.evaluator != nil {
		return c.evaluator.reuseDurationPredictions(group, gpuLabel)
	}
	return estimateReuseDurationsDetailed(c.predCfg, gpuLabel, group.Jobs)
}

func (c reuseEstimateContext) downloadBytes(inputs []string) int64 {
	if c.evaluator != nil {
		return c.evaluator.reuseDownloadBytes(inputs)
	}
	if len(inputs) == 0 {
		return 0
	}
	totalBytes, _, _ := dataloc.ResolveInputSizes(inputs, nil)
	return totalBytes
}

func estimateReuseGroupWithSharedData(ctx reuseEstimateContext, group InstanceGroup, cap InstanceCapacity) (CostEstimate, bool) {
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
	downloadBytes := ctx.downloadBytes(incrementalInputs)

	instanceCtx := estimate.InstanceContext{
		DataCenter:      inst.DataCenter,
		DLPerf:          inst.DLPerf,
		InetDownMbps:    inst.InetDownMbps,
		InetUpMbps:      inst.InetUpMbps,
		DownloadedBytes: downloadBytes,
	}

	bw := effectiveDownloadBandwidth(ctx.database, &offer, cloud.MbpsToBytesPerSec(inst.InetDownMbps))
	provision := estimate.EstimateProvision(estimate.ProvisionInput{
		ModelDownloadBytes:   downloadBytes,
		BandwidthBytesPerSec: bw,
	})
	jobSetup := estimate.EstimateJobSetup(ctx.overheadModel, instanceCtx)
	upload := estimate.EstimateUpload(ctx.overheadModel, instanceCtx)

	var runEst estimate.Estimate
	jobDurations := make(map[int64]time.Duration, len(group.Jobs))
	jobDurationQuantiles := make(map[int64]*predictor.QuantilePrediction, len(group.Jobs))
	jobRuntimeMetadata := make(map[int64]predictor.RuntimeMetadata, len(group.Jobs))
	jobDurationList := make([]time.Duration, 0, len(group.Jobs))
	jobQuantileList := make([]*predictor.QuantilePrediction, 0, len(group.Jobs))
	jobMetadataList := make([]*predictor.RuntimeMetadata, 0, len(group.Jobs))
	runtimePred := offerRuntimePrediction{feasible: true, complete: true}
	gpuLabel := offer.GPUName
	if gpuLabel == "" {
		gpuLabel = inst.GPUClass
	}
	reusePredictions := ctx.predictions(group, gpuLabel)
	for idx, job := range group.Jobs {
		pred := estimate.DurationPrediction{Estimate: estimate.DefaultJobDuration}
		if got, ok := reusePredictions[int64(idx+1)]; ok {
			pred = got
		}
		runEst = runEst.Add(pred.Estimate)
		jobDurations[job.ID] = pred.Estimate.Mean
		if pred.Quantiles != nil {
			jobDurationQuantiles[job.ID] = pred.Quantiles
		}
		jobDurationList = append(jobDurationList, pred.Estimate.Mean)
		jobQuantileList = append(jobQuantileList, pred.Quantiles)
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
	runtimePred.jobQuantiles = jobQuantileList
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
		Group:                group,
		Offer:                groupOffer,
		Breakdown:            estimate.Breakdown{Provision: provision, JobSetup: jobSetup, Run: runEst, Upload: upload, Total: total},
		JobDurations:         jobDurations,
		JobDurationQuantiles: jobDurationQuantiles,
		JobRuntimeMetadata:   jobRuntimeMetadata,
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
			ID:         int64(idx + 1),
			Command:    job.Command,
			Project:    job.Project,
			GPUClass:   gpuLabel,
			WorkingDir: job.WorkingDir,
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
		CPUCores:          inst.CPUCores,
		CPUName:           inst.CPUName,
		RAMGB:             inst.RAMGB,
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

func scoreGroupingWithOverlap(estimates []CostEstimate, profile bidding.ScoreProfile, overlapSavedHours float64) float64 {
	return ScoreEstimatesWithProfile(estimates, profile) - profile.Weights().Time*overlapSavedHours
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
	evaluator := newPlanEvaluator(database, predCfg, overheadModel, survivalModel, minSurvival, defaultPlanOptions())
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
		score := scoreGroupingWithOverlap(estimates, profile, cand.overlapSavedHours)
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
	est.JobDurationQuantiles = make(map[int64]*predictor.QuantilePrediction, len(est.Group.Jobs))
	for idx, job := range est.Group.Jobs {
		if job == nil {
			continue
		}
		est.JobDurations[job.ID] = jobDurations[idx]
		if idx < len(pred.jobQuantiles) && pred.jobQuantiles[idx] != nil {
			est.JobDurationQuantiles[job.ID] = pred.jobQuantiles[idx]
		}
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

// filterOffersByClaim returns a copy of offers with entries dropped when a
// peer group earlier in this ranking pass has already claimed the same
// (provider, machine_id) or the same (provider, ProviderID). machine_id is
// the granularity vastai serializes create-instance on; ProviderID guards
// against duplicate offer IDs on machines where machine_id is empty
// (runpod, etc.). Empty maps short-circuit to the input slice.
func filterOffersByClaim(offers []cloud.Offer, claimedMachines, claimedOffers map[string]struct{}, distinctMachines bool) []cloud.Offer {
	if len(claimedMachines) == 0 && len(claimedOffers) == 0 {
		return offers
	}
	out := offers[:0:0]
	for _, o := range offers {
		if key := offerMachineClaimKey(o); key != "" {
			if _, taken := claimedMachines[key]; taken {
				continue
			}
		}
		if shouldClaimOfferID(o, distinctMachines) {
			if _, taken := claimedOffers[string(o.Provider)+"/"+o.ProviderID]; taken {
				continue
			}
		}
		out = append(out, o)
	}
	return out
}

func filterOffersByMachineAffinity(offers []cloud.Offer, machineAffinity map[string]struct{}) []cloud.Offer {
	if len(machineAffinity) == 0 {
		return offers
	}
	out := offers[:0:0]
	for _, o := range offers {
		if key := offerMachineClaimKey(o); key != "" {
			if _, allowed := machineAffinity[key]; allowed {
				out = append(out, o)
			}
		}
	}
	return out
}

func filterReusableByMachineAffinity(instances []InstanceCapacity, machineAffinity map[string]struct{}) []InstanceCapacity {
	if len(machineAffinity) == 0 {
		return instances
	}
	out := instances[:0:0]
	for _, inst := range instances {
		if inst.Instance == nil {
			continue
		}
		key := db.ProviderMachineKey(inst.Instance.Provider, inst.Instance.MachineID)
		if _, allowed := machineAffinity[key]; allowed {
			out = append(out, inst)
		}
	}
	return out
}

// recordOfferClaim marks an offer as claimed for the remainder of the
// ranking pass.
func recordOfferClaim(o cloud.Offer, claimedMachines, claimedOffers map[string]struct{}, distinctMachines bool) {
	if key := offerMachineClaimKey(o); key != "" {
		claimedMachines[key] = struct{}{}
	}
	if shouldClaimOfferID(o, distinctMachines) {
		claimedOffers[string(o.Provider)+"/"+o.ProviderID] = struct{}{}
	}
}

func offerMachineClaimKey(o cloud.Offer) string {
	return db.ProviderMachineKey(string(o.Provider), o.MachineID)
}

func shouldClaimOfferID(o cloud.Offer, distinctMachines bool) bool {
	if o.ProviderID == "" {
		return false
	}
	return !(distinctMachines && o.Provider == cloud.ProviderRunpod && strings.TrimSpace(o.MachineID) == "")
}

func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}
