package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
)

var autoLaunchBackoffDelays = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
}

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

func (m watchModel) retryFailedInstances(extraAttempts int) tea.Cmd {
	database := m.database
	cfg := m.appConfig
	retryBudgetMultipliers := m.retryBudgetMultiplier
	scopeJobIDs := rentalScopedUnplacedJobIDs(m.unplacedJobs)
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, extraAttempts, retryBudgetMultipliers, scopeJobIDs, m.projectFilter, false, false)
		if err != nil {
			return retryResultMsg{err: err}
		}
		if result == nil {
			return retryResultMsg{}
		}
		return retryResultMsg{
			instanceIDs:         result.InstanceIDs,
			skipped:             result.Skipped,
			budgetSkip:          result.BudgetSkip,
			blockedReason:       result.BlockedReason,
			notReplacedReasons:  result.NotReplacedReasons,
			budgetBlockedByInst: result.BudgetBlocked,
		}
	}
}

func rentalScopedUnplacedJobIDs(jobs []*db.Job) []int64 {
	scope := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued || job.HasTag(db.TagInventory) {
			continue
		}
		scope = append(scope, job.ID)
	}
	return scope
}

func (m watchModel) hasRetryableFailures() bool {
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && db.IsRetryableTermination(u.Launch) {
			return true
		}
	}
	return len(m.partialErrors) > 0 && !m.partialErrorsRetried
}

func (m watchModel) countRetryableFailedInstances() int {
	count := 0
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && db.IsRetryableTermination(u.Launch) {
			count++
		}
	}
	if len(m.partialErrors) > 0 && !m.partialErrorsRetried {
		count++
	}
	return count
}

// ---------------------------------------------------------------------------
// Auto-pilot commands
// ---------------------------------------------------------------------------

// runAutoPilot fires auto-placement and auto-launch commands if applicable.
// It sets autoPlacing/autoLaunching flags on m (caller must use the returned
// model state, as in Bubble Tea's value-receiver pattern).
func (m *watchModel) runAutoPilot() tea.Cmd {
	if m.autoNoopReasons == nil {
		m.autoNoopReasons = map[int64]string{}
	}
	// New unplaced set => clear stale diagnostics and launch backoff.
	if sig := autoUnplacedSignature(m.unplacedJobs); sig != m.autoLastUnplacedSig {
		m.autoLastUnplacedSig = sig
		m.autoNoopReasons = map[int64]string{}
		m.autoStatusLine = ""
		m.resetAutoLaunchBackoff()
	}

	var cmds []tea.Cmd
	globalReasons := make([]string, 0, 2)
	reasonsByJob := map[int64]string{}
	hasPlaceable := false
	if len(m.unplacedJobs) > 0 && !m.autoPlacing {
		cmd, reason, byJob, placeable := m.autoPlaceUnplacedJobs()
		hasPlaceable = placeable
		for jobID, detail := range byJob {
			reasonsByJob[jobID] = detail
		}
		if cmd != nil {
			m.autoPlacing = true
			cmds = append(cmds, cmd)
		} else if reason != "" {
			globalReasons = append(globalReasons, reason)
		}
	}
	if len(m.unplacedJobs) > 0 && !m.autoLaunching {
		cmd, reason := m.autoLaunchForUnplacedJobs(hasPlaceable)
		if cmd != nil {
			m.autoLaunching = true
			cmds = append(cmds, cmd)
		} else if reason != "" {
			globalReasons = append(globalReasons, reason)
		}
	}
	m.autoNoopReasons = reasonsByJob
	m.autoStatusLine = strings.Join(globalReasons, " | ")
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// autoPlaceUnplacedJobs tries to submit unplaced jobs to compatible active instances.
func (m watchModel) autoPlaceUnplacedJobs() (tea.Cmd, string, map[int64]string, bool) {
	reasonsByJob := map[int64]string{}
	capacities := m.buildInstanceCapacities()
	queued := make([]*db.Job, 0, len(m.unplacedJobs))
	for _, job := range m.unplacedJobs {
		if job != nil && job.EffectiveStatus() == db.StatusQueued {
			queued = append(queued, job)
		}
	}
	if len(queued) == 0 {
		return nil, "", reasonsByJob, false
	}
	if len(capacities) == 0 {
		for _, job := range queued {
			reasonsByJob[job.ID] = "auto-place: no active reusable instances"
		}
		return nil, "auto-place: no active reusable instances", reasonsByJob, false
	}

	plan, err := buildAutoPlacementPlan(m.database, m.appConfig, queued, capacities)
	if err != nil {
		for _, job := range queued {
			reasonsByJob[job.ID] = "auto-place: planner error"
		}
		return nil, fmt.Sprintf("auto-place: planner error: %v", err), reasonsByJob, false
	}
	for jobID, reason := range plan.BlockedReasons {
		if reason != "" {
			reasonsByJob[jobID] = "auto-place: " + reason
		}
	}
	if len(plan.ReuseAssignments) == 0 {
		if len(plan.LaunchJobIDs) > 0 {
			return nil, "auto-place: no reusable match (launch candidates available)", reasonsByJob, false
		}
		for _, job := range queued {
			if _, ok := reasonsByJob[job.ID]; !ok {
				reasonsByJob[job.ID] = autoNoCompatibleReason(job, capacities)
			}
		}
		return nil, "auto-place: no compatible active instances", reasonsByJob, false
	}

	assignment := plan.ReuseAssignments[0]
	if assignment.Job == nil || assignment.Instance.Instance == nil {
		return nil, "auto-place: planner produced invalid assignment", reasonsByJob, false
	}
	jobID := assignment.Job.ID
	instanceID := assignment.Instance.Instance.ID
	ctx := m.ctx
	database := m.database
	r2Client := m.r2Client
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			return autoPlaceDoneMsg{jobID: jobID, instanceID: instanceID, err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, []*db.Job{job}); err != nil {
			return autoPlaceDoneMsg{jobID: jobID, instanceID: instanceID, err: err}
		}
		return autoPlaceDoneMsg{jobID: jobID, instanceID: instanceID}
	}, "", reasonsByJob, true
}

