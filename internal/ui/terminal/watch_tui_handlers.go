package terminal

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// ---------------------------------------------------------------------------
// Update handlers: watch updates
// ---------------------------------------------------------------------------

func (m watchModel) handleWatchUpdate(msg watchUpdateMsg) (tea.Model, tea.Cmd) {
	m.ensurePreservedJobAttachmentMap()
	if msg.closed {
		clearWatchJobProgressHWM(m.jobProgressHWM, m.updates[msg.instanceID])
		delete(m.preservedJobAttachment, msg.instanceID)
		if m.mode == watchModeSystem {
			delete(m.channels, msg.instanceID)
			delete(m.clients, msg.instanceID)
		}
		return m, m.checkAllDone()
	}

	prev := m.updates[msg.instanceID]
	if msg.update.Launch != nil && campaign.IsInstanceTerminal(msg.update.Launch.Status) {
		var usedPreservedAttachment bool
		msg.update.Jobs, usedPreservedAttachment = preserveWatchCurrentJobs(msg.update.Launch.ID, prev.Jobs, msg.update.Jobs)
		if usedPreservedAttachment {
			m.preservedJobAttachment[msg.instanceID] = true
		} else {
			delete(m.preservedJobAttachment, msg.instanceID)
		}
	} else {
		delete(m.preservedJobAttachment, msg.instanceID)
	}
	updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
	m.updates[msg.instanceID] = msg.update

	// Auto-relaunch on retryable infrastructure failure (instance-based modes)
	ci := msg.update.Launch
	ch := m.channels[msg.instanceID]
	if m.autoMode && ci != nil && db.IsRetryableTermination(ci) && !m.retrying && m.database != nil {
		m.logRetryAutoTriggered(ci)
		m.retrying = true
		m.retryResult = ""
		return m, tea.Batch(
			waitForUpdate(msg.instanceID, ch),
			m.retryFailedInstances(0),
		)
	}

	return m, waitForUpdate(msg.instanceID, ch)
}

// ---------------------------------------------------------------------------
// Update handlers: check-done
// ---------------------------------------------------------------------------

// logRetryAutoTriggered records a lifecycle event for an auto-relaunch trigger.
func (m watchModel) logRetryAutoTriggered(ci *db.Launch) {
	_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
		EventKind:  db.EventRetryAutoTriggered,
		LaunchID:   ci.ID,
		CampaignID: m.campaignID,
		GPUSpec:    ci.GPUSpec,
		Detail:     ci.DisplayTerminationReason(),
	})
}

func (m watchModel) handleCheckDone() (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	return m, m.checkAllDone()
}

func (m watchModel) handleCheckDoneResult(msg watchCheckDoneResultMsg) (tea.Model, tea.Cmd) {
	if m.launchPending {
		return m, scheduleCheckDone()
	}
	if !msg.allTerminal {
		return m, scheduleCheckDone()
	}

	switch {
	case m.mode.isInstanceBased():
		if m.retrying {
			return m, scheduleCheckDone()
		}
		return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
	case m.mode == watchModeSystem:
		// System mode: also check on-prem and unplaced
		if len(m.onPremHosts) > 0 || len(m.unplacedJobs) > 0 {
			return m, scheduleCheckDone()
		}
		// All empty — trigger final refresh and quit
		return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
	}
	return m, nil
}

func (m watchModel) checkAllDone() tea.Cmd {
	database := m.database
	instanceIDs := m.instanceIDs
	return func() tea.Msg {
		for _, id := range instanceIDs {
			ci, err := db.GetLaunch(database, id)
			if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
				return watchCheckDoneResultMsg{allTerminal: false}
			}
		}
		return watchCheckDoneResultMsg{allTerminal: true}
	}
}

// ---------------------------------------------------------------------------
// Update handlers: unplace
// ---------------------------------------------------------------------------

func (m watchModel) handleUnplaceDone(msg watchUnplaceDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.flash.Set(msg.err.Error(), true)
	}
	if m.mode == watchModeSystem {
		if msg.job != nil {
			m.removeOnPremJob(msg.job.ID)
			m.upsertUnplacedJob(msg.job)
			m.clampCursor()
		}
		flashCmd := m.flash.Set(msg.message, false)
		m.refreshing = true
		return m, tea.Batch(flashCmd, refreshWatchSystem(m.database, m.appConfig, m.cloudInstances))
	}
	if m.mode == watchModeProject {
		return m, tea.Batch(m.flash.Set(msg.message, false), m.reloadProjectGroups())
	}
	// Instance-based mode: just refresh unplaced
	return m, m.flash.Set(msg.message, false)
}

