package terminal

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

type listKeyBinding struct {
	keys    string
	aliases []string
	action  string
	handler func(listTUIModel) (tea.Model, tea.Cmd)
}

func (b listKeyBinding) matches(key string) bool {
	if key == b.keys {
		return true
	}
	for _, alias := range b.aliases {
		if key == alias {
			return true
		}
	}
	return false
}

func (b listKeyBinding) helpLine() string {
	return "  " + b.keys + " " + b.action
}

func (b listKeyBinding) footerToken() string {
	return b.keys + ":" + b.action
}

func handleListKeyBinding(m listTUIModel, key string, bindings []listKeyBinding) (tea.Model, tea.Cmd, bool) {
	for _, binding := range bindings {
		if binding.matches(key) {
			if binding.handler == nil {
				return m, nil, true
			}
			next, cmd := binding.handler(m)
			if nextList, ok := next.(listTUIModel); ok {
				nextList.clampCursor()
				nextList.adjustOffset()
				next = nextList
			}
			return next, cmd, true
		}
	}
	return m, nil, false
}

func listCommonKeyBindings(grouped bool) []listKeyBinding {
	upAliases := []string{"k"}
	if grouped {
		upAliases = nil
	}
	bindings := []listKeyBinding{
		{keys: "q", aliases: []string{"ctrl+c", "esc"}, action: "quit", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if grouped && m.moveLookupPending {
				m.moveLookupPending = false
				m.moveLookupRequestID = 0
				m.statusMessage = "Move lookup canceled"
				return m, nil
			}
			m.shutdown()
			return m, tea.Quit
		}},
		{keys: "ctrl+z", action: "suspend", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			return m, tea.Suspend
		}},
		{keys: "up", aliases: upAliases, action: "move up", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.isGroupedView() {
				if m.cursor > 0 {
					m.cursor--
				}
			} else if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		}},
		{keys: "down", aliases: []string{"j"}, action: "move down", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.isGroupedView() {
				if m.cursor < len(m.groupedSelectableRows)-1 {
					m.cursor++
				}
			} else if m.cursor < len(m.jobs)-1 {
				m.cursor++
			}
			return m, nil
		}},
		{keys: "g", aliases: []string{"home"}, action: "top", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.cursor = 0
			return m, nil
		}},
		{keys: "G", aliases: []string{"end"}, action: "bottom", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.isGroupedView() {
				if len(m.groupedSelectableRows) > 0 {
					m.cursor = len(m.groupedSelectableRows) - 1
				}
			} else if len(m.jobs) > 0 {
				m.cursor = len(m.jobs) - 1
			}
			return m, nil
		}},
		{keys: "pgdown", aliases: []string{"space"}, action: "page down", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.cursor += m.pageSize()
			if m.isGroupedView() {
				if m.cursor >= len(m.groupedSelectableRows) {
					m.cursor = max(0, len(m.groupedSelectableRows)-1)
				}
			} else if m.cursor >= len(m.jobs) {
				m.cursor = max(0, len(m.jobs)-1)
			}
			return m, nil
		}},
		{keys: "pgup", aliases: []string{"b"}, action: "page up", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.cursor -= m.pageSize()
			if m.cursor < 0 {
				m.cursor = 0
			}
			return m, nil
		}},
	}
	return bindings
}

