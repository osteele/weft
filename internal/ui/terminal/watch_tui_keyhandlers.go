package terminal

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// ---------------------------------------------------------------------------
// Key handling
// ---------------------------------------------------------------------------

func (m watchModel) handleToggleAutoPilot() (tea.Model, tea.Cmd) {
	m.autoMode = !m.autoMode
	if m.autoMode {
		cmds := []tea.Cmd{m.flash.Set("Auto-pilot ON", false)}
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if !m.retrying && m.hasRetryableFailures() {
			m.retryAttempt = 0
			m.retrying = true
			m.retryResult = ""
			cmds = append(cmds, m.retryFailedInstances(0))
		}
		return m, tea.Batch(cmds...)
	}
	return m, m.flash.Set("Auto-pilot OFF", false)
}

func (m watchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Delegate to move picker overlay when active
	if m.movePicker.active {
		return m.handleMovePickerKey(msg)
	}
	if m.projectHelp {
		switch msg.String() {
		case "?", "esc", "q", "enter":
			m.projectHelp = false
			return m, nil
		}
		return m, nil
	}

	if m.mode == watchModeProject {
		return m.handleProjectKey(msg)
	}

	switch msg.String() {
	case "ctrl+c", "q":
		if m.dbWatcher != nil {
			_ = m.dbWatcher.Close()
		}
		m.cancel()
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "pgup", "ctrl+u":
		m.moveCursor(-m.pageSize())
		return m, nil
	case "pgdown", "ctrl+d":
		m.moveCursor(m.pageSize())
		return m, nil
	case "home", "g":
		m.cursor = 0
		return m, nil
	case "end", "G":
		count := m.selectableRowCount()
		if count > 0 {
			m.cursor = count - 1
		}
		return m, nil
	case "r":
		if !m.retrying && m.hasRetryableFailures() {
			if m.database != nil {
				_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
					EventKind:  db.EventRetryManualTriggered,
					CampaignID: m.campaignID,
				})
			}
			m.retryAttempt = 0
			m.retrying = true
			m.retryResult = ""
			m.retryExtraAttempts = campaign.DefaultMaxCloudAttempts
			return m, m.retryFailedInstances(m.retryExtraAttempts)
		}
	case "B":
		if m.retrying {
			return m, nil
		}
		instID := m.selectedCloudInstanceID()
		if instID == 0 {
			return m, m.flash.Set("Select a failed instance header row first", true)
		}
		if !m.budgetBlockedFailed[instID] {
			return m, m.flash.Set("Selected instance was not blocked by retry budget", true)
		}
		scale := m.retryBudgetMultiplier[instID]
		if scale <= 0 {
			scale = 1
		}
		scale *= 2
		m.retryBudgetMultiplier[instID] = scale
		m.retryAttempt = 0
		m.retrying = true
		m.retryResult = fmt.Sprintf("Retry: doubled budget for failed instance %d (x%.1f)", instID, scale)
		return m, m.retryFailedInstances(0)
	case "u":
		// Try cloud job row first
		if job := m.selectedCloudJob(); job != nil && job.EffectiveStatus() == db.StatusQueued {
			return m, requestWatchJobUnplace(m.database, job.ID)
		}
		job := m.selectedUnplacedJob()
		if job == nil {
			job = m.selectedOnPremJob()
		}
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			return m, nil
		}
		return m, requestWatchJobUnplace(m.database, job.ID)
	case "x":
		var job *db.Job
		if j := m.selectedCloudJob(); j != nil && j.EffectiveStatus() == db.StatusRunning {
			job = j
		} else if j := m.selectedOnPremJob(); j != nil && j.EffectiveStatus() == db.StatusRunning {
			job = j
		}
		if job == nil {
			if j := m.selectedUnplacedJob(); j != nil && j.EffectiveStatus() == db.StatusQueued {
				job = j
			}
		}
		if job != nil {
			flashCmd := m.flash.Set(m.spinner.View()+fmt.Sprintf(" Killing job #%d...", job.ID), false)
			return m, tea.Batch(flashCmd, requestWatchJobKill(m.database, job.ID))
		}
	case "t":
		// Terminate a cloud instance
		if instID := m.selectedCloudInstanceID(); instID != 0 {
			flashCmd := m.flash.Set(m.spinner.View()+fmt.Sprintf(" Terminating instance #%d...", instID), false)
			return m, tea.Batch(flashCmd, requestWatchInstanceTerminate(m.database, instID))
		}
	case "s":
		job := m.selectedUnplacedJob()
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			return m, nil
		}
		capacities := m.buildInstanceCapacities()
		if len(capacities) == 0 {
			return m, m.flash.Set("No active instances available", true)
		}
		ranked := campaign.RankForJob(job, capacities)
		if len(ranked) == 0 {
			_, reason := campaign.MatchJobToInstance(job, capacities[0])
			return m, m.flash.Set(fmt.Sprintf("No compatible instance for job #%d (%s)", job.ID, reason), true)
		}
		best := ranked[0]
		flashCmd := m.flash.Set(m.spinner.View()+fmt.Sprintf(" Submitting job #%d to instance #%d...", job.ID, best.Instance.ID), false)
		return m, tea.Batch(flashCmd, requestWatchJobSubmit(m.ctx, m.database, m.r2Client, job.ID, best.Instance.ID))
	case "l":
		if len(m.unplacedJobs) == 0 {
			return m, m.flash.Set("No unplaced jobs to launch", true)
		}
		return m, func() tea.Msg { return switchToLaunchMsg{} }
	case "m":
		job := m.selectedCloudJob()
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			return m, nil
		}
		sourceInstanceID := int64(0)
		if job.LaunchID != nil {
			sourceInstanceID = *job.LaunchID
		}
		capacities := m.buildInstanceCapacities()
		// Snapshot queued-job counts on the main goroutine to avoid a data
		// race — the updates map must not be read from a background goroutine.
		queuedCounts := make(map[int64]int)
		for id, u := range m.updates {
			for _, j := range u.Jobs {
				if j != nil && j.EffectiveStatus() == db.StatusQueued {
					queuedCounts[id]++
				}
			}
		}
		flashCmd := m.flash.Set(m.spinner.View()+" Searching for destinations...", false)
		return m, tea.Batch(flashCmd, requestMoveOptions(
			m.database, m.appConfig, m.cloudClients,
			job, capacities, queuedCounts, sourceInstanceID,
		))
	case "a":
		return m.handleToggleAutoPilot()
	case "?":
		m.projectHelp = true
		return m, nil
	}
	return m, nil
}