// ---------------------------------------------------------------------------
// Update handlers: generic action done (kill, terminate)
// ---------------------------------------------------------------------------

func (m watchModel) handleActionDone(verb, message string, err error) (tea.Model, tea.Cmd) {
	if err != nil {
		return m, m.flash.Set(fmt.Sprintf("%s failed: %v", verb, err), true)
	}
	return m, tea.Batch(
		m.flash.Set(message, false),
		refreshWatchOnPrem(m.database),
	)
}

// ---------------------------------------------------------------------------
// Update handlers: submit to instance
// ---------------------------------------------------------------------------

func (m watchModel) handleSubmitDone(msg watchSubmitDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.flash.Set(fmt.Sprintf("Submit failed: %v", msg.err), true)
	}
	flashCmd := m.flash.Set(fmt.Sprintf("Submitted job #%d to instance #%d", msg.jobID, msg.instanceID), false)
	m.removeUnplacedJob(msg.jobID)
	m.clampCursor()
	if m.mode == watchModeSystem {
		m.refreshing = true
		return m, tea.Batch(flashCmd, refreshWatchSystem(m.database, m.appConfig, m.cloudInstances))
	}
	return m, flashCmd
}

// ---------------------------------------------------------------------------
// Update handlers: move picker
// ---------------------------------------------------------------------------

func (m watchModel) handleMoveOptionsReady(msg moveOptionsReadyMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.flash.Set(fmt.Sprintf("Move: %v", msg.err), true)
	}
	m.movePicker = movePickerModel{
		active:  true,
		jobID:   msg.jobID,
		options: msg.options,
		cursor:  0,
	}
	return m, nil
}

func (m watchModel) handleMoveExecuteDone(msg moveExecuteDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.flash.Set(fmt.Sprintf("Move failed: %v", msg.err), true)
	}
	flashCmd := m.flash.Set(fmt.Sprintf("Moved job #%d to %s", msg.jobID, msg.targetDesc), false)
	if m.mode == watchModeSystem {
		m.refreshing = true
		return m, tea.Batch(flashCmd, refreshWatchSystem(m.database, m.appConfig, m.cloudInstances))
	}
	return m, flashCmd
}

// ---------------------------------------------------------------------------
// Update handlers: auto-pilot
// ---------------------------------------------------------------------------