var (
	listKeyAttempts          = listKeyBinding{keys: "a", action: "attempts"}
	listKeyAIAssist          = listKeyBinding{keys: "c", action: "coding-assistant"}
	listKeyToggleProcessed   = listKeyBinding{keys: "p", action: "toggle processed"}
	listKeyKillCancel        = listKeyBinding{keys: "x", aliases: []string{"k"}, action: "kill/cancel"}
	listKeyUnplace           = listKeyBinding{keys: "u", action: "unplace"}
	listKeyPriority          = listKeyBinding{keys: "P", action: "priority"}
	listKeyMove              = listKeyBinding{keys: "m", action: "move"}
	listKeyLaunchSelected    = listKeyBinding{keys: "N", action: "new for selected"}
	listKeyToggleQueuedDraft = listKeyBinding{keys: "d", action: "draft/queued"}
	listKeyToggleUnprocessed = listKeyBinding{keys: "U", action: "unprocessed"}
	listKeyGroupedView       = listKeyBinding{keys: "v", action: "group"}
	listKeyListView          = listKeyBinding{keys: "v", action: "group"}
	listKeyInstances         = listKeyBinding{keys: "i", action: "instances"}
	listKeyRefresh           = listKeyBinding{keys: "r", action: "refresh"}
	listKeyProjectFilter     = listKeyBinding{keys: "/", action: "project filter"}
	listKeyToggleStatusArea  = listKeyBinding{keys: "S", action: "status area"}
	listKeyGroupedAuto       = listKeyBinding{keys: "A", action: "auto"}
	listKeyLaunchQueued      = listKeyBinding{keys: "n", action: "new instance"}
	listKeyRebalance         = listKeyBinding{keys: "R", action: "rebalance"}
	listKeyAutoErrorDetails  = listKeyBinding{keys: "e", action: "error details"}
	listKeyAutoHideError     = listKeyBinding{keys: "e", action: "hide error"}
)

func listFlatKeyBindings() []listKeyBinding {
	bindings := listCommonKeyBindings(false)
	bindings = append(bindings,
		listKeyBinding{keys: listKeyRefresh.keys, action: listKeyRefresh.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			next, cmd := m.triggerManualRefresh()
			return next, cmd
		}},
		listKeyBinding{keys: listKeyProjectFilter.keys, action: listKeyProjectFilter.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			return m.beginProjectFilterInput()
		}},
		listKeyBinding{keys: listKeyToggleStatusArea.keys, action: listKeyToggleStatusArea.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.hideStatusArea = !m.hideStatusArea
			if m.hideStatusArea {
				m.statusMessage = "Status area hidden"
			} else {
				m.statusMessage = "Status area shown"
			}
			m.rebuildLayout()
			m.clampCursor()
			m.adjustOffset()
			return m, nil
		}},
		listKeyBinding{keys: listKeyToggleUnprocessed.keys, action: listKeyToggleUnprocessed.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.unprocessedView = !m.unprocessedView
			m.groupedUnprocessedView = m.unprocessedView
			if m.unprocessedView {
				m.statusMessage = "Showing unprocessed jobs"
			} else {
				m.statusMessage = "Showing all jobs"
			}
			return m, m.reloadJobs()
		}},
		listKeyBinding{keys: listKeyToggleQueuedDraft.keys, action: listKeyToggleQueuedDraft.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.statusView == db.StatusDraft {
				m.statusView = db.StatusQueued
				m.statusMessage = "Showing queued jobs"
			} else {
				m.statusView = db.StatusDraft
				m.statusMessage = "Showing draft jobs"
			}
			return m, m.reloadJobs()
		}},
		listKeyBinding{keys: listKeyKillCancel.keys, aliases: listKeyKillCancel.aliases, action: listKeyKillCancel.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.currentSelectedJob()
			if !listJobCanKillOrCancel(job) {
				m.statusMessage = "Select a queued, draft, or running job to cancel/kill"
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			if job.EffectiveStatus() == db.StatusQueued || job.EffectiveStatus() == db.StatusDraft {
				m.statusMessage = fmt.Sprintf("Canceling job #%d...", job.ID)
			} else {
				m.statusMessage = fmt.Sprintf("Killing job #%d...", job.ID)
			}
			return m, requestWatchJobKill(m.database, job.ID)
		}},
		listKeyBinding{keys: listKeyToggleProcessed.keys, action: listKeyToggleProcessed.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.currentSelectedJob()
			if job == nil {
				m.statusMessage = "Select a job row to toggle processed"
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			if job.HasTag(db.ProcessedTag) {
				m.statusMessage = fmt.Sprintf("Marking job #%d as unprocessed...", job.ID)
			} else {
				m.statusMessage = fmt.Sprintf("Marking job #%d as processed...", job.ID)
			}
			return m, requestWatchJobToggleProcessed(m.database, job.ID)
		}},
		listKeyBinding{keys: listKeyGroupedView.keys, action: listKeyGroupedView.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.moveLookupPending {
				m.moveLookupPending = false
				m.moveLookupRequestID = 0
			}
			nextMode := m.nextGroupMode()
			m.setGroupMode(nextMode)
			m.statusMessage = "Grouped by " + listGroupModeLabel(nextMode)
			return m, nil
		}},
		listKeyBinding{keys: listKeyInstances.keys, action: listKeyInstances.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			return m, func() tea.Msg { return switchToSystemWatchMsg{} }
		}},
		listKeyBinding{keys: "?", action: "help", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.showHelp = true
			return m, nil
		}},
		listKeyBinding{keys: listKeyAttempts.keys, action: listKeyAttempts.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.currentSelectedJob()
			if job == nil {
				m.statusMessage = "Select a job row to view attempts"
				return m, nil
			}
			return m, func() tea.Msg { return switchToAttemptsMsg{jobID: job.ID} }
		}},
		listKeyBinding{keys: listKeyAIAssist.keys, action: listKeyAIAssist.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.currentSelectedJob()
			if job == nil {
				m.statusMessage = "Select a job row for coding-assistant"
				return m, nil
			}
			return m, m.beginAIAssist(job)
		}},
	)
	return bindings
}

