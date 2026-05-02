package terminal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/util"
)

// renderStructuredBlock renders an instance block's structured lines, making
// the instance header and job rows selectable while other lines are plain.
func renderStructuredBlock(lines []watchInstanceLine, addSelectable, addPlain func(string)) {
	if len(lines) == 0 {
		return
	}
	addSelectable(lines[0].text)
	for _, sl := range lines[1:] {
		if sl.jobID != 0 {
			addSelectable(sl.text)
		} else {
			addPlain(sl.text)
		}
	}
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m watchModel) View() string {
	if m.movePicker.active {
		return m.movePicker.View(m.width, m.height)
	}
	if m.projectHelp && m.mode != watchModeProject {
		return m.renderWatchHelpView()
	}

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
	hasVisibleInstances := false
	for _, id := range m.instanceIDs {
		if !m.cachedHiddenIDs[id] {
			hasVisibleInstances = true
			break
		}
	}
	showInstanceHeader := hasVisibleInstances || m.done
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
	if showInstanceHeader && m.campaignID > 0 {
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
	if showInstanceHeader {
		if summary := formatWatchSummaryLine(m.launchedAt, m.instanceViews(), now, m.estimateSummaryLine); summary != "" {
			addLine(summary)
			addLine("")
		}
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
				structured := formatWatchInstanceBlockStructured(update, nil, watchInstanceBlockOptions{
					spinner:                  m.spinner.View(),
					showSpinnerIfNonTerminal: true,
					resolvedJobsOverride:     &resolved,
					dimJobStatuses:           true,
					predecessors:             donors,
					replacementReason:        m.failedReplaceReason[id],
				})
				renderStructuredBlock(structured, addSelectable, addLine)
			} else {
				addLine(m.spinner.View() + fmt.Sprintf(" Instance %s — waiting for data...", ids.FormatInstanceID(id)))
			}
			addLine("")
			continue
		}

		structured := formatWatchInstanceBlockStructured(u, m.jobProgressHWM, watchInstanceBlockOptions{
			predecessors:      donors,
			replacementReason: m.failedReplaceReason[id],
		})
		renderStructuredBlock(structured, addSelectable, addLine)
		addLine("")
	}

	if len(m.onPremHosts) > 0 {
		projectWidth := len("PROJECT")
		for _, host := range m.onPremHosts {
			for _, job := range host.Jobs {
				if w := len(campaign.JobProjectLabel(job)); w > projectWidth {
					projectWidth = w
				}
			}
		}
		addLine(watchTitleStyle.Render(fmt.Sprintf("Inventory Hosts (%d active)", len(m.onPremHosts))))
		for _, host := range m.onPremHosts {
			header := "  " + host.Name
			if summary := summarizeOnPremHostBlock(host); summary != "" {
				header += "  " + watchDimStyle.Render(truncate(summary, max(width-len(host.Name)-6, 20)))
			}
			addLine(watchStatusStyle.Render(header))
			for _, job := range host.Jobs {
				addSelectable("    " + truncate(m.formatOnPremJobRow(job, projectWidth), max(width-4, 40)))
			}
		}
		addLine("")
	}

	// Unplaced jobs section (instance-based modes)
	if !m.launchPending && len(m.unplacedJobs) > 0 {
		addLine(watchTitleStyle.Render(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs))))
		formattedRows := m.formatUnplacedJobRows(m.unplacedJobs, max(width-4, 40))
		for i := range m.unplacedJobs {
			addSelectable("  " + truncate(formattedRows[i], max(width-2, 40)))
		}
		addLine("")
	}

	// Partial launch errors
	if len(m.partialErrors) > 0 && !m.partialErrorsRetried {
		block := formatPartialErrors(m.partialErrors, width)
		b.WriteString(block)
		b.WriteString("\n")
		lineCount += strings.Count(block, "\n") + 1
	}

	// Retry status
	if m.retrying {
		addLine(m.spinner.View() + fmt.Sprintf(" Retrying %d retryable failed instance(s)...", m.countRetryableFailedInstances()))
	} else if m.retryResult != "" {
		addLine(m.retryResult)
	}
	for _, line := range renderSharedTUIStatusLines(m.database, width, m.autoRunRateTargetCents) {
		addLine(line)
	}
	if line := m.autoPilotStatusLine(); line != "" {
		addLine(watchDimStyle.Render(line))
	}

	if !m.done && !m.launchPending {
		hint := "u unplace  x kill  t terminate  s submit  m move  J jobs  U grouped jobs  ? help  q quit (instances run in background)"
		if !m.retrying && m.hasRetryableFailures() {
			hint = "u unplace  x kill  t terminate  s submit  m move  r retry  B budget+retry  J jobs  U grouped jobs  ? help  q quit (instances run in background)"
		} else if len(m.unplacedJobs) > 0 {
			hint = "u unplace  x kill  t terminate  s submit  m move  l launch  J jobs  U grouped jobs  ? help  q quit (instances run in background)"
		}
		hint += "  " + m.autoModeHint()
		addLine(watchDimStyle.Render(hint))
	}
	if m.hasPreservedJobAttachment() {
		addLine(watchDimStyle.Render(degraded.TerminalJobAttachmentDegradedNote()))
	}

	return b.String(), selectedVisualLine
}