func (m watchModel) handleAutoPlaceDone(msg autoPlaceDoneMsg) (tea.Model, tea.Cmd) {
	m.autoPlacing = false
	if msg.err != nil {
		return m, m.flash.Set(fmt.Sprintf("Auto-place failed: %v", msg.err), true)
	}
	if msg.jobID > 0 {
		flashCmd := m.flash.Set(fmt.Sprintf("Auto-placed job #%d → instance #%d", msg.jobID, msg.instanceID), false)
		m.removeUnplacedJob(msg.jobID)
		m.clampCursor()

		// Continue placing remaining jobs
		var cmds []tea.Cmd
		cmds = append(cmds, flashCmd)
		if m.autoMode && len(m.unplacedJobs) > 0 {
			m.autoPlacing = true
			cmds = append(cmds, m.autoPlaceUnplacedJobs())
		}
		if m.mode == watchModeSystem {
			m.refreshing = true
			cmds = append(cmds, refreshWatchSystem(m.database, m.appConfig, m.cloudInstances))
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m watchModel) handleAutoLaunchDone(msg autoLaunchDoneMsg) (tea.Model, tea.Cmd) {
	m.autoLaunching = false
	if msg.err != nil {
		return m, m.flash.Set(fmt.Sprintf("Auto-launch failed: %v", msg.err), true)
	}
	if len(msg.instanceIDs) > 0 {
		// Add new instance IDs and start watching them
		var cmds []tea.Cmd
		for _, id := range msg.instanceIDs {
			if !slices.Contains(m.instanceIDs, id) {
				m.instanceIDs = append(m.instanceIDs, id)
				if cmd := m.startWatchingInstance(id); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
		m.rebuildReplacementCache()
		flashMsg := fmt.Sprintf("Auto-launched %d instance(s)", len(msg.instanceIDs))
		if msg.skipped > 0 {
			flashMsg += fmt.Sprintf(" (%d skipped)", msg.skipped)
		}
		cmds = append(cmds, m.flash.Set(flashMsg, false))
		return m, tea.Batch(cmds...)
	}
	if msg.skipped > 0 {
		return m, m.flash.Set(fmt.Sprintf("Auto-launch: %d job(s) exceeded max attempts", msg.skipped), true)
	}
	return m, nil
}

func countRunningJobs(jobs []*db.Job) int {
	n := 0
	for _, j := range jobs {
		if j != nil && (j.Status == db.StatusRunning || j.Status == db.StatusStarting) {
			n++
		}
	}
	return n
}

func (m watchModel) buildInstanceCapacities() []campaign.InstanceCapacity {
	var result []campaign.InstanceCapacity

	switch m.mode {
	case watchModeCampaign, watchModeInstances:
		for _, id := range m.instanceIDs {
			u, ok := m.updates[id]
			if !ok || u.Launch == nil {
				continue
			}
			if cap, ok := campaign.NewInstanceCapacity(u.Launch, countRunningJobs(u.Jobs)); ok {
				result = append(result, cap)
			}
		}
	case watchModeSystem:
		for _, ci := range m.cloudInstances {
			var jobs []*db.Job
			if u, ok := m.updates[ci.ID]; ok {
				jobs = u.Jobs
			}
			if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(jobs)); ok {
				result = append(result, cap)
			}
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// Update handlers: instance-based modes
// ---------------------------------------------------------------------------

func (m watchModel) handleInstanceSyncTick() (tea.Model, tea.Cmd) {
	m.requestOnPremSyncs()
	return m, tea.Batch(
		func() tea.Msg {
			if _, err := db.ResetJobsOnTerminalLaunches(m.database); err != nil {
				// log suppressed in TUI mode
			}
			if _, err := campaign.ReconcileCampaigns(m.database); err != nil {
				// log suppressed in TUI mode
			}
			campaign.MaybeSweepOrphanedInstances(m.database, m.cloudClients)
			return watchSyncDoneMsg{}
		},
		refreshWatchOnPrem(m.database),
		scheduleSyncTick(),
	)
}

func (m watchModel) handleInstanceSyncDone() (tea.Model, tea.Cmd) {
	terminalIDs := make([]int64, 0)
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
			terminalIDs = append(terminalIDs, id)
		}
	}
	if len(terminalIDs) == 0 {
		return m, nil
	}
	return m, refreshWatchInstancesFromDB(m.database, terminalIDs, false)
}

func (m watchModel) handleJobsRefreshed(msg watchJobsRefreshedMsg) (tea.Model, tea.Cmd) {
	m.ensurePreservedJobAttachmentMap()
	// Track instances that transitioned to retryable-failed via reconciliation,
	// so we can trigger auto-relaunch (handleWatchUpdate handles the watch-channel path).
	var newlyFailed *db.Launch
	for id, ci := range msg.cloudInstances {
		u := m.updates[id]
		if m.autoMode && !m.retrying {
			prev := u.Launch
			wasTerminal := prev != nil && campaign.IsInstanceTerminal(prev.Status)
			if !wasTerminal && db.IsRetryableTermination(ci) {
				newlyFailed = ci
			}
		}
		u.Launch = ci
		m.updates[id] = u
	}
	for id, jobs := range msg.jobs {
		u := m.updates[id]
		if u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
			var usedPreservedAttachment bool
			u.Jobs, usedPreservedAttachment = preserveWatchCurrentJobs(u.Launch.ID, u.Jobs, jobs)
			if usedPreservedAttachment {
				m.preservedJobAttachment[id] = true
			} else {
				delete(m.preservedJobAttachment, id)
			}
		} else {
			u.Jobs = jobs
			delete(m.preservedJobAttachment, id)
		}
		if outcomes, ok := msg.outcomes[id]; ok {
			u.JobAttemptOutcomes = outcomes
		}
		m.updates[id] = u
	}
	if msg.quitAfter {
		m.done = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return tea.QuitMsg{}
		})
	}

	// Auto-relaunch if reconciliation detected a retryable failure
	if newlyFailed != nil && m.database != nil {
		m.logRetryAutoTriggered(newlyFailed)
		m.retrying = true
		m.retryResult = ""
		return m, m.retryFailedInstances(0)
	}

	return m, nil
}

func (m watchModel) handleRetryResult(msg retryResultMsg) (tea.Model, tea.Cmd) {
	m.retrying = false
	if msg.err != nil {
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:  db.EventRetryError,
			CampaignID: m.campaignID,
			ErrorText:  msg.err.Error(),
		})
		m.retryResult = fmt.Sprintf("Retry failed: %v", msg.err)
		return m, m.checkAllDone()
	}
	if len(msg.instanceIDs) == 0 {
		if msg.skipped > 0 {
			_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
				EventKind:  db.EventRetryMaxAttempts,
				CampaignID: m.campaignID,
				JobCount:   msg.skipped,
			})
			m.retryExtraAttempts = 0
			m.retryResult = fmt.Sprintf("Retry: %d job(s) exceeded max cloud attempts, giving up", msg.skipped)
			return m, m.checkAllDone()
		}
		if m.retryAttempt < len(retryBackoffDelays) {
			delay := retryBackoffDelays[m.retryAttempt]
			m.retryAttempt++
			_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
				EventKind:     db.EventRetryNoOffers,
				CampaignID:    m.campaignID,
				AttemptNumber: m.retryAttempt,
				MaxAttempts:   len(retryBackoffDelays) + 1,
				Detail:        fmt.Sprintf("backoff %s", delay),
			})
			m.retryResult = fmt.Sprintf("Retry: no offers available, retrying in %s (attempt %d/%d)",
				delay, m.retryAttempt+1, len(retryBackoffDelays)+1)
			return m, tea.Tick(delay, func(time.Time) tea.Msg { return retryBackoffMsg{} })
		}
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:     db.EventRetryExhausted,
			CampaignID:    m.campaignID,
			AttemptNumber: len(retryBackoffDelays) + 1,
		})
		m.retryExtraAttempts = 0
		m.retryResult = fmt.Sprintf("Retry: no instances launched after %d attempts (no offers available)", len(retryBackoffDelays)+1)
		return m, m.checkAllDone()
	}

	// Success
	_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
		EventKind:  db.EventRetrySuccess,
		CampaignID: m.campaignID,
		JobCount:   len(msg.instanceIDs),
		Detail:     fmt.Sprintf("launched instances %v", msg.instanceIDs),
	})
	m.retryAttempt = 0
	m.retryExtraAttempts = 0

	var cmds []tea.Cmd
	for _, id := range msg.instanceIDs {
		m.instanceIDs = append(m.instanceIDs, id)
		// Pre-fetch initial info for display before first channel update
		if m.initInfo != nil {
			ci, _ := db.GetLaunch(m.database, id)
			jobs, _ := db.GetLaunchJobsIncludingAttempts(m.database, id)
			outcomes, _ := db.GetAttemptOutcomesByLaunch(m.database, id)
			m.initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
		}
		if cmd := m.startWatchingInstance(id); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	m.partialErrorJobs = nil
	m.partialErrorsRetried = true
	m.rebuildReplacementCache()
	m.scrollOff = 0
	return m, tea.Batch(cmds...)
}

