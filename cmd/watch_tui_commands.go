package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

func (m watchModel) retryFailedInstances(extraAttempts int) tea.Cmd {
	database := m.database
	cfg := m.appConfig
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, extraAttempts)
		if err != nil {
			return retryResultMsg{err: err}
		}
		if result == nil {
			return retryResultMsg{}
		}
		return retryResultMsg{instanceIDs: result.InstanceIDs, skipped: result.Skipped}
	}
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

func (m watchModel) countFailedInstances() int {
	count := 0
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && u.Launch.Status == db.LaunchStatusFailed {
			count++
		}
	}
	if len(m.partialErrorJobs) > 0 {
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
	var cmds []tea.Cmd
	if len(m.unplacedJobs) > 0 && !m.autoPlacing {
		if cmd := m.autoPlaceUnplacedJobs(); cmd != nil {
			m.autoPlacing = true
			cmds = append(cmds, cmd)
		}
	}
	if len(m.unplacedJobs) > 0 && !m.autoLaunching {
		if cmd := m.autoLaunchForUnplacedJobs(); cmd != nil {
			m.autoLaunching = true
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// autoPlaceUnplacedJobs tries to submit unplaced jobs to compatible active instances.
func (m watchModel) autoPlaceUnplacedJobs() tea.Cmd {
	capacities := m.buildInstanceCapacities()
	if len(capacities) == 0 {
		return nil
	}

	// Find the first unplaced job that can be placed
	for _, job := range m.unplacedJobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		ranked := campaign.RankForJob(job, capacities)
		if len(ranked) == 0 {
			continue
		}
		best := ranked[0]
		jobID := job.ID
		instanceID := best.Instance.ID
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
		}
	}
	return nil
}

// autoLaunchForUnplacedJobs launches new instances for unplaced rental jobs.
func (m watchModel) autoLaunchForUnplacedJobs() tea.Cmd {
	// Only launch if no placement was possible
	capacities := m.buildInstanceCapacities()
	hasPlaceable := false
	for _, job := range m.unplacedJobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if ranked := campaign.RankForJob(job, capacities); len(ranked) > 0 {
			hasPlaceable = true
			break
		}
	}
	if hasPlaceable {
		return nil // auto-place will handle these
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
		return nil
	}

	database := m.database
	cfg := m.appConfig
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, 0)
		if err != nil {
			return autoLaunchDoneMsg{err: err}
		}
		if result == nil {
			return autoLaunchDoneMsg{}
		}
		return autoLaunchDoneMsg{instanceIDs: result.InstanceIDs, skipped: result.Skipped}
	}
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
		m.syncWorker.Request(tui.SyncRequest{
			Host: host,
			Rate: tui.GetHostSyncRate(hostJobs),
		})
	}
}

// ---------------------------------------------------------------------------
// Job preservation
// ---------------------------------------------------------------------------

func preserveWatchCurrentJobs(instanceID int64, prevJobs, jobs []*db.Job) []*db.Job {
	if instanceID == 0 {
		return jobs
	}
	if len(jobs) == 0 && len(prevJobs) > 0 {
		return prevJobs
	}
	if len(jobs) == 0 {
		return jobs
	}

	currentJobIDs := make(map[int64]struct{})
	for _, job := range prevJobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID != instanceID {
			continue
		}
		currentJobIDs[job.ID] = struct{}{}
	}
	if len(currentJobIDs) == 0 {
		return jobs
	}

	preserved := make([]*db.Job, 0, len(jobs))
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
	}
	return preserved
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

func refreshWatchSystem(database *sql.DB, cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
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
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			onPremHosts:  groupOnPremHosts(onPremJobs),
			unplacedJobs: unplacedJobs,
		}
	}
}

func refreshWatchUnplacedJobs(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{unplacedJobs: unplacedJobs}
	}
}

// ---------------------------------------------------------------------------
// Tick scheduling
// ---------------------------------------------------------------------------

func scheduleSyncTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

func scheduleCheckDone() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return watchCheckDoneMsg{}
	})
}

func scheduleWatchAllTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
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
			groups = filterProjectGroups(groups, projectWatchFilter)
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
	dbFile := db.Path()
	if dbFile == "" {
		return nil
	}
	dir := filepath.Dir(dbFile)
	targets := map[string]struct{}{}
	addTarget := func(name string) {
		if name == "" {
			return
		}
		targets[filepath.Clean(filepath.Join(dir, name))] = struct{}{}
	}
	base := filepath.Base(dbFile)
	addTarget(base)
	addTarget(base + "-wal")
	addTarget(base + "-shm")

	return func() tea.Msg {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return watchDBWatcherReadyMsg{err: err}
		}
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return watchDBWatcherReadyMsg{err: err}
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
				if !listTUIWatchedDBFile(event.Name, targets) {
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
		m.syncWorker.Request(tui.SyncRequest{
			Host: host,
			Rate: tui.GetHostSyncRate(jobs),
		})
	}
}

func scheduleProjectSyncTick() tea.Cmd {
	return tea.Tick(projectWatchSyncInterval, func(time.Time) tea.Msg {
		return watchProjectSyncTickMsg{}
	})
}
