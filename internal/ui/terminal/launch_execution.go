package terminal

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

type launchExecutionPlan struct {
	RequestedJobs     int
	StrategyPlan      campaign.StrategyPlan
	LaunchGroups      []campaign.InstanceGroup
	LaunchGroupOffers []campaign.GroupOffer
	Offers            []cloud.Offer
	Estimates         []campaign.CostEstimate
}

func prepareLaunchExecutionPlan(
	database *sql.DB,
	clients []cloud.Client,
	providerErr error,
	groups []campaign.InstanceGroup,
	selected map[int64]bool,
	profile bidding.ScoreProfile,
	minSurvival float64,
	minReliability float64,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	onProgress campaign.PlanProgressFunc,
) (launchExecutionPlan, error) {
	selectedGroups, requestedJobs := selectLaunchGroups(groups, selected)
	plan := launchExecutionPlan{
		RequestedJobs: requestedJobs,
	}
	if requestedJobs == 0 {
		return plan, nil
	}

	reusable, _ := campaign.FindReusableInstances(database)
	if providerErr != nil && len(reusable) == 0 {
		return plan, providerErr
	}

	// HealthFloor temporarily raises minSurvival during bad-day windows.
	effectiveMinSurvival := survivalModel.HealthFloor(minSurvival)

	plans, _ := campaign.BuildProfilePlansWithProgress(
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

	plan.LaunchGroups, plan.Offers, plan.LaunchGroupOffers, plan.Estimates = collectLaunchExecution(strategyPlan.NewCandidate)
	return plan, nil
}

func selectLaunchGroups(groups []campaign.InstanceGroup, selected map[int64]bool) ([]campaign.InstanceGroup, int) {
	var selectedGroups []campaign.InstanceGroup
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
		selectedGroups = append(selectedGroups, campaign.InstanceGroup{
			GPUClass:    group.GPUClass,
			Provider:    group.Provider,
			GPUMemGB:    group.GPUMemGB,
			MaxGPUMemGB: group.MaxGPUMemGB,
			DiskGB:      group.DiskGB,
			Image:       group.Image,
			VastCapAdd:  group.VastCapAdd,
			Preemptible: group.Preemptible,
			Jobs:        selectedJobs,
		})
	}
	return selectedGroups, requestedJobs
}

func collectLaunchExecution(result *campaign.CandidateResult) ([]campaign.InstanceGroup, []cloud.Offer, []campaign.GroupOffer, []campaign.CostEstimate) {
	if result == nil {
		return nil, nil, nil, nil
	}

	var groups []campaign.InstanceGroup
	var offers []cloud.Offer
	var groupOffers []campaign.GroupOffer
	var estimates []campaign.CostEstimate
	for i, groupOffer := range result.Offers {
		if groupOffer.Err != nil || groupOffer.Offer == nil {
			continue
		}
		offerCopy := *groupOffer.Offer
		groups = append(groups, groupOffer.Group)
		offers = append(offers, offerCopy)
		groupOffers = append(groupOffers, campaign.GroupOffer{
			Group:          groupOffer.Group,
			Offer:          &offerCopy,
			SurvivalProb:   groupOffer.SurvivalProb,
			RejectedGroups: append([]bidding.RejectedGroup(nil), groupOffer.RejectedGroups...),
		})
		if i < len(result.Estimates) {
			estimateCopy := result.Estimates[i]
			estimateCopy.Offer = groupOffers[len(groupOffers)-1]
			estimates = append(estimates, estimateCopy)
		}
	}

	return groups, offers, groupOffers, estimates
}
