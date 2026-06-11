package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/explain"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/retrypolicy"
)

type GroupedAutoPilotResult struct {
	Placed        int
	Rebalanced    int
	Launched      int
	AutoReplanned int
	ReuseFilled   int
	OverloadMoved int
	LaunchedClass string
	// BlockedReasons holds the authoritative one-line reason per still-unplaced
	// job (back-compat flat form).
	BlockedReasons map[int64]string
	// StructuredBlocked holds the full launch/reuse breakdown for jobs whose
	// blocker is a placement-avenue failure. Single-cause blockers
	// (preconditions, cooldowns) are absent — their BlockedReasons entry is
	// already the whole story.
	StructuredBlocked map[int64]*blockreason.Structured
}

var autoPilotBuildPlan = buildAutoPlacementPlan
var autoPilotBuildPlanWithOptions = buildAutoPlacementPlanWithOptions

// autoPilotCheckProviderCreditHealth is overridable in tests; it gates the
// pre-launch path so the autopilot stops calling provider create endpoints
// when all enabled provider accounts are out of credit.
var autoPilotCheckProviderCreditHealth = CheckProviderCreditHealth

var autoPilotRelaunch = RelaunchOrphanedJobs
var autoPilotSubmitJobsToInstance = campaign.SubmitJobsToInstance
var autoPilotPlaceComputeIntensive = placeComputeIntensiveOnPremBeforeRental
var autoPilotLaunchMoveIntentRetry = launchMoveIntentRetry
var autoPilotDrainOverloadedInventoryHosts = drainOverloadedInventoryHosts
var autoPilotRebalanceQueuedRentalJobsToOnPrem = rebalanceQueuedRentalJobsToOnPrem
var autoPilotRebalanceQueuedJobsAcrossInstances = RebalanceQueuedJobsAcrossInstances
var autoPilotFillReusableInstances = fillReusableInstances
var autoReplanUnplaceQueuedJob = ops.UnplaceQueuedJob

// autoReplanConfigEnabled reports whether the autopilot should run
// AutoReplanStuckInventoryDispatch this pass. Wrapped in a var so tests
// can swap it without touching the on-disk config.
var autoReplanConfigEnabled = func() bool {
	cfg, err := config.Load()
	if err != nil {
		// Conservative on config-load failure: behave as if disabled
		// rather than silently changing job placement.
		return false
	}
	return cfg.AutoReplanStuckInventoryDispatchEnabled()
}

// placementIntentProtectionWindow shields freshly-opened intents from
// auto-prune; it must exceed a healthy relaunch's offer-search →
// instance-create window (a few seconds typically, up to ~90s under load).
const placementIntentProtectionWindow = 5 * time.Minute

func RunGroupedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job) (out *GroupedAutoPilotResult, outErr error) {
	if database == nil {
		return &GroupedAutoPilotResult{}, nil
	}
	// Inventory-tagged unplaced jobs are surfaced via this map so every return
	// path can attach a fresh "waiting for on-prem host" reason to them
	// without each path needing to know about the inventory partition.
	var inventoryAwaitingReasons map[int64]string
	defer func() {
		if out != nil && len(inventoryAwaitingReasons) > 0 {
			mergeInventoryReasons(out, inventoryAwaitingReasons)
		}
	}()
	scoped := make(map[int64]struct{}, len(scopedJobs))
	for _, job := range scopedJobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	// Requeue jobs stranded on dead cloud-instance host references. Intent
	// drift is now resolved by DB triggers (see 00004 + 00005); the old
	// imperative reconciler that swept open MoveIntent / PlacementIntent
	// rows is gone, because every placement-landing event closes the
	// intent in the same transaction.
	if stale, err := db.ReconcileStaleCloudHostJobs(database, db.ReconcileStaleCloudHostJobsOptions{}); err != nil {
		oplog.Log("auto_pilot.stale_cloud_host_reconcile_error", oplog.WithError(err))
	} else if len(stale.Actions) > 0 {
		jobIDs := make([]int64, 0, len(stale.Actions))
		for _, action := range stale.Actions {
			jobIDs = append(jobIDs, action.JobID)
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: db.EventPlacementIntentPruned,
				JobID:     action.JobID,
				Detail:    fmt.Sprintf("stale_cloud_host detail=%s", action.Detail),
			})
		}
		oplog.Log("auto_pilot.stale_cloud_host_requeued",
			oplog.WithDetailf("actions=%d job_ids=%v", len(stale.Actions), jobIDs))
	}
	moveRetryLaunches, err := fulfillOpenMoveToNewIntents(ctx, database, scoped)
	if err != nil {
		oplog.Log("auto_pilot.move_intents_retry_error", oplog.WithError(err))
	}

	// Any job with an open intent (move or placement) is being handled by
	// another path; the autopilot must not race it. See
	// specs/job-move.allium § AutopilotIgnoresMovingJobs.
	movingJobs, err := db.JobIDsWithOpenMoveOrPlacementIntents(database)
	if err != nil {
		return nil, err
	}
	// Auto-replan is off by default — it converts an on-prem inventory job
	// into an unplaced (rental-eligible) job after 10 min of dispatch
	// failures, which is rarely what the user wants when the host's problem
	// is transient (slow HF download, SSH wedge, host reboot). Enable via
	// [autopilot] auto_replan_stuck_inventory_dispatch = true. See
	// internal/config AutopilotConfig and specs/campaign-lifecycle.allium §
	// AutoReplanStuckInventoryDispatch.
	var autoReplanned int
	if autoReplanConfigEnabled() {
		autoReplanned, err = autoReplanStuckInventoryJobs(database, scoped, movingJobs)
		if err != nil {
			oplog.Log("auto_pilot.auto_replan_error", oplog.WithError(err))
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return &GroupedAutoPilotResult{
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
		}, err
	}
	overloadMoved, err := autoPilotDrainOverloadedInventoryHosts(ctx, database, cfg, scoped, movingJobs)
	if err != nil {
		oplog.Log("auto_pilot.overload_drain_error", oplog.WithError(err))
	}
	onPremRebalanced, err := autoPilotRebalanceQueuedRentalJobsToOnPrem(ctx, database, cfg, scoped, movingJobs)
	if err != nil {
		oplog.Log("auto_pilot.rebalance_onprem_error", oplog.WithError(err))
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, err
	}
	unplaced := make([]*db.Job, 0, len(unplacedJobs))
	inventoryAwaiting := make([]*db.Job, 0)
	for _, job := range unplacedJobs {
		// IsUnplacedAwaitingPlacement (rather than IsUnplacedQueued) so jobs
		// stuck in pending_placement still get a blocked reason — see
		// internal/orchestration/pending_placement.go.
		if !job.IsUnplacedAwaitingPlacement() {
			continue
		}
		// A job with an open MoveIntent or PlacementIntent is already being
		// handled by another path; treat it as placed for the autopilot's
		// purposes. See specs/job-move.allium § AutopilotIgnoresMovingJobs.
		if _, moving := movingJobs[job.ID]; moving {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		// Inventory-tagged jobs belong on on-prem hosts; the cloud planner
		// must not see them. Otherwise it builds phantom launch groups that
		// inflate run-rate projections (triggering spurious "run-rate
		// headroom exhausted" reasons on cloud-eligible jobs) and never
		// execute because the rentalScope filter at line 337 — and
		// relaunch.go:1756/2041 — correctly exclude inventory jobs from
		// cloud launches. Placement onto on-prem hosts happens in
		// internal/app/hostsync/worker.go.
		if job.HasTag(db.TagInventory) {
			inventoryAwaiting = append(inventoryAwaiting, job)
			continue
		}
		unplaced = append(unplaced, job)
	}
	inventoryAwaitingReasons = persistInventoryAwaitingReasons(database, inventoryAwaiting)
	if len(unplaced) == 0 {
		rebalanceResult, rebErr := autoPilotRebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
			Apply:      true,
			Operation:  "auto_pilot.rebalance",
			MovingJobs: movingJobs,
		})
		if rebErr != nil {
			return &GroupedAutoPilotResult{
				Launched:      moveRetryLaunches,
				AutoReplanned: autoReplanned,
				OverloadMoved: overloadMoved,
			}, rebErr
		}
		return &GroupedAutoPilotResult{
			Rebalanced:    onPremRebalanced + len(rebalanceResult.Moves),
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			OverloadMoved: overloadMoved,
		}, nil
	}

	unplaced, prePlaced := autoPilotPlaceComputeIntensive(database, cfg, unplaced)
	if len(unplaced) == 0 {
		rebalanceResult, err := autoPilotRebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
			Apply:      true,
			Operation:  "auto_pilot.rebalance",
			MovingJobs: movingJobs,
		})
		if err != nil {
			return &GroupedAutoPilotResult{
				Placed:        prePlaced,
				Launched:      moveRetryLaunches,
				AutoReplanned: autoReplanned,
				OverloadMoved: overloadMoved,
			}, err
		}
		return &GroupedAutoPilotResult{
			Placed:        prePlaced,
			Rebalanced:    onPremRebalanced + len(rebalanceResult.Moves),
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			OverloadMoved: overloadMoved,
		}, nil
	}
	logAutoPilotOnPremDiagnostics(database, unplaced)
	r2Client, _ := BuildR2Client(cfg)

	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return &GroupedAutoPilotResult{
			Placed:        prePlaced,
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			OverloadMoved: overloadMoved,
		}, err
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
		return &GroupedAutoPilotResult{
			Placed:        prePlaced,
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			OverloadMoved: overloadMoved,
		}, err
	}
	blockedReasons := map[int64]string{}
	structuredBlocked := map[int64]*blockreason.Structured{}
	for jobID, reason := range plan.BlockedReasons {
		if strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}
	// Lift the planner's per-job full-detail map (e.g. multi-line vastai
	// stderr) into the structured form so the TUI disclosure expand view and
	// CLI surfaces can show the underlying provider message. Lift the
	// fingerprint independently so jobs with a structured upstream error but
	// short single-line detail still participate in incident coalescing.
	liftStructured := func(jobID int64) *blockreason.Structured {
		if s, ok := structuredBlocked[jobID]; ok && s != nil {
			return s
		}
		flat, ok := blockedReasons[jobID]
		if !ok || strings.TrimSpace(flat) == "" {
			return nil
		}
		s := &blockreason.Structured{Summary: flat, Launch: flat}
		structuredBlocked[jobID] = s
		return s
	}
	for jobID, detail := range plan.BlockedReasonDetails {
		if s := liftStructured(jobID); s != nil {
			s.LaunchDetail = detail
		}
	}
	for jobID, fp := range plan.BlockedReasonFingerprints {
		if strings.TrimSpace(fp) == "" {
			continue
		}
		if s := liftStructured(jobID); s != nil {
			s.Fingerprint = fp
		}
	}
	// Reuse-rejection diagnostics are kept separate from blockedReasons so an
	// opportunistic "could not reuse" note never masks the authoritative
	// launch-path reason. They are appended as detail by
	// finalizeUnplacedBlockedReasons once primary reasons are settled.
	reuseDiagnostics := map[int64]string{}
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
							Placed:            0,
							Launched:          moveRetryLaunches,
							AutoReplanned:     autoReplanned,
							OverloadMoved:     overloadMoved,
							LaunchedClass:     "",
							BlockedReasons:    blockedReasons,
							StructuredBlocked: structuredBlocked,
						}, nil
					}
				}
			}
		}
	}

	placed := prePlaced
	placed += submitAutoPilotReuseAssignments(ctx, database, r2Client, plan.ReuseAssignments, reuseDiagnostics, blockedReasons)

	rebalanceResult, err := autoPilotRebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
		Apply:      true,
		R2Client:   r2Client,
		Operation:  "auto_pilot.rebalance",
		MovingJobs: movingJobs,
	})
	if err != nil {
		return &GroupedAutoPilotResult{
			Placed:        placed,
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			OverloadMoved: overloadMoved,
		}, err
	}
	rebalanced := len(rebalanceResult.Moves)
	rebalanced += onPremRebalanced

	reuseFilled, err := autoPilotFillReusableInstances(ctx, database, r2Client, scoped, movingJobs, reuseDiagnostics, blockedReasons)
	if err != nil {
		oplog.Log("auto_pilot.reuse_fill_error", oplog.WithError(err))
	}
	placed += reuseFilled

	remaining, err := db.ListUnplacedJobs(database)
	if err != nil {
		return &GroupedAutoPilotResult{
			Placed:        placed,
			Rebalanced:    rebalanced,
			Launched:      moveRetryLaunches,
			AutoReplanned: autoReplanned,
			ReuseFilled:   reuseFilled,
			OverloadMoved: overloadMoved,
		}, err
	}
	rentalScope := make([]int64, 0, len(remaining))
	allCandidates := make([]int64, 0, len(remaining))
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
		if job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if _, moving := movingJobs[job.ID]; moving {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		allCandidates = append(allCandidates, job.ID)
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
		// Detail capture: when launchScope > 0 but rentalScope drops to 0, the
		// planner picked jobs that the rentalScope filter rejected — log enough
		// state to identify which filter (remaining/launchScope/scoped/moving)
		// is responsible without re-running the pass.
		if len(launchScope) > 0 {
			remainingIDs := make([]int64, 0, len(remaining))
			for _, j := range remaining {
				if j != nil {
					remainingIDs = append(remainingIDs, j.ID)
				}
			}
			launchScopeIDs := make([]int64, 0, len(launchScope))
			for id := range launchScope {
				launchScopeIDs = append(launchScopeIDs, id)
			}
			sort.Slice(launchScopeIDs, func(i, j int) bool { return launchScopeIDs[i] < launchScopeIDs[j] })
			scopedIDs := make([]int64, 0, len(scoped))
			for id := range scoped {
				scopedIDs = append(scopedIDs, id)
			}
			sort.Slice(scopedIDs, func(i, j int) bool { return scopedIDs[i] < scopedIDs[j] })
			movingIDs := make([]int64, 0, len(movingJobs))
			for id := range movingJobs {
				movingIDs = append(movingIDs, id)
			}
			oplog.Log("auto_pilot.no_rental_scope.detail",
				oplog.WithDetailf("remaining=%v launch_scope_ids=%v scoped_ids=%v moving_ids=%v all_candidates=%v",
					remainingIDs, launchScopeIDs, scopedIDs, movingIDs, allCandidates))
		}
		oplog.Log("auto_pilot.no_rental_scope",
			oplog.WithDetailf("launch_scope=%d blocked=%d", len(launchScope), len(blockedReasons)))
		finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked, reuseDiagnostics, remainingByID, allCandidates, capacities, r2Client)
		return &GroupedAutoPilotResult{
			Placed:            placed,
			Rebalanced:        rebalanced,
			Launched:          moveRetryLaunches,
			AutoReplanned:     autoReplanned,
			ReuseFilled:       reuseFilled,
			OverloadMoved:     overloadMoved,
			LaunchedClass:     "",
			BlockedReasons:    blockedReasons,
			StructuredBlocked: structuredBlocked,
		}, nil
	}

	// Pre-launch credit gate: if every enabled provider's account credit is
	// exhausted, every CreateInstance call below would fail with a cryptic
	// "provider returned empty response" / "account lacks credit" error, mint
	// a failed-instance row per attempt, and pollute the survival model with
	// non-signal noise. Skip the launch and attribute the blocked reason to
	// the actual cause.
	creditStatuses := autoPilotCheckProviderCreditHealth(cfg)
	logProviderCreditGate(creditStatuses, len(rentalScope))
	if AllEnabledProvidersExhausted(creditStatuses) {
		reason := FormatCreditExhaustionReason(creditStatuses)
		for _, jobID := range rentalScope {
			blockedReasons[jobID] = reason
		}
		finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked, reuseDiagnostics, remainingByID, allCandidates, capacities, r2Client)
		return &GroupedAutoPilotResult{
			Placed:            placed,
			Rebalanced:        rebalanced,
			Launched:          moveRetryLaunches,
			AutoReplanned:     autoReplanned,
			ReuseFilled:       reuseFilled,
			OverloadMoved:     overloadMoved,
			LaunchedClass:     "",
			BlockedReasons:    blockedReasons,
			StructuredBlocked: structuredBlocked,
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
		launchReason := campaign.SanitizeBlockedReason(err.Error())
		for _, jobID := range rentalScope {
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = launchReason
			}
		}
		finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked, reuseDiagnostics, remainingByID, allCandidates, capacities, r2Client)
		return &GroupedAutoPilotResult{
			Placed:            placed,
			Rebalanced:        rebalanced,
			Launched:          moveRetryLaunches,
			AutoReplanned:     autoReplanned,
			ReuseFilled:       reuseFilled,
			OverloadMoved:     overloadMoved,
			LaunchedClass:     "",
			BlockedReasons:    blockedReasons,
			StructuredBlocked: structuredBlocked,
		}, err
	}
	if result == nil {
		finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked, reuseDiagnostics, remainingByID, allCandidates, capacities, r2Client)
		return &GroupedAutoPilotResult{
			Placed:            placed,
			Rebalanced:        rebalanced,
			Launched:          moveRetryLaunches,
			AutoReplanned:     autoReplanned,
			ReuseFilled:       reuseFilled,
			OverloadMoved:     overloadMoved,
			LaunchedClass:     "",
			BlockedReasons:    blockedReasons,
			StructuredBlocked: structuredBlocked,
		}, nil
	}
	placedAfterLaunchBlock := map[int64]struct{}{}
	if result.BlockedReason != "" {
		for _, jobID := range rentalScope {
			blockedReasons[jobID] = result.BlockedReason
		}
		if len(capacities) > 0 {
			fallbackJobs := jobsByIDInOrder(remainingByID, rentalScope)
			retryOptions := campaign.AutoPlannerOptions(cfg)
			retryOptions.PreferReuse = true
			retryPlan, retryErr := autoPilotBuildPlanWithOptions(database, cfg, fallbackJobs, capacities, retryOptions)
			if retryErr == nil && len(retryPlan.ReuseAssignments) > 0 {
				fallbackPlaced := submitAutoPilotReuseAssignments(ctx, database, r2Client, retryPlan.ReuseAssignments, reuseDiagnostics, blockedReasons)
				if fallbackPlaced > 0 {
					placed += fallbackPlaced
					for _, assignment := range retryPlan.ReuseAssignments {
						if assignment.Job != nil {
							delete(blockedReasons, assignment.Job.ID)
							placedAfterLaunchBlock[assignment.Job.ID] = struct{}{}
						}
					}
					oplog.Log("auto_pilot.reuse_after_launch_block",
						oplog.WithDetailf("placed=%d launch_scope=%d reason=%q", fallbackPlaced, len(rentalScope), result.BlockedReason))
				}
			} else if retryErr != nil {
				oplog.Log("auto_pilot.reuse_after_launch_block_error",
					oplog.WithError(retryErr),
					oplog.WithDetailf("launch_scope=%d", len(rentalScope)))
			}
		}
	}
	mergeRelaunchReasonsIntoBlockedReasons(blockedReasons, result, failedInstanceByJob)
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
			if _, placed := placedAfterLaunchBlock[jobID]; placed {
				continue
			}
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = "no offers available"
			}
		}
	}
	finalizeUnplacedBlockedReasons(database, blockedReasons, structuredBlocked, reuseDiagnostics, remainingByID, allCandidates, capacities, r2Client)

	return &GroupedAutoPilotResult{
		Placed:            placed,
		Rebalanced:        rebalanced,
		Launched:          moveRetryLaunches + len(result.InstanceIDs),
		AutoReplanned:     autoReplanned,
		ReuseFilled:       reuseFilled,
		OverloadMoved:     overloadMoved,
		LaunchedClass:     launchedClassFromResult(database, result.InstanceIDs),
		BlockedReasons:    blockedReasons,
		StructuredBlocked: structuredBlocked,
	}, nil
}

