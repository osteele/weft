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

func PrepareLaunchExecutionPlan(database *sql.DB, clients []cloud.Client, providerErr error, groups []InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel) (LaunchExecutionPlan, error) {
	return PrepareLaunchExecutionPlanWithProgress(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel, nil)
}

func PrepareLaunchExecutionPlanWithProgress(database *sql.DB, clients []cloud.Client, providerErr error, groups []InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel, onProgress PlanProgressFunc) (LaunchExecutionPlan, error) {
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

	plans, _ := BuildProfilePlansWithProgress(
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
