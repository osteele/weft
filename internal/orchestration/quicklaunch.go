package orchestration

import (
	"context"
	"database/sql"
	"errors"
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

	byID := make(map[int64]*db.Job, len(scopedLaunchable))
	for _, job := range scopedLaunchable {
		if job != nil {
			byID[job.ID] = job
		}
	}
	launchJobID := plan.LaunchJobIDs[0]
	launchJob := byID[launchJobID]
	if launchJob == nil {
		return QuickLaunchResult{}, fmt.Errorf("launch job %s not found in current scope", ids.FormatJobID(launchJobID))
	}

	emit(fmt.Sprintf("Preparing job #%d for new instance...", launchJobID))
	if launchJob.TargetKind() != db.JobTargetUnplaced {
		if _, err := ops.UnplaceQueuedJob(database, launchJob, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
			return QuickLaunchResult{}, fmt.Errorf("prepare launch anchor job %s: %w", ids.FormatJobID(launchJob.ID), err)
		}
	}

	emit(fmt.Sprintf("Launching new instance for job #%d...", launchJobID))
	relaunchResult, err := RelaunchOrphanedJobs(database, cfg, 0, nil, []int64{launchJobID}, "", false, true)
	if err != nil {
		return QuickLaunchResult{}, err
	}
	if relaunchResult != nil && relaunchResult.BlockedReason != "" {
		return QuickLaunchResult{}, errors.New(relaunchResult.BlockedReason)
	}
	if relaunchResult == nil || len(relaunchResult.InstanceIDs) == 0 {
		return QuickLaunchResult{}, fmt.Errorf("no compatible offer found")
	}

	newInstanceID := relaunchResult.InstanceIDs[0]
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

	return QuickLaunchResult{
		InstanceIDs:  []int64{newInstanceID},
		MovedJobs:    movedJobs,
		Warning:      warning,
		RunningJobID: runningJobID,
	}, nil
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

type queueETAState struct {
	AvailByInstance  map[int64][]time.Duration
	QueuedByInstance map[int64]int
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
	scopedQueued, err := listScopedLaunchableJobs(database, scoped)
	if err != nil {
		return 0, "", err
	}

	targetLaunch, err := db.GetLaunch(database, targetInstanceID)
	if err != nil || targetLaunch == nil {
		return 0, "", fmt.Errorf("load launched instance %s: %w", ids.FormatInstanceID(targetInstanceID), err)
	}
	targetLaunchJobs, _ := db.GetLaunchJobsIncludingAttempts(database, targetInstanceID)
	targetCap, ok := campaign.NewInstanceCapacity(targetLaunch, countRunningJobs(targetLaunchJobs))
	if !ok {
		return 0, "", fmt.Errorf("launched instance %s is not reusable", ids.FormatInstanceID(targetInstanceID))
	}

	candidates := make([]*db.Job, 0)
	for _, job := range scopedQueued {
		if job == nil || job.ID == anchorJobID {
			continue
		}
		if job.LaunchID == nil || *job.LaunchID <= 0 || *job.LaunchID == targetInstanceID {
			continue
		}
		if ok, _ := campaign.MatchJobToInstance(job, targetCap); !ok {
			continue
		}
		candidates = append(candidates, job)
	}
	if len(candidates) == 0 {
		return 0, "", nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return db.SchedulingLess(candidates[i], candidates[j])
	})

	state := buildQueueETAState(scopedQueued, time.Now())
	currentMean := meanQueuedCompletionETA(state)
	if currentMean <= 0 {
		return 0, "", nil
	}

	for {
		bestGain := time.Duration(0)
		bestIdx := -1
		for i, job := range candidates {
			if job == nil || job.LaunchID == nil {
				continue
			}
			src := *job.LaunchID
			if src == 0 || src == targetInstanceID {
				continue
			}
			if state.QueuedByInstance[src] <= 0 {
				continue
			}
			next := cloneQueueETAState(state)
			next.QueuedByInstance[src]--
			next.QueuedByInstance[targetInstanceID]++
			nextMean := meanQueuedCompletionETA(next)
			gain := currentMean - nextMean
			if gain > bestGain {
				bestGain = gain
				bestIdx = i
			}
		}
		if bestIdx < 0 || bestGain <= 0 {
			break
		}
		job := candidates[bestIdx]
		candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)
		if job == nil || job.LaunchID == nil {
			continue
		}
		src := *job.LaunchID
		if _, unplaceErr := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); unplaceErr != nil {
			warning = "some queued jobs could not be moved"
			continue
		}
		refreshed, getErr := db.GetJobByID(database, job.ID)
		if getErr != nil || refreshed == nil {
			return moved, warning, fmt.Errorf("reload moved job %s: %w", ids.FormatJobID(job.ID), getErr)
		}
		if submitErr := campaign.SubmitJobsToInstance(ctx, database, r2Client, targetInstanceID, []*db.Job{refreshed}); submitErr != nil {
			return moved, warning, fmt.Errorf("submit moved job %s to instance %s: %w", ids.FormatJobID(job.ID), ids.FormatInstanceID(targetInstanceID), submitErr)
		}
		moved++
		state.QueuedByInstance[src]--
		state.QueuedByInstance[targetInstanceID]++
		currentMean -= bestGain
	}
	return moved, warning, nil
}

func buildQueueETAState(jobs []*db.Job, now time.Time) queueETAState {
	state := queueETAState{
		AvailByInstance:  make(map[int64][]time.Duration),
		QueuedByInstance: make(map[int64]int),
	}
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		instanceID := *job.LaunchID
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			state.AvailByInstance[instanceID] = append(
				state.AvailByInstance[instanceID],
				runningJobRemaining(job, now),
			)
		case db.StatusQueued, db.StatusPendingPlacement:
			state.QueuedByInstance[instanceID]++
		}
	}
	return state
}

func cloneQueueETAState(state queueETAState) queueETAState {
	copyState := queueETAState{
		AvailByInstance:  make(map[int64][]time.Duration, len(state.AvailByInstance)),
		QueuedByInstance: make(map[int64]int, len(state.QueuedByInstance)),
	}
	for id, avail := range state.AvailByInstance {
		copyState.AvailByInstance[id] = append([]time.Duration(nil), avail...)
	}
	for id, queued := range state.QueuedByInstance {
		copyState.QueuedByInstance[id] = queued
	}
	return copyState
}

func meanQueuedCompletionETA(state queueETAState) time.Duration {
	totalQueued := 0
	var total time.Duration
	for instanceID, queued := range state.QueuedByInstance {
		if queued <= 0 {
			continue
		}
		totalQueued += queued
		avail := append([]time.Duration(nil), state.AvailByInstance[instanceID]...)
		if len(avail) == 0 {
			avail = []time.Duration{0}
		}
		for i := 0; i < queued; i++ {
			slot := 0
			for idx := 1; idx < len(avail); idx++ {
				if avail[idx] < avail[slot] {
					slot = idx
				}
			}
			completion := avail[slot] + 30*time.Minute
			avail[slot] = completion
			total += completion
		}
	}
	if totalQueued == 0 {
		return 0
	}
	return total / time.Duration(totalQueued)
}

func runningJobRemaining(job *db.Job, now time.Time) time.Duration {
	if job == nil {
		return 0
	}
	if job.StartTime <= 0 {
		return 15 * time.Minute
	}
	elapsed := now.Unix() - job.StartTime
	if elapsed < 0 {
		elapsed = 0
	}
	elapsedDur := time.Duration(elapsed) * time.Second
	defaultDur := 30 * time.Minute
	if elapsedDur >= defaultDur {
		return 0
	}
	return defaultDur - elapsedDur
}