func fillReusableInstances(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	scoped map[int64]struct{},
	movingJobs map[int64]struct{},
	reuseDiagnostics map[int64]string,
	blockedReasons map[int64]string,
) (int, error) {
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return 0, fmt.Errorf("list unplaced jobs for reuse fill: %w", err)
	}
	candidates := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued || job.HasTag(db.TagInventory) {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		if _, moving := movingJobs[job.ID]; moving {
			continue
		}
		candidates = append(candidates, job)
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	capacities, err := campaign.FindReusableInstances(database)
	if err != nil {
		return 0, fmt.Errorf("find reusable instances: %w", err)
	}
	if len(capacities) == 0 {
		return 0, nil
	}
	assignments, remaining := campaign.PlanReuse(candidates, capacities)
	placed := submitAutoPilotReuseAssignments(ctx, database, r2Client, assignments, reuseDiagnostics, blockedReasons)
	if placed > 0 {
		oplog.Log("auto_pilot.reuse_fill", oplog.WithDetailf("placed=%d candidates=%d reusable=%d", placed, len(candidates), len(capacities)))
	}
	for _, job := range remaining {
		if reason := reuseRejectionReason(job, capacities, r2Client); reason != "" {
			addAutoPilotBlockedReason(reuseDiagnostics, job.ID, reason)
			oplog.LogJob("auto_pilot.reuse_fill_blocked", job.ID, "",
				oplog.WithDetail(reason))
		}
	}
	return placed, nil
}

