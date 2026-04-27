package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
)

type GroupedAutoPilotResult struct {
	Placed         int
	Rebalanced     int
	Launched       int
	LaunchedClass  string
	BlockedReasons map[int64]string
}

var autoPilotBuildPlan = buildAutoPlacementPlan
var autoPilotBuildPlanWithOptions = buildAutoPlacementPlanWithOptions

var autoPilotRelaunch = RelaunchOrphanedJobs
var autoPilotSubmitJobsToInstance = campaign.SubmitJobsToInstance

func RunGroupedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job) (*GroupedAutoPilotResult, error) {
	if database == nil {
		return &GroupedAutoPilotResult{}, nil
	}
	scoped := make(map[int64]struct{}, len(scopedJobs))
	for _, job := range scopedJobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, err
	}
	unplaced := make([]*db.Job, 0, len(unplacedJobs))
	for _, job := range unplacedJobs {
		// IsUnplacedAwaitingPlacement (rather than IsUnplacedQueued) so jobs
		// stuck in pending_placement still get a blocked reason — see
		// internal/orchestration/pending_placement.go.
		if !job.IsUnplacedAwaitingPlacement() {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		unplaced = append(unplaced, job)
	}
	if len(unplaced) == 0 {
		rebalanceResult, err := RebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
			Apply:     true,
			Operation: "auto_pilot.rebalance",
		})
		if err != nil {
			return nil, err
		}
		return &GroupedAutoPilotResult{
			Rebalanced: len(rebalanceResult.Moves),
		}, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	r2Client, _ := BuildR2Client(cfg)

	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return nil, err
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	for _, ci := range launches {
		jobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(jobs)); ok {
			capacities = append(capacities, cap)
		}
	}

	plan, err := autoPilotBuildPlan(database, cfg, unplaced, capacities)
	if err != nil {
		oplog.Log("auto_pilot.planner_error",
			oplog.WithError(err),
			oplog.WithDetailf("unplaced=%d capacities=%d", len(unplaced), len(capacities)))
		return nil, err
	}
	blockedReasons := map[int64]string{}
	for jobID, reason := range plan.BlockedReasons {
		if strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}
	planDetail := fmt.Sprintf("unplaced=%d capacities=%d reuse=%d launch=%d blocked=%d",
		len(unplaced), len(capacities), len(plan.ReuseAssignments), len(plan.LaunchJobIDs), len(blockedReasons))
	if sampleReason := sampleBlockedReason(blockedReasons); sampleReason != "" {
		planDetail += fmt.Sprintf(" reason=%q", sampleReason)
	}
	oplog.Log("auto_pilot.plan", oplog.WithDetail(planDetail))

	runRateTarget := cfg.AutoRunRateSoftTargetCentsPerHour()
	reusePreferredFallback := false
	if runRateTarget > 0 && len(plan.LaunchJobIDs) > 0 && plan.LaunchRateCentsPerHour > 0 {
		currentRate, rateErr := db.SumActiveLaunchCostPerHourCents(database)
		if rateErr == nil {
			projectedRate := currentRate + plan.LaunchRateCentsPerHour
			if projectedRate > runRateTarget {
				headroom := max(0, runRateTarget-currentRate)
				acceptedGroups, rejectedGroups, usedHeadroom := selectLaunchGroupsWithinHeadroom(plan.LaunchGroups, headroom)
				remainingHeadroom := max(0, headroom-usedHeadroom)
				oplog.Log("auto_pilot.run_rate_subset",
					oplog.WithDetailf(
						"headroom=%s/hr accepted_groups=%d rejected_groups=%d accepted_rate=%s/hr rejected_rate=%s/hr",
						formatRateCents(headroom),
						len(acceptedGroups),
						len(rejectedGroups),
						formatRateCents(sumLaunchGroupCost(acceptedGroups)),
						formatRateCents(sumLaunchGroupCost(rejectedGroups)),
					))
				if len(acceptedGroups) > 0 {
					applyAcceptedLaunchGroups(&plan, acceptedGroups, blockedReasons)
					for _, group := range rejectedGroups {
						reason := fmt.Sprintf(
							"run-rate headroom exhausted (%s/hr free, this group needs %s/hr)",
							formatRateCents(remainingHeadroom),
							formatRateCents(group.CostPerHourCents),
						)
						for _, jobID := range group.JobIDs {
							blockedReasons[jobID] = reason
						}
					}
				} else {
					cheapest := cheapestLaunchGroupCost(plan.LaunchGroups)
					if cheapest > 0 && headroom < cheapest && len(capacities) > 0 {
						retryOptions := campaign.AutoPlannerOptions(cfg)
						retryOptions.PreferReuse = true
						retryPlan, retryErr := autoPilotBuildPlanWithOptions(database, cfg, unplaced, capacities, retryOptions)
						if retryErr == nil && len(retryPlan.ReuseAssignments) > 0 {
							reusePreferredFallback = true
							plan.ReuseAssignments = retryPlan.ReuseAssignments
							plan.LaunchGroups = nil
							plan.LaunchJobIDs = nil
							plan.LaunchRateCentsPerHour = 0
							plan.BlockedReasons = retryPlan.BlockedReasons
							for jobID, reason := range retryPlan.BlockedReasons {
								if strings.TrimSpace(reason) != "" {
									blockedReasons[jobID] = reason
								}
							}
							for _, job := range unplaced {
								if job == nil {
									continue
								}
								if _, exists := blockedReasons[job.ID]; exists {
									continue
								}
								reason := noRentalHeadroomReason(job, capacities, r2Client)
								blockedReasons[job.ID] = reason
								plan.BlockedReasons[job.ID] = reason
							}
						}
					}
					if !reusePreferredFallback {
						reason := fmt.Sprintf(
							"run-rate target exceeded (no subset fits): target %s/hr, current %s/hr + planned %s/hr = %s/hr (headroom %s/hr, cheapest group %s/hr)",
							formatRateCents(runRateTarget),
							formatRateCents(currentRate),
							formatRateCents(plan.LaunchRateCentsPerHour),
							formatRateCents(projectedRate),
							formatRateCents(headroom),
							formatRateCents(cheapest),
						)
						for _, jobID := range plan.LaunchJobIDs {
							blockedReasons[jobID] = reason
						}
						return &GroupedAutoPilotResult{
							Placed:         0,
							Launched:       0,
							LaunchedClass:  "",
							BlockedReasons: blockedReasons,
						}, nil
					}
				}
			}
		}
	}

	placed := 0
	for _, assignment := range plan.ReuseAssignments {
		if assignment.Job == nil || assignment.Instance.Instance == nil {
			continue
		}
		if err := autoPilotSubmitJobsToInstance(ctx, database, r2Client, assignment.Instance.Instance.ID, []*db.Job{assignment.Job}); err != nil {
			oplog.LogJob("auto_pilot.reuse_failed", assignment.Job.ID, "",
				oplog.WithError(err),
				oplog.WithDetailf("instance=%d", assignment.Instance.Instance.ID))
			blockedReasons[assignment.Job.ID] = fmt.Sprintf("reuse instance %d failed: %s", assignment.Instance.Instance.ID, summarizeAutoPilotError(err))
			continue
		}
		placed++
	}

	rebalanceResult, err := RebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
		Apply:     true,
		R2Client:  r2Client,
		Operation: "auto_pilot.rebalance",
	})
	if err != nil {
		return nil, err
	}
	rebalanced := len(rebalanceResult.Moves)

	remaining, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, err
	}
	rentalScope := make([]int64, 0, len(remaining))
	launchScope := make(map[int64]struct{}, len(plan.LaunchJobIDs))
	for _, jobID := range plan.LaunchJobIDs {
		launchScope[jobID] = struct{}{}
	}
	plannerMadeDecisions := len(plan.LaunchJobIDs) > 0 || len(plan.BlockedReasons) > 0
	remainingByID := make(map[int64]*db.Job, len(remaining))
	for _, job := range remaining {
		if job != nil {
			remainingByID[job.ID] = job
		}
		if job == nil || job.HasTag(db.TagInventory) {
			continue
		}
		// Accept both queued and pending_placement unplaced rentals; the
		// latter can get stuck when a prior reset leaves the attempt in
		// pending_placement and the autopilot would otherwise never touch
		// it — producing unplaced jobs with no blocked reason.
		if es := job.EffectiveStatus(); es != db.StatusQueued && es != db.StatusPendingPlacement {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		if len(launchScope) == 0 {
			if plannerMadeDecisions {
				continue
			}
			rentalScope = append(rentalScope, job.ID)
			continue
		}
		if _, ok := launchScope[job.ID]; ok {
			rentalScope = append(rentalScope, job.ID)
		}
	}
	if len(rentalScope) == 0 {
		oplog.Log("auto_pilot.no_rental_scope",
			oplog.WithDetailf("launch_scope=%d blocked=%d", len(launchScope), len(blockedReasons)))
		return &GroupedAutoPilotResult{
			Placed:         placed,
			Rebalanced:     rebalanced,
			Launched:       0,
			LaunchedClass:  "",
			BlockedReasons: blockedReasons,
		}, nil
	}
	oplog.Log("auto_pilot.launching",
		oplog.WithDetailf("rental_scope=%d", len(rentalScope)))

	failedInstanceByJob := BuildFailedInstanceByJob(database, rentalScope)
	result, err := autoPilotRelaunch(database, cfg, 0, nil, rentalScope, "", false, true)
	if err != nil {
		oplog.Log("auto_pilot.launch_error",
			oplog.WithError(err),
			oplog.WithDetailf("rental_scope=%d", len(rentalScope)))
		launchReason := "launch failed: " + campaign.SanitizeBlockedReason(err.Error())
		for _, jobID := range rentalScope {
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = launchReason
			}
		}
		return &GroupedAutoPilotResult{
			Placed:         placed,
			Rebalanced:     rebalanced,
			Launched:       0,
			LaunchedClass:  "",
			BlockedReasons: blockedReasons,
		}, err
	}
	if result == nil {
		return &GroupedAutoPilotResult{
			Placed:         placed,
			Rebalanced:     rebalanced,
			Launched:       0,
			LaunchedClass:  "",
			BlockedReasons: blockedReasons,
		}, nil
	}
	if result.BlockedReason != "" {
		for _, jobID := range rentalScope {
			blockedReasons[jobID] = result.BlockedReason
		}
	}
	for jobID, failedID := range failedInstanceByJob {
		if failedID == 0 {
			continue
		}
		if reason, ok := result.NotReplacedReasons[failedID]; ok && strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}
	for jobID, reason := range result.JobReasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		if _, exists := blockedReasons[jobID]; !exists {
			blockedReasons[jobID] = reason
		}
	}
	// Use per-job queue-floor (QueuedAt/CreatedAt) rather than a single
	// passStartedAt floor: skip events are often logged on a prior pass
	// (e.g. retry-budget cooldown), but remain the authoritative reason
	// while the job is still queued. Filtering by passStartedAt dropped
	// those recent-but-not-current-pass reasons, leaving the generic
	// "no offers available" fallback as the only surfaced reason.
	floorByJob := make(map[int64]int64, len(rentalScope))
	for _, jobID := range rentalScope {
		if j, ok := remainingByID[jobID]; ok && j != nil {
			floor := j.QueuedAt
			if floor <= 0 {
				floor = j.CreatedAt
			}
			floorByJob[jobID] = floor
		}
	}
	eventReasons := RelaunchBlockedReasonsFromEventsWithFloor(database, floorByJob)
	for jobID, reason := range eventReasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		if _, exists := blockedReasons[jobID]; !exists {
			blockedReasons[jobID] = reason
		}
	}
	for jobID := range blockedReasons {
		if _, exists := remainingByID[jobID]; !exists {
			delete(blockedReasons, jobID)
		}
	}
	if len(result.InstanceIDs) == 0 {
		for _, jobID := range rentalScope {
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = "no offers available"
			}
		}
	}

	return &GroupedAutoPilotResult{
		Placed:         placed,
		Rebalanced:     rebalanced,
		Launched:       len(result.InstanceIDs),
		LaunchedClass:  launchedClassFromResult(database, result.InstanceIDs),
		BlockedReasons: blockedReasons,
	}, nil
}