// autoLaunchForUnplacedJobs launches new instances for unplaced rental jobs.
func (m *watchModel) autoLaunchForUnplacedJobs(hasPlaceable bool) (tea.Cmd, string) {
	if hasPlaceable {
		m.resetAutoLaunchBackoff()
		return nil, "auto-launch: waiting for auto-place to finish"
	}

	// Check if any unplaced jobs are rental-eligible
	hasRental := false
	for _, job := range m.unplacedJobs {
		if job != nil && !job.HasTag(db.TagInventory) {
			hasRental = true
			break
		}
	}
	if !hasRental {
		m.resetAutoLaunchBackoff()
		return nil, "auto-launch: no rental-eligible unplaced jobs"
	}

	// Back off repeated launch attempts when conditions are unchanged.
	if !m.autoLaunchBackoffUntil.IsZero() && time.Now().Before(m.autoLaunchBackoffUntil) {
		wait := time.Until(m.autoLaunchBackoffUntil).Round(time.Second)
		if wait < time.Second {
			wait = time.Second
		}
		if !m.autoLaunchBackoffArmed {
			m.autoLaunchBackoffArmed = true
			return tea.Tick(wait, func(time.Time) tea.Msg { return autoPilotBackoffReadyMsg{} }),
				fmt.Sprintf("auto-launch: backing off for %s (%s)", wait, m.autoLaunchBackoffReason)
		}
		return nil, fmt.Sprintf("auto-launch: backing off for %s (%s)", wait, m.autoLaunchBackoffReason)
	}

	database := m.database
	cfg := m.appConfig
	scopeJobIDs := rentalScopedUnplacedJobIDs(m.unplacedJobs)
	planScope := make([]*db.Job, 0, len(m.unplacedJobs))
	for _, job := range m.unplacedJobs {
		if job != nil && job.EffectiveStatus() == db.StatusQueued && !job.HasTag(db.TagInventory) {
			planScope = append(planScope, job)
		}
	}
	capacities := m.buildInstanceCapacities()
	plan, planErr := buildAutoPlacementPlan(database, cfg, planScope, capacities)
	if planErr == nil && len(plan.LaunchJobIDs) > 0 {
		scopeJobIDs = append([]int64(nil), plan.LaunchJobIDs...)
	}
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, 0, nil, scopeJobIDs, m.projectFilter, false, true)
		if err != nil {
			return autoLaunchDoneMsg{err: err}
		}
		if result == nil {
			return autoLaunchDoneMsg{}
		}
		return autoLaunchDoneMsg{
			instanceIDs:   result.InstanceIDs,
			skipped:       result.Skipped,
			budgetSkip:    result.BudgetSkip,
			blockedReason: result.BlockedReason,
			reasons:       result.NotReplacedReasons,
		}
	}, ""
}