func reuseRejectionReason(job *db.Job, capacities []campaign.InstanceCapacity, r2Client *r2.Client) string {
	if job == nil || len(capacities) == 0 {
		return ""
	}
	reasons := make([]string, 0, 3)
	for _, cap := range capacities {
		if cap.Instance == nil {
			continue
		}
		ok, reason := campaign.MatchJobToInstanceWithUV(job, cap, r2Client)
		if ok {
			return ""
		}
		if len(reasons) < 3 {
			reasons = append(reasons, fmt.Sprintf("%s %s: %s", ids.FormatInstanceID(cap.Instance.ID), cap.Instance.DisplayGPUBrief(), reason))
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	if len(capacities) > len(reasons) {
		reasons = append(reasons, fmt.Sprintf("+%d more", len(capacities)-len(reasons)))
	}
	return "could not reuse running instances: " + strings.Join(reasons, "; ")
}

// inventoryAwaitingReason is the authoritative blocked reason for an
// inventory-tagged unplaced job. Inventory jobs belong on on-prem hosts,
// not cloud rentals; placement onto an inventory host happens in
// internal/app/hostsync/worker.go. The autopilot's job here is just to
// surface the wait, not to act on it.
const inventoryAwaitingReason = "inventory-tagged: waiting for on-prem host"

// persistInventoryAwaitingReasons writes the "waiting for on-prem host"
// reason to placement_reasons for each inventory-tagged unplaced job and
// returns the same map for inclusion in the autopilot result so the TUI's
// in-memory autoBlockReasons is consistent with what was just persisted.
func persistInventoryAwaitingReasons(database *sql.DB, jobs []*db.Job) map[int64]string {
	if len(jobs) == 0 {
		return nil
	}
	reasons := make(map[int64]string, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		reasons[job.ID] = inventoryAwaitingReason
		appendPlacementReason(database, job.ID, inventoryAwaitingReason)
	}
	return reasons
}

// mergeInventoryReasons folds inventory-awaiting reasons into an
// autopilot result. Existing entries are not overwritten — the cloud
// path's reason wins if (somehow) the same job appears in both.
func mergeInventoryReasons(result *GroupedAutoPilotResult, inventory map[int64]string) *GroupedAutoPilotResult {
	if result == nil || len(inventory) == 0 {
		return result
	}
	if result.BlockedReasons == nil {
		result.BlockedReasons = make(map[int64]string, len(inventory))
	}
	for jobID, reason := range inventory {
		if _, exists := result.BlockedReasons[jobID]; exists {
			continue
		}
		result.BlockedReasons[jobID] = reason
	}
	return result
}

func addAutoPilotBlockedReason(blockedReasons map[int64]string, jobID int64, reason string) {
	reason = strings.TrimSpace(reason)
	if blockedReasons == nil || reason == "" {
		return
	}
	existing := strings.TrimSpace(blockedReasons[jobID])
	if existing == "" {
		blockedReasons[jobID] = reason
		return
	}
	if existing == reason || strings.Contains(existing, reason) {
		return
	}
	blockedReasons[jobID] = existing + "; " + reason
}

func persistBlockedReasonsForUnplaced(database *sql.DB, blockedReasons map[int64]string, structuredBlocked map[int64]*blockreason.Structured) {
	if database == nil || len(blockedReasons) == 0 {
		return
	}
	jobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return
	}
	unplaced := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job != nil {
			unplaced[job.ID] = struct{}{}
		}
	}
	for jobID, reason := range blockedReasons {
		if _, ok := unplaced[jobID]; !ok {
			delete(blockedReasons, jobID)
			continue
		}
		appendPlacementReason(database, jobID, reason)
		// Refresh the structured breakdown every pass. A single-cause
		// blocker has no entry in structuredBlocked; Marshal of a nil
		// reason is the empty string, which clears any stale value.
		_ = db.SetJobPlacementBlocked(database, jobID, structuredBlocked[jobID].Marshal())
	}
}

