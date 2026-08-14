package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/ssh"
)

func (m Model) handleMouseClick(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Ignore mouse events when in input mode or showing overlays
	if m.inputMode || m.showHelp || m.showCloudMenu || m.restarting || m.creatingJob {
		return m, nil
	}

	// Calculate list panel height (same as in View)
	listHeight := int(float64(m.height) * jobListHeightRatio)

	// Handle mouse wheel
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		scrollUp := msg.Button == tea.MouseButtonWheelUp

		// Check if mouse is in the log/detail panel area (bottom portion)
		if msg.Y >= listHeight {
			scrollDelta := 3
			if scrollUp {
				scrollDelta = -scrollDelta
			}
			switch m.detailTab {
			case DetailTabLogs:
				m.logViewport.SetYOffset(m.logViewport.YOffset + scrollDelta)
				return m, nil
			case DetailTabDetails:
				if m.jobSelectionActive {
					m.detailViewport.SetYOffset(m.detailViewport.YOffset + scrollDelta)
					return m, nil
				}
			}
		}

		// Scroll job list by moving cursor (bubbles/list doesn't handle wheel events)
		if m.viewMode == ViewModeJobs && len(m.jobs) > 0 {
			prevIdx := m.jobList.Index()
			for i := 0; i < 3; i++ {
				if scrollUp {
					m.jobList.CursorUp()
				} else {
					m.jobList.CursorDown()
				}
			}
			if m.jobList.Index() != prevIdx {
				return m, m.handleSelectionChanged()
			}
		}
		return m, nil
	}

	// Handle mouse clicks manually (bubbles/list has limited mouse support)
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
		// Layout within panel: border(1) + header(1) + filter(1) + jobs start at Y=3
		if m.viewMode == ViewModeJobs && msg.Y >= 3 && msg.Y < listHeight-1 {
			clickedRow := msg.Y - 3
			// Get the visible range from the list's paginator
			start, _ := m.jobList.Paginator.GetSliceBounds(len(m.jobs))
			clickedIndex := start + clickedRow
			if clickedIndex >= 0 && clickedIndex < len(m.jobs) {
				prevIdx := m.jobList.Index()
				if clickedIndex == prevIdx {
					if m.jobSelectionActive {
						m.clearJobSelection()
						return m, nil
					}
					m.jobSelectionActive = true
					return m, m.handleSelectionChanged()
				}
				m.jobList.Select(clickedIndex)
				m.jobSelectionActive = true
				if m.jobList.Index() != prevIdx {
					return m, m.handleSelectionChanged()
				}
			}
			return m, nil
		}
	}

	// Handle host clicks manually since hosts don't use bubbles/list
	if m.viewMode == ViewModeHosts {
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			// Layout within panel: border(1) + header(1) + hosts start at Y=2 (no filter row)
			if msg.Y >= 2 && msg.Y < listHeight-1 {
				clickedRow := msg.Y - 2
				if idx := m.hostIndexAtRow(clickedRow, listHeight); idx >= 0 {
					m.selectedHostIdx = idx
					if cmd := m.handleHostSelectionChanged(); cmd != nil {
						return m, cmd
					}
				}
			}
		}
	}

	return m, nil
}

