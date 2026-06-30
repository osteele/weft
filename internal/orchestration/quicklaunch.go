package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
)

type QuickLaunchResult struct {
	InstanceIDs  []int64
	MovedJobs    int
	Warning      string
	RunningJobID int64
}

func RunQuickLaunch(
	ctx context.Context,
	database *sql.DB,
	scopeOwner string,
	scope string,
	jobs []*db.Job,
	onProgress func(string),
) (QuickLaunchResult, error) {
	emit := func(message string) {
		if onProgress == nil || strings.TrimSpace(message) == "" {
			return
		}
		onProgress(message)
	}

	emit("Acquiring launch lease...")
	acquired, err := db.AcquireAutoLease(database, scope, scopeOwner, 30*time.Second)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	if !acquired {
		return QuickLaunchResult{}, fmt.Errorf("another TUI is launching for this scope")
	}
	defer db.ReleaseAutoLease(database, scope, scopeOwner)

	scoped := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	emit("Planning placement...")
	scopedLaunchable, err := listScopedLaunchableJobs(database, scoped)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	if len(scopedLaunchable) == 0 {
		return QuickLaunchResult{}, fmt.Errorf("no launchable queued jobs in this view")
	}

	cfg, err := config.Load()
	if err != nil {
		return QuickLaunchResult{}, err
	}
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	for _, ci := range launches {
		liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(liveJobs)); ok {
			capacities = append(capacities, cap)
		}
	}
	plan, err := buildAutoPlacementPlan(database, cfg, scopedLaunchable, capacities)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	if len(plan.LaunchJobIDs) == 0 {
		return QuickLaunchResult{}, fmt.Errorf("queued jobs can be placed on existing instances")
	}

	_, launchJobID, relaunchResult, skipped, err := launchBestQuickLaunchGroup(database, cfg, scopedLaunchable, plan, emit)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	if relaunchResult == nil || len(relaunchResult.InstanceIDs) == 0 {
		if len(skipped) > 0 {
			return QuickLaunchResult{}, fmt.Errorf("no compatible offer found after skipping %s", strings.Join(skipped, "; "))
		}
		return QuickLaunchResult{}, fmt.Errorf("no compatible offer found")
	}

	newInstanceID := relaunchResult.InstanceIDs[0]
	recordLaunchDecisions(database, relaunchResult.InstanceIDs, "quicklaunch", "launched on demand")
	emit(fmt.Sprintf("Instance %s created; rebalancing queued jobs...", ids.FormatInstanceID(newInstanceID)))
	movedJobs, warning, err := rebalanceQueuedJobsToLaunchedInstance(ctx, database, cfg, newInstanceID, launchJobID, scoped)
	if err != nil {
		return QuickLaunchResult{
			InstanceIDs: []int64{newInstanceID},
			MovedJobs:   movedJobs,
			Warning:     warning,
		}, err
	}

	runningJobID, waitWarning, err := waitForQuickLaunchRunningState(ctx, database, newInstanceID, 90*time.Second, onProgress)
	if err != nil {
		return QuickLaunchResult{
			InstanceIDs: []int64{newInstanceID},
			MovedJobs:   movedJobs,
			Warning:     warning,
		}, err
	}
	if strings.TrimSpace(waitWarning) != "" {
		if strings.TrimSpace(warning) != "" {
			warning = warning + "; " + waitWarning
		} else {
			warning = waitWarning
		}
	}
	if len(skipped) > 0 {
		warning = appendQuickLaunchWarning(warning, "skipped "+strings.Join(skipped, "; "))
	}

	return QuickLaunchResult{
		InstanceIDs:  []int64{newInstanceID},
		MovedJobs:    movedJobs,
		Warning:      warning,
		RunningJobID: runningJobID,
	}, nil
}

func launchBestQuickLaunchGroup(
	database *sql.DB,
	cfg *config.Config,
	jobs []*db.Job,
	plan campaign.AutoPlacementPlan,
	emit func(string),
) (campaign.LaunchGroup, int64, *campaign.RelaunchResult, []string, error) {
	byID := make(map[int64]*db.Job, len(jobs))
	for _, job := range jobs {
		if job != nil {
			byID[job.ID] = job
		}
	}
	groups := rankedQuickLaunchGroups(plan)
	var skipped []string
	for _, group := range groups {
		launchJobID := quickLaunchAnchorJobID(group, byID)
		if launchJobID == 0 {
			continue
		}
		launchJob := byID[launchJobID]
		if launchJob == nil {
			skipped = append(skipped, fmt.Sprintf("%s: missing job", ids.FormatJobID(launchJobID)))
			continue
		}
		emit(fmt.Sprintf("Preparing job #%d for new instance...", launchJobID))
		if launchJob.TargetKind() != db.JobTargetUnplaced {
			if _, err := ops.UnplaceQueuedJob(database, launchJob, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
				skipped = append(skipped, fmt.Sprintf("%s: %v", ids.FormatJobID(launchJob.ID), err))
				continue
			}
		}

		scope := group.JobIDs
		if len(scope) == 0 {
			scope = []int64{launchJobID}
		}
		emit(fmt.Sprintf("Launching new instance for job #%d...", launchJobID))
		relaunchResult, err := RelaunchOrphanedJobs(database, cfg, 0, nil, scope, "", false, true)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", ids.FormatJobID(launchJobID), err))
			continue
		}
		if relaunchResult != nil && relaunchResult.BlockedReason != "" {
			skipped = append(skipped, fmt.Sprintf("%s: %s", ids.FormatJobID(launchJobID), relaunchResult.BlockedReason))
			continue
		}
		if relaunchResult == nil || len(relaunchResult.InstanceIDs) == 0 {
			reason := quickLaunchNoInstanceReason(relaunchResult, scope)
			skipped = append(skipped, fmt.Sprintf("%s: %s", ids.FormatJobID(launchJobID), reason))
			continue
		}
		return group, launchJobID, relaunchResult, skipped, nil
	}
	return campaign.LaunchGroup{}, 0, nil, skipped, nil
}