// finalizeUnplacedBlockedReasons settles the blocked reason for every
// still-unplaced candidate, then persists. It guarantees each candidate carries
// an authoritative launch-path reason before appending reuse-rejection
// diagnostics as detail — opportunistic reuse failures are never the primary
// reason and never mask why a job could not be launched. See
// specs/campaign-lifecycle.allium § AutoPilotBlockedReasonIsAuthoritative.
func finalizeUnplacedBlockedReasons(
	database *sql.DB,
	blockedReasons map[int64]string,
	structuredBlocked map[int64]*blockreason.Structured,
	reuseDiagnostics map[int64]string,
	remainingByID map[int64]*db.Job,
	candidateIDs []int64,
	capacities []campaign.InstanceCapacity,
	r2Client *r2.Client,
) {
	// Safety net: a candidate the planner classified as neither launch nor
	// blocked (e.g. a failed reuse assignment) would otherwise end the tick
	// with no reason. Give it an authoritative launch-path reason.
	for _, jobID := range candidateIDs {
		job, ok := remainingByID[jobID]
		if !ok || job == nil {
			continue
		}
		if _, has := blockedReasons[jobID]; has {
			continue
		}
		s := placementFailureStructured(job, capacities, r2Client, unclassifiedLaunchReason)
		blockedReasons[jobID] = s.Flat()
		if structuredBlocked != nil && s.IsPlacementFailure() {
			structuredBlocked[jobID] = s
		}
	}
	// Append reuse-rejection diagnostics after the authoritative reason.
	for jobID, diag := range reuseDiagnostics {
		if _, ok := remainingByID[jobID]; !ok {
			continue
		}
		addAutoPilotBlockedReason(blockedReasons, jobID, diag)
	}
	// Attach the structured launch/reuse breakdown to any still-unplaced job
	// whose authoritative reason was settled by an earlier path as a
	// placeholder (no-rental-headroom or unclassified) without having the
	// structured form attached.
	if structuredBlocked != nil {
		for jobID, flat := range blockedReasons {
			if _, done := structuredBlocked[jobID]; done {
				continue
			}
			if !isPlaceholderLaunchReason(flat) {
				continue
			}
			job, ok := remainingByID[jobID]
			if !ok || job == nil {
				continue
			}
			launch := noRentalHeadroomLaunchReason
			if strings.HasPrefix(flat, unclassifiedLaunchReason) {
				launch = unclassifiedLaunchReason
			}
			if s := placementFailureStructured(job, capacities, r2Client, launch); s.IsPlacementFailure() {
				structuredBlocked[jobID] = s
			}
		}
	}
	persistBlockedReasonsForUnplaced(database, blockedReasons, structuredBlocked)
}

func autoReplanStuckInventoryJobs(database *sql.DB, scoped map[int64]struct{}, movingJobs map[int64]struct{}) (int, error) {
	jobs, err := db.ListJobs(database, db.StatusQueued, "", 0, nil, "unprocessed")
	if err != nil {
		return 0, fmt.Errorf("list queued jobs for auto-replan: %w", err)
	}
	now := time.Now()
	replanned := 0
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		if _, moving := movingJobs[job.ID]; moving {
			continue
		}
		x := explain.ForJob(database, job, now)
		if !x.AutoReplanAllowed {
			continue
		}
		oldTarget := job.TargetDisplay()
		if _, err := autoReplanUnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
			return replanned, fmt.Errorf("auto-replan %s from %s: %w", ids.FormatJobID(job.ID), oldTarget, err)
		}
		reason := fmt.Sprintf("auto-replanned from %s after sustained dispatch block: %s", oldTarget, x.PrimaryReason)
		appendPlacementReason(database, job.ID, reason)
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventQueueDispatchAutoReplanned,
			JobID:     job.ID,
			Detail:    reason,
		})
		oplog.LogJob("auto_pilot.auto_replan", job.ID, oldTarget, oplog.WithDetail(reason))
		replanned++
	}
	if replanned > 0 {
		oplog.Log("auto_pilot.auto_replan", oplog.WithDetailf("jobs=%d", replanned))
	}
	return replanned, nil
}

func fulfillOpenMoveToNewIntents(ctx context.Context, database *sql.DB, scoped map[int64]struct{}) (int, error) {
	intents, err := db.ListOpenNewMoveIntents(database)
	if err != nil {
		return 0, err
	}
	if len(intents) == 0 {
		return 0, nil
	}
	var launched int
	for _, intent := range intents {
		if intent == nil || intent.JobID <= 0 {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[intent.JobID]; !ok {
				continue
			}
		}
		action, err := moveIntentRetryAction(database, intent)
		if err != nil {
			return launched, err
		}
		switch action {
		case moveIntentActionWait:
			continue
		case moveIntentActionConfirm:
			if err := db.ConfirmMoveTargetAccepted(database, intent.ID, "target accepted job before terminal"); err != nil {
				return launched, err
			}
			continue
		case moveIntentActionExhaust:
			if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, fmt.Sprintf("move-to-new exhausted %d launch attempts", intent.MaxAttempts)); err != nil {
				return launched, err
			}
			continue
		case moveIntentActionRetry:
			n, err := autoPilotLaunchMoveIntentRetry(ctx, database, intent)
			launched += n
			if err != nil {
				return launched, err
			}
		}
	}
	return launched, nil
}

type moveIntentAction int

const (
	moveIntentActionWait moveIntentAction = iota
	moveIntentActionRetry
	moveIntentActionExhaust
	moveIntentActionConfirm
)

