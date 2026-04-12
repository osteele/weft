package campaign

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

// AutoPlacementPlan captures planner decisions for one unattended auto pass.
type AutoPlacementPlan struct {
	ReuseAssignments []ReuseAssignment
	LaunchJobIDs     []int64
	BlockedReasons   map[int64]string
}

// AutoPlannerProfile returns the cost/time profile for unattended auto mode.
func AutoPlannerProfile(cfg *config.Config) bidding.ScoreProfile {
	objective := "cost_first"
	if cfg != nil {
		objective = cfg.AutoObjective()
	}
	switch objective {
	case "balanced":
		return bidding.ScoreProfile{ID: "auto_balanced", Weights_: bidding.StrategyWeights{Cost: 1.0, Time: 1.0}}
	case "time_first":
		return bidding.ScoreProfile{ID: "auto_time_first", Weights_: bidding.StrategyWeights{Cost: 0.35, Time: 1.0}}
	default:
		return bidding.ScoreProfile{ID: "auto_cost_first", Weights_: bidding.StrategyWeights{Cost: 1.0, Time: 0.35}}
	}
}

// AutoPlannerOptions returns planner options for unattended auto mode.
func AutoPlannerOptions(cfg *config.Config) PlanOptions {
	opts := defaultPlanOptions()
	if cfg != nil {
		opts.OpportunityCostWeight = cfg.CampaignOpportunityCostWeight()
	}
	return opts
}

// BuildAutoPlacementPlan computes a single-pass reuse-vs-launch plan for the
// provided queued jobs.
func BuildAutoPlacementPlan(
	database *sql.DB,
	cfg *config.Config,
	clients []cloud.Client,
	jobs []*db.Job,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	minSurvival float64,
) (AutoPlacementPlan, error) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	if len(jobs) == 0 {
		return plan, nil
	}

	groups := GroupByAffinity(jobs, nil)
	groups = SplitGroupsByImage(groups)
	if len(groups) == 0 {
		return plan, nil
	}

	profile := AutoPlannerProfile(cfg)
	specs := []ProfilePlanSpec{{Profile: profile, CandidateMode: CandidatePlanModeFull}}
	plans := BuildProfilePlansFromSplitRawWithPlanSpecsAndOptions(
		database,
		clients,
		groups,
		nil,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		specs,
		minSurvival,
		nil,
		AutoPlannerOptions(cfg),
	)
	strategyPlan, ok := plans[profile.ID]
	if !ok {
		return plan, fmt.Errorf("auto planner did not return profile %q", profile.ID)
	}
	plan.ReuseAssignments = append(plan.ReuseAssignments, strategyPlan.ReuseAssignments...)

	reused := make(map[int64]struct{}, len(strategyPlan.ReuseAssignments))
	for _, assignment := range strategyPlan.ReuseAssignments {
		if assignment.Job != nil {
			reused[assignment.Job.ID] = struct{}{}
		}
	}

	for i, group := range groups {
		if len(group.Jobs) == 0 {
			continue
		}
		offer := GroupOffer{}
		if i < len(strategyPlan.DisplayOffers) {
			offer = strategyPlan.DisplayOffers[i]
		}
		applyGroupOffer(&plan, group, offer, reused)
	}

	sort.Slice(plan.LaunchJobIDs, func(i, j int) bool { return plan.LaunchJobIDs[i] < plan.LaunchJobIDs[j] })
	return plan, nil
}

func applyGroupOffer(plan *AutoPlacementPlan, group InstanceGroup, offer GroupOffer, reused map[int64]struct{}) {
	launchable := offer.Offer != nil && offer.Err == nil
	reason := ""
	if !launchable {
		if offer.Err != nil {
			reason = offer.Err.Error()
		} else {
			constraintStr := FormatOfferConstraints(offerConstraintsForGroup(group))
			reason = offer.FilterStats.NoOffersDetail(constraintStr)
		}
	}
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		if _, ok := reused[job.ID]; ok {
			continue
		}
		if launchable {
			plan.LaunchJobIDs = append(plan.LaunchJobIDs, job.ID)
			continue
		}
		plan.BlockedReasons[job.ID] = "planner: " + strings.TrimSpace(reason)
	}
}