func rankedQuickLaunchGroups(plan campaign.AutoPlacementPlan) []campaign.LaunchGroup {
	groups := append([]campaign.LaunchGroup(nil), plan.LaunchGroups...)
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority > groups[j].Priority
		}
		if len(groups[i].JobIDs) != len(groups[j].JobIDs) {
			return len(groups[i].JobIDs) > len(groups[j].JobIDs)
		}
		if groups[i].CostPerHourCents != groups[j].CostPerHourCents {
			return groups[i].CostPerHourCents < groups[j].CostPerHourCents
		}
		return firstLaunchGroupJobID(groups[i]) < firstLaunchGroupJobID(groups[j])
	})
	return groups
}

func firstLaunchGroupJobID(group campaign.LaunchGroup) int64 {
	for _, id := range group.JobIDs {
		if id > 0 {
			return id
		}
	}
	return 0
}

func quickLaunchAnchorJobID(group campaign.LaunchGroup, byID map[int64]*db.Job) int64 {
	var best *db.Job
	for _, id := range group.JobIDs {
		job := byID[id]
		if job == nil {
			continue
		}
		if best == nil || db.SchedulingLess(job, best) {
			best = job
		}
	}
	if best == nil {
		return 0
	}
	return best.ID
}

func quickLaunchNoInstanceReason(result *campaign.RelaunchResult, scope []int64) string {
	if result == nil {
		return "no instance launched"
	}
	for _, id := range scope {
		if result.JobReasons != nil {
			if reason := strings.TrimSpace(result.JobReasons[id]); reason != "" {
				return reason
			}
		}
	}
	if len(result.Errors) > 0 && result.Errors[0] != nil {
		return result.Errors[0].Error()
	}
	return "no instance launched"
}

func appendQuickLaunchWarning(existing, extra string) string {
	existing = strings.TrimSpace(existing)
	extra = strings.TrimSpace(extra)
	if existing == "" {
		return extra
	}
	if extra == "" {
		return existing
	}
	return existing + "; " + extra
}

func waitForQuickLaunchRunningState(
	ctx context.Context,
	database *sql.DB,
	instanceID int64,
	maxWait time.Duration,
	onProgress func(string),
) (int64, string, error) {
	emit := func(message string) {
		if onProgress != nil && strings.TrimSpace(message) != "" {
			onProgress(message)
		}
	}
	if instanceID <= 0 || database == nil {
		return 0, "", nil
	}
	emit(fmt.Sprintf("Waiting for first running job on instance %s...", ids.FormatInstanceID(instanceID)))
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastProgress := time.Time{}
	for {
		if launchJobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID); err == nil {
			for _, job := range launchJobs {
				if job == nil {
					continue
				}
				status := job.EffectiveStatus()
				if status == db.StatusRunning || status == db.StatusStarting {
					return job.ID, "", nil
				}
			}
		}
		if launch, err := db.GetLaunch(database, instanceID); err == nil && launch != nil && campaign.IsInstanceTerminal(launch.Status) {
			return 0, "", fmt.Errorf("instance %s ended before any job started (%s)", ids.FormatInstanceID(instanceID), launch.Status)
		}
		if time.Now().After(deadline) {
			return 0, "waiting for scheduler to start first job", nil
		}
		if time.Since(lastProgress) >= 5*time.Second {
			emit(fmt.Sprintf("Waiting for first running job on instance %s... (%ds)", ids.FormatInstanceID(instanceID), int(time.Since(deadline.Add(-maxWait)).Seconds())))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func listScopedLaunchableJobs(database *sql.DB, scoped map[int64]struct{}) ([]*db.Job, error) {
	if database == nil {
		return nil, nil
	}
	idsSorted := make([]int64, 0, len(scoped))
	for id := range scoped {
		idsSorted = append(idsSorted, id)
	}
	sort.Slice(idsSorted, func(i, j int) bool { return idsSorted[i] < idsSorted[j] })
	out := make([]*db.Job, 0, len(idsSorted))
	for _, id := range idsSorted {
		job, err := db.GetJobByID(database, id)
		if err != nil || job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			continue
		}
		if job.HasTag(db.TagInventory) {
			continue
		}
		out = append(out, job)
	}
	return out, nil
}

func rebalanceQueuedJobsToLaunchedInstance(
	ctx context.Context,
	database *sql.DB,
	cfg *config.Config,
	targetInstanceID int64,
	anchorJobID int64,
	scoped map[int64]struct{},
) (moved int, warning string, err error) {
	if database == nil || targetInstanceID == 0 {
		return 0, "", nil
	}
	r2Client, _ := BuildR2Client(cfg)
	jobScope := make(map[int64]struct{}, len(scoped))
	for id := range scoped {
		if id == anchorJobID {
			continue
		}
		jobScope[id] = struct{}{}
	}
	result, err := RebalanceQueuedJobsAcrossInstances(ctx, database, QueueRebalanceOptions{
		Apply:     true,
		JobScope:  jobScope,
		Operation: "quick_launch.rebalance",
		R2Client:  r2Client,
	})
	if err != nil {
		return len(result.Moves), "some queued jobs could not be moved", err
	}
	return len(result.Moves), "", nil
}
