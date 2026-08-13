package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/blockkind"
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
	// BlockedReasons carries the sanitized, single-line reason for each job
	// the planner could not place. Use these for display surfaces that need a
	// compact form.
	BlockedReasons map[int64]string
	// BlockedReasonDetails carries the full, untruncated underlying reason
	// (e.g. multi-line vastai stderr) per job ID, suitable for the TUI
	// disclosure expand view and CLI diagnose/show surfaces. Entries are
	// present only when the detail is meaningfully richer than the sanitized
	// BlockedReasons entry — callers should fall back to BlockedReasons when
	// the map has no entry for a job.
	BlockedReasonDetails map[int64]string
	// BlockedReasonFingerprints carries the stable error-class fingerprint
	// (e.g. "vastai/search-offers/400/bad-field:driver_vers") per job ID
	// when applyGroupOffer's offer.Err was a *cloud.ProviderError. Display
	// surfaces use these to coalesce systemic upstream failures across many
	// jobs into one incident headline instead of fragmenting per
	// filter-prefix variation. Empty entries mean "no upstream
	// classification available" and the row keeps today's per-message
	// bucketing.
	BlockedReasonFingerprints map[int64]string
}

// LaunchGroup captures one launchable candidate group and its projected run-rate cost.
type LaunchGroup struct {
	JobIDs           []int64
	CostPerHourCents int
	Offer            *cloud.Offer
	Priority         int
	// RateCapAuthorized is true when every job in the group carries an
	// explicit hourly-rate cap and the selected offer satisfies all caps.
	// Such a group bypasses the global unattended run-rate soft target.
	RateCapAuthorized bool
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
		opts.MinReliability = cfg.CampaignReliability()
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
	ApplyBidLossEscalation(database, groups)
	groups = EstimateGroupDisks(groups, database, nil)
	if len(groups) == 0 {
		return plan, fmt.Errorf("auto planner produced no groups for %d queued jobs", len(jobs))
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
		options.RawOffers,
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
	// Every job must leave the planner classified as reuse, launch, or blocked
	// by a concrete offer/capacity reason. A missing classification is an
	// internal planner defect, not a user-facing placement reason.
	launchedIDs := make(map[int64]struct{}, 16)
	for _, lg := range plan.LaunchGroups {
		for _, id := range lg.JobIDs {
			launchedIDs[id] = struct{}{}
		}
	}
	var unclassified []int64
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if _, ok := reused[job.ID]; ok {
			continue
		}
		if _, ok := launchedIDs[job.ID]; ok {
			continue
		}
		if _, ok := plan.BlockedReasons[job.ID]; ok {
			continue
		}
		unclassified = append(unclassified, job.ID)
	}
	if len(unclassified) > 0 {
		return plan, fmt.Errorf("auto planner left queued jobs unclassified: %v", unclassified)
	}
	plan.LaunchRateCentsPerHour = 0
	for _, group := range plan.LaunchGroups {
		plan.LaunchRateCentsPerHour += group.CostPerHourCents
		plan.LaunchJobIDs = append(plan.LaunchJobIDs, group.JobIDs...)
	}

	sortLaunchJobIDsByScheduling(plan.LaunchJobIDs, jobs)
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
		sort.SliceStable(jobIDs, func(i, j int) bool {
			return db.SchedulingLess(jobByID(group.Jobs, jobIDs[i]), jobByID(group.Jobs, jobIDs[j]))
		})
		launchGroups = append(launchGroups, LaunchGroup{
			JobIDs:            jobIDs,
			CostPerHourCents:  int(math.Round(groupOffer.Offer.CostPerHour * 100)),
			Offer:             groupOffer.Offer,
			Priority:          maxJobPriority(group.Jobs),
			RateCapAuthorized: launchGroupHasAuthorizedRateCap(group, *groupOffer.Offer),
		})
	}
	return launchGroups
}

func maxJobPriority(jobs []*db.Job) int {
	maxPriority := 0
	for _, job := range jobs {
		if job != nil && job.Priority > maxPriority {
			maxPriority = job.Priority
		}
	}
	return maxPriority
}

func sortLaunchJobIDsByScheduling(jobIDs []int64, jobs []*db.Job) {
	sort.SliceStable(jobIDs, func(i, j int) bool {
		return db.SchedulingLess(jobByID(jobs, jobIDs[i]), jobByID(jobs, jobIDs[j]))
	})
}

func jobByID(jobs []*db.Job, id int64) *db.Job {
	for _, job := range jobs {
		if job != nil && job.ID == id {
			return job
		}
	}
	return &db.Job{ID: id}
}