// ---------------------------------------------------------------------------
// Update handlers: system-specific
// ---------------------------------------------------------------------------

func (m watchModel) handleSystemTick() (tea.Model, tea.Cmd) {
	if m.refreshing {
		return m, scheduleWatchAllTick()
	}
	m.refreshing = true
	return m, tea.Batch(
		refreshWatchSystem(m.database, m.appConfig, m.cloudInstances),
		scheduleWatchAllTick(),
	)
}

func (m watchModel) handleSystemRefreshed(msg watchAllRefreshedMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.err = nil
	snapshot := msg.snapshot
	m.cloudInstances = snapshot.Launches
	m.onPremHosts = msg.snapshot.OnPremHosts
	m.unplacedJobs = msg.snapshot.UnplacedJobs
	m.cloudReason = snapshot.CloudReason

	// Update instanceIDs from discovered instances
	m.instanceIDs = make([]int64, len(snapshot.Launches))
	for i, ci := range snapshot.Launches {
		m.instanceIDs[i] = ci.ID
	}

	cmds := m.mergeSnapshot(snapshot)
	m.rebuildReplacementCache()

	for _, host := range m.onPremHosts {
		m.syncWorker.Request(hostsync.Request{
			Host: host.Name,
			Rate: hostsync.GetHostSyncRate(host.Jobs),
		})
	}
	m.clampCursor()

	// Trigger auto-pilot if enabled
	if m.autoMode {
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

// ---------------------------------------------------------------------------
// Update handlers: project-mode
// ---------------------------------------------------------------------------

func (m watchModel) handleProjectLoaded(msg watchProjectLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.projectStatus = fmt.Sprintf("Refresh error: %v", msg.err)
		return m, nil
	}
	m.projectGroups = msg.groups
	m.projectLines, m.projectMeta = m.computeProjectLines()
	if len(m.projectGroups) == 0 && m.projectStatus == "" {
		m.projectStatus = "No project activity."
	}
	m.clampCursor()
	m.adjustProjectOffset()
	return m, nil
}

func (m watchModel) handleProjectSyncFinished(msg watchProjectSyncFinishedMsg) (tea.Model, tea.Cmd) {
	if !msg.full {
		if len(msg.warnings) > 0 {
			m.projectStatus = strings.Join(msg.warnings, " | ")
		} else {
			m.projectStatus = "Running full sync..."
		}
		return m, tea.Batch(m.reloadProjectGroups(), m.runProjectBackgroundSync(true))
	}

	m.projectSyncing = false
	if len(msg.warnings) > 0 {
		m.projectStatus = strings.Join(msg.warnings, " | ")
	} else {
		m.projectStatus = "Synced."
	}
	return m, m.reloadProjectGroups()
}

func (m watchModel) handleProjectSyncWorkerResult(msg watchProjectSyncResultMsg) (tea.Model, tea.Cmd) {
	m.projectSyncing = false
	cmds := []tea.Cmd{
		m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
			return watchProjectSyncResultMsg{result: r}
		}),
	}
	if msg.result.Error != nil {
		m.projectStatus = fmt.Sprintf("Sync error (%s): %v", msg.result.Host, msg.result.Error)
	} else if msg.result.Updated > 0 {
		m.projectStatus = ""
		cmds = append(cmds, m.reloadProjectGroups())
	} else if m.projectStatus == "Refreshing..." {
		m.projectStatus = ""
	}
	return m, tea.Batch(cmds...)
}