func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Cloud menu overlay - intercept all keys
	if m.showCloudMenu {
		return m.handleCloudMenuKeyPress(msg)
	}

	// Help overlay - dismiss with ? or Esc
	if m.showHelp {
		if key.Matches(msg, keys.Help) || key.Matches(msg, keys.Escape) {
			m.showHelp = false
		}
		return m, nil
	}

	// When in log view, forward scroll keys to viewport
	if m.detailTab == DetailTabLogs {
		switch msg.String() {
		case "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.logViewport, cmd = m.logViewport.Update(msg)
			return m, cmd
		}
	}
	if m.detailTab == DetailTabDetails && m.jobSelectionActive {
		switch msg.String() {
		case "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
			updated, cmd := m.detailViewport.Update(msg)
			*m.detailViewport = updated
			return m, cmd
		}
	}

	// Toggle help overlay
	if key.Matches(msg, keys.Help) {
		m.showHelp = true
		return m, nil
	}

	// Allow cancelling job creation with Escape
	if m.creatingJob && key.Matches(msg, keys.Escape) {
		m.creatingJob = false
		m.createJobStep = ""
		return m, m.setFlash("Job creation running in background...", false)
	}

	switch {
	case key.Matches(msg, keys.Quit):
		// Cancel all background SSH operations
		if m.cancel != nil {
			m.cancel()
		}
		if m.llmGenerator != nil {
			m.llmGenerator.Stop()
		}
		// Stop the sync worker
		if m.syncWorker != nil {
			m.syncWorker.Stop()
		}
		// Close SSH session pool
		ssh.ClosePool()
		return m, tea.Quit

	case key.Matches(msg, keys.Suspend):
		return m, tea.Suspend

	case key.Matches(msg, keys.Tab), key.Matches(msg, keys.ShiftTab):
		forward := key.Matches(msg, keys.Tab)
		return m.cycleTab(forward)

	case key.Matches(msg, keys.HostsView):
		// Toggle between hosts and jobs view
		if m.viewMode == ViewModeHosts {
			m.viewMode = ViewModeJobs
			return m, nil
		}
		m.viewMode = ViewModeHosts
		// Refresh hosts when switching to hosts view, but only if needed
		var cmds []tea.Cmd
		for _, host := range m.hosts {
			// Only refresh if not queried this session or if online (for dynamic data)
			if !m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline {
				m.requestHostInfoRefresh(host.Name, false)
			}
		}
		if m.hostDetailTab == HostDetailTabCPU {
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case key.Matches(msg, keys.JobsView):
		// Toggle between jobs and hosts view
		if m.viewMode == ViewModeJobs {
			m.viewMode = ViewModeHosts
			// Refresh hosts when switching to hosts view, but only if needed
			var cmds []tea.Cmd
			for _, host := range m.hosts {
				// Only refresh if not queried this session or if online (for dynamic data)
				if !m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline {
					m.requestHostInfoRefresh(host.Name, false)
				}
			}
			if m.hostDetailTab == HostDetailTabCPU {
				if cmd := m.handleHostSelectionChanged(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			return m, tea.Batch(cmds...)
		}
		m.viewMode = ViewModeJobs
		return m, nil

	case key.Matches(msg, keys.ToggleSummaries):
		// Toggle AI host summaries (only in hosts view)
		if m.viewMode == ViewModeHosts {
			m.showHostSummaries = !m.showHostSummaries
			if m.showHostSummaries {
				// Trigger summary generation for all hosts
				return m, tea.Batch(m.setFlash("AI summaries enabled", false), m.generateAllHostSummaries())
			}
			return m, m.setFlash("AI summaries disabled", false)
		}
		return m, nil

	case key.Matches(msg, keys.PageDownList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.pageJobList(1); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.PageUpList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.pageJobList(-1); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.TopList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.jumpJobListToTop(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.Up):
		if m.viewMode == ViewModeHosts {
			if m.selectedHostIdx > 0 {
				m.selectedHostIdx--
			}
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		// Forward to list and handle selection change
		prevIdx := m.jobList.Index()
		newList, cmd := m.jobList.Update(msg)
		m.jobList = newList
		if m.jobList.Index() != prevIdx {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		if !m.jobSelectionActive {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		return m, cmd

	case key.Matches(msg, keys.Down):
		if m.viewMode == ViewModeHosts {
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts)-1 {
				m.selectedHostIdx++
			}
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		// Forward to list and handle selection change
		prevIdx := m.jobList.Index()
		newList, cmd := m.jobList.Update(msg)
		m.jobList = newList
		if m.jobList.Index() != prevIdx {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		if !m.jobSelectionActive {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		return m, cmd

	case key.Matches(msg, keys.EditRestart):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Backend == db.BackendSkyPilot {
			return m, m.setFlash("SkyPilot jobs cannot be edited/restarted by Weft; submit a new external job instead", true)
		}
		// Open new job form pre-populated with ALL fields from this job
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flash.Clear()
		m.inputs[inputHost].SetValue(job.Host)
		m.inputs[inputCommand].SetValue(job.Command)
		m.inputs[inputDescription].SetValue(job.Description)
		m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		return m, nil

	case key.Matches(msg, keys.Logs):
		if m.viewMode == ViewModeJobs {
			// Toggle to logs tab (or toggle if already there)
			if m.detailTab == DetailTabLogs {
				// Already in logs mode - go back to details
				m.detailTab = DetailTabDetails
				m.detailViewport.GotoTop()
			} else {
				idx := m.jobList.Index()
				if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
					// Enter logs mode
					m.detailTab = DetailTabLogs
					m.jobSelectionActive = true
					m.selectedJob = m.jobs[idx]
					m.logLoading = true
					// Show cached content immediately while fetching fresh logs
					if cached, ok := m.logCache[m.selectedJob.ID]; ok {
						m.logContent = cached
						m.logStale = true
						m.logViewport.SetContent(m.logContent)
					} else {
						m.logContent = ""
						m.logStale = false
					}
					// For terminal jobs, try local cache first (no SSH needed)
					if m.selectedJob.Status == db.StatusCompleted || m.selectedJob.Status == db.StatusDead || m.selectedJob.Status == db.StatusFailed || m.selectedJob.Status == db.StatusKilled || m.selectedJob.Status == db.StatusCanceled {
						if cached, err := logcache.Read(m.selectedJob.ID); err == nil {
							lines := strings.Split(cached, "\n")
							if len(lines) > 500 {
								lines = lines[len(lines)-500:]
							}
							content := strings.Join(lines, "\n")
							m.logContent = content
							m.logLoading = false
							m.logStale = false
							m.logViewport.SetContent(content)
							prog := progress.FindLastProgressPreferExplicit(content)
							if prog != nil {
								m.jobProgress[m.selectedJob.ID] = prog
							}
						}
					}
					// Tell the monitor to start polling for this job's log and stats
					if cmd := m.startSelectedJobLog(); cmd != nil {
						return m, cmd
					}
					return m, nil
				}
			}
		}
		return m, nil

	case key.Matches(msg, keys.Escape):
		m.clearJobSelection()
		m.flash.Clear()
		return m, nil

	case key.Matches(msg, keys.Kill):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		status := job.EffectiveStatus()
		switch status {
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=k action=kill"))
			return m, tea.Batch(m.setFlash("Killing job...", false), m.killJob(job))
		case db.StatusQueued, db.StatusPendingPlacement:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=k action=cancel"))
			return m, tea.Batch(m.setFlash("Cancelling queued job...", false), m.cancelQueuedJob(job))
		case db.StatusCompleted:
			return m, m.setFlash(fmt.Sprintf("Job %s already completed", ids.FormatJobID(job.ID)), true)
		case db.StatusDead:
			return m, m.setFlash(fmt.Sprintf("Job %s already failed to start", ids.FormatJobID(job.ID)), true)
		case db.StatusFailed:
			return m, m.setFlash(fmt.Sprintf("Job %s already crashed", ids.FormatJobID(job.ID)), true)
		case db.StatusKilled:
			return m, m.setFlash(fmt.Sprintf("Job %s already killed", ids.FormatJobID(job.ID)), true)
		case db.StatusCanceled:
			return m, m.setFlash(fmt.Sprintf("Job %s already canceled", ids.FormatJobID(job.ID)), true)
		default:
			return m, m.setFlash(fmt.Sprintf("Can't kill job %s (status: %s)", ids.FormatJobID(job.ID), status), true)
		}

	case key.Matches(msg, keys.Pause):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		status := job.EffectiveStatus()
		switch status {
		case db.StatusRunning, db.StatusStarting:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=p action=pause"))
			return m, tea.Batch(m.setFlash("Pausing job...", false), m.pauseJob(job))
		case db.StatusPaused:
			return m, m.setFlash(fmt.Sprintf("Job %s already paused (press g to resume)", ids.FormatJobID(job.ID)), false)
		default:
			return m, m.setFlash(fmt.Sprintf("Can't pause job %s (status: %s)", ids.FormatJobID(job.ID), status), true)
		}

	case key.Matches(msg, keys.Draft):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		// Toggle behavior based on effective (displayed) status: draft→queued, anything else→draft
		if job.EffectiveStatus() == db.StatusDraft {
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=d action=queue_draft"))
			return m, tea.Batch(m.setFlash("Queueing draft job...", false), m.queueDraftJob(job))
		}
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=d action=draft"))
		return m, tea.Batch(m.setFlash("Marking job draft...", false), m.draftJob(job))

	case key.Matches(msg, keys.Restart):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Backend == db.BackendSkyPilot {
			return m, m.setFlash("SkyPilot jobs cannot be restarted by Weft; submit a new external job instead", true)
		}
		if m.restarting {
			return m, m.setFlash("Restart already in progress...", false)
		}
		m.restarting = true
		m.restartingJobName = fmt.Sprintf("job %s", ids.FormatJobID(job.ID))
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=r action=restart"))
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Restarting job %s...", ids.FormatJobID(job.ID)), false), m.restartJob(job))

	case key.Matches(msg, keys.Retry):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Backend == db.BackendSkyPilot {
			return m, m.setFlash("SkyPilot jobs cannot be retried by Weft; submit a new external job instead", true)
		}
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=y action=retry"))
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Retrying job %s...", ids.FormatJobID(job.ID)), false), m.retryJob(job))

	case key.Matches(msg, keys.Remove):
		if m.viewMode == ViewModeHosts {
			// Delete host in hosts view
			if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
				return m, nil
			}
			host := m.hosts[m.selectedHostIdx]
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Deleting host %s...", host.Name), false), m.deleteHost(host.Name))
		}
		// Remove job in jobs view
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		// Refuse to remove active jobs - suggest killing first
		status := job.EffectiveStatus()
		if status == db.StatusRunning || status == db.StatusQueued || status == db.StatusStarting || status == db.StatusPaused {
			return m, m.setFlash(fmt.Sprintf("Job %s is %s. Kill it first (k)", ids.FormatJobID(job.ID), status), true)
		}
		return m, tea.Batch(m.setFlash("Removing job...", false), m.removeJob(job))

	case key.Matches(msg, keys.NewJob):
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flash.Clear()

		// Pre-populate from highlighted job if inputs are empty
		job := m.getTargetJob()
		if job != nil && m.inputs[inputHost].Value() == "" {
			m.inputs[inputHost].SetValue(job.Host)
			m.inputs[inputCommand].SetValue(job.Command)
			// Don't pre-populate description - it may contain error messages from failed jobs
			// and descriptions are usually different for each job anyway
			m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
			m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		}
		return m, nil

	case key.Matches(msg, keys.Filter):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		m.jobFilter = jobFilterMode((int(m.jobFilter) + 1) % int(jobFilterModeCount))
		m.applyJobFilter()
		var cmds []tea.Cmd
		cmds = append(cmds, m.setFlash(fmt.Sprintf("View: %s", jobFilterDescription(m.jobFilter)), false))
		if len(m.jobs) == 0 {
			m.clearJobSelection()
		} else if cmd := m.jumpJobListToTop(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case key.Matches(msg, keys.HostFilter):
		if m.viewMode == ViewModeHosts && m.selectedHostIdx >= 0 && m.selectedHostIdx < len(m.hosts) {
			host := m.hosts[m.selectedHostIdx].Name
			if host != "" {
				m.jobHostFilterMode = hostFilterSpecific
				m.jobHostFilterHost = host
			}
		} else {
			m.cycleHostFilter()
		}
		m.saveHostFilter()
		m.applyJobFilter()
		var cmds []tea.Cmd
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Host: %s", hostFilterDescription(m)), false))
		if len(m.jobs) == 0 {
			m.clearJobSelection()
		} else if cmd := m.jumpJobListToTop(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case key.Matches(msg, keys.Prune):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		return m, tea.Batch(m.setFlash("Pruning completed/dead jobs...", false), m.pruneJobs())

	case key.Matches(msg, keys.StartNow):
		if m.viewMode == ViewModeHosts {
			// 'g' in hosts view switches to GPU Summary tab
			m.hostDetailTab = HostDetailTabGPUSummary
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		// Check for pending start first (idempotent behavior)
		if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning {
			return m, m.setFlash(fmt.Sprintf("Job %s is already starting (pending)", ids.FormatJobID(job.ID)), false)
		}
		// Use effective status so pending state is respected
		switch job.EffectiveStatus() {
		case db.StatusPaused:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=resume"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Resuming job %s...", ids.FormatJobID(job.ID)), false), m.resumeJob(job))
		case db.StatusQueued:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=start_now"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting job %s now...", ids.FormatJobID(job.ID)), false), m.startQueuedJobNow(job))
		case db.StatusDraft:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=run_draft"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Running draft job %s...", ids.FormatJobID(job.ID)), false), m.runDraftJob(job))
		case db.StatusRunning:
			return m, m.setFlash(fmt.Sprintf("Job %s is already running", ids.FormatJobID(job.ID)), false)
		default:
			return m, m.setFlash(fmt.Sprintf("Can only start queued or draft jobs (job %s is %s)", ids.FormatJobID(job.ID), job.EffectiveStatus()), true)
		}

	case key.Matches(msg, keys.MoveToFront):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job != nil && job.EffectiveStatus() == db.StatusQueued {
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=G action=move_to_front"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Moving job %s to front...", ids.FormatJobID(job.ID)), false), m.moveJobToFront(job))
		}
		return m, m.setFlash("Can only move queued jobs to front", true)

	case key.Matches(msg, keys.Sort):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		m.jobSort = (m.jobSort + 1) % jobSortModeCount
		m.applyJobFilter() // Re-filter and sort
		return m, m.setFlash(fmt.Sprintf("Sort: %s", m.jobSort), false)

	case key.Matches(msg, keys.RegenerateDesc):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if m.llmGenerator == nil {
			return m, m.setFlash("AI description generation not available", true)
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		return m, tea.Batch(
			m.setFlash(fmt.Sprintf("Generating description for job %s...", ids.FormatJobID(job.ID)), false),
			m.regenerateDescription(job),
		)

	case key.Matches(msg, keys.Edit):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Backend == db.BackendSkyPilot {
			return m, m.setFlash("SkyPilot execution fields cannot be edited by Weft", true)
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusDraft {
			return m, m.setFlash("Can only edit queued or draft jobs", true)
		}
		// Enter edit mode with form pre-populated
		m.inputMode = true
		m.editMode = true
		m.editingJobID = job.ID
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flash.Clear()
		// Pre-populate all fields
		m.inputs[inputHost].SetValue(job.Host)
		m.inputs[inputCommand].SetValue(job.Command)
		m.inputs[inputDescription].SetValue(job.Description)
		m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		m.inputs[inputGPU].SetValue("")
		m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		m.inputs[inputEnvVars].SetValue("")
		m.editingJobDepSpec = ""
		return m, m.fetchQueuedJobEnv(job)

	case key.Matches(msg, keys.Sync):
		if m.viewMode == ViewModeJobs && m.syncWorker != nil {
			// Request high-priority sync for all hosts
			m.requestSyncAllHosts(true)
			return m, m.setFlash("Sync requested", false)
		}
		return m, nil

	case key.Matches(msg, keys.Cloud):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Backend == db.BackendSkyPilot {
			return m, m.setFlash("SkyPilot jobs cannot be launched on a Weft rental", true)
		}
		if job.EffectiveStatus() != db.StatusQueued {
			return m, m.setFlash("Rental GPU only available for queued jobs", true)
		}
		return m, m.openCloudMenu(job)
	}

	// Host view specific key bindings (handled after the switch)
	if m.viewMode == ViewModeHosts {
		switch msg.String() {
		case "i":
			// Switch to Info tab
			m.hostDetailTab = HostDetailTabInfo
			return m, nil
		case "S":
			// Start queue runner on selected host
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts) {
				host := m.hosts[m.selectedHostIdx]
				return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting queue on %s...", host.Name), false), m.startQueue(host.Name))
			}
			return m, nil
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
			// Switch to specific GPU tab by hardware index
			gpuIndex, _ := strconv.Atoi(msg.String())
			gpuIndices := m.getHostGPUIndices()
			for pos, idx := range gpuIndices {
				if idx == gpuIndex {
					m.hostDetailTab = HostDetailTabGPUBase + HostDetailTab(pos)
					return m, nil
				}
			}
			// GPU not found - show message
			return m, m.setFlash(fmt.Sprintf("No GPU %d on this host", gpuIndex), true)
		}
	}

	return m, nil
}