func selectLaunchGroupsWithinHeadroom(groups []campaign.LaunchGroup, headroom int) ([]campaign.LaunchGroup, []campaign.LaunchGroup, int) {
	if len(groups) == 0 || headroom <= 0 {
		return nil, append([]campaign.LaunchGroup(nil), groups...), 0
	}
	sorted := append([]campaign.LaunchGroup(nil), groups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a := sorted[i]
		b := sorted[j]
		ratioA := launchGroupValue(a)
		ratioB := launchGroupValue(b)
		if ratioA != ratioB {
			return ratioA > ratioB
		}
		if a.CostPerHourCents != b.CostPerHourCents {
			return a.CostPerHourCents < b.CostPerHourCents
		}
		return minLaunchGroupJobID(a) < minLaunchGroupJobID(b)
	})

	accepted := make([]campaign.LaunchGroup, 0, len(sorted))
	rejected := make([]campaign.LaunchGroup, 0, len(sorted))
	used := 0
	for _, group := range sorted {
		if group.CostPerHourCents <= 0 {
			continue
		}
		if used+group.CostPerHourCents <= headroom {
			accepted = append(accepted, group)
			used += group.CostPerHourCents
			continue
		}
		rejected = append(rejected, group)
	}
	return accepted, rejected, used
}

