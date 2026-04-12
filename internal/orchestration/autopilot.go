package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
)

type GroupedAutoPilotResult struct {
	Placed         int
	Launched       int
	LaunchedClass  string
	BlockedReasons map[int64]string
}

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
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
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
		return &GroupedAutoPilotResult{}, nil
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

	plan, err := buildAutoPlacementPlan(database, cfg, unplaced, capacities)
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
	oplog.Log("auto_pilot.plan",
		oplog.WithDetailf("unplaced=%d capacities=%d reuse=%d launch=%d blocked=%d",
			len(unplaced), len(capacities), len(plan.ReuseAssignments), len(plan.LaunchJobIDs), len(blockedReasons)))

	placed := 0
	for _, assignment := range plan.ReuseAssignments {
		if assignment.Job == nil || assignment.Instance.Instance == nil {
			continue
		}
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, assignment.Instance.Instance.ID, []*db.Job{assignment.Job}); err != nil {
			oplog.LogJob("auto_pilot.reuse_failed", assignment.Job.ID, "",
				oplog.WithError(err),
				oplog.WithDetailf("instance=%d", assignment.Instance.Instance.ID))
			blockedReasons[assignment.Job.ID] = fmt.Sprintf("reuse instance %d failed: %s", assignment.Instance.Instance.ID, summarizeAutoPilotError(err))
			continue
		}
		placed++
	}

	remaining, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, err
	}
	rentalScope := make([]int64, 0, len(remaining))
	launchScope := make(map[int64]struct{}, len(plan.LaunchJobIDs))
	for _, jobID := range plan.LaunchJobIDs {
		launchScope[jobID] = struct{}{}
	}
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
			Launched:       0,
			LaunchedClass:  "",
			BlockedReasons: blockedReasons,
		}, nil
	}
	oplog.Log("auto_pilot.launching",
		oplog.WithDetailf("rental_scope=%d", len(rentalScope)))

	failedInstanceByJob := BuildFailedInstanceByJob(database, rentalScope)
	result, err := RelaunchOrphanedJobs(database, cfg, 0, nil, rentalScope, "", false, true)
	if err != nil {
		oplog.Log("auto_pilot.launch_error",
			oplog.WithError(err),
			oplog.WithDetailf("rental_scope=%d", len(rentalScope)))
		for _, jobID := range rentalScope {
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = "launch failed"
			}
		}
		return nil, err
	}
	if result == nil {
		return &GroupedAutoPilotResult{
			Placed:         placed,
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
		Launched:       len(result.InstanceIDs),
		LaunchedClass:  launchedClassFromResult(database, result.InstanceIDs),
		BlockedReasons: blockedReasons,
	}, nil
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