func (m Model) handleInputKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Cancel input mode
		m.inputMode = false
		m.editMode = false
		m.editingJobID = 0
		m.editingJobDepSpec = ""
		m.inputs[m.inputFocus].Blur()
		return m, nil

	case tea.KeyTab, tea.KeyShiftTab:
		// Cycle through inputs
		m.inputs[m.inputFocus].Blur()
		if msg.Type == tea.KeyShiftTab {
			m.inputFocus--
			if m.inputFocus < 0 {
				m.inputFocus = len(m.inputs) - 1
			}
		} else {
			m.inputFocus++
			if m.inputFocus >= len(m.inputs) {
				m.inputFocus = 0
			}
		}
		m.inputs[m.inputFocus].Focus()
		return m, nil

	case tea.KeyEnter:
		// Submit if we have required fields
		host := strings.TrimSpace(m.inputs[inputHost].Value())
		command := strings.TrimSpace(m.inputs[inputCommand].Value())

		if host == "" || command == "" {
			return m, m.setFlash("Host and command are required", true)
		}

		// Exit input mode
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		m.flash.Clear()

		if m.editMode {
			// Edit existing job
			m.editMode = false
			m.editingJobDepSpec = ""
			return m, m.editJob()
		}

		// Create new job
		m.creatingJob = true
		m.createJobStart = time.Now()
		m.createJobStep = "Connecting..."
		return m, tea.Batch(m.createJob(), m.startCreateTicker())
	}

	// Forward other keys to the focused input
	var cmd tea.Cmd
	m.inputs[m.inputFocus], cmd = m.inputs[m.inputFocus].Update(msg)
	return m, cmd
}