func (m *watchModel) resetAutoLaunchBackoff() {
	m.autoLaunchBackoffUntil = time.Time{}
	m.autoLaunchBackoffReason = ""
	m.autoLaunchBackoffArmed = false
	m.autoLaunchBackoffStep = 0
}

func autoUnplacedSignature(jobs []*db.Job) string {
	if len(jobs) == 0 {
		return ""
	}
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.EffectiveStatus() == db.StatusQueued {
			ids = append(ids, job.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%d", id))
	}
	return b.String()
}

func autoNoCompatibleReason(job *db.Job, capacities []campaign.InstanceCapacity) string {
	if job == nil {
		return "auto-place: no compatible active instances"
	}
	for _, cap := range capacities {
		if ok, reason := campaign.MatchJobToInstance(job, cap); !ok && reason != "" {
			return "auto-place: " + reason
		}
	}
	return "auto-place: no compatible active instances"
}

// ---------------------------------------------------------------------------
// On-prem sync helpers
// ---------------------------------------------------------------------------

func (m watchModel) requestOnPremSyncs() {
	if m.syncWorker == nil {
		return
	}
	jobs, err := db.ListActiveOnPremJobs(m.database)
	if err != nil {
		return
	}
	byHost := make(map[string][]*db.Job)
	for _, job := range jobs {
		if job != nil && job.Host != "" {
			byHost[job.Host] = append(byHost[job.Host], job)
		}
	}
	for host, hostJobs := range byHost {
		m.syncWorker.Request(hostsync.Request{
			Host: host,
			Rate: hostsync.GetHostSyncRate(hostJobs),
		})
	}
}

// ---------------------------------------------------------------------------
// Job preservation
// ---------------------------------------------------------------------------

func preserveWatchCurrentJobs(instanceID int64, prevJobs, jobs []*db.Job) ([]*db.Job, bool) {
	if instanceID == 0 {
		return jobs, false
	}
	if len(jobs) == 0 && len(prevJobs) > 0 {
		return prevJobs, true
	}
	if len(jobs) == 0 {
		return jobs, false
	}

	currentJobIDs := make(map[int64]struct{})
	for _, job := range prevJobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID != instanceID {
			continue
		}
		currentJobIDs[job.ID] = struct{}{}
	}
	if len(currentJobIDs) == 0 {
		return jobs, false
	}

	preserved := make([]*db.Job, 0, len(jobs))
	usedPreservedAttachment := false
	for _, job := range jobs {
		if job == nil {
			preserved = append(preserved, nil)
			continue
		}
		if _, ok := currentJobIDs[job.ID]; !ok {
			preserved = append(preserved, job)
			continue
		}
		if job.LaunchID != nil && *job.LaunchID == instanceID {
			preserved = append(preserved, job)
			continue
		}

		jobCopy := *job
		preservedInstanceID := instanceID
		jobCopy.LaunchID = &preservedInstanceID
		preserved = append(preserved, &jobCopy)
		usedPreservedAttachment = true
	}
	return preserved, usedPreservedAttachment
}

// ---------------------------------------------------------------------------
// Refresh commands
// ---------------------------------------------------------------------------

func refreshWatchInstancesFromDB(database *sql.DB, instanceIDs []int64, quitAfter bool) tea.Cmd {
	return func() tea.Msg {
		cloudInstances := make(map[int64]*db.Launch, len(instanceIDs))
		jobs := make(map[int64][]*db.Job, len(instanceIDs))
		outcomes := make(map[int64]map[int64]string, len(instanceIDs))
		for _, id := range instanceIDs {
			if ci, err := db.GetLaunch(database, id); err == nil && ci != nil {
				cloudInstances[id] = ci
			}
			if instanceJobs, err := db.GetLaunchJobsIncludingAttempts(database, id); err == nil && instanceJobs != nil {
				jobs[id] = instanceJobs
			}
			if instanceOutcomes, err := db.GetAttemptOutcomesByLaunch(database, id); err == nil {
				outcomes[id] = instanceOutcomes
			}
		}
		return watchJobsRefreshedMsg{
			cloudInstances: cloudInstances,
			jobs:           jobs,
			outcomes:       outcomes,
			quitAfter:      quitAfter,
		}
	}
}