func applyAcceptedLaunchGroups(plan *campaign.AutoPlacementPlan, groups []campaign.LaunchGroup, blockedReasons map[int64]string) {
	plan.LaunchGroups = append([]campaign.LaunchGroup(nil), groups...)
	plan.LaunchJobIDs = nil
	plan.LaunchRateCentsPerHour = 0
	for _, group := range groups {
		plan.LaunchRateCentsPerHour += group.CostPerHourCents
		plan.LaunchJobIDs = append(plan.LaunchJobIDs, group.JobIDs...)
		for _, jobID := range group.JobIDs {
			delete(blockedReasons, jobID)
		}
	}
	sort.Slice(plan.LaunchJobIDs, func(i, j int) bool { return plan.LaunchJobIDs[i] < plan.LaunchJobIDs[j] })
}

func cheapestLaunchGroupCost(groups []campaign.LaunchGroup) int {
	cheapest := 0
	for _, group := range groups {
		if group.CostPerHourCents <= 0 {
			continue
		}
		if cheapest == 0 || group.CostPerHourCents < cheapest {
			cheapest = group.CostPerHourCents
		}
	}
	return cheapest
}

func launchGroupValue(group campaign.LaunchGroup) float64 {
	cost := group.CostPerHourCents
	if cost <= 0 {
		return 0
	}
	return float64(len(group.JobIDs)) / float64(cost)
}