// handleMovePickerKey handles keys when the move picker overlay is active.
func (m watchModel) handleMovePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.movePicker.reset()
		return m, nil
	case "up", "k":
		m.movePicker.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.movePicker.moveCursor(1)
		return m, nil
	case "enter":
		opt := m.movePicker.selectedOption()
		if opt == nil {
			return m, nil
		}
		jobID := m.movePicker.jobID
		selected := *opt
		m.movePicker.reset()
		targetDesc := fmt.Sprintf("instance #%d", selected.instanceID)
		if selected.isNew {
			targetDesc = fmt.Sprintf("new %s instance", selected.gpuName)
		}
		flashCmd := m.flash.Set(m.spinner.View()+fmt.Sprintf(" Moving job #%d to %s...", jobID, targetDesc), false)
		return m, tea.Batch(flashCmd, requestMoveExecute(
			m.ctx, m.database, m.r2Client, m.appConfig, m.cloudClients,
			jobID, selected,
		))
	}
	return m, nil
}

func (m watchModel) handleProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.projectHelp {
		switch msg.String() {
		case "?", "esc", "q", "enter":
			m.projectHelp = false
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c", "q", "esc":
		if m.dbWatcher != nil {
			_ = m.dbWatcher.Close()
		}
		m.cancel()
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
		m.adjustProjectOffset()
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		m.adjustProjectOffset()
		return m, nil
	case "g", "home":
		m.cursor = 0
		m.adjustProjectOffset()
		return m, nil
	case "G", "end":
		if lines := m.projectLines; len(lines) > 0 {
			m.cursor = len(lines) - 1
		}
		m.adjustProjectOffset()
		return m, nil
	case "pgdown", "space":
		m.moveCursor(m.projectPageSize())
		m.adjustProjectOffset()
		return m, nil
	case "pgup", "b":
		m.moveCursor(-m.projectPageSize())
		m.adjustProjectOffset()
		return m, nil
	case "u":
		job, bucket := m.selectedProjectJob()
		if job == nil {
			return m, m.flash.Set("Select a queued job row to unplace", true)
		}
		if bucket != "queued" {
			if bucket == "unplaced" {
				return m, m.flash.Set(fmt.Sprintf("Job #%d is already unplaced", job.ID), true)
			}
			return m, m.flash.Set("Only queued job rows can be unplaced", true)
		}
		if job.EffectiveStatus() != db.StatusQueued {
			return m, m.flash.Set("Only queued jobs can be unplaced", true)
		}
		flashCmd := m.flash.Set(m.spinner.View()+fmt.Sprintf(" Unplacing job #%d...", job.ID), false)
		return m, tea.Batch(flashCmd, requestWatchJobUnplace(m.database, job.ID))
	case "r":
		if m.projectSyncing {
			return m, nil
		}
		m.projectSyncing = true
		m.projectStatus = "Refreshing..."
		if m.syncWorker != nil {
			m.requestProjectActiveSyncs()
			return m, m.reloadProjectGroups()
		}
		return m, m.runProjectBackgroundSync(false)
	case "l":
		// Check if there are any unplaced jobs across all projects
		hasUnplaced := false
		for _, g := range m.projectGroups {
			if len(g.Unplaced) > 0 {
				hasUnplaced = true
				break
			}
		}
		if !hasUnplaced {
			return m, m.flash.Set("No unplaced jobs to launch", true)
		}
		return m, func() tea.Msg { return switchToLaunchMsg{} }
	case "a":
		return m.handleToggleAutoPilot()
	case "?":
		m.projectHelp = true
		return m, nil
	}
	return m, nil
}