func refreshWatchSystem(database *sql.DB, cfg *config.Config, previousLaunches []*db.Launch) tea.Cmd {
	previousLaunchIDs := make([]int64, 0, len(previousLaunches))
	for _, launch := range previousLaunches {
		if launch == nil {
			continue
		}
		previousLaunchIDs = append(previousLaunchIDs, launch.ID)
	}
	return func() tea.Msg {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false, previousLaunchIDs)
		return watchAllRefreshedMsg{snapshot: snapshot, err: err}
	}
}

func requestWatchJobUnplace(database *sql.DB, jobID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("job %d not found", jobID)}
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return watchUnplaceDoneMsg{err: err}
		}
		updatedJob, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("reload job %d: %w", jobID, err)}
		}
		return watchUnplaceDoneMsg{job: updatedJob, message: result.Message}
	}
}

func requestWatchJobSubmit(ctx context.Context, database *sql.DB, r2Client *r2.Client, jobID, instanceID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: fmt.Errorf("job %d not found", jobID)}
		}
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, []*db.Job{job}); err != nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: err}
		}
		return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID}
	}
}

func requestWatchJobKill(database *sql.DB, jobID int64) tea.Cmd {
	return func() tea.Msg {
		msg, err := killOrCancelCloudJob(database, jobID, db.StatusKilled)
		if err != nil {
			return watchKillDoneMsg{jobID: jobID, err: err}
		}
		if msg != "" {
			return watchKillDoneMsg{jobID: jobID, message: msg}
		}
		// Non-cloud job: use ops.KillJob
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchKillDoneMsg{jobID: jobID, err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchKillDoneMsg{jobID: jobID, err: fmt.Errorf("job %d not found", jobID)}
		}
		result, err := ops.KillJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return watchKillDoneMsg{jobID: jobID, err: err}
		}
		return watchKillDoneMsg{jobID: jobID, message: result.Message}
	}
}

func requestWatchInstanceTerminate(database *sql.DB, instanceID int64) tea.Cmd {
	return func() tea.Msg {
		_, errs := terminateInstancesParallel(database, []int64{instanceID})
		if len(errs) > 0 {
			return watchTerminateDoneMsg{instanceID: instanceID, err: errs[0]}
		}
		return watchTerminateDoneMsg{instanceID: instanceID, message: fmt.Sprintf("Instance %d terminated", instanceID)}
	}
}

func refreshWatchOnPrem(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		onPremJobs, err := db.ListActiveOnPremJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		queueblock.Apply(onPremJobs, queueblock.Fetch(onPremJobs, 5*time.Second))
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			updateOnPremHosts:  true,
			onPremHosts:        groupOnPremHosts(onPremJobs),
			updateUnplacedJobs: true,
			unplacedJobs:       unplacedJobs,
		}
	}
}

func refreshWatchUnplacedJobs(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			updateUnplacedJobs: true,
			unplacedJobs:       unplacedJobs,
		}
	}
}

// ---------------------------------------------------------------------------
// Tick scheduling
// ---------------------------------------------------------------------------

func (m watchModel) scheduleSyncTick() tea.Cmd {
	return tea.Tick(throttledInterval(15*time.Second, m.focused), func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

func (m watchModel) scheduleCheckDone() tea.Cmd {
	return tea.Tick(throttledInterval(5*time.Second, m.focused), func(time.Time) tea.Msg {
		return watchCheckDoneMsg{}
	})
}

func (m watchModel) scheduleWatchAllTick() tea.Cmd {
	return tea.Tick(throttledInterval(15*time.Second, m.focused), func(time.Time) tea.Msg {
		return watchAllTickMsg{}
	})
}

// ---------------------------------------------------------------------------
// Project-mode commands
// ---------------------------------------------------------------------------

func (m watchModel) reloadProjectGroups() tea.Cmd {
	database := m.database
	recentWindow := m.projectRecent
	return func() tea.Msg {
		groups, err := loadProjectWatchGroups(database, recentWindow)
		if err == nil {
			groups = filterProjectGroups(groups, m.projectFilter)
		}
		return watchProjectLoadedMsg{groups: groups, err: err}
	}
}

func (m watchModel) runProjectBackgroundSync(full bool) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		return watchProjectSyncFinishedMsg{warnings: syncProjectWatchTUIData(database, full), full: full}
	}
}