func moveIntentRetryAction(database *sql.DB, intent *db.MoveIntent) (moveIntentAction, error) {
	if intent.TargetLaunchID == nil || *intent.TargetLaunchID <= 0 {
		if time.Since(time.Unix(intent.CreatedAt, 0)) < placementIntentProtectionWindow {
			return moveIntentActionWait, nil
		}
		if intent.AttemptCount >= intent.MaxAttempts {
			return moveIntentActionExhaust, nil
		}
		return moveIntentActionRetry, nil
	}
	launch, err := db.GetLaunch(database, *intent.TargetLaunchID)
	if err != nil {
		return moveIntentActionWait, err
	}
	if launch == nil {
		if intent.AttemptCount >= intent.MaxAttempts {
			return moveIntentActionExhaust, nil
		}
		return moveIntentActionRetry, nil
	}
	if launch.AgentReadyAtUnix != nil {
		return moveIntentActionConfirm, nil
	}
	started, err := moveIntentTargetStartedJob(database, intent.JobID, *intent.TargetLaunchID)
	if err != nil {
		return moveIntentActionWait, err
	}
	if started {
		return moveIntentActionConfirm, nil
	}
	if !campaign.IsInstanceTerminal(launch.Status) {
		return moveIntentActionWait, nil
	}
	transition, err := db.HandleMoveTargetFailedBeforeStart(database, intent.JobID, *intent.TargetLaunchID, db.AttemptOutcomeOrphaned)
	if err != nil {
		return moveIntentActionWait, err
	}
	if !transition.Handled {
		if _, err := db.ResetLaunchJobs(database, *intent.TargetLaunchID, db.AttemptOutcomeOrphaned); err != nil {
			return moveIntentActionWait, err
		}
	}
	if transition.Exhausted || intent.AttemptCount >= intent.MaxAttempts {
		return moveIntentActionExhaust, nil
	}
	return moveIntentActionRetry, nil
}