func (m watchModel) renderSystemView() (string, int) {
	width := m.width
	if width <= 0 {
		width = 100
	}
	sharedStatusLines := renderSharedTUIStatusLines(m.database, width, m.autoRunRateTargetCents)

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
	if m.cloudReason != "" {
		addPlain(watchDimStyle.Render("  note: " + m.cloudReason))
	}
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
			structured := formatWatchInstanceBlockStructured(update, m.jobProgressHWM, watchInstanceBlockOptions{
				predecessors:      donors,
				replacementReason: m.failedReplaceReason[ci.ID],
			})
			if len(structured) == 0 {
				continue
			}
			truncSelect := func(s string) { addSelectable(truncate(s, width)) }
			truncPlain := func(s string) { addPlain(truncate(s, width)) }
			renderStructuredBlock(structured, truncSelect, truncPlain)
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
		formattedRows := m.formatUnplacedJobRows(m.unplacedJobs, max(width-4, 40))
		for i := range m.unplacedJobs {
			addSelectable("  " + truncate(formattedRows[i], width-2))
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
	if m.hasPreservedJobAttachment() {
		footerParts = append(footerParts, watchDimStyle.Render(degraded.TerminalJobAttachmentDegradedFooter()))
	}
	// Retry status in footer for system mode
	if m.retrying {
		footerParts = append(footerParts, m.spinner.View()+fmt.Sprintf(" Retrying %d retryable failed instance(s)...", m.countRetryableFailedInstances()))
	} else if m.retryResult != "" {
		footerParts = append(footerParts, m.retryResult)
	}

	controls := "[^u/^d] page  [u] unplace  [x] kill  [t] terminate  [s] submit  [m] move  [l] launch  [J] jobs  [U] grouped jobs  [r] retry  [B] budget+retry  [?] help  [q] quit"
	if !m.hasRetryableFailures() {
		controls = "[^u/^d] page  [u] unplace  [x] kill  [t] terminate  [s] submit  [m] move  [l] launch  [J] jobs  [U] grouped jobs  [?] help  [q] quit"
	}
	controls += "  " + m.autoModeHint()
	footerParts = append(footerParts, watchDimStyle.Render(controls))

	// Render rows into content string
	// Reserve: one blank separator + footer controls line + status/footer lines.
	reservedFooterLines := 2 + len(sharedStatusLines)
	if m.autoPilotStatusLine() != "" {
		reservedFooterLines++
	}
	contentHeight := m.height - reservedFooterLines
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
	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if line := m.autoPilotStatusLine(); line != "" {
		b.WriteString(watchDimStyle.Render(line))
		b.WriteString("\n")
	}
	footer := strings.Join(footerParts, "  ")
	b.WriteString(footer)

	// For system mode, scrolling is done via watchScrollStart above, so we
	// return -1 to skip applyViewport's cursor-based slicing.
	return b.String(), -1
}

func (m watchModel) hasPreservedJobAttachment() bool {
	for _, preserved := range m.preservedJobAttachment {
		if preserved {
			return true
		}
	}
	return false
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

// summarizeOnPremHostBlock returns a compact host-level note about why nothing
// is starting on the host, derived from queued jobs' QueueBlockedReason. Only
// emits when no job on the host is currently running (running jobs already
// communicate progress).
func summarizeOnPremHostBlock(host onPremHostSummary) string {
	queued := 0
	running := 0
	counts := make(map[string]int)
	for _, job := range host.Jobs {
		if job == nil {
			continue
		}
		if isRunningOnPremJob(job) {
			running++
			continue
		}
		if job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		queued++
		reason := strings.TrimSpace(job.QueueBlockedReason)
		if reason == "" {
			continue
		}
		counts[reason]++
	}
	if running > 0 || queued == 0 || len(counts) == 0 {
		return ""
	}
	type kv struct {
		reason string
		n      int
	}
	ranked := make([]kv, 0, len(counts))
	for r, n := range counts {
		ranked = append(ranked, kv{r, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].reason < ranked[j].reason
	})
	top := ranked[0]
	if top.n == queued {
		if queued == 1 {
			return "blocked: " + top.reason
		}
		return fmt.Sprintf("all %d queued blocked: %s", queued, top.reason)
	}
	if len(ranked) == 1 {
		return fmt.Sprintf("%d/%d blocked: %s", top.n, queued, top.reason)
	}
	return fmt.Sprintf("%d/%d blocked: %s; +%d other", top.n, queued, top.reason, len(ranked)-1)
}

func isRunningOnPremJob(job *db.Job) bool {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return true
	}
	return false
}

func (m watchModel) formatOnPremJobRow(job *db.Job, projectWidth int) string {
	display := queueblock.Display(job, nil)
	status := display.Status
	duration := "—"
	if job.StartTime > 0 {
		d := time.Duration(time.Now().Unix()-job.StartTime) * time.Second
		duration = d.Truncate(time.Second).String()
	}
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = campaign.TruncateCommand(job.Command, 50)
	}
	row := fmt.Sprintf("#%-4d  %s  %-10s  %-*s  %s",
		job.ID,
		renderWatchJobStatusText(status, status, watchInstanceBlockOptions{}),
		duration,
		projectWidth, campaign.JobProjectLabel(job),
		desc,
	)
	if display.Blocked {
		row += "  " + watchDimStyle.Render(display.Reason)
	}
	return row
}