func sumLaunchGroupCost(groups []campaign.LaunchGroup) int {
	total := 0
	for _, group := range groups {
		if group.CostPerHourCents > 0 {
			total += group.CostPerHourCents
		}
	}
	return total
}

func minLaunchGroupJobID(group campaign.LaunchGroup) int64 {
	if len(group.JobIDs) == 0 {
		return 0
	}
	minID := group.JobIDs[0]
	for _, id := range group.JobIDs[1:] {
		if id < minID {
			minID = id
		}
	}
	return minID
}

func noRentalHeadroomReason(job *db.Job, capacities []campaign.InstanceCapacity, r2Client *r2.Client) string {
	const base = "no rental headroom; running instances couldn't accept this job"
	if job == nil || len(capacities) == 0 {
		return base
	}
	sorted := append([]campaign.InstanceCapacity(nil), capacities...)
	sort.Slice(sorted, func(i, j int) bool {
		a := int64(0)
		b := int64(0)
		if sorted[i].Instance != nil {
			a = sorted[i].Instance.ID
		}
		if sorted[j].Instance != nil {
			b = sorted[j].Instance.ID
		}
		return a < b
	})
	firstReason := ""
	for _, cap := range sorted {
		ok := false
		reason := ""
		if r2Client != nil {
			ok, reason = campaign.MatchJobToInstanceWithUV(job, cap, r2Client)
		} else {
			ok, reason = campaign.MatchJobToInstance(job, cap)
		}
		if ok {
			return base
		}
		if firstReason == "" && strings.TrimSpace(reason) != "" {
			firstReason = reason
		}
	}
	if firstReason != "" {
		return base + ": " + firstReason
	}
	return base
}

func countRunningJobs(jobs []*db.Job) int {
	count := 0
	for _, j := range jobs {
		if j != nil && j.EffectiveStatus() == db.StatusRunning {
			count++
		}
	}
	return count
}

func launchedClassFromResult(database *sql.DB, instanceIDs []int64) string {
	if database == nil || len(instanceIDs) != 1 {
		return ""
	}
	launch, err := db.GetLaunch(database, instanceIDs[0])
	if err != nil || launch == nil {
		return ""
	}
	if label := strings.TrimSpace(launch.DisplayGPUBrief()); label != "" {
		return label
	}
	if label := strings.TrimSpace(launch.DisplayGPUSpec()); label != "" {
		return label
	}
	return ""
}

func summarizeAutoPilotError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return ""
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 140
	if len(msg) <= maxLen {
		return msg
	}
	return strings.TrimSpace(msg[:maxLen-1]) + "…"
}

func sampleBlockedReason(reasons map[int64]string) string {
	for _, reason := range reasons {
		if trimmed := strings.TrimSpace(reason); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func formatRateCents(cents int) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}
