package cmd

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/tui"
)

// ---------------------------------------------------------------------------
// Update handlers: watch updates
// ---------------------------------------------------------------------------

func (m watchModel) handleWatchUpdate(msg watchUpdateMsg) (tea.Model, tea.Cmd) {
	if msg.closed {
		clearWatchJobProgressHWM(m.jobProgressHWM, m.updates[msg.instanceID])
		if m.mode == watchModeSystem {
			delete(m.channels, msg.instanceID)
			delete(m.clients, msg.instanceID)
		}
		return m, m.checkAllDone()
	}

	prev := m.updates[msg.instanceID]
	if msg.update.Launch != nil && campaign.IsInstanceTerminal(msg.update.Launch.Status) {
		msg.update.Jobs = preserveWatchCurrentJobs(msg.update.Launch.ID, prev.Jobs, msg.update.Jobs)
	}
	updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
	m.updates[msg.instanceID] = msg.update

	// Auto-relaunch on retryable infrastructure failure (instance-based modes)
	ci := msg.update.Launch
	ch := m.channels[msg.instanceID]
	if ci != nil && db.IsRetryableTermination(ci) && !m.retrying && m.database != nil {
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:  db.EventRetryAutoTriggered,
			LaunchID:   ci.ID,
			CampaignID: m.campaignID,
			GPUSpec:    ci.GPUSpec,
			Detail:     ci.TerminationReason,
		})
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

func (m watchModel) handleCheckDone() (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	return m, m.checkAllDone()
}

func (m watchModel) handleCheckDoneResult(msg watchCheckDoneResultMsg) (tea.Model, tea.Cmd) {
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
		return m, m.flash.set(msg.err.Error(), true)
	}
	if m.mode == watchModeSystem {
		if msg.job != nil {
			m.removeOnPremJob(msg.job.ID)
			m.upsertUnplacedJob(msg.job)
			m.clampCursor()
		}
		flashCmd := m.flash.set(msg.message, false)
		m.refreshing = true
		return m, tea.Batch(flashCmd, refreshWatchSystem(m.database, m.appConfig))
	}
	// Instance-based mode: just refresh unplaced
	return m, m.flash.set(msg.message, false)
}

// ---------------------------------------------------------------------------
// Update handlers: submit to instance
// ---------------------------------------------------------------------------

func (m watchModel) handleSubmitDone(msg watchSubmitDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.flash.set(fmt.Sprintf("Submit failed: %v", msg.err), true)
	}
	flashCmd := m.flash.set(fmt.Sprintf("Submitted job #%d to instance #%d", msg.jobID, msg.instanceID), false)
	m.removeUnplacedJob(msg.jobID)
	m.clampCursor()
	if m.mode == watchModeSystem {
		m.refreshing = true
		return m, tea.Batch(flashCmd, refreshWatchSystem(m.database, m.appConfig))
	}
	return m, flashCmd
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
	case watchModeCampaign:
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
	for id, ci := range msg.cloudInstances {
		u := m.updates[id]
		u.Launch = ci
		m.updates[id] = u
	}
	for id, jobs := range msg.jobs {
		u := m.updates[id]
		if u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
			u.Jobs = preserveWatchCurrentJobs(u.Launch.ID, u.Jobs, jobs)
		} else {
			u.Jobs = jobs
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
		refreshWatchSystem(m.database, m.appConfig),
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
	m.cloudInstances = msg.snapshot.Launches
	m.onPremHosts = msg.snapshot.OnPremHosts
	m.unplacedJobs = msg.snapshot.UnplacedJobs

	// Update instanceIDs from discovered instances
	m.instanceIDs = make([]int64, len(msg.snapshot.Launches))
	for i, ci := range msg.snapshot.Launches {
		m.instanceIDs[i] = ci.ID
	}

	cmds := m.mergeSnapshot(msg.snapshot)
	m.rebuildReplacementCache()

	for _, host := range m.onPremHosts {
		m.syncWorker.Request(tui.SyncRequest{
			Host: host.Name,
			Rate: tui.GetHostSyncRate(host.Jobs),
		})
	}
	m.clampCursor()
	return m, tea.Batch(cmds...)
}

func (m *watchModel) mergeSnapshot(snapshot watchSystemSnapshot) []tea.Cmd {
	active := make(map[int64]bool, len(snapshot.Launches))
	var cmds []tea.Cmd

	for _, ci := range snapshot.Launches {
		active[ci.ID] = true
		snapUpdate := snapshot.InstanceUpdates[ci.ID]
		if existing, ok := m.updates[ci.ID]; ok {
			existing.Launch = snapUpdate.Launch
			existing.Jobs = snapUpdate.Jobs
			existing.JobAttemptOutcomes = snapUpdate.JobAttemptOutcomes
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
		}
	}

	return cmds
}