// cycleTab handles Tab/Shift+Tab navigation for both Jobs and Hosts views
func (m *Model) cycleTab(forward bool) (Model, tea.Cmd) {
	if m.viewMode == ViewModeJobs {
		return m.cycleJobsTab(forward)
	}
	return m.cycleHostsTab(forward)
}

// cycleJobsTab toggles between Details and Logs tabs in Jobs view
func (m *Model) cycleJobsTab(forward bool) (Model, tea.Cmd) {
	tabs := []DetailTab{DetailTabDetails, DetailTabLogs, DetailTabCPU}
	if job := m.getTargetJob(); job != nil && job.Backend == db.BackendSkyPilot {
		tabs = tabs[:2]
	}
	currentIdx := 0
	for i, tab := range tabs {
		if tab == m.detailTab {
			currentIdx = i
			break
		}
	}

	if forward {
		currentIdx = (currentIdx + 1) % len(tabs)
	} else {
		currentIdx = (currentIdx - 1 + len(tabs)) % len(tabs)
	}

	return m.switchToJobTab(tabs[currentIdx])
}

// switchToLogsTab switches to the Logs tab and fetches log content
func (m *Model) switchToLogsTab() (Model, tea.Cmd) {
	m.detailTab = DetailTabLogs
	m.jobSelectionActive = true
	idx := m.jobList.Index()
	if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
		m.selectedJob = m.jobs[idx]
		m.logLoading = true
		// Show cached content immediately while fetching fresh logs
		if cached, ok := m.logCache[m.selectedJob.ID]; ok {
			m.logContent = cached
			m.logStale = true
			m.logViewport.SetContent(m.logContent)
		} else {
			m.logContent = ""
			m.logStale = false
		}
		if cmd := m.startSelectedJobLog(); cmd != nil {
			return *m, cmd
		}
		return *m, nil
	}
	return *m, nil
}