func moveIntentTargetStartedJob(database *sql.DB, jobID, launchID int64) (bool, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND launch_id = ? AND start_time IS NOT NULL AND start_time > 0`,
		jobID, launchID,
	).Scan(&count)
	return count > 0, err
}

func launchMoveIntentRetry(ctx context.Context, database *sql.DB, intent *db.MoveIntent) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, err
	}
	clients, err := BuildCloudClients(cfg)
	if err != nil {
		return 0, err
	}
	if len(clients) == 0 {
		return 0, fmt.Errorf("no cloud providers available")
	}
	job, err := db.GetJobByID(database, intent.JobID)
	if err != nil {
		return 0, err
	}
	if job == nil {
		return 0, fmt.Errorf("job %d not found", intent.JobID)
	}
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return 0, err
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	queuedCounts := make(map[int64]int, len(launches))
	for _, ci := range launches {
		liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobsForMove(liveJobs)); ok {
			capacities = append(capacities, cap)
		}
		for _, j := range liveJobs {
			if j != nil && j.EffectiveStatus() == db.StatusQueued {
				queuedCounts[ci.ID]++
			}
		}
	}
	var sourceLaunchID int64
	if intent.SourceLaunchID != nil {
		sourceLaunchID = *intent.SourceLaunchID
	} else if job.LaunchID != nil {
		sourceLaunchID = *job.LaunchID
	}
	excludedMachines, err := db.FailedMoveTargetMachineIDs(database, intent)
	if err != nil {
		return 0, err
	}
	options, err := BuildOptionsWithSurvival(clients, job, capacities, queuedCounts, sourceLaunchID, cfg.CampaignReliability(), buildSurvivalModel(database), 0.4, excludedMachines)
	if err != nil {
		return 0, err
	}
	var selected *Option
	for i := range options {
		if options[i].IsNew {
			selected = &options[i]
			break
		}
	}
	if selected == nil {
		oplog.LogJob("auto_pilot.move_intent_retry_blocked", intent.JobID, "",
			oplog.WithDetail("no compatible new-instance destination found"))
		return 0, nil
	}
	if _, err := executeMoveOption(ctx, database, nil, cfg, clients, job, *selected, intent); err != nil {
		return 0, err
	}
	return 1, nil
}

func placeComputeIntensiveOnPremBeforeRental(database *sql.DB, cfg *config.Config, jobs []*db.Job) ([]*db.Job, int) {
	if database == nil || len(jobs) == 0 {
		return jobs, 0
	}
	hostNames, err := placement.LoadHostNames()
	if err != nil || len(hostNames) == 0 {
		return jobs, 0
	}
	metrics := placement.CollectMetrics(database, hostNames, 5*time.Second)
	if len(metrics) == 0 {
		return jobs, 0
	}
	onPremSource := &placement.OnPremSource{Metrics: metrics}
	remaining := make([]*db.Job, 0, len(jobs))
	placed := 0
	for _, job := range jobs {
		if job == nil || !job.HasTag(db.TagComputeIntensive) {
			remaining = append(remaining, job)
			continue
		}
		constraints := placement.ConstraintsFromJob(job)
		if db.HasRentalTag(constraints.Tags) || constraints.Provider != "" {
			remaining = append(remaining, job)
			continue
		}
		predict := placement.BuildJobPredictorFromConfig(cfg, constraints)
		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: constraints,
			Predictor:   predict,
			Sources:     []placement.CandidateSource{onPremSource},
			Database:    database,
		})
		if err != nil || plan == nil || plan.Unplaced {
			remaining = append(remaining, job)
			continue
		}
		pick := plan.Fast
		if pick == nil {
			pick = plan.Cheap
		}
		if pick == nil || pick.Kind != placement.CandidateOnPrem || pick.OnPrem == nil || pick.OnPrem.Host == "" {
			remaining = append(remaining, job)
			continue
		}
		onPrem := pick.OnPrem
		rental := placement.EstimateRentalCompletion(database, constraints, predict)
		if rental != nil && placement.ShouldSpillToRental(onPrem.CompletionEst, rental.Total, constraints.Tags) {
			remaining = append(remaining, job)
			continue
		}
		assigned, err := db.AssignJobHost(database, job.ID, onPrem.Host)
		if err != nil || !assigned {
			remaining = append(remaining, job)
			continue
		}
		placed++
		oplog.LogJob("auto_pilot.compute_intensive_onprem", job.ID, onPrem.Host,
			oplog.WithDetailf("onprem_min=%.0f", onPrem.CompletionEst.Mean.Minutes()))
	}
	return remaining, placed
}

func logAutoPilotOnPremDiagnostics(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	hostNames, err := placement.LoadHostNames()
	if err != nil || len(hostNames) == 0 {
		oplog.Log("auto_pilot.onprem_diagnostics_unavailable", oplog.WithError(err))
		return
	}
	metrics, recent, err := placement.RecentHostMetrics(database, hostNames, db.HostInfoStaleThreshold)
	if err != nil || !recent {
		oplog.Log("auto_pilot.onprem_diagnostics_unavailable", oplog.WithError(err), oplog.WithDetailf("recent=%t hosts=%d", recent, len(hostNames)))
		return
	}
	for _, job := range jobs {
		if job == nil || db.HasRentalTag(job.Tags) {
			continue
		}
		constraints := placement.ConstraintsFromJob(job)
		if constraints.Provider != "" {
			continue
		}
		scores, err := placement.ScoreHostsWithPredictor(database, constraints, metrics, nil)
		if err != nil {
			oplog.LogJob("auto_pilot.onprem_diagnostics_error", job.ID, "", oplog.WithError(err))
			continue
		}
		best := ""
		for _, score := range scores {
			if score.Eligible {
				best = score.Host
				break
			}
		}
		if best != "" {
			oplog.LogJob("auto_pilot.onprem_diagnostics", job.ID, best, oplog.WithDetail("eligible_onprem=true"))
			continue
		}
		oplog.LogJob("auto_pilot.onprem_diagnostics", job.ID, "", oplog.WithDetail("eligible_onprem=false "+placement.FormatScoreRejectionDetail(scores, 4)))
	}
}

func submitAutoPilotReuseAssignments(ctx context.Context, database *sql.DB, r2Client *r2.Client, assignments []campaign.ReuseAssignment, reuseDiagnostics, blockedReasons map[int64]string) int {
	placed := 0
	sourceChecked := map[int64]error{}
	for _, assignment := range assignments {
		if assignment.Job == nil || assignment.Instance.Instance == nil {
			continue
		}
		// Deterministic source validation, before any claim. A job whose
		// source cannot ship (e.g. over the size cap) is blocked on EVERY
		// cloud avenue, so the error is the authoritative blocked reason —
		// not a per-instance reuse diagnostic — and the job is skipped
		// without creating attempt rows.
		srcErr, checked := sourceChecked[assignment.Job.ID]
		if !checked {
			srcErr = campaign.ValidateJobSourceForCloud(assignment.Job)
			sourceChecked[assignment.Job.ID] = srcErr
		}
		if srcErr != nil {
			if blockedReasons != nil {
				blockedReasons[assignment.Job.ID] = srcErr.Error()
			}
			oplog.LogJob("auto_pilot.reuse_skipped", assignment.Job.ID, "",
				oplog.WithError(srcErr),
				oplog.WithDetailf("instance=%d reason=source_validation", assignment.Instance.Instance.ID))
			continue
		}
		if remaining, count := reuseBackoffRemaining(database, assignment.Job.ID, time.Now()); remaining > 0 {
			detail := fmt.Sprintf("reuse backoff %s remaining (after %d failed submit(s))", remaining.Truncate(time.Second), count)
			emitReuseBackoffEvent(database, assignment.Job.ID, count, detail)
			addAutoPilotBlockedReason(reuseDiagnostics, assignment.Job.ID, detail)
			continue
		}
		if ok, reason := campaign.MatchJobToInstanceWithUV(assignment.Job, assignment.Instance, r2Client); !ok {
			addAutoPilotBlockedReason(reuseDiagnostics, assignment.Job.ID,
				fmt.Sprintf("could not reuse %s: %s", ids.FormatInstanceID(assignment.Instance.Instance.ID), reason))
			oplog.LogJob("auto_pilot.reuse_skipped", assignment.Job.ID, "",
				oplog.WithDetailf("instance=%d reason=%s", assignment.Instance.Instance.ID, reason))
			continue
		}
		if err := autoPilotSubmitJobsToInstance(ctx, database, r2Client, assignment.Instance.Instance.ID, []*db.Job{assignment.Job}); err != nil {
			oplog.LogJob("auto_pilot.reuse_failed", assignment.Job.ID, "",
				oplog.WithError(err),
				oplog.WithDetailf("instance=%d", assignment.Instance.Instance.ID))
			addAutoPilotBlockedReason(reuseDiagnostics, assignment.Job.ID,
				fmt.Sprintf("could not reuse %s: submit failed: %s", ids.FormatInstanceID(assignment.Instance.Instance.ID), SummarizeAutoPilotError(err)))
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: db.EventReuseSubmitFailed,
				JobID:     assignment.Job.ID,
				LaunchID:  assignment.Instance.Instance.ID,
				ErrorText: SummarizeAutoPilotError(err),
			})
			continue
		}
		reuseBackoffEventEmitted.Delete(assignment.Job.ID)
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventReuseSubmitOK,
			JobID:     assignment.Job.ID,
			LaunchID:  assignment.Instance.Instance.ID,
		})
		placed++
	}
	return placed
}

// reuseBackoffEventEmitted tracks the last streak count for which a
// reuse.skipped.backoff event was recorded per job, so the per-pass loop
// emits at most one event per (job, count) pair (mirrors the relaunch
// path's backoffEventEmitted).
var reuseBackoffEventEmitted sync.Map

// reuseBackoffRemaining derives the reuse-path backoff from the persisted
// reuse.submit_failed streak. Zero means the job is eligible; an unknown
// last-failure time also yields zero (better a redundant submit attempt than
// stalling the job indefinitely). Only the automatic autopilot paths are
// gated; manual weft job move bypasses this.
func reuseBackoffRemaining(database *sql.DB, jobID int64, now time.Time) (time.Duration, int) {
	count, lastFailureAt, err := db.ReuseFailureStreak(database, jobID)
	if err != nil || count <= 0 || lastFailureAt <= 0 {
		return 0, count
	}
	delay := retrypolicy.BackoffDelayClamped(count - 1)
	elapsed := now.Sub(time.Unix(lastFailureAt, 0))
	if elapsed >= delay {
		return 0, count
	}
	return delay - elapsed, count
}

func emitReuseBackoffEvent(database *sql.DB, jobID int64, count int, detail string) {
	if prev, ok := reuseBackoffEventEmitted.Load(jobID); ok && prev.(int) == count {
		return
	}
	reuseBackoffEventEmitted.Store(jobID, count)
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:     db.EventReuseSkippedBackoff,
		JobID:         jobID,
		AttemptNumber: count,
		Detail:        detail,
	})
}

func jobsByIDInOrder(jobs map[int64]*db.Job, ids []int64) []*db.Job {
	out := make([]*db.Job, 0, len(ids))
	for _, id := range ids {
		if job := jobs[id]; job != nil {
			out = append(out, job)
		}
	}
	return out
}

// mergeRelaunchReasonsIntoBlockedReasons populates blockedReasons from a
// RelaunchResult. Per-job reasons (result.JobReasons) take precedence over
// per-instance reasons (result.NotReplacedReasons), because the latter is
// keyed by failed predecessor instance and aggregates reasons across every
// sibling job that shared that instance — yielding a "multiple reasons (...)"
// string that is incorrect for any individual job. The per-instance map is
// retained as a fallback for jobs that lack a per-job reason entry.
func mergeRelaunchReasonsIntoBlockedReasons(
	blockedReasons map[int64]string,
	result *campaign.RelaunchResult,
	failedInstanceByJob map[int64]int64,
) {
	if blockedReasons == nil || result == nil {
		return
	}
	for jobID, reason := range result.JobReasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		blockedReasons[jobID] = reason
	}
	for jobID, failedID := range failedInstanceByJob {
		if failedID == 0 {
			continue
		}
		if _, exists := blockedReasons[jobID]; exists {
			continue
		}
		if reason, ok := result.NotReplacedReasons[failedID]; ok && strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}
}

func selectLaunchGroupsWithinHeadroom(groups []campaign.LaunchGroup, headroom int) ([]campaign.LaunchGroup, []campaign.LaunchGroup, int) {
	if len(groups) == 0 || headroom <= 0 {
		return nil, append([]campaign.LaunchGroup(nil), groups...), 0
	}
	sorted := append([]campaign.LaunchGroup(nil), groups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a := sorted[i]
		b := sorted[j]
		if maxLaunchGroupPriority(a) != maxLaunchGroupPriority(b) {
			return maxLaunchGroupPriority(a) > maxLaunchGroupPriority(b)
		}
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
		jobIDs := append([]int64(nil), group.JobIDs...)
		sort.Slice(jobIDs, func(i, j int) bool { return jobIDs[i] < jobIDs[j] })
		plan.LaunchJobIDs = append(plan.LaunchJobIDs, jobIDs...)
		for _, jobID := range jobIDs {
			delete(blockedReasons, jobID)
		}
	}
}

func maxLaunchGroupPriority(group campaign.LaunchGroup) int {
	return group.Priority
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

// Launch-side reasons used to compose the launch+reuse breakdown. The
// run-rate gate path (autopilot.go run-rate-exceeded fallback) is the only
// caller for which "no rental headroom" is the literal truth — it's the
// budget verdict. The safety-net path uses unclassifiedLaunchReason because
// the planner produced no launch decision for the candidate at all, which is
// almost never a budget verdict; calling it "no rental headroom" misleads
// users when the status bar shows full headroom and zero running instances.
const (
	noRentalHeadroomLaunchReason  = "no rental headroom"
	unclassifiedLaunchReason      = "no launch path determined"
	reuseRejectedHeadlineFragment = "running instances couldn't accept this job"
)

// blockReasonBase composes the combined launch+reuse summary headline from a
// launch-side reason. The result reads "<launch>; running instances couldn't
// accept this job".
func blockReasonBase(launch string) string {
	return launch + "; " + reuseRejectedHeadlineFragment
}

// isPlaceholderLaunchReason reports whether a launch-side string is one of the
// safety-net placeholders this file produces. finalizeUnplacedBlockedReasons
// keys structured-detail reattachment off these prefixes.
func isPlaceholderLaunchReason(s string) bool {
	return strings.HasPrefix(s, noRentalHeadroomLaunchReason) ||
		strings.HasPrefix(s, unclassifiedLaunchReason)
}

func noRentalHeadroomReason(job *db.Job, capacities []campaign.InstanceCapacity, r2Client *r2.Client) string {
	return placementFailureStructured(job, capacities, r2Client, noRentalHeadroomLaunchReason).Flat()
}

// placementFailureStructured builds the full launch/reuse breakdown for a job
// the autopilot could neither launch a new instance for nor reuse onto a
// running one. Launch is the authoritative primary reason (supplied by the
// caller — "no rental headroom" for the run-rate gate, a placeholder for the
// safety-net catch-all); each running instance that refused the job
// contributes a secondary reuse-rejection entry. When any running instance is
// compatible, the launch reason is the whole story and no reuse detail is
// attached.
func placementFailureStructured(job *db.Job, capacities []campaign.InstanceCapacity, r2Client *r2.Client, launchReason string) *blockreason.Structured {
	summary := blockReasonBase(launchReason)
	s := &blockreason.Structured{
		Summary: summary,
		Launch:  launchReason,
	}
	if job == nil || len(capacities) == 0 {
		return s
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
	rejections := make([]blockreason.ReuseRejection, 0, len(sorted))
	for _, cap := range sorted {
		ok := false
		reason := ""
		if r2Client != nil {
			ok, reason = campaign.MatchJobToInstanceWithUV(job, cap, r2Client)
		} else {
			ok, reason = campaign.MatchJobToInstance(job, cap)
		}
		if ok {
			// A running instance is compatible; reuse was blocked elsewhere,
			// so the launch reason is the whole story.
			return &blockreason.Structured{Summary: summary, Launch: launchReason}
		}
		instLabel := ""
		if cap.Instance != nil {
			instLabel = ids.FormatInstanceID(cap.Instance.ID)
		}
		reason = strings.TrimSpace(reason)
		rejections = append(rejections, blockreason.ReuseRejection{Instance: instLabel, Reason: reason})
		if firstReason == "" && reason != "" {
			firstReason = reason
		}
	}
	s.Reuse = rejections
	if firstReason != "" {
		s.Summary = summary + ": " + firstReason
	}
	return s
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
