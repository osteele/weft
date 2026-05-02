package campaign

import (
	"database/sql"
	"fmt"
	"math"
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
	LaunchGroups     []LaunchGroup
	LaunchJobIDs     []int64
	// LaunchRateCentsPerHour is the projected aggregate $/hr for new instances
	// implied by this pass's launchable candidate groups.
	LaunchRateCentsPerHour int
	BlockedReasons         map[int64]string
}

// LaunchGroup captures one launchable candidate group and its projected run-rate cost.
type LaunchGroup struct {
	JobIDs           []int64
	CostPerHourCents int
	Offer            *cloud.Offer
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
	return BuildAutoPlacementPlanWithOptions(
		database,
		cfg,
		clients,
		jobs,
		reusable,
		predCfg,
		overheadModel,
		survivalModel,
		minSurvival,
		AutoPlannerOptions(cfg),
	)
}

// BuildAutoPlacementPlanWithOptions computes a single-pass reuse-vs-launch plan
// for the provided queued jobs using explicit planner options.
func BuildAutoPlacementPlanWithOptions(
	database *sql.DB,
	cfg *config.Config,
	clients []cloud.Client,
	jobs []*db.Job,
	reusable []InstanceCapacity,
	predCfg *predictor.Config,
	overheadModel *estimate.OverheadModel,
	survivalModel *bidding.SurvivalModel,
	minSurvival float64,
	options PlanOptions,
) (AutoPlacementPlan, error) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	if len(jobs) == 0 {
		return plan, nil
	}

	groups := GroupByAffinity(jobs, nil)
	groups = SplitGroupsByImage(database, groups)
	groups = ApplyImageMetadataRequirements(cfg, groups)
	if len(groups) == 0 {
		return plan, nil
	}

	profile := AutoPlannerProfile(cfg)
	minReliability := 0.95
	if cfg != nil {
		minReliability = cfg.CampaignReliability()
	}
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
		minReliability,
		minSurvival,
		nil,
		options,
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
		applyGroupOffer(&plan, group, offer, reused, minReliability)
	}
	plan.LaunchGroups = buildLaunchGroups(strategyPlan.NewCandidate, reused)
	plan.LaunchRateCentsPerHour = 0
	for _, group := range plan.LaunchGroups {
		plan.LaunchRateCentsPerHour += group.CostPerHourCents
		plan.LaunchJobIDs = append(plan.LaunchJobIDs, group.JobIDs...)
	}

	sort.Slice(plan.LaunchJobIDs, func(i, j int) bool { return plan.LaunchJobIDs[i] < plan.LaunchJobIDs[j] })
	return plan, nil
}

func buildLaunchGroups(candidate *CandidateResult, reused map[int64]struct{}) []LaunchGroup {
	if candidate == nil || len(candidate.Groups) == 0 || len(candidate.Offers) == 0 {
		return nil
	}
	launchGroups := make([]LaunchGroup, 0, len(candidate.Groups))
	for idx, group := range candidate.Groups {
		if idx < 0 || idx >= len(candidate.Offers) {
			continue
		}
		groupOffer := candidate.Offers[idx]
		if groupOffer.Err != nil || groupOffer.Offer == nil {
			continue
		}
		jobIDs := make([]int64, 0, len(group.Jobs))
		for _, job := range group.Jobs {
			if job == nil {
				continue
			}
			if _, ok := reused[job.ID]; ok {
				continue
			}
			jobIDs = append(jobIDs, job.ID)
		}
		if len(jobIDs) == 0 {
			continue
		}
		sort.Slice(jobIDs, func(i, j int) bool { return jobIDs[i] < jobIDs[j] })
		launchGroups = append(launchGroups, LaunchGroup{
			JobIDs:           jobIDs,
			CostPerHourCents: int(math.Round(groupOffer.Offer.CostPerHour * 100)),
			Offer:            groupOffer.Offer,
		})
	}
	return launchGroups
}

func applyGroupOffer(plan *AutoPlacementPlan, group InstanceGroup, offer GroupOffer, reused map[int64]struct{}, minReliability float64) {
	launchable := offer.Offer != nil && offer.Err == nil
	reason := ""
	if !launchable {
		if offer.Err != nil {
			reason = offer.Err.Error()
		} else {
			constraintStr := FormatOfferConstraints(offerConstraintsForGroup(group, minReliability))
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
		if !launchable {
			plan.BlockedReasons[job.ID] = "planner: " + SanitizeBlockedReason(reason)
		}
	}
}

// SanitizeBlockedReason collapses multi-line errors (e.g. Python tracebacks
// bubbled up from the vastai CLI, or wrapped S3 upload failures) into a compact
// single-line summary suitable for TUI display and oplog details. It keeps the
// first non-empty line and appends the final exception-style line when present,
// then truncates to a reasonable length.
func SanitizeBlockedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return reason
	}
	lines := strings.Split(reason, "\n")
	first := ""
	last := ""
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		last = ln
	}
	summary := first
	if last != "" && last != first && looksLikeExceptionLine(last) {
		summary = first + " … " + last
	}
	const maxLen = 240
	if len(summary) > maxLen {
		summary = summary[:maxLen-1] + "…"
	}
	return summary
}

func looksLikeExceptionLine(s string) bool {
	// Heuristic: Python exception lines are "pkg.Error: message" — a
	// colon-separated identifier prefix with no spaces before the colon.
	idx := strings.Index(s, ":")
	if idx <= 0 {
		return false
	}
	prefix := s[:idx]
	if strings.ContainsAny(prefix, " \t") {
		return false
	}
	return strings.Contains(prefix, "Error") || strings.Contains(prefix, "Exception")
}
