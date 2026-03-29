package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m watchModel) View() string {
	switch {
	case m.mode.isInstanceBased():
		content, cursorLine := m.renderInstanceView()
		return m.applyViewport(content, cursorLine)
	case m.mode == watchModeSystem:
		content, cursorLine := m.renderSystemView()
		return m.applyViewport(content, cursorLine)
	case m.mode == watchModeProject:
		return m.renderProjectView()
	}
	return ""
}

func (m watchModel) renderInstanceView() (string, int) {
	var b strings.Builder
	now := time.Now()
	width := m.width
	if width <= 0 {
		width = 100
	}
	selectableIndex := 0
	selectedVisualLine := -1
	lineCount := 0

	countLine := func() { lineCount++ }
	addLine := func(text string) {
		b.WriteString(text)
		b.WriteString("\n")
		countLine()
	}
	addSelectable := func(text string) {
		if selectableIndex == m.cursor {
			text = watchSelectedRowStyle.Render(padToWidth(text, width))
			selectedVisualLine = lineCount
		}
		b.WriteString(text)
		b.WriteString("\n")
		countLine()
		selectableIndex++
	}

	// Header
	if m.campaignID > 0 {
		var header string
		switch m.mode {
		case watchModeCampaign:
			header = fmt.Sprintf("Campaign %d", m.campaignID)
		case watchModeInstances:
			header = "Launched"
		}
		if !m.launchedAt.IsZero() {
			header += fmt.Sprintf(" — launched %s (%s ago)",
				m.launchedAt.Format("15:04"),
				now.Sub(m.launchedAt).Truncate(time.Second))
		}
		addLine(watchTitleStyle.Render(header))
		addLine("")
	}
	if summary := formatWatchSummaryLine(m.launchedAt, m.instanceViews(), now, m.estimateSummaryLine); summary != "" {
		addLine(summary)
		addLine("")
	}

	for _, id := range m.instanceIDs {
		if m.cachedHiddenIDs[id] {
			continue
		}
		donors := m.cachedReplacementChains[id]

		u, ok := m.updates[id]
		if !ok {
			info := m.initInfo[id]
			ci := info.ci
			jobs := info.jobs
			if ci != nil {
				update := campaign.InstanceUpdate{
					Launch:             ci,
					Jobs:               jobs,
					JobAttemptOutcomes: info.outcomes,
				}
				resolved := 0
				lines := formatWatchInstanceBlockLines(update, nil, watchInstanceBlockOptions{
					spinner:                  m.spinner.View(),
					showSpinnerIfNonTerminal: true,
					resolvedJobsOverride:     &resolved,
					dimJobStatuses:           true,
					predecessors:             donors,
				})
				if len(lines) > 0 {
					addSelectable(lines[0])
					for _, line := range lines[1:] {
						addLine(line)
					}
				}
			} else {
				addLine(m.spinner.View() + fmt.Sprintf(" Instance %d — waiting for data...", id))
			}
			addLine("")
			continue
		}

		lines := formatWatchInstanceBlockLines(u, m.jobProgressHWM, watchInstanceBlockOptions{
			predecessors: donors,
		})
		if len(lines) > 0 {
			addSelectable(lines[0])
			for _, line := range lines[1:] {
				addLine(line)
			}
		}
		addLine("")
	}

	// Unplaced jobs section (instance-based modes)
	if len(m.unplacedJobs) > 0 {
		addLine(watchTitleStyle.Render(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs))))
		for _, job := range m.unplacedJobs {
			addSelectable("  " + truncate(m.formatUnplacedJobRow(job), max(width-2, 40)))
		}
		addLine("")
	}

	// Partial launch errors
	if len(m.partialErrors) > 0 && !m.partialErrorsRetried {
		b.WriteString(formatPartialErrors(m.partialErrors))
		b.WriteString("\n")
		countLine()
	}

	// Retry status
	if m.retrying {
		addLine(m.spinner.View() + fmt.Sprintf(" Retrying %d failed instance(s)...", m.countFailedInstances()))
	} else if m.retryResult != "" {
		addLine(m.retryResult)
	}

	if !m.done {
		hint := "j/k scroll  ^u/^d page  g/G top/bottom  s submit  q quit (instances continue in background)"
		if !m.retrying && m.hasRetryableFailures() {
			hint = "j/k scroll  ^u/^d page  g/G top/bottom  s submit  r retry  q quit (instances continue in background)"
		} else if len(m.unplacedJobs) > 0 {
			hint = "j/k scroll  ^u/^d page  g/G top/bottom  s submit  l launch  q quit (instances continue in background)"
		}
		hint += "  " + m.autoModeHint()
		addLine(watchDimStyle.Render(hint))
	}

	return b.String(), selectedVisualLine
}