func (m *Model) switchToCPUTab() (Model, tea.Cmd) {
	job := m.getTargetJob()
	if job != nil && job.Backend == db.BackendSkyPilot {
		m.detailTab = DetailTabDetails
		m.jobSelectionActive = true
		return *m, m.setFlash("CPU telemetry is not available for SkyPilot-managed jobs", true)
	}
	m.detailTab = DetailTabCPU
	m.jobSelectionActive = true
	if cmd := m.requestJobCPUTop(job); cmd != nil {
		return *m, cmd
	}
	return *m, nil
}

func (m *Model) switchToJobTab(tab DetailTab) (Model, tea.Cmd) {
	switch tab {
	case DetailTabLogs:
		return m.switchToLogsTab()
	case DetailTabCPU:
		return m.switchToCPUTab()
	default:
		m.detailTab = DetailTabDetails
		m.detailViewport.GotoTop()
		return *m, nil
	}
}

func (m *Model) switchToHostCPUTab() (Model, tea.Cmd) {
	m.hostDetailTab = HostDetailTabCPU
	if len(m.hosts) == 0 || m.selectedHostIdx < 0 || m.selectedHostIdx >= len(m.hosts) {
		m.hostCPUTopEntries = nil
		m.hostCPUTopError = ""
		m.hostCPUTopRequestedHost = ""
		m.hostCPUTopDataHost = ""
		m.hostCPUTopLoading = false
		return *m, nil
	}
	host := m.hosts[m.selectedHostIdx]
	if cmd := m.requestHostCPUTop(host.Name); cmd != nil {
		return *m, cmd
	}
	return *m, nil
}