// formatUnplacedJobRows formats all unplaced jobs as a table with dynamically
// sized columns that adapt to content and fill the available terminal width.
func (m watchModel) formatUnplacedJobRows(jobs []*db.Job, availWidth int) []string {
	if len(jobs) == 0 {
		return nil
	}

	// Measure column widths from actual data
	idWidth := 4 // "#NNN" minimum
	projWidth := 7
	gpuWidth := 5
	for _, job := range jobs {
		if w := len(fmt.Sprintf("%d", job.ID)); w > idWidth {
			idWidth = w
		}
		if w := len(campaign.JobProjectLabel(job)); w > projWidth {
			projWidth = w
		}
		if w := len(formatWatchGPUConstraint(job)); w > gpuWidth {
			gpuWidth = w
		}
	}

	// Fixed overhead: "#" + spaces between columns
	// Layout: #ID  PROJECT  DESCRIPTION  GPU  REASON
	fixedCols := 1 + idWidth + 1 + projWidth + 1 + gpuWidth // #id proj gpu + separators
	descWidth := availWidth - fixedCols - 4                 // 4 = spaces between remaining cols
	if descWidth < 12 {
		descWidth = 12
	}
	// Cap description width to leave room for placement reason
	if descWidth > 32 {
		descWidth = 32
	}

	rows := make([]string, len(jobs))
	for i, job := range jobs {
		row := fmt.Sprintf("#%-*d %-*s %-*s %-*s",
			idWidth, job.ID,
			projWidth, campaign.JobProjectLabel(job),
			descWidth, truncate(job.EffectiveDescription(), descWidth),
			gpuWidth, formatWatchGPUConstraint(job),
		)
		if reason := m.autoNoopReasons[job.ID]; reason != "" {
			row += "  " + watchDimStyle.Render(reason)
		} else if len(job.PlacementReasons) > 0 {
			row += "  " + watchDimStyle.Render(job.PlacementReasons[0])
		}
		rows[i] = row
	}
	return rows
}

func (m watchModel) selectedStatusDetail() string {
	if instID := m.selectedCloudInstanceID(); instID != 0 {
		if reason := m.failedReplaceReason[instID]; reason != "" {
			return fmt.Sprintf("instance %s not replaced: %s", ids.FormatInstanceID(instID), reason)
		}
	}
	job := m.selectedUnplacedJob()
	if job != nil {
		if reason := m.autoNoopReasons[job.ID]; reason != "" {
			return fmt.Sprintf("#%d auto: %s", job.ID, reason)
		}
		if len(job.PlacementReasons) > 0 {
			return fmt.Sprintf("#%d unplaced: %s", job.ID, strings.Join(job.PlacementReasons, " | "))
		}
	}
	return ""
}

