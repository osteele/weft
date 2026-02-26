package tui

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/weft/internal/db"
)

func (m Model) renderWithModal(background, message string) string {
	// Create modal box
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 3).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("229"))

	modal := modalStyle.Render(message)

	// Place modal centered on screen
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) renderHelpOverlay(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(50)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true).Width(12) // Cyan, bold
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Bold(true)         // Medium gray, bold

	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\n\n")

	if m.viewMode == ViewModeJobs {
		b.WriteString(titleStyle.Render("Jobs View"))
		b.WriteString("\n")
		shortcuts := []struct{ key, desc string }{
			{"↑/↓", "Navigate job list"},
			{"space", "Page down job list"},
			{"b", "Page up job list"},
			{"t", "Jump to top of jobs"},
			{"←/→", "Switch to hosts view"},
			{"l", "Toggle logs view"},
			{"f", "Cycle job filters"},
			{"H", "Cycle host filter"},
			{"r", "Refresh job statuses"},
			{"n", "New job"},
			{"e", "Edit queued job"},
			{"R", "Restart job"},
			{"E", "Edit & restart job"},
			{"k", "Kill/cancel job"},
			{"p", "Pause running job"},
			{"d", "Toggle draft/queue status"},
			{"g", "Start queued/draft job now or resume paused"},
			{"G", "Generate AI description"},
			{"x", "Remove job from list"},
			{"P", "Prune completed/dead jobs"},
			{"Esc", "Clear selection/messages"},
		}
		for _, s := range shortcuts {
			b.WriteString(keyStyle.Render(s.key))
			b.WriteString(descStyle.Render(s.desc))
			b.WriteString("\n")
		}
	} else {
		b.WriteString(titleStyle.Render("Hosts View"))
		b.WriteString("\n")
		shortcuts := []struct{ key, desc string }{
			{"↑/↓", "Navigate host list"},
			{"←/→", "Switch to jobs view"},
			{"d", "Toggle AI summaries"},
			{"x", "Delete host"},
		}
		for _, s := range shortcuts {
			b.WriteString(keyStyle.Render(s.key))
			b.WriteString(descStyle.Render(s.desc))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(titleStyle.Render("General"))
	b.WriteString("\n")
	generalShortcuts := []struct{ key, desc string }{
		{"?", "Show/hide this help"},
		{"q", "Quit"},
		{"Ctrl+Z", "Suspend (fg to resume)"},
	}
	for _, s := range generalShortcuts {
		b.WriteString(keyStyle.Render(s.key))
		b.WriteString(descStyle.Render(s.desc))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Render("Press ? or Esc to close"))

	modal := modalStyle.Render(b.String())

	// Place modal centered
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
	)
}

func (m Model) renderInputForm(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(60)

	labelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("245"))
	focusedLabelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("69")).Bold(true)

	var b strings.Builder
	if m.editMode {
		b.WriteString(fmt.Sprintf("Edit Job %d\n\n", m.editingJobID))
	} else {
		b.WriteString("New Job\n\n")
	}

	labels := []string{"Host:", "Description:", "Command:", "Working Dir:", "GPU:", "CPU:", "Env Vars:"}
	for i, input := range m.inputs {
		label := labelStyle
		if i == m.inputFocus {
			label = focusedLabelStyle
		}
		b.WriteString(label.Render(labels[i]))
		b.WriteString(input.View())
		if i == inputCPUAllotment {
			if hint := m.cpuAllotmentHint(); hint != "" {
				b.WriteString(dimStyle.Render(" " + hint))
			}
		}
		b.WriteString("\n\n")
	}

	b.WriteString("\n")
	var helpText string
	if m.editMode {
		helpText = "Tab: next field • Enter: save changes • Esc: cancel"
	} else {
		helpText = "Tab: next field • Enter: create job • Esc: cancel"
	}
	if m.flashIsError && m.flashMessage != "" {
		helpText = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.flashMessage)
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(helpText))

	modal := modalStyle.Render(b.String())

	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) cpuAllotmentHint() string {
	hostName := strings.TrimSpace(m.inputs[inputHost].Value())
	if hostName == "" {
		return ""
	}
	host := m.findHostByName(hostName)
	if host == nil || host.CPUs == 0 {
		return ""
	}
	allotment, err := parseCPUAllotmentInput(m.inputs[inputCPUAllotment].Value())
	if err != nil {
		return "use 1-100 or default"
	}
	percent, ok := defaultAllotmentPercent(host)
	if !ok {
		return ""
	}
	if allotment != nil {
		percent = *allotment
	}
	cores := float64(host.CPUs) * float64(percent) / 100.0
	coreText := fmt.Sprintf("%.1f", cores)
	if rounded := math.Round(cores); math.Abs(cores-rounded) < 0.05 {
		coreText = fmt.Sprintf("%.0f", rounded)
	}
	return fmt.Sprintf("%d%% ≈ %s cores on %d-core host", percent, coreText, host.CPUs)
}