func (m *Model) switchToHostTab(tab HostDetailTab) (Model, tea.Cmd) {
	switch tab {
	case HostDetailTabCPU:
		return m.switchToHostCPUTab()
	default:
		m.hostDetailTab = tab
		return *m, nil
	}
}

// cycleHostsTab cycles through host detail tabs (Info, CPU, GPUs, GPU 0, GPU 1, ...)
func (m *Model) cycleHostsTab(forward bool) (Model, tea.Cmd) {
	gpuIndices := m.getHostGPUIndices()
	order := []HostDetailTab{HostDetailTabInfo, HostDetailTabCPU, HostDetailTabGPUSummary}
	for i := range gpuIndices {
		order = append(order, HostDetailTabGPUBase+HostDetailTab(i))
	}
	if len(order) == 0 {
		order = []HostDetailTab{HostDetailTabInfo}
	}

	current := 0
	for i, tab := range order {
		if tab == m.hostDetailTab {
			current = i
			break
		}
	}

	if forward {
		current = (current + 1) % len(order)
	} else {
		current = (current - 1 + len(order)) % len(order)
	}

	return m.switchToHostTab(order[current])
}

func (m *Model) clearJobSelection() {
	m.jobSelectionActive = false
	m.detailTab = DetailTabDetails
	m.selectedJob = nil
	m.logContent = ""
	m.logStale = false
	m.logLoading = false
	m.processStats = nil
	m.prevProcessStats = nil
	m.processStatsJobID = 0
	m.detailViewport.SetContent("")
	m.detailViewport.GotoTop()
	if m.monitor != nil {
		m.monitor.WatchJobLog(nil)
		m.monitor.WatchJobStats(nil)
	}
}

func (m *Model) jumpJobListToTop() tea.Cmd {
	if len(m.jobs) == 0 {
		m.clearJobSelection()
		return nil
	}

	prevIdx := m.jobList.Index()
	m.jobList.Select(0)
	m.jobList.Paginator.Page = 0
	m.jobSelectionActive = true
	if m.jobList.Index() != prevIdx {
		return m.handleSelectionChanged()
	}
	return nil
}

func (m *Model) pageJobList(direction int) tea.Cmd {
	if len(m.jobs) == 0 {
		m.clearJobSelection()
		return nil
	}
	pageSize := m.jobListContentHeight
	if pageSize <= 0 {
		pageSize = 10
	}
	idx := m.jobList.Index()
	if idx < 0 {
		idx = 0
	}
	newIdx := idx + direction*pageSize
	if newIdx < 0 {
		newIdx = 0
	} else if newIdx >= len(m.jobs) {
		newIdx = len(m.jobs) - 1
	}
	prevIdx := m.jobList.Index()
	m.jobList.Select(newIdx)
	m.jobSelectionActive = true
	if m.jobList.Index() != prevIdx {
		return m.handleSelectionChanged()
	}
	return nil
}