func (m watchModel) truncateFooterDetail(detail string, prefixWidth int) string {
	if m.width <= 0 {
		return detail
	}
	controlsWidth := lipgloss.Width("[u] unplace  [x] kill  [t] terminate  [s] submit  [l] launch  [J] jobs  [U] grouped jobs  [r] retry  [q] quit")
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
	// The target value lives on the Auto-pilot status line; keep only the
	// `[$]` key hint here so the binding is still discoverable.
	state := "OFF"
	if m.autoMode {
		state = "ON"
	}
	return fmt.Sprintf("[A] auto: %s  [$] target", state)
}

func (m watchModel) autoPilotUnplacedCount() int {
	n := 0
	for _, job := range m.unplacedJobs {
		if job.IsUnplacedAwaitingPlacement() {
			n++
		}
	}
	return n
}

func (m watchModel) autoPilotRunningCount() int {
	n := 0
	switch {
	case m.mode == watchModeSystem:
		for _, host := range m.onPremHosts {
			for _, job := range host.Jobs {
				if job != nil && job.EffectiveStatus() == db.StatusRunning {
					n++
				}
			}
		}
		for _, u := range m.updates {
			for _, job := range u.Jobs {
				if job != nil && job.EffectiveStatus() == db.StatusRunning {
					n++
				}
			}
		}
	case m.mode.isInstanceBased():
		for _, id := range m.instanceIDs {
			u, ok := m.updates[id]
			if !ok {
				continue
			}
			for _, job := range u.Jobs {
				if job != nil && job.EffectiveStatus() == db.StatusRunning {
					n++
				}
			}
		}
	case m.mode == watchModeProject:
		for _, g := range m.projectGroups {
			for _, job := range g.Running {
				if job != nil && job.EffectiveStatus() == db.StatusRunning {
					n++
				}
			}
		}
	}
	return n
}

func (m watchModel) autoPilotSyncInProgress() bool {
	switch {
	case m.mode == watchModeSystem:
		return m.refreshing
	case m.mode == watchModeProject:
		return m.projectSyncing
	default:
		return false
	}
}

// formatAgentBuildStatus returns a status line describing in-progress agent
// builds, or "" if no build is running. The autopilot's hostsync queue
// publishes phases via agentdeploy.SetBuildPhase so the TUI can render them
// here instead of letting subprocess output bleed onto the screen.
func formatAgentBuildStatus() string {
	phases := agentdeploy.ActiveBuildPhases()
	if len(phases) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(phases))
	for h := range phases {
		hosts = append(hosts, h)
	}
	util.NaturalSortStrings(hosts)
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, fmt.Sprintf("%s: %s", h, phases[h]))
	}
	return "Auto-pilot: building agent (" + strings.Join(parts, ", ") + ")"
}

