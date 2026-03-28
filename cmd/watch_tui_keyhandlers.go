package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// ---------------------------------------------------------------------------
// Key handling
// ---------------------------------------------------------------------------

func (m watchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.cancel()
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
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
	case "u":
		job := m.selectedUnplacedJob()
		if job == nil {
			// Try on-prem job (system mode)
			if m.mode == watchModeSystem {
				job = m.selectedOnPremJob()
			}
		}
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			return m, nil
		}
		if job.HasInventoryHost() || job.Host == "" {
			return m, requestWatchJobUnplace(m.database, job.ID)
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
		if m.mode == watchModeSystem {
			m.cancel()
			if m.syncWorker != nil {
				m.syncWorker.Stop()
			}
			return m, func() tea.Msg { return switchToLaunchMsg{} }
		}
	}
	return m, nil
}