func (m watchModel) renderSystemView() (string, int) {
	width := m.width
	if width <= 0 {
		width = 100
	}

	rows := make([]watchRenderRow, 0, 8+len(m.cloudInstances)+len(m.unplacedJobs))
	selectedVisualIndex := -1
	selectableIndex := 0

	addHeader := func(text string) {
		rows = append(rows, watchRenderRow{text: watchTitleStyle.Render(text)})
	}
	addSelectable := func(text string) {
		if selectableIndex == m.cursor {
			text = watchSelectedRowStyle.Render(padToWidth(text, width))
			selectedVisualIndex = len(rows)
		}
		rows = append(rows, watchRenderRow{text: text})
		selectableIndex++
	}
	addPlain := func(text string) {
		rows = append(rows, watchRenderRow{text: text})
	}

	title := "System Watch"
	if m.refreshing {
		title += "  " + m.spinner.View() + " " + watchDimStyle.Render("refreshing")
	}
	addHeader(title)
	addPlain("")

	addHeader(fmt.Sprintf("Rental Instances (%d)", len(m.cloudInstances)))
	if len(m.cloudInstances) == 0 {
		addPlain(watchDimStyle.Render("  no active rental instances"))
	} else {
		views := make([]cloudInstanceView, 0, len(m.cloudInstances))
		for _, ci := range m.cloudInstances {
			update := normalizeWatchInstanceUpdate(m.updates[ci.ID], ci)
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
		}
		if summary := formatCloudAggregateSummary("  Summary:", summarizeLaunches(views, time.Now())); summary != "" {
			addPlain(summary)
			addPlain("")
		}
		for i, ci := range m.cloudInstances {
			if m.cachedHiddenIDs[ci.ID] {
				continue
			}
			if i > 0 {
				addPlain("")
			}
			update := normalizeWatchInstanceUpdate(m.updates[ci.ID], ci)
			donors := m.cachedReplacementChains[ci.ID]
			lines := formatWatchInstanceBlockLines(update, m.jobProgressHWM, watchInstanceBlockOptions{
				predecessors: donors,
			})
			if len(lines) == 0 {
				continue
			}
			addSelectable(truncate(lines[0], width))
			for _, line := range lines[1:] {
				addPlain(truncate(line, width))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Inventory Hosts (%d active)", len(m.onPremHosts)))
	if len(m.onPremHosts) == 0 {
		addPlain(watchDimStyle.Render("  no active inventory jobs"))
	} else {
		projectWidth := len("PROJECT")
		for _, host := range m.onPremHosts {
			for _, job := range host.Jobs {
				if w := len(campaign.JobProjectLabel(job)); w > projectWidth {
					projectWidth = w
				}
			}
		}
		for _, host := range m.onPremHosts {
			addPlain(watchStatusStyle.Render("  " + host.Name))
			for _, job := range host.Jobs {
				addSelectable("    " + truncate(m.formatOnPremJobRow(job, projectWidth), width-4))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs)))
	if len(m.unplacedJobs) == 0 {
		addPlain(watchDimStyle.Render("  no unplaced jobs"))
	} else {
		for _, job := range m.unplacedJobs {
			addSelectable("  " + truncate(m.formatUnplacedJobRow(job), width-2))
		}
	}

	// Build footer
	footerParts := make([]string, 0, 3)
	footerPrefixWidth := 0
	if m.err != nil {
		errText := fmt.Sprintf("Error: %v", m.err)
		footerParts = append(footerParts, watchFailedStyle.Render(errText))
		footerPrefixWidth = lipgloss.Width(errText)
	} else if rendered := m.flash.Render(); rendered != "" {
		footerParts = append(footerParts, rendered)
		footerPrefixWidth = lipgloss.Width(rendered)
	}
	if detail := m.selectedStatusDetail(); detail != "" {
		detail = m.truncateFooterDetail(detail, footerPrefixWidth)
		if detail != "" {
			footerParts = append(footerParts, watchDimStyle.Render(detail))
		}
	}

	// Retry status in footer for system mode
	if m.retrying {
		footerParts = append(footerParts, m.spinner.View()+fmt.Sprintf(" Retrying %d failed instance(s)...", m.countFailedInstances()))
	} else if m.retryResult != "" {
		footerParts = append(footerParts, m.retryResult)
	}

	controls := "[^u/^d] page  [u] unplace  [s] submit  [l] launch  [r] retry  [q] quit"
	if !m.hasRetryableFailures() {
		controls = "[^u/^d] page  [u] unplace  [s] submit  [l] launch  [q] quit"
	}
	controls += "  " + m.autoModeHint()
	footerParts = append(footerParts, watchDimStyle.Render(controls))

	// Render rows into content string
	contentHeight := m.height - 1
	if contentHeight < 1 {
		contentHeight = len(rows)
	}
	start := watchScrollStart(len(rows), selectedVisualIndex, contentHeight)
	end := start + contentHeight
	if end > len(rows) {
		end = len(rows)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(rows[i].text)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if end-start < contentHeight {
		padLine := strings.Repeat(" ", max(0, m.width))
		for i := end - start; i < contentHeight; i++ {
			b.WriteString("\n")
			b.WriteString(padLine)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	footer := strings.Join(footerParts, "  ")
	b.WriteString(footer)

	// For system mode, scrolling is done via watchScrollStart above, so we
	// return -1 to skip applyViewport's cursor-based slicing.
	return b.String(), -1
}

// applyViewport slices rendered content to fit the terminal height.
// If cursorLine >= 0, it centers the viewport around that line.
// Otherwise, bottom-anchored: scrollOff=0 shows the bottom of the content.
func (m watchModel) applyViewport(content string, cursorLine int) string {
	if m.height <= 0 || m.done {
		return content
	}

	// System mode uses its own scrolling in renderSystemView
	if cursorLine < 0 && m.mode == watchModeSystem {
		return content
	}

	// Fast path: count newlines to check fit without allocating a []string
	if strings.Count(content, "\n") < m.height {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) <= m.height {
		return content
	}

	// If we have a cursor line, center viewport around it
	if cursorLine >= 0 {
		start := watchScrollStart(len(lines), cursorLine, m.height)
		end := start + m.height
		if end > len(lines) {
			end = len(lines)
		}
		visible := lines[start:end]
		if start > 0 {
			visible[0] = watchDimStyle.Render(fmt.Sprintf("↑ %d more lines above", start))
		}
		off := len(lines) - end
		if off > 0 {
			visible[len(visible)-1] = watchDimStyle.Render(fmt.Sprintf("↓ %d more lines below", off))
		}
		return strings.Join(visible, "\n")
	}

	// Bottom-anchored scrolling (fallback for instance-based modes without cursor)
	maxOff := len(lines) - m.height
	off := m.scrollOff
	if off > maxOff {
		off = maxOff
	}

	end := len(lines) - off
	start := end - m.height
	if start < 0 {
		start = 0
	}

	visible := lines[start:end]

	if start > 0 {
		visible[0] = watchDimStyle.Render(fmt.Sprintf("↑ %d more lines above", start))
	}
	if off > 0 {
		visible[len(visible)-1] = watchDimStyle.Render(fmt.Sprintf("↓ %d more lines below", off))
	}

	return strings.Join(visible, "\n")
}

// ---------------------------------------------------------------------------
// View helpers: instance-based modes
// ---------------------------------------------------------------------------

func (m watchModel) instanceViews() []cloudInstanceView {
	views := make([]cloudInstanceView, 0, len(m.instanceIDs))
	for _, id := range m.instanceIDs {
		if update, ok := m.updates[id]; ok {
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
			continue
		}
		if m.initInfo != nil {
			if info, ok := m.initInfo[id]; ok {
				views = append(views, cloudInstanceView{Launch: info.ci})
			}
		}
	}
	return views
}

func formatWatchSummaryLine(launchedAt time.Time, views []cloudInstanceView, now time.Time, estimateLine string) string {
	agg := summarizeLaunches(views, now)
	label := "Summary:"
	if !launchedAt.IsZero() {
		label = "Summary: uptime: " + now.Sub(launchedAt).Truncate(time.Second).String()
	}
	s := formatCloudAggregateSummary(label, agg)
	if s != "" && estimateLine != "" {
		s += "\n" + estimateLine
	}
	return s
}

// ---------------------------------------------------------------------------
// View helpers: system mode
// ---------------------------------------------------------------------------

func (m watchModel) formatOnPremJobRow(job *db.Job, projectWidth int) string {
	status := job.EffectiveStatus()
	duration := "—"
	if job.StartTime > 0 {
		d := time.Duration(time.Now().Unix()-job.StartTime) * time.Second
		duration = d.Truncate(time.Second).String()
	}
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = campaign.TruncateCommand(job.Command, 50)
	}
	return fmt.Sprintf("#%-4d  %s  %-10s  %-*s  %s",
		job.ID,
		renderWatchJobStatusText(status, status, watchInstanceBlockOptions{}),
		duration,
		projectWidth, campaign.JobProjectLabel(job),
		desc,
	)
}

func (m watchModel) formatUnplacedJobRow(job *db.Job) string {
	return fmt.Sprintf("#%-4d %-12s %-28s %s",
		job.ID,
		campaign.JobProjectLabel(job),
		truncate(job.EffectiveDescription(), 28),
		formatWatchGPUConstraint(job),
	)
}

func (m watchModel) selectedStatusDetail() string {
	job := m.selectedUnplacedJob()
	if job == nil || len(job.PlacementReasons) == 0 {
		return ""
	}
	return fmt.Sprintf("#%d unplaced: %s", job.ID, strings.Join(job.PlacementReasons, " | "))
}

func (m watchModel) truncateFooterDetail(detail string, prefixWidth int) string {
	if m.width <= 0 {
		return detail
	}
	controlsWidth := lipgloss.Width("[u] unplace  [s] submit  [l] launch  [r] retry  [q] quit")
	available := m.width - controlsWidth
	if prefixWidth > 0 {
		available -= prefixWidth + lipgloss.Width("  ")
	}
	available -= lipgloss.Width("  ")
	if available <= 0 {
		return ""
	}
	if available < 16 {
		return truncate(detail, max(available, 3))
	}
	return truncate(detail, available)
}

// autoModeHint returns a short hint for the current auto-pilot state.
func (m watchModel) autoModeHint() string {
	if m.autoMode {
		return "[a] auto: ON"
	}
	return "[a] auto: OFF"
}

// ---------------------------------------------------------------------------
// Scroll helpers
// ---------------------------------------------------------------------------

func watchScrollStart(totalRows, selectedIndex, viewportHeight int) int {
	if viewportHeight <= 0 || totalRows <= viewportHeight || selectedIndex < 0 {
		return 0
	}
	start := selectedIndex - viewportHeight/2
	if start < 0 {
		start = 0
	}
	maxStart := totalRows - viewportHeight
	if start > maxStart {
		start = maxStart
	}
	return start
}

func padToWidth(s string, width int) string {
	current := lipgloss.Width(s)
	if current >= width {
		return truncate(s, width)
	}
	return s + strings.Repeat(" ", width-current)
}

// ---------------------------------------------------------------------------
// View: project mode
// ---------------------------------------------------------------------------

func (m watchModel) renderProjectView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	lines := m.projectLines
	rows := m.projectPageSize()
	var b strings.Builder
	title := fmt.Sprintf("Project Watch (%d projects)", len(m.projectGroups))
	b.WriteString(watchTitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")

	if len(lines) == 0 {
		empty := m.projectEmptyStateText()
		b.WriteString(watchDimStyle.Render(truncateDisplayWidth(empty, m.width)))
		b.WriteString("\n")
		for i := 1; i < rows; i++ {
			b.WriteString("\n")
		}
	} else {
		for i := 0; i < rows; i++ {
			idx := m.projectOffset + i
			if idx >= len(lines) {
				b.WriteString("\n")
				continue
			}
			line := truncateDisplayWidth(lines[idx], m.width)
			if idx == m.cursor {
				line = watchSelectedRowStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString(watchDimStyle.Render(truncateDisplayWidth(m.projectFooterText(), m.width)))
	return b.String()
}

func (m watchModel) projectFooterText() string {
	rows := m.projectPageSize()
	total := len(m.projectLines)
	start := 0
	end := 0
	if total > 0 {
		start = m.projectOffset + 1
		end = min(total, m.projectOffset+rows)
	}

	state := fmt.Sprintf("[%d-%d/%d]", start, end, total)
	if m.projectSyncing {
		state += " syncing..."
	}
	if m.projectStatus != "" {
		state += "  " + m.projectStatus
	}
	state += "  up/down move  space/b page  g/G top/bottom  r refresh  l launch  q quit  " + m.autoModeHint()
	return state
}

func (m watchModel) projectEmptyStateText() string {
	if m.projectSyncing {
		return "No project activity yet. Waiting for startup sync and DB updates..."
	}
	if m.projectStatus != "" {
		return "No project activity. " + m.projectStatus
	}
	return "No project activity."
}