func listGroupedKeyBindings() []listKeyBinding {
	bindings := listCommonKeyBindings(true)
	bindings = append(bindings,
		listKeyBinding{keys: listKeyRefresh.keys, action: listKeyRefresh.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			next, cmd := m.triggerManualRefresh()
			return next, cmd
		}},
		listKeyBinding{keys: listKeyProjectFilter.keys, action: listKeyProjectFilter.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			return m.beginProjectFilterInput()
		}},
		listKeyBinding{keys: listKeyToggleStatusArea.keys, action: listKeyToggleStatusArea.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.hideStatusArea = !m.hideStatusArea
			if m.hideStatusArea {
				m.statusMessage = "Status area hidden"
			} else {
				m.statusMessage = "Status area shown"
			}
			m.rebuildGroupedRows()
			m.clampCursor()
			return m, nil
		}},
		listKeyBinding{keys: listKeyAttempts.keys, action: listKeyAttempts.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.selectedGroupedJob()
			if job == nil {
				m.statusMessage = "Select a job row to view attempts"
				return m, nil
			}
			return m, func() tea.Msg { return switchToAttemptsMsg{jobID: job.ID} }
		}},
		listKeyBinding{keys: listKeyAIAssist.keys, action: listKeyAIAssist.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.selectedGroupedJob()
			if job == nil {
				m.statusMessage = "Select a job row for coding-assistant"
				return m, nil
			}
			return m, m.beginAIAssist(job)
		}},
		listKeyBinding{keys: listKeyGroupedAuto.keys, action: listKeyGroupedAuto.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Auto-pilot is available in grouped-by-status view"
				return m, nil
			}
			return m.toggleAutopilot()
		}},
		listKeyBinding{keys: "$", action: "budget", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Budget controls are available in grouped-by-status view"
				return m, nil
			}
			return m.beginAutoRunRateInput()
		}},
		listKeyBinding{keys: listKeyListView.keys, action: listKeyListView.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if m.moveLookupPending {
				m.moveLookupPending = false
				m.moveLookupRequestID = 0
			}
			nextMode := m.nextGroupMode()
			m.setGroupMode(nextMode)
			if nextMode == listGroupUngrouped {
				m.statusMessage = "Ungrouped list view"
			} else {
				m.statusMessage = "Grouped by " + listGroupModeLabel(nextMode)
			}
			return m, nil
		}},
		listKeyBinding{keys: listKeyInstances.keys, action: listKeyInstances.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			return m, func() tea.Msg { return switchToSystemWatchMsg{} }
		}},
		listKeyBinding{keys: "?", action: "help", handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			m.showHelp = true
			return m, nil
		}},
		listKeyBinding{keys: listKeyAutoErrorDetails.keys, action: listKeyAutoErrorDetails.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if strings.TrimSpace(m.lastAutoPilotErrorRaw) == "" {
				m.statusMessage = "No auto-pilot error details."
				return m, nil
			}
			m.showAutoPilotErrorDetails = !m.showAutoPilotErrorDetails
			return m, nil
		}},
		listKeyBinding{keys: listKeyLaunchQueued.keys, action: listKeyLaunchQueued.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Launch controls are available in grouped-by-status view"
				return m, nil
			}
			if m.quickLaunching {
				m.statusMessage = "Launch already in progress..."
				return m, nil
			}
			if !computeGroupedETA(m.groupedJobsWithAutoReasons(), m.launchLiveByID, time.Now()).HasQueued {
				m.statusMessage = "No queued jobs to launch."
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			m.quickLaunching = true
			m.statusMessage = "Launching new instance..."
			progressCh := make(chan listQuickLaunchProgressMsg, 16)
			doneCh := make(chan listQuickLaunchDoneMsg, 1)
			m.quickLaunchProgress = progressCh
			m.quickLaunchDone = doneCh
			return m, tea.Batch(
				m.startQuickLaunch(progressCh, doneCh),
				m.waitForQuickLaunchProgress(),
				m.waitForQuickLaunchDone(),
			)
		}},
		listKeyBinding{keys: listKeyLaunchSelected.keys, action: listKeyLaunchSelected.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Launch controls are available in grouped-by-status view"
				return m, nil
			}
			return m.beginSelectedLaunchNew()
		}},
		listKeyBinding{keys: listKeyPriority.keys, action: listKeyPriority.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Priority controls are available in grouped-by-status view"
				return m, nil
			}
			return m.toggleSelectedGroupedPriority()
		}},
		listKeyBinding{keys: listKeyRebalance.keys, action: listKeyRebalance.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Rebalance preview is available in grouped-by-status view"
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
			m.rebalancePreview = rebalancePreviewModel{active: true, loading: true}
			m.statusMessage = "Planning rebalance moves..."
			return m, requestRebalancePreview(m.database)
		}},
		listKeyBinding{keys: listKeyKillCancel.keys, aliases: listKeyKillCancel.aliases, action: listKeyKillCancel.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.selectedGroupedJob()
			if job == nil {
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			m.statusMessage = fmt.Sprintf("Killing job #%d...", job.ID)
			return m, requestWatchJobKill(m.database, job.ID)
		}},
		listKeyBinding{keys: listKeyUnplace.keys, action: listKeyUnplace.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.selectedGroupedJob()
			if job == nil {
				return m, nil
			}
			if job.EffectiveStatus() != db.StatusQueued {
				m.statusMessage = fmt.Sprintf("Job #%d is %s; only queued jobs can be unplaced", job.ID, job.EffectiveStatus())
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			m.statusMessage = fmt.Sprintf("Unplacing job #%d...", job.ID)
			return m, requestWatchJobUnplace(m.database, job.ID)
		}},
		listKeyBinding{keys: listKeyToggleProcessed.keys, action: listKeyToggleProcessed.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			job := m.selectedGroupedJob()
			if job == nil {
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			if job.HasTag(db.ProcessedTag) {
				m.statusMessage = fmt.Sprintf("Marking job #%d as unprocessed...", job.ID)
			} else {
				m.statusMessage = fmt.Sprintf("Marking job #%d as processed...", job.ID)
			}
			return m, requestWatchJobToggleProcessed(m.database, job.ID)
		}},
		listKeyBinding{keys: listKeyMove.keys, action: listKeyMove.action, handler: func(m listTUIModel) (tea.Model, tea.Cmd) {
			if !m.isStatusGroupedView() {
				m.statusMessage = "Move controls are available in grouped-by-status view"
				return m, nil
			}
			return m.beginGroupedMove()
		}},
	)
	return bindings
}