func applyGroupOffer(plan *AutoPlacementPlan, group InstanceGroup, offer GroupOffer, reused map[int64]struct{}, minReliability float64) {
	launchable := offer.Offer != nil && offer.Err == nil
	rawReason := ""
	fingerprint := ""
	if !launchable {
		if offer.Err != nil {
			rawReason = offer.Err.Error()
			fingerprint = cloud.FingerprintOf(offer.Err)
			var imageErr *ImagePrestartFailureError
			if errors.As(offer.Err, &imageErr) {
				fingerprint = imageErr.Fingerprint()
			}
		} else {
			constraintStr := FormatOfferConstraints(offerConstraintsForGroup(group, minReliability))
			rawReason = offer.FilterStats.NoOffersDetail(constraintStr)
			// Empty-after-filter is itself a categorized failure; coalesce
			// jobs that share the same filter-stage failure into one incident
			// even though the constraint strings differ across groups.
			fingerprint = filterStatsFingerprint(offer.FilterStats)
		}
	}
	compact := SanitizeBlockedReason(rawReason)
	full := strings.TrimSpace(rawReason)
	// Whether the full text actually adds information beyond the compact form
	// (it does when SanitizeBlockedReason collapsed multiple lines or hit the
	// 240-char cap). Offer-fetch failures are intentionally preserved even
	// when single-line: the compact line tells users the market is unknown,
	// while the detail carries the concrete provider/cache cause.
	detailAddsInfo := full != "" && full != compact &&
		(strings.ContainsRune(full, '\n') || len(full) > len(compact) || errors.Is(offer.Err, ErrOfferSnapshotUnavailable))
	detail := full
	if errors.Is(offer.Err, ErrOfferSnapshotUnavailable) {
		detail = offerFetchUnavailableDetail(full)
	}
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		if _, ok := reused[job.ID]; ok {
			continue
		}
		if !launchable {
			plan.BlockedReasons[job.ID] = "planner: " + compact
			if detailAddsInfo {
				if plan.BlockedReasonDetails == nil {
					plan.BlockedReasonDetails = map[int64]string{}
				}
				plan.BlockedReasonDetails[job.ID] = detail
			}
			if fingerprint != "" {
				if plan.BlockedReasonFingerprints == nil {
					plan.BlockedReasonFingerprints = map[int64]string{}
				}
				plan.BlockedReasonFingerprints[job.ID] = fingerprint
			}
		}
	}
}

func offerFetchUnavailableDetail(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	const prefix = "offer fetch unavailable"
	if detail, ok := strings.CutPrefix(reason, prefix+":"); ok {
		return strings.TrimSpace(detail)
	}
	return reason
}

// filterStatsFingerprint maps the OfferFilterStats post-filter outcome to a
// stable coalescing key. Returns "" when the stats don't identify a
// categorical failure stage (so the caller falls back to today's per-message
// bucketing).
func filterStatsFingerprint(s OfferFilterStats) string {
	switch {
	case s.RawCount == 0:
		return "vastai/search-offers/empty-result:no-offers"
	case s.SKUMemoryFiltered > 0 && s.AfterSKUMemory == 0:
		return "vastai/search-offers/empty-result:sku-memory"
	case s.AfterVRAM == 0:
		return "vastai/search-offers/empty-result:vram"
	case s.AfterCUDA == 0:
		return "vastai/search-offers/empty-result:cuda"
	case s.ProviderCompatibilityFiltered > 0 && s.AfterProvider == 0:
		return "vastai/search-offers/empty-result:provider-driver"
	case s.ForwardCompatFiltered > 0 && s.AfterForward == 0:
		return "vastai/search-offers/empty-result:forward-compat-driver"
	case (s.TorchArchMinCap != "" || s.TorchArchMaxCap != "") && s.AfterTorchArch == 0:
		return "vastai/search-offers/empty-result:torch-arch"
	case s.HourlyRateCapCents > 0 && s.AfterHourlyRate == 0:
		return "vastai/search-offers/empty-result:hourly-rate-cap"
	case s.AfterSurvival == 0:
		return "vastai/search-offers/empty-result:survival"
	}
	return ""
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
	summary = strings.ReplaceAll(summary, "provider returned success=false: provider rejected request", "provider rejected request")
	summary = strings.ReplaceAll(summary,
		"There are no longer any instances available with the requested specifications. Please refresh and try again.",
		"requested instance type is no longer available; Weft will retry with fresh offers")
	if isRetryableCreateProviderRejection(summary) && !strings.Contains(summary, "Weft will retry with fresh offers") {
		summary += "; Weft will retry with fresh offers"
	}
	summary = blockkind.NormalizeOfferFetchUnavailable(summary)
	summary = strings.TrimRight(summary, ".;")
	const maxLen = 240
	if len(summary) > maxLen {
		summary = summary[:maxLen-1] + "…"
	}
	return summary
}

func isRetryableCreateProviderRejection(reason string) bool {
	reason = strings.ToLower(reason)
	return strings.Contains(reason, "provider rejected request") &&
		strings.Contains(reason, "create-instance")
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