func (m watchModel) startDBWatcher() tea.Cmd {
	return func() tea.Msg {
		watcher, targets, err := dbwatch.OpenJobsDBWatcher()
		if err != nil {
			return watchDBWatcherReadyMsg{err: err}
		}
		if watcher == nil {
			return nil
		}
		return watchDBWatcherReadyMsg{watcher: watcher, targets: targets}
	}
}

func (m watchModel) waitForDBEvent() tea.Cmd {
	if m.dbWatcher == nil || len(m.dbWatcherTargets) == 0 {
		return nil
	}
	watcher := m.dbWatcher
	targets := m.dbWatcherTargets
	return func() tea.Msg {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return watchDBWatchEventMsg{err: fmt.Errorf("db watcher closed")}
				}
				if !dbwatch.IsWatchedFile(event.Name, targets) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				return watchDBWatchEventMsg{}
			case err, ok := <-watcher.Errors:
				if !ok {
					return watchDBWatchEventMsg{err: fmt.Errorf("db watcher error channel closed")}
				}
				return watchDBWatchEventMsg{err: err}
			}
		}
	}
}

func (m watchModel) requestProjectActiveSyncs() {
	if m.syncWorker == nil {
		return
	}
	hosts := make(map[string][]*db.Job)
	for _, g := range m.projectGroups {
		for _, job := range append(g.Running, g.Queued...) {
			if job != nil && job.Host != "" {
				hosts[job.Host] = append(hosts[job.Host], job)
			}
		}
	}
	for host, jobs := range hosts {
		m.syncWorker.Request(hostsync.Request{
			Host: host,
			Rate: hostsync.GetHostSyncRate(jobs),
		})
	}
}

func (m watchModel) scheduleProjectSyncTick() tea.Cmd {
	return tea.Tick(throttledInterval(projectWatchSyncInterval, m.focused), func(time.Time) tea.Msg {
		return watchProjectSyncTickMsg{}
	})
}

// ---------------------------------------------------------------------------
// Move picker commands
// ---------------------------------------------------------------------------

// requestMoveOptions builds the list of move destinations (existing instances + new offers).
// sourceInstanceID is the instance the job is currently on (excluded from existing options).
// queuedCounts is a snapshot of queued-job counts per instance (built on the main goroutine
// to avoid a data race on the updates map).
func requestMoveOptions(
	database *sql.DB,
	cfg *config.Config,
	cloudClients []cloud.Client,
	job *db.Job,
	capacities []campaign.InstanceCapacity,
	queuedCounts map[int64]int,
	sourceInstanceID int64,
) tea.Cmd {
	jobID := job.ID
	return func() tea.Msg {
		var options []moveOption

		// --- Existing instances ---
		var filtered []campaign.InstanceCapacity
		for _, cap := range capacities {
			if cap.Instance.ID == sourceInstanceID {
				continue
			}
			filtered = append(filtered, cap)
		}
		ranked := campaign.RankForJob(job, filtered)
		for _, cap := range ranked {
			inst := cap.Instance
			gpuName := inst.ResolvedGPUName
			if gpuName == "" {
				gpuName = inst.GPUClass
			}

			waitTime := time.Duration(queuedCounts[inst.ID]) * 30 * time.Minute
			costPerHour := float64(inst.CostPerHourCents) / 100.0

			options = append(options, moveOption{
				isNew:       false,
				instanceID:  inst.ID,
				gpuName:     gpuName,
				waitTime:    waitTime,
				costPerHour: costPerHour,
			})
		}

		// --- New instance offers (one per strategy) ---
		if len(cloudClients) > 0 {
			group := campaign.InstanceGroup{
				GPUClass: job.GPUClass,
				Jobs:     []*db.Job{job},
			}
			if job.GPUMemGB != nil {
				group.GPUMemGB = *job.GPUMemGB
			}

			rawOffers := campaign.FetchGroupRawOffers(cloudClients, []campaign.InstanceGroup{group})

			strategies := []bidding.SelectionStrategy{
				bidding.StrategyCheap,
				bidding.StrategyFast,
				bidding.StrategyFastest,
			}

			survivalModel := buildSurvivalModel(database)
			overheadModel := buildOverheadModel(database)
			predCfg := buildPredictorConfig(cfg)
			setupFactory := campaign.OfferSetupOverheadFactory(database, overheadModel)

			seen := make(map[string]bool) // deduplicate by offer key
			for _, strategy := range strategies {
				ranked := campaign.RankGroupOffersWithPredictor(rawOffers, &predCfg, survivalModel, setupFactory, strategy, 0)
				if len(ranked) == 0 || ranked[0].Offer == nil {
					continue
				}
				offer := ranked[0].Offer
				key := offer.Key()
				if seen[key] {
					// Different strategy picked same offer — show it once with first strategy
					continue
				}
				seen[key] = true

				// Estimate setup time: ~8 min as rough default
				setupTime := 8 * time.Minute

				options = append(options, moveOption{
					isNew:       true,
					offer:       offer,
					strategy:    strategy,
					gpuName:     offer.GPUName,
					waitTime:    setupTime,
					costPerHour: offer.CostPerHour,
				})
			}
		}

		if len(options) == 0 {
			return moveOptionsReadyMsg{jobID: jobID, err: fmt.Errorf("no compatible destinations found")}
		}
		return moveOptionsReadyMsg{jobID: jobID, options: options}
	}
}

