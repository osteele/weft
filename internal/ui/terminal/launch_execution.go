package terminal

import (
	"database/sql"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

type launchExecutionPlan = campaign.LaunchExecutionPlan

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
	options campaign.LaunchExecutionPlanOptions,
) (launchExecutionPlan, error) {
	return campaign.PrepareLaunchExecutionPlanWithProgressAndOptions(
		database,
		clients,
		providerErr,
		groups,
		selected,
		profile,
		minSurvival,
		minReliability,
		predCfg,
		overheadModel,
		survivalModel,
		onProgress,
		options,
	)
}

func selectLaunchGroups(groups []campaign.InstanceGroup, selected map[int64]bool) ([]campaign.InstanceGroup, int) {
	return campaign.SelectLaunchGroups(groups, selected)
}

func collectLaunchExecution(result *campaign.CandidateResult) ([]campaign.InstanceGroup, []cloud.Offer, []campaign.GroupOffer, []campaign.CostEstimate) {
	return campaign.CollectLaunchExecution(result)
}