func (m watchModel) handleDBWatcherReady(msg watchDBWatcherReadyMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if m.mode == watchModeProject {
			m.projectStatus = fmt.Sprintf("DB watch error: %v", msg.err)
		}
		return m, nil
	}
	m.dbWatcher = msg.watcher
	m.dbWatcherTargets = msg.targets
	return m, m.waitForDBEvent()
}

func (m watchModel) handleDBWatchEvent(msg watchDBWatchEventMsg, triggerMsg tea.Msg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if m.mode == watchModeProject {
			m.projectStatus = fmt.Sprintf("DB watch error: %v", msg.err)
		}
		return m, nil
	}
	var cmds []tea.Cmd
	if !m.debounceActive {
		m.debounceActive = true
		cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
			return triggerMsg
		}))
	}
	if cmd := m.waitForDBEvent(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m watchModel) handleProjectSyncTick() (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{scheduleProjectSyncTick()}
	if m.syncWorker != nil {
		m.requestProjectActiveSyncs()
		cmds = append(cmds, m.reloadProjectGroups())
	} else if !m.projectSyncing {
		m.projectSyncing = true
		m.projectStatus = "Refreshing..."
		cmds = append(cmds, m.runProjectBackgroundSync(true))
	}
	return m, tea.Batch(cmds...)
}

func (m *watchModel) mergeSnapshot(snapshot watchSystemSnapshot) []tea.Cmd {
	m.ensurePreservedJobAttachmentMap()
	active := make(map[int64]bool, len(snapshot.Launches))
	var cmds []tea.Cmd

	for _, ci := range snapshot.Launches {
		active[ci.ID] = true
		snapUpdate := snapshot.InstanceUpdates[ci.ID]
		if existing, ok := m.updates[ci.ID]; ok {
			existing.Launch = snapUpdate.Launch
			// Don't overwrite jobs from an active streaming channel with
			// potentially stale snapshot data — the stream is authoritative.
			if _, streaming := m.channels[ci.ID]; !streaming {
				existing.Jobs = snapUpdate.Jobs
				existing.JobAttemptOutcomes = snapUpdate.JobAttemptOutcomes
			}
			m.updates[ci.ID] = existing
		} else {
			m.updates[ci.ID] = snapUpdate
		}
		if cmd := m.startWatchingInstance(ci.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	for id := range m.updates {
		if !active[id] {
			delete(m.updates, id)
			delete(m.preservedJobAttachment, id)
		}
	}

	return cmds
}

func (m *watchModel) ensurePreservedJobAttachmentMap() {
	if m.preservedJobAttachment == nil {
		m.preservedJobAttachment = map[int64]bool{}
	}
}