func (m watchModel) autoPilotStatusLine() string {
	if !m.autoMode {
		return ""
	}
	if m.autoRunRateInputActive {
		// Prompt is rendered in the controls line; keep this line empty
		// so the prompt does not appear twice in slightly different forms.
		return ""
	}
	if line := formatAgentBuildStatus(); line != "" {
		return line
	}
	unplaced := m.autoPilotUnplacedCount()
	target := formatAutoRunRateTarget(m.autoRunRateTargetCents)
	if m.autoPilotSyncInProgress() {
		return "Auto-pilot: syncing cloud state... (target " + target + ")"
	}
	if m.autoPassInFlight || m.autoPlacing {
		elapsed := ""
		if !m.autoPassStartedAt.IsZero() {
			elapsed = fmt.Sprintf(" (%s)", time.Since(m.autoPassStartedAt).Round(time.Second))
		}
		phase := activePassPhase(m.autoPassPhase, m.autoPassStartedAt)
		return fmt.Sprintf("Auto-pilot: evaluating %s%s%s... (target %s)", pluralize(unplaced, "unplaced job", "unplaced jobs"), phase, elapsed, target)
	}
	if m.autoLaunching {
		return "Auto-pilot: launching instance... (target " + target + ")"
	}
	if strings.TrimSpace(m.autoPersistentError) != "" {
		return "Auto-pilot: failed — " + m.autoPersistentError
	}
	if strings.TrimSpace(m.autoPersistentBlocked) != "" {
		return fmt.Sprintf("Auto-pilot: paused — %s (%d jobs)", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
	if !m.autoLaunchBackoffUntil.IsZero() && time.Now().Before(m.autoLaunchBackoffUntil) {
		return formatAutoPilotNextPass(m.autoLaunchBackoffUntil, unplaced)
	}
	return fmt.Sprintf("Auto-pilot: monitoring (%d unplaced, %d running, target %s)", unplaced, m.autoPilotRunningCount(), target)
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
	if m.projectHelp {
		return m.renderProjectHelpView()
	}

	lines := m.projectLines
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width, m.autoRunRateTargetCents)
	autoLine := m.autoPilotStatusLine()
	rows := m.projectPageSize()
	if rows > 1 {
		rows-- // blank separator before footer/status block
	}
	if len(sharedStatusLines) > 0 && rows > len(sharedStatusLines) {
		rows -= len(sharedStatusLines)
	}
	if autoLine != "" && rows > 1 {
		rows--
	}
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

	b.WriteString("\n")
	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if autoLine != "" {
		b.WriteString(watchDimStyle.Render(truncateDisplayWidth(autoLine, m.width)))
		b.WriteString("\n")
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
	if detail := m.selectedProjectStatusDetail(); detail != "" {
		state += "  " + truncate(detail, max(24, m.width-lipgloss.Width(state)-4))
	}
	state += "  up/down move  space/b page  g/G top/bottom  u unplace  r refresh  l launch  ? help  q quit  " + m.autoModeHint()
	return state
}

func (m watchModel) renderProjectHelpView() string {
	rows := max(1, m.height-1)
	lines := []string{
		"Project Watch Keybindings",
		"",
		"Navigation:",
		"  up/down (or j/k) move selection",
		"  space/b page down/up",
		"  g/G jump top/bottom",
		"",
		"Actions:",
		"  a view attempts for selected job",
		"  u unplace selected queued job",
		"  r refresh",
		"  l open launch planner",
		"  A toggle auto-pilot",
		"  $ set run-rate + daily cap (Enter steps; Ctrl-R resets breaker)",
		"",
		"Help:",
		"  ? toggle this help",
		"  q or Esc close help",
	}

	var b strings.Builder
	title := watchTitleStyle.Render(truncateDisplayWidth(lines[0], m.width))
	b.WriteString(title)
	b.WriteString("\n")
	for i := 1; i < rows; i++ {
		idx := i
		if idx >= len(lines) {
			b.WriteString("\n")
			continue
		}
		b.WriteString(watchDimStyle.Render(truncateDisplayWidth(lines[idx], m.width)))
		b.WriteString("\n")
	}
	b.WriteString(watchDimStyle.Render(truncateDisplayWidth("? close help", m.width)))
	return b.String()
}

func (m watchModel) renderWatchHelpView() string {
	rows := max(1, m.height-1)
	lines := []string{
		"Watch Keybindings",
		"",
		"Navigation:",
		"  up/down (or j/k) move selection",
		"  pgup/pgdown (or Ctrl+u/Ctrl+d) page up/down",
		"  g/G jump top/bottom",
		"",
		"Actions:",
		"  a view attempts for selected job",
		"  u unplace selected queued job",
		"  x kill selected running job",
		"  t terminate selected cloud instance",
		"  s submit selected unplaced job",
		"  m move selected queued cloud job",
		"  N launch new instance now (fast strategy)",
		"  l open launch planner",
		"  J open ungrouped jobs list",
		"  U open grouped jobs list",
		"  r retry failed instances",
		"  B double retry budget for selected failed instance and retry",
		"  A toggle auto-pilot",
		"  $ set run-rate + daily cap (Enter steps; Ctrl-R resets breaker)",
		"",
		"Help:",
		"  ? toggle this help",
		"  q or Esc close help",
	}

	var b strings.Builder
	b.WriteString(watchTitleStyle.Render(truncateDisplayWidth(lines[0], m.width)))
	b.WriteString("\n")
	for i := 1; i < rows; i++ {
		if i >= len(lines) {
			b.WriteString("\n")
			continue
		}
		b.WriteString(watchDimStyle.Render(truncateDisplayWidth(lines[i], m.width)))
		b.WriteString("\n")
	}
	b.WriteString(watchDimStyle.Render(truncateDisplayWidth("? close help", m.width)))
	return b.String()
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