func (m Model) formatCPUAllotmentDisplay(job *db.Job) string {
	source := "default"
	if job.CPUAllotment != nil {
		source = "requested"
	}
	host := m.findHostByName(job.Host)
	if host == nil || host.CPUs == 0 {
		if source == "default" {
			return "default"
		}
		return fmt.Sprintf("%d%%", *job.CPUAllotment)
	}
	allotment := 0
	if job.CPUAllotment != nil {
		allotment = *job.CPUAllotment
	}
	if source == "default" {
		percent, ok := defaultAllotmentPercent(host)
		if !ok {
			return "default"
		}
		allotment = percent
	}
	cores := float64(host.CPUs) * float64(allotment) / 100.0
	coreText := fmt.Sprintf("%.1f", cores)
	if rounded := math.Round(cores); math.Abs(cores-rounded) < 0.05 {
		coreText = fmt.Sprintf("%.0f", rounded)
	}
	if source == "default" {
		return fmt.Sprintf("%d%% (%s cores on %d-core host, %s)", allotment, coreText, host.CPUs, source)
	}
	return fmt.Sprintf("%d%% (%s cores on %d-core host)", allotment, coreText, host.CPUs)
}

func (m Model) formatCPUUsageDisplay(job *db.Job) string {
	if job == nil || job.Metadata == nil || job.Metadata.CPU == nil {
		return ""
	}
	stats := job.Metadata.CPU
	parts := []string{}
	if job.Status == db.StatusRunning && stats.Latest != nil {
		parts = append(parts, fmt.Sprintf("latest %s", formatCPUPercent(*stats.Latest)))
	}
	if stats.Max != nil {
		parts = append(parts, fmt.Sprintf("max %s", formatCPUPercent(*stats.Max)))
	}
	if stats.Mean != nil {
		parts = append(parts, fmt.Sprintf("mean %s", formatCPUPercent(*stats.Mean)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func (m Model) findHostByName(name string) *Host {
	for _, host := range m.hosts {
		if host.Name == name {
			return host
		}
	}
	return nil
}

func (m Model) renderJobList(height int) string {
	panelWidth := m.width - 2
	if panelWidth < 0 {
		panelWidth = 0
	}
	frameWidth, frameHeight := listPanelStyle.GetFrameSize()
	contentWidth := panelWidth - frameWidth
	if contentWidth < 20 {
		contentWidth = 20
	}

	var rows []string

	hostSummary := m.renderInlineHostSummary(contentWidth)
	if hostSummary != "" {
		rows = append(rows, hostSummary)
	}

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %-4s %s",
		"ID", "HOST", "STATUS", "TIME", "GPU", "DESCRIPTION")
	headerIndex := len(rows)
	rows = append(rows, headerStyle.Render(header))
	filterLabel := fmt.Sprintf(" View: %s | Host: %s | Sort: %s", jobFilterDescription(m.jobFilter), hostFilterDescription(m), m.jobSort)
	filterLine := dimStyle.Render(filterLabel)

	if len(m.jobs) == 0 {
		rows = append(rows, dimStyle.Render(" No jobs match this view"))
		rows = append(rows, filterLine)
		content := strings.Join(rows, "\n")
		return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
	}

	// Render jobs manually using list's paginator for scroll offset
	contentTarget := height - frameHeight
	if contentTarget < 0 {
		contentTarget = 0
	}
	contentHeight := contentTarget - 2 // header + filter
	if hostSummary != "" {
		contentHeight--
	}
	if contentHeight < 0 {
		contentHeight = 0
	}
	start, end := m.jobList.Paginator.GetSliceBounds(len(m.jobs))
	if end > len(m.jobs) {
		end = len(m.jobs)
	}
	// Limit to visible height
	if end-start > contentHeight {
		end = start + contentHeight
	}

	// Check if any visible jobs have GPU or GPU class specified
	showGPU := false
	for i := start; i < end; i++ {
		if m.jobs[i].GetGPU() != "" || m.jobs[i].GPUClass != "" {
			showGPU = true
			break
		}
	}

	// Determine project column width based on available space
	projectWidth := 20
	if contentWidth < 100 {
		projectWidth = 12
	}
	if contentWidth < 80 {
		projectWidth = 8
	}
	if contentWidth < 60 {
		projectWidth = 4
	}

	// Update header based on GPU column visibility
	if showGPU {
		header := fmt.Sprintf("  %-4s %-10s %-*s %-12s %-12s %-4s %s",
			"ID", "HOST", projectWidth, "PROJECT", "STATUS", "TIME", "GPU", "DESCRIPTION")
		rows[headerIndex] = headerStyle.Render(header)
	} else {
		header := fmt.Sprintf("  %-4s %-10s %-*s %-12s %-12s %s",
			"ID", "HOST", projectWidth, "PROJECT", "STATUS", "TIME", "DESCRIPTION")
		rows[headerIndex] = headerStyle.Render(header)
	}

	selectedIdx := m.jobList.Index()
	jobLines := 0
	for i := start; i < end; i++ {
		job := m.jobs[i]
		status := m.formatStatus(job)
		timeCol := formatJobTime(job)
		hostCol := fmt.Sprintf("%-10s", truncate(job.Host, 10))
		statusCol := status + strings.Repeat(" ", max(0, 12-lipgloss.Width(status)))
		timeColFormatted := fmt.Sprintf("%-12s", timeCol)

		project := job.Project
		if project == "" {
			project = filepath.Base(job.EffectiveWorkingDir())
		}
		if project == "" || project == "." {
			project = "—"
		} else {
			project = abbreviateProject(project, projectWidth)
		}
		projectCol := fmt.Sprintf("%-*s", projectWidth, truncate(project, projectWidth))

		prefixPlain := fmt.Sprintf("  %-4d %s %s %s %s ", job.ID, hostCol, projectCol, statusCol, timeColFormatted)
		gpuField := ""
		if showGPU {
			gpu := job.GetGPU()
			if gpu == "" && job.GPUClass != "" {
				if job.Metadata != nil && job.Metadata.Resource != nil && job.Metadata.Resource.GPUDevices != "" {
					gpu = job.Metadata.Resource.GPUDevices
				} else {
					gpu = job.GPUClass
				}
			}
			if gpu == "" {
				gpu = "—"
			}
			gpuField = fmt.Sprintf("%-4s ", truncate(gpu, 4))
			prefixPlain += gpuField
		}

		descWidth := contentWidth - lipgloss.Width(prefixPlain)
		if descWidth < 5 {
			descWidth = 5
		}
		display := truncate(job.EffectiveDescription(), descWidth)
		generatedDesc := job.Description == "" && job.GeneratedDescription != ""

		rowStyle := m.styleForJob(job)
		selected := i == selectedIdx && m.jobSelectionActive
		if selected {
			rowStyle = rowStyle.Copy().Background(selectedBg)
		}

		// Processed indicator: ✓ for processed jobs, space otherwise
		indicator := " "
		if job.HasTag(db.ProcessedTag) {
			indicator = dimStyle.Render("✓")
		}
		indicatorSegment := rowStyle.Render(indicator)

		idSegment := rowStyle.Render(fmt.Sprintf("%-4d ", job.ID))
		hostSegment := rowStyle.Render(fmt.Sprintf("%s ", hostCol))
		projectSegment := rowStyle.Render(fmt.Sprintf("%s ", projectCol))
		statusSegment := rowStyle.Render(fmt.Sprintf("%s ", statusCol))
		timeSegment := rowStyle.Render(fmt.Sprintf("%s ", timeColFormatted))

		segments := []string{indicatorSegment, idSegment, hostSegment, projectSegment, statusSegment, timeSegment}
		if showGPU {
			segments = append(segments, rowStyle.Render(gpuField))
		}

		descStyle := rowStyle
		if generatedDesc {
			descStyle = descStyle.Copy().Italic(true)
		}
		segments = append(segments, descStyle.Render(display))

		line := lipgloss.JoinHorizontal(lipgloss.Left, segments...)
		lineWidth := lipgloss.Width(line)
		if lineWidth < contentWidth {
			line += rowStyle.Render(strings.Repeat(" ", contentWidth-lineWidth))
		}
		rows = append(rows, line)
		jobLines++
	}

	if jobLines < contentHeight {
		blank := strings.Repeat(" ", contentWidth)
		for i := jobLines; i < contentHeight; i++ {
			rows = append(rows, blank)
		}
	}

	rows = append(rows, filterLine)

	if len(rows) < contentTarget {
		pad := strings.Repeat(" ", contentWidth)
		for len(rows) < contentTarget {
			rows = append(rows, pad)
		}
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

func (m Model) renderInlineHostSummary(maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	// Filter to recently-synced hosts
	var recentHosts []*Host
	for _, host := range m.hosts {
		if host != nil && m.isHostRecentlySynced(host.Name) {
			recentHosts = append(recentHosts, host)
		}
	}

	if len(recentHosts) == 0 {
		if len(m.hosts) == 0 {
			return dimStyle.Render(" Hosts: no hosts configured")
		}
		return dimStyle.Render(" Hosts: no host data")
	}

	hostCount := len(recentHosts)
	label := labelStyle.Render("Hosts")
	labelWidth := lipgloss.Width(label)
	separatorWidth := 2 * (hostCount - 1)
	segmentWidthAvailable := maxWidth - labelWidth - 1
	if segmentWidthAvailable <= 0 {
		return truncate(label, maxWidth)
	}

	// Select format based on available width
	format := hostSummaryFull
	widthPerHost := (segmentWidthAvailable - separatorWidth) / hostCount
	if widthPerHost < hostSummaryFullWidth {
		format = hostSummaryAbbrev
	}

	segments := make([]string, 0, hostCount)
	for _, host := range recentHosts {
		segments = append(segments, m.renderHostSummarySegment(host, format))
	}

	segmentsText := strings.Join(segments, "  ")
	content := lipgloss.JoinHorizontal(
		lipgloss.Left,
		label,
		" ",
		segmentsText,
	)

	if lipgloss.Width(content) <= maxWidth {
		return content
	}

	tickerBase := segmentsText + hostSummaryTickerGap
	tickerWidth := lipgloss.Width(tickerBase)
	if tickerWidth == 0 {
		return truncate(content, maxWidth)
	}

	offset := m.hostSummaryTickerOffset % tickerWidth
	end := offset + segmentWidthAvailable
	segmentView := ""
	if end <= tickerWidth {
		segmentView = ansi.Cut(tickerBase, offset, end)
	} else {
		segmentView = ansi.Cut(tickerBase, offset, tickerWidth) + ansi.Cut(tickerBase, 0, end-tickerWidth)
	}

	viewWidth := lipgloss.Width(segmentView)
	if viewWidth < segmentWidthAvailable {
		segmentView += strings.Repeat(" ", segmentWidthAvailable-viewWidth)
	}

	return lipgloss.JoinHorizontal(
		lipgloss.Left,
		label,
		" ",
		segmentView,
	)
}

func (m Model) renderLogPanel(height int) string {
	// Render based on active tab
	switch m.detailTab {
	case DetailTabLogs:
		return m.renderLogsOnly(height)
	case DetailTabCPU:
		return m.renderJobCPUTop(height)
	default:
		return m.renderJobDetails(height)
	}
}

// renderTabHeader renders the "Details  Logs" tab header as visual tabs
func (m Model) renderTabHeader() string {
	tabs := []struct {
		label string
		tab   DetailTab
	}{
		{"Details", DetailTabDetails},
		{"Logs", DetailTabLogs},
		{"CPU", DetailTabCPU},
	}

	var rendered []string
	for _, t := range tabs {
		if m.detailTab == t.tab {
			rendered = append(rendered, activeTabStyle.Render(t.label))
		} else {
			rendered = append(rendered, inactiveTabStyle.Render(t.label))
		}
	}

	// Add hint about Tab key switching
	hint := dimStyle.Render(" (Tab to switch)")

	return strings.Join(rendered, " ") + hint
}

func (m Model) renderLogsOnly(height int) string {
	job := m.selectedJob
	var content string
	var staleIndicator string

	// Handle case where no job is selected yet
	if job == nil {
		job = m.getTargetJob()
		if job == nil {
			panelContent := m.renderTabHeader() + "\n" + dimStyle.Render("No job selected")
			return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
		}
	}

	// Calculate viewport dimensions (account for borders, title, tab header, padding)
	viewportHeight := height - 5
	viewportWidth := m.width - 6 // Account for panel borders and padding
	if m.logStale {
		viewportHeight -= 1 // Make room for stale indicator
	}

	if m.logLoading {
		content = dimStyle.Render(m.spinner.View() + " Loading logs...")
	} else if m.logContent == "" {
		content = dimStyle.Render("No log content available")
	} else {
		// Create viewport with correct dimensions and content for rendering
		vp := m.logViewport
		vp.Width = viewportWidth
		vp.Height = viewportHeight
		vp.SetContent(m.logContent)

		// Use viewport for scrollable content
		if m.logStale {
			// Use slightly dimmer style for stale content
			staleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
			content = staleStyle.Render(vp.View())
		} else {
			content = vp.View()
		}
	}

	jobInfo := fmt.Sprintf("Job %d on %s", job.ID, job.Host)
	if m.logStale {
		staleIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render(" (cached - host offline)")
	}

	// Show scroll position if there's more content
	scrollInfo := ""
	totalLines := strings.Count(m.logContent, "\n") + 1
	if totalLines > viewportHeight && viewportHeight > 0 {
		scrollInfo = fmt.Sprintf(" [%d/%d]", m.logViewport.YOffset+viewportHeight, totalLines)
	}

	panelContent := m.renderTabHeader() + "\n" + dimStyle.Render(jobInfo) + staleIndicator + scrollInfo + "\n" + content
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderJobCPUTop(height int) string {
	job := m.getTargetJob()
	panelHeader := m.renderTabHeader() + "\n"
	if job == nil {
		panelContent := panelHeader + dimStyle.Render("No job selected")
		return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
	}

	var b strings.Builder
	host := job.Host
	b.WriteString(fmt.Sprintf("Top CPU processes on %s\n\n", host))

	switch {
	case m.jobCPUTopLoading && (m.jobCPUTopRequestedHost == host):
		b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		b.WriteString("\n")
	case m.jobCPUTopError != "" && m.jobCPUTopDataHost == host:
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed to fetch: %s", m.jobCPUTopError)))
		b.WriteString("\n")
	case m.jobCPUTopDataHost == host && len(m.jobCPUTopEntries) > 0:
		userWidth := 10
		cpuWidth := 6
		panelWidth := m.width - 6
		if panelWidth < 40 {
			panelWidth = 40
		}
		processWidth := panelWidth - (userWidth + cpuWidth + 4)
		if processWidth < 20 {
			processWidth = 20
		}

		header := fmt.Sprintf(" %-*s %*s %s", userWidth, "User", cpuWidth, "%CPU", "Process")
		b.WriteString(lipgloss.NewStyle().Bold(true).Render(header))
		b.WriteString("\n")

		availableLines := height - 4
		maxRows := len(m.jobCPUTopEntries)
		if availableLines > 0 && maxRows > availableLines-2 {
			maxRows = availableLines - 2
		}
		if maxRows < 0 {
			maxRows = 0
		}

		totalCPU := 0.0
		for i, proc := range m.jobCPUTopEntries {
			if i >= maxRows {
				break
			}
			process := proc.Command
			if process == "" {
				process = fmt.Sprintf("PID %d", proc.PID)
			}
			process = truncate(shortenCommandPath(process), processWidth)
			line := fmt.Sprintf(" %-*s %*.1f %s",
				userWidth, truncate(proc.User, userWidth),
				cpuWidth, proc.CPU,
				process,
			)
			b.WriteString(line)
			b.WriteString("\n")
			totalCPU += proc.CPU
		}

		coreEstimate := ""
		if totalCPU > 0 {
			coreEstimate = fmt.Sprintf("(~%.0f cores)", totalCPU/100.0)
		}
		totalLine := fmt.Sprintf(" %-*s %*.1f %s",
			userWidth, "TOTAL",
			cpuWidth, totalCPU,
			dimStyle.Render(coreEstimate),
		)
		b.WriteString(totalLine)
		b.WriteString("\n")

		remaining := len(m.jobCPUTopEntries) - maxRows
		if remaining > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			b.WriteString("\n")
		}

		if !m.jobCPUTopUpdated.IsZero() {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("Updated %s ago", time.Since(m.jobCPUTopUpdated).Truncate(time.Second))))
		}
	case m.jobCPUTopDataHost == host && len(m.jobCPUTopEntries) == 0:
		b.WriteString(dimStyle.Render("No active processes reported"))
		b.WriteString("\n")
	default:
		if m.jobCPUTopRequestedHost != "" {
			b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		} else {
			b.WriteString(dimStyle.Render("Select a job to load CPU data"))
		}
		b.WriteString("\n")
	}

	panelContent := panelHeader + b.String()
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderJobDetails(height int) string {
	if !m.jobSelectionActive {
		if m.detailViewport != nil {
			m.detailViewport.SetContent("")
		}
		return m.renderJobSummaryPanel(height)
	}

	panelHeader := m.renderTabHeader()
	job := m.getTargetJob()

	if job == nil {
		emptyContent := dimStyle.Render("No jobs to display")
		if m.detailViewport != nil {
			m.detailViewport.SetContent(emptyContent)
		}
		panelContent := panelHeader + "\n" + emptyContent
		return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
	}

	viewportWidth := m.width - 6
	if viewportWidth < 1 {
		viewportWidth = 1
	}
	viewportHeight := height - 4
	if viewportHeight < 1 {
		viewportHeight = 1
	}

	detailBody := m.jobDetailContent(job)
	m.detailViewport.SetContent(detailBody)

	vp := *m.detailViewport
	vp.Width = viewportWidth
	vp.Height = viewportHeight

	body := vp.View()
	if body == "" {
		body = dimStyle.Render("No jobs to display")
	}

	panelContent := panelHeader + "\n" + body
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) jobDetailContent(job *db.Job) string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Width(11)
	valueStyle := lipgloss.NewStyle()
	headerStyle := lipgloss.NewStyle().Bold(true)
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Underline(true)

	// Header line with job ID and host (progress shown in Progress section)
	b.WriteString(headerStyle.Render(fmt.Sprintf("Job %d", job.ID)))
	b.WriteString(dimStyle.Render(" on "))
	b.WriteString(headerStyle.Render(job.Host))

	// Show dependency info on same line if present
	if depSpec := m.jobDependencies[job.ID]; depSpec != "" {
		if strings.HasSuffix(depSpec, "+") {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s)", strings.TrimSuffix(depSpec, "+"))))
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s succeeds)", depSpec)))
		}
	}
	b.WriteString("\n\n")

	// Description (if any)
	if job.Description != "" {
		b.WriteString(labelStyle.Render("Desc"))
		descStyle := lipgloss.NewStyle().Italic(true)
		b.WriteString(descStyle.Render(job.Description))
		b.WriteString("\n")
	}
	if len(job.Tags) > 0 {
		b.WriteString(labelStyle.Render("Tags"))
		b.WriteString(valueStyle.Render(strings.Join(job.Tags, ", ")))
		b.WriteString("\n")
	}

	// Directory (show remote home when unset)
	b.WriteString(labelStyle.Render("Directory"))
	b.WriteString(valueStyle.Render(job.DisplayWorkingDir()))
	b.WriteString("\n")

	// Command (most important) - wrap and indent continuation lines, but limit height
	b.WriteString(labelStyle.Render("Command"))
	cmd := job.EffectiveCommand()
	labelWidth := 11
	// Panel content width is m.width - 6 (borders + padding), minus label
	availableWidth := m.width - 6 - labelWidth
	if availableWidth < 20 {
		availableWidth = 60 // fallback if window too narrow
	}
	// Wrap first (on plain text), then apply syntax highlighting to each line
	wrappedCmd := wrapTextWithIndent(cmd, availableWidth, labelWidth)
	// Limit command display to 4 lines max to avoid overwhelming the details panel
	const maxCmdLines = 4
	cmdLines := strings.Split(wrappedCmd, "\n")
	if len(cmdLines) > maxCmdLines {
		cmdLines = cmdLines[:maxCmdLines]
		cmdLines = append(cmdLines, strings.Repeat(" ", labelWidth)+"...")
	}
	// Apply syntax highlighting to each line
	for i, line := range cmdLines {
		cmdLines[i] = highlightCommand(line)
	}
	b.WriteString(strings.Join(cmdLines, "\n"))
	b.WriteString("\n")

	// Environment variables (if any)
	envVars := job.ParseExportVars()
	if len(envVars) > 0 {
		b.WriteString(labelStyle.Render("Env"))
		b.WriteString(valueStyle.Render(strings.Join(envVars, ", ")))
		b.WriteString("\n")
	}

	if job.Status == db.StatusQueued || job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
		b.WriteString(labelStyle.Render("CPU"))
		b.WriteString(valueStyle.Render(m.formatCPUAllotmentDisplay(job)))
		b.WriteString("\n")
	}
	if cpuUsage := m.formatCPUUsageDisplay(job); cpuUsage != "" {
		b.WriteString(labelStyle.Render("CPU Usage"))
		b.WriteString(valueStyle.Render(cpuUsage))
		b.WriteString("\n")
	}

	_ = m.writeJobTimingSection(&b, job, labelStyle, valueStyle)

	// Exit status
	if job.Status == db.StatusCompleted && job.ExitCode != nil {
		b.WriteString(labelStyle.Render("Exit"))
		if *job.ExitCode == 0 {
			b.WriteString(completedStyle.Render("0 (success)"))
		} else {
			b.WriteString(failedStyle.Render(fmt.Sprintf("%d (failed)", *job.ExitCode)))
		}
		b.WriteString("\n")
	} else if job.Status == db.StatusKilled {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(deadStyle.Render("killed"))
		b.WriteString("\n")
	} else if job.Status == db.StatusCanceled {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(deadStyle.Render("canceled"))
		b.WriteString("\n")
	} else if job.Status == db.StatusDead {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(failedStyle.Render("failed to start"))
		b.WriteString("\n")
		if job.ErrorMessage != "" {
			b.WriteString(labelStyle.Render("Error"))
			b.WriteString(errorStyle.Render(job.ErrorMessage))
			b.WriteString("\n")
		}
	} else if job.Status == db.StatusFailed {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(failedStyle.Render("crashed"))
		b.WriteString("\n")
	}

	// Resource usage section for completed/failed jobs
	if job.Metadata != nil && job.Metadata.Resource != nil {
		r := job.Metadata.Resource
		hasData := r.UserCPUSecs != nil || r.PeakRSSKB != nil || r.MaxGPUMemMiB != nil
		if hasData {
			b.WriteString("\n")
			b.WriteString(sectionStyle.Render("Resource Usage"))
			b.WriteString("\n")
			if r.UserCPUSecs != nil || r.SysCPUSecs != nil {
				userStr := "0s"
				sysStr := "0s"
				if r.UserCPUSecs != nil {
					userStr = db.FormatDuration(int64(*r.UserCPUSecs))
				}
				if r.SysCPUSecs != nil {
					sysStr = db.FormatDuration(int64(*r.SysCPUSecs))
				}
				b.WriteString(labelStyle.Render("CPU Time"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%s user, %s sys", userStr, sysStr)))
				b.WriteString("\n")
			}
			if r.PeakRSSKB != nil {
				b.WriteString(labelStyle.Render("Peak Memory"))
				b.WriteString(valueStyle.Render(tuiFormatMemoryKB(*r.PeakRSSKB)))
				b.WriteString("\n")
			}
			if r.MaxGPUMemMiB != nil {
				b.WriteString(labelStyle.Render("GPU Memory"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%d MiB (peak)", *r.MaxGPUMemMiB)))
				b.WriteString("\n")
			}
		}
	}

	// Process stats and progress section for running jobs
	// Reserve a fixed number of lines to prevent log preview from jumping
	if job.Status == db.StatusRunning {
		const statsReservedLines = 7 // header + CPU + Memory + Threads + 2 GPUs + Progress
		linesWritten := 0

		b.WriteString("\n")
		b.WriteString(sectionStyle.Render("Process Stats"))
		b.WriteString("\n")
		linesWritten++ // header

		if m.processStats != nil && m.processStatsJobID == job.ID {
			// CPU: percentage and cumulative time
			if m.processStats.CPUPct > 0 || m.processStats.CPUUser != "" {
				b.WriteString(labelStyle.Render("CPU"))
				cpuVal := ""
				if m.processStats.CPUPct > 0 {
					cpuVal = fmt.Sprintf("%.1f%%", m.processStats.CPUPct)
				}
				if m.processStats.CPUUser != "" {
					if cpuVal != "" {
						cpuVal += " "
					}
					cpuVal += fmt.Sprintf("(%s user, %s sys)", m.processStats.CPUUser, m.processStats.CPUSys)
				}
				b.WriteString(valueStyle.Render(cpuVal))
				b.WriteString("\n")
				linesWritten++
			}

			// Memory: absolute and percentage
			if m.processStats.MemoryRSS != "" {
				b.WriteString(labelStyle.Render("Memory"))
				mem := m.processStats.MemoryRSS
				if m.processStats.MemoryPct != "" {
					mem += " (" + m.processStats.MemoryPct + ")"
				}
				b.WriteString(valueStyle.Render(mem))
				b.WriteString("\n")
				linesWritten++
			}

			// Threads
			if m.processStats.Threads > 0 {
				b.WriteString(labelStyle.Render("Threads"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%d", m.processStats.Threads)))
				b.WriteString("\n")
				linesWritten++
			}

			// GPUs with utilization and memory
			if len(m.processStats.GPUs) > 0 {
				for _, gpu := range m.processStats.GPUs {
					b.WriteString(labelStyle.Render(fmt.Sprintf("GPU %d", gpu.Index)))
					gpuVal := ""
					if gpu.Utilization > 0 {
						gpuVal += fmt.Sprintf("%d%% util, ", gpu.Utilization)
					}
					gpuVal += gpu.MemUsed
					b.WriteString(valueStyle.Render(gpuVal))
					b.WriteString("\n")
					linesWritten++
				}
			}
		} else {
			// Stats not loaded yet - show placeholder
			b.WriteString(dimStyle.Render("Loading..."))
			b.WriteString("\n")
			linesWritten++
		}

		// Progress (on same reserved block)
		if prog, ok := m.jobProgress[job.ID]; ok {
			pct := prog.DisplayPercent()
			if pct >= 0 || prog.Total > 0 {
				b.WriteString(labelStyle.Render("Progress"))
				b.WriteString("  ")
				if pct >= 0 {
					b.WriteString(renderProgressBar(pct, 20))
					b.WriteString(fmt.Sprintf(" %d%%", pct))
				}
				if prog.Total > 0 {
					b.WriteString(fmt.Sprintf(" (%d/%d)", prog.Current, prog.Total))
				}
				b.WriteString("\n")
				linesWritten++
			}
		}

		// Pad with blank lines to reach reserved count
		for linesWritten < statsReservedLines {
			b.WriteString("\n")
			linesWritten++
		}
	}

	// Log preview section - show last few lines of log if available
	logPreview := ""
	if m.logContent != "" {
		logPreview = m.logContent
	} else if cached, ok := m.logCache[job.ID]; ok {
		logPreview = cached
	}
	if logPreview != "" {
		logPreview = processCarriageReturns(logPreview)

		const maxPreviewLines = 5
		allLines := strings.Split(strings.TrimSpace(logPreview), "\n")
		var previewLines []string
		for i := len(allLines) - 1; i >= 0 && len(previewLines) < maxPreviewLines; i-- {
			line := strings.TrimSpace(allLines[i])
			if line != "" {
				previewLines = append([]string{line}, previewLines...)
			}
		}

		if len(previewLines) > 0 {
			b.WriteString("\n")
			b.WriteString(sectionStyle.Render("Log (last lines)"))
			b.WriteString("\n")
			for _, line := range previewLines {
				// Truncate long lines to fit panel width
				maxWidth := m.width - 10
				if maxWidth < 40 {
					maxWidth = 40
				}
				if len(line) > maxWidth {
					line = line[:maxWidth-3] + "..."
				}
				b.WriteString(dimStyle.Render(line))
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

func (m Model) writeJobTimingSection(b *strings.Builder, job *db.Job, labelStyle, valueStyle lipgloss.Style) bool {
	wrote := false
	if job.CreatedAt > 0 || job.StartTime > 0 || job.EndTime != nil {
		if job.CreatedAt > 0 && (job.StartTime == 0 || job.StartTime-job.CreatedAt > 60) {
			createdTime := time.Unix(job.CreatedAt, 0)
			label := "Created"
			if job.Status == db.StatusQueued {
				label = "Queued"
			}
			b.WriteString(labelStyle.Render(label))
			b.WriteString(valueStyle.Render(formatDetailTimestamp(createdTime)))
			b.WriteString("\n")
			wrote = true
		}

		startTimestamp := job.StartTime
		if startTimestamp == 0 && job.CreatedAt > 0 && db.IsTerminalStatus(job.Status) {
			startTimestamp = job.CreatedAt
		}

		if startTimestamp > 0 {
			startTime := time.Unix(startTimestamp, 0)
			var endTime time.Time
			hasEnd := job.EndTime != nil
			if hasEnd {
				endTime = time.Unix(*job.EndTime, 0)
			}

			sameDay := hasEnd && sameLocalDay(startTime, endTime)

			if sameDay {
				startStr, suffix := formatDetailTimeParts(startTime)
				endStr, _ := formatDetailTimeParts(endTime)
				line := fmt.Sprintf("%s – %s%s", startStr, endStr, suffix)
				b.WriteString(labelStyle.Render("Start/End"))
				b.WriteString(valueStyle.Render(line))
				b.WriteString("\n")
			} else {
				b.WriteString(labelStyle.Render("Started"))
				b.WriteString(valueStyle.Render(formatDetailTimestamp(startTime)))
				b.WriteString("\n")

				if hasEnd {
					b.WriteString(labelStyle.Render("Ended"))
					b.WriteString(valueStyle.Render(formatDetailTimestamp(endTime)))
					b.WriteString("\n")
				}
			}
			wrote = true

			if job.Status == db.StatusRunning {
				elapsed := time.Since(startTime)
				b.WriteString(labelStyle.Render("Elapsed"))
				b.WriteString(valueStyle.Render(formatDuration(elapsed)))
				b.WriteString("\n")
			} else if hasEnd {
				duration := endTime.Sub(startTime)
				b.WriteString(labelStyle.Render("Duration"))
				b.WriteString(valueStyle.Render(formatDuration(duration)))
				b.WriteString("\n")
			}
		} else if job.EndTime != nil {
			endTime := time.Unix(*job.EndTime, 0)
			b.WriteString(labelStyle.Render("Ended"))
			b.WriteString(valueStyle.Render(formatDetailTimestamp(endTime)))
			b.WriteString("\n")
			wrote = true
		}
	}
	return wrote
}

func (m Model) renderJobSummaryPanel(height int) string {
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Underline(true)
	headerStyle := lipgloss.NewStyle().Bold(true)

	var b strings.Builder
	b.WriteString(sectionStyle.Render("Hosts Overview"))
	b.WriteString("\n")

	if len(m.hosts) == 0 {
		b.WriteString(dimStyle.Render("No hosts have reported yet. Run a job to gather host data."))
	} else {
		header := fmt.Sprintf(" %-12s %-10s %-16s %-5s %-7s %-6s %-5s %-5s",
			"HOST", "STATUS", "ARCH", "RUN", "QUEUE", "LOAD", "CPU", "RAM")
		b.WriteString(headerStyle.Render(header))
		b.WriteString("\n")

		for _, host := range m.hosts {
			running, _ := m.jobCountsForHost(host.Name)
			load := host.LoadAvgShort()
			if load == "" {
				load = "-"
			}
			line := fmt.Sprintf(" %-12s %-10s %-16s %-5d %-7s %-6s %-5s %-5s",
				truncate(host.Name, 12),
				strings.TrimSpace(m.formatHostStatus(host)),
				truncate(host.Arch, 16),
				running,
				m.queueSummaryForHost(host),
				load,
				host.CPUUtilization(),
				host.RAMUtilization(),
			)
			b.WriteString(m.styleForHostStatus(host.Status).Render(line))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(sectionStyle.Render("System Summary"))
	b.WriteString("\n")
	b.WriteString(m.renderSystemSummaryLines(m.width - 6))

	return logPanelStyle.Width(m.width - 2).Height(height).Render(b.String())
}

func (m Model) renderSystemSummaryLines(width int) string {
	if width < 20 {
		width = 20
	}
	if len(m.hosts) == 0 {
		return dimStyle.Render("No hosts have reported yet.")
	}
	if !m.showHostSummaries {
		return dimStyle.Render("AI host summaries are disabled (press d in Hosts view to enable).")
	}
	if m.llmGenerator == nil {
		return dimStyle.Render("Host summaries unavailable (LLM generator not running).")
	}
	var lines []string
	for _, host := range m.hosts {
		summary := strings.TrimSpace(m.hostSummaries[host.Name])
		switch {
		case summary == "" && m.hostSummaryPending[host.Name]:
			summary = "Generating summary..."
		case summary == "":
			summary = "No summary available yet."
		}
		wrapped := wrapText(summary, width-4)
		segments := strings.Split(wrapped, "\n")
		for i, segment := range segments {
			prefix := "  "
			if i == 0 {
				prefix = fmt.Sprintf("%s ", lipgloss.NewStyle().Bold(true).Render(truncate(host.Name, 12)))
			}
			lines = append(lines, prefix+segment)
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderFlash() string {
	if m.flashMessage == "" {
		return ""
	}

	// Style for flash message box
	var style lipgloss.Style
	if m.flashIsError {
		style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).  // White text
			Background(lipgloss.Color("124")). // Dark red background
			Bold(true).
			Padding(0, 1)
	} else {
		style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).  // White text
			Background(lipgloss.Color("240")). // Dark gray background
			Padding(0, 1)
	}

	return " " + style.Render(m.flashMessage)
}

func (m Model) renderStatusBar() string {
	help := helpStyle.Render("?:help q:quit ↑/↓:nav space/b/t:page ←/→:views l:logs f:filter H:host o:sort r:refresh n:new e:edit R:restart k:kill d:draft P:prune")

	// Right-align the help text
	gap := m.width - lipgloss.Width(help) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + strings.Repeat(" ", gap) + help
}
