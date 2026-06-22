package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

type LaunchExecutionPlan struct {
	RequestedJobs     int
	StrategyPlan      StrategyPlan
	LaunchGroups      []InstanceGroup
	LaunchGroupOffers []GroupOffer
	Offers            []cloud.Offer
	Estimates         []CostEstimate
}

type LaunchExecutionPlanOptions struct {
	InitialClaimedMachines map[string]struct{}
}

func PrepareLaunchExecutionPlan(database *sql.DB, clients []cloud.Client, providerErr error, groups []InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel) (LaunchExecutionPlan, error) {
	return PrepareLaunchExecutionPlanWithProgress(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel, nil)
}

func PrepareLaunchExecutionPlanWithProgress(database *sql.DB, clients []cloud.Client, providerErr error, groups []InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel, onProgress PlanProgressFunc) (LaunchExecutionPlan, error) {
	return PrepareLaunchExecutionPlanWithProgressAndOptions(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel, onProgress, LaunchExecutionPlanOptions{})
}

func PrepareLaunchExecutionPlanWithProgressAndOptions(database *sql.DB, clients []cloud.Client, providerErr error, groups []InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel, onProgress PlanProgressFunc, options LaunchExecutionPlanOptions) (LaunchExecutionPlan, error) {
	selectedGroups, requestedJobs := SelectLaunchGroups(groups, selected)
	plan := LaunchExecutionPlan{RequestedJobs: requestedJobs}
	if requestedJobs == 0 {
		return plan, nil
	}

	reusable, _ := FindReusableInstances(database)
	if providerErr != nil && len(reusable) == 0 {
		return plan, providerErr
	}

	effectiveMinSurvival := survivalModel.HealthFloor(minSurvival)

	plans, _ := BuildProfilePlansWithProgressAndOptions(
		database,
		clients,
		selectedGroups,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		[]bidding.ScoreProfile{profile},
		minReliability,
		effectiveMinSurvival,
		onProgress,
		PlanOptions{InitialClaimedMachines: options.InitialClaimedMachines},
	)
	strategyPlan, ok := plans[profile.ID]
	if !ok {
		return plan, fmt.Errorf("could not build launch plan for profile %s", profile.ID)
	}
	plan.StrategyPlan = strategyPlan
	if strategyPlan.NewCandidate == nil {
		return plan, nil
	}

	plan.LaunchGroups, plan.Offers, plan.LaunchGroupOffers, plan.Estimates = CollectLaunchExecution(strategyPlan.NewCandidate)
	return plan, nil
}

func SelectLaunchGroups(groups []InstanceGroup, selected map[int64]bool) ([]InstanceGroup, int) {
	var selectedGroups []InstanceGroup
	requestedJobs := 0
	for _, group := range groups {
		var selectedJobs = group.Jobs[:0:0]
		for _, job := range group.Jobs {
			if selected == nil || selected[job.ID] {
				selectedJobs = append(selectedJobs, job)
			}
		}
		if len(selectedJobs) == 0 {
			continue
		}
		requestedJobs += len(selectedJobs)
		selectedGroup := group
		selectedGroup.Jobs = selectedJobs
		selectedGroups = append(selectedGroups, selectedGroup)
	}
	return selectedGroups, requestedJobs
}

// PrepareNewInstanceLaunchPlan is PrepareLaunchExecutionPlan's
// force-new-instances variant: it never considers reusing an existing
// rental. The split/merged/parallel candidate-grouping evaluation still
// runs, so `AssetOverlapLaunchGrouping`'s overlap scoring (see
// specs/campaign-lifecycle.allium) still applies. Used by
// `weft move --to new`, whose user contract requires fresh instances even
// when a reuse target would be cheaper.
//
// `reusable` is intentionally passed as an empty (non-nil) slice so
// BuildProfilePlansWithProgress skips its `FindReusableInstances` fetch and
// chooseReuseGroups returns no decisions; the full set of split groups then
// proceeds through new-candidate evaluation.
func PrepareNewInstanceLaunchPlan(
	database *sql.DB,
	clients []cloud.Client,
	providerErr error,
	groups []InstanceGroup,
	selected map[int64]bool,
	profile bidding.ScoreProfile,
	minSurvival float64,
	minReliability float64,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	onProgress PlanProgressFunc,
) (LaunchExecutionPlan, error) {
	return PrepareNewInstanceLaunchPlanWithOptions(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel, onProgress, LaunchExecutionPlanOptions{})
}

func PrepareNewInstanceLaunchPlanWithOptions(
	database *sql.DB,
	clients []cloud.Client,
	providerErr error,
	groups []InstanceGroup,
	selected map[int64]bool,
	profile bidding.ScoreProfile,
	minSurvival float64,
	minReliability float64,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	onProgress PlanProgressFunc,
	options LaunchExecutionPlanOptions,
) (LaunchExecutionPlan, error) {
	selectedGroups, requestedJobs := SelectLaunchGroups(groups, selected)
	plan := LaunchExecutionPlan{RequestedJobs: requestedJobs}
	if requestedJobs == 0 {
		return plan, nil
	}
	if providerErr != nil {
		return plan, providerErr
	}

	effectiveMinSurvival := survivalModel.HealthFloor(minSurvival)
	plans, _ := BuildProfilePlansWithProgressAndOptions(
		database,
		clients,
		selectedGroups,
		[]InstanceCapacity{},
		predCfg,
		overheadModel,
		survivalModel,
		[]bidding.ScoreProfile{profile},
		minReliability,
		effectiveMinSurvival,
		onProgress,
		PlanOptions{InitialClaimedMachines: options.InitialClaimedMachines},
	)
	strategyPlan, ok := plans[profile.ID]
	if !ok {
		return plan, fmt.Errorf("could not build launch plan for profile %s", profile.ID)
	}
	plan.StrategyPlan = strategyPlan
	if strategyPlan.NewCandidate == nil {
		return plan, nil
	}
	plan.LaunchGroups, plan.Offers, plan.LaunchGroupOffers, plan.Estimates = CollectLaunchExecution(strategyPlan.NewCandidate)
	return plan, nil
}

func CollectLaunchExecution(result *CandidateResult) ([]InstanceGroup, []cloud.Offer, []GroupOffer, []CostEstimate) {
	if result == nil {
		return nil, nil, nil, nil
	}

	var groups []InstanceGroup
	var offers []cloud.Offer
	var groupOffers []GroupOffer
	var estimates []CostEstimate
	for i, groupOffer := range result.Offers {
		if groupOffer.Err != nil || groupOffer.Offer == nil {
			continue
		}
		offerCopy := *groupOffer.Offer
		groups = append(groups, groupOffer.Group)
		offers = append(offers, offerCopy)
		groupOffers = append(groupOffers, GroupOffer{
			Group:          groupOffer.Group,
			Offer:          &offerCopy,
			SurvivalProb:   groupOffer.SurvivalProb,
			RejectedGroups: append([]bidding.RejectedGroup(nil), groupOffer.RejectedGroups...),
			Alternatives:   append([]RankedOfferAlternative(nil), groupOffer.Alternatives...),
		})
		if i < len(result.Estimates) {
			estimateCopy := result.Estimates[i]
			estimateCopy.Offer = groupOffers[len(groupOffers)-1]
			estimates = append(estimates, estimateCopy)
		}
	}

	return groups, offers, groupOffers, estimates
}