// requestMoveExecute performs the actual move: unplace from source, then
// either submit to existing instance or launch a new one.
func requestMoveExecute(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	cfg *config.Config,
	cloudClients []cloud.Client,
	jobID int64,
	opt moveOption,
) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("job %d not found", jobID)}
		}

		// Step 1: unplace from current instance
		if _, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("unplace: %w", err)}
		}

		// Re-fetch after unplace
		job, err = db.GetJobByID(database, jobID)
		if err != nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("reload job %d: %w", jobID, err)}
		}

		if !opt.isNew {
			// Step 2a: submit to existing instance
			if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, opt.instanceID, []*db.Job{job}); err != nil {
				return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("submit to instance %d: %w", opt.instanceID, err)}
			}
			return moveExecuteDoneMsg{
				jobID:      jobID,
				targetDesc: fmt.Sprintf("instance #%d", opt.instanceID),
			}
		}

		// Step 2b: launch new instance with this job
		offer := *opt.offer
		group := campaign.InstanceGroup{
			GPUClass: job.GPUClass,
			Jobs:     []*db.Job{job},
		}
		if job.GPUMemGB != nil {
			group.GPUMemGB = *job.GPUMemGB
		}

		// Resolve grace period
		gracePeriod := 5 * time.Minute
		if cfg != nil {
			if gp := cfg.DefaultGracePeriod(); gp != "" && gp != "0" {
				if d, parseErr := time.ParseDuration(gp); parseErr == nil {
					gracePeriod = d
				}
			}
		}

		launchOpts := campaign.LaunchOpts{
			GracePeriodSeconds: int(gracePeriod.Seconds()),
			Strategy:           opt.strategy,
		}

		// Compute estimates for auto-budget
		predCfg := buildPredictorConfig(cfg)
		survivalModel := buildSurvivalModel(database)
		groupOffer := campaign.GroupOffer{Group: group, Offer: &offer}
		estimates := campaign.EstimateCosts(database, []campaign.GroupOffer{groupOffer}, &predCfg, nil, nil, survivalModel, nil)
		if len(estimates) > 0 {
			launchOpts.ApplyAutoBudget(estimates)
		}

		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		client := cloudClientForProvider(cloudClients, offer.Provider)
		if client == nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("no client for provider %s", offer.Provider)}
		}

		createOpts, err := createOptsForProvider(cfg, offer.Provider)
		if err != nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("create opts: %w", err)}
		}

		result, err := campaign.LaunchCampaign(
			[]cloud.Client{client},
			database,
			[]campaign.InstanceGroup{group},
			[]cloud.Offer{offer},
			estimates,
			survivalModel,
			launchOpts,
			r2Cfg,
			func(p cloud.Provider) (cloud.CreateOpts, error) { return createOpts, nil },
			func(campaign.InstanceGroup, string) {},
			nil,
			func(campaign.InstanceGroup, int64) {},
		)
		if err != nil {
			return moveExecuteDoneMsg{jobID: jobID, err: fmt.Errorf("launch: %w", err)}
		}

		desc := fmt.Sprintf("new %s instance", opt.gpuName)
		if len(result.InstanceIDs) > 0 {
			desc = fmt.Sprintf("new %s instance #%d", opt.gpuName, result.InstanceIDs[0])
		}
		return moveExecuteDoneMsg{jobID: jobID, targetDesc: desc}
	}
}
