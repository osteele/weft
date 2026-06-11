package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func (m Model) renderHostSummarySegment(host *Host, format hostSummaryFormat) string {
	statusSymbol, statusStyle := hostStatusIndicator(host)

	isStale := !host.LastCheck.IsZero() && time.Since(host.LastCheck) > db.HostInfoStaleThreshold

	nameStyle := hostSummaryNameStyle
	if host.Status != HostStatusOnline || isStale {
		nameStyle = hostSummaryOfflineStyle.Copy()
	}

	// Highlight host name in red if low disk space
	if host.HasLowDiskSpace() {
		nameStyle = hostSummaryCriticalStyle
	}

	name := nameStyle.Render(truncate(host.Name, 12))

	// Determine whether we have metrics to show.
	// Online hosts always show metrics. Offline hosts show stale metrics
	// (preserved from the last successful check) until the stale threshold.
	hasMetrics := host.Status == HostStatusOnline
	if !hasMetrics && !isStale && host.LoadAvg != "" {
		// Recently went offline but we still have metrics from last check
		hasMetrics = true
	}

	if !hasMetrics {
		return statusStyle.Render(statusSymbol) + " " + name
	}

	// Show stats (fresh or stale)
	cpuPct, cpuOK := hostCPULoadPercent(host)
	memPct, memOK := hostMemUsagePercent(host)
	gpuPct, gpuOK := hostGPULoadPercent(host)

	var cpuText, memText, gpuText, diskText string
	if format == hostSummaryAbbrev {
		cpuText = formatHostSummaryMetricAbbrev("C", cpuPct, cpuOK)
		memText = formatHostSummaryMetricAbbrev("R", memPct, memOK)
		gpuText = formatHostSummaryMetricAbbrev("G", gpuPct, gpuOK)
		diskText = formatDiskSummaryAbbrev(host)
	} else {
		cpuText = formatHostSummaryMetric("CPU", cpuPct, cpuOK)
		memText = formatHostSummaryMetric("RAM", memPct, memOK)
		gpuText = formatHostSummaryMetric("GPU", gpuPct, gpuOK)
		diskText = formatDiskSummary(host)
	}

	// Use italic style for stale metrics to indicate they're not fresh
	applyStyle := func(base lipgloss.Style, text string) string {
		if host.Status != HostStatusOnline {
			return base.Italic(true).Render(text)
		}
		return base.Render(text)
	}

	cpuStyle := hostSummaryStyleForMetric(cpuPct, cpuOK)
	memStyle := hostSummaryStyleForMetric(memPct, memOK)
	gpuStyle := hostSummaryStyleForMetric(gpuPct, gpuOK)
	diskStyle := hostSummaryNormalStyle
	if host.HasLowDiskSpace() {
		diskStyle = hostSummaryCriticalStyle
	}

	fields := []string{
		statusStyle.Render(statusSymbol),
		name,
		applyStyle(cpuStyle, cpuText),
		applyStyle(memStyle, memText),
		applyStyle(gpuStyle, gpuText),
	}

	// Only add disk if we have disk info
	if diskText != "" {
		fields = append(fields, applyStyle(diskStyle, diskText))
	}

	return strings.Join(fields, " ")
}

func hostStatusIndicator(host *Host) (string, lipgloss.Style) {
	switch host.Status {
	case HostStatusOnline:
		return "●", hostSummaryNormalStyle
	case HostStatusChecking:
		return "◐", hostSummaryWarningStyle
	case HostStatusOffline:
		return "○", hostSummaryOfflineStyle
	default:
		return "?", hostSummaryWarningStyle
	}
}

func hostSummaryStyleForMetric(pct int, ok bool) lipgloss.Style {
	if !ok {
		return dimStyle
	}
	switch {
	case pct >= 90:
		return hostSummaryCriticalStyle
	case pct >= 70:
		return hostSummaryWarningStyle
	default:
		return hostSummaryNormalStyle
	}
}

func formatHostSummaryMetric(label string, pct int, ok bool) string {
	if !ok {
		return label + "--"
	}
	return fmt.Sprintf("%s%3d%%", label, pct)
}

// formatHostSummaryMetricAbbrev formats a metric with abbreviated label (e.g., "C45%")
func formatHostSummaryMetricAbbrev(label string, pct int, ok bool) string {
	if !ok {
		return label + "--"
	}
	return fmt.Sprintf("%s%d%%", label, pct)
}

// formatDiskSummary formats disk free space for the host summary (e.g., "Disk:45G")
func formatDiskSummary(host *Host) string {
	if host.DiskFree == 0 {
		return ""
	}
	return "Disk:" + host.DiskFreeSummary()
}

// formatDiskSummaryAbbrev formats disk free space abbreviated (e.g., "D45G")
func formatDiskSummaryAbbrev(host *Host) string {
	if host.DiskFree == 0 {
		return ""
	}
	return "D" + host.DiskFreeSummary()
}

func (m Model) renderHostList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-12s %-16s %-16s %-6s %-5s %-5s %-8s",
		"HOST", "STATUS", "ARCH", "QUEUE", "CPU", "RAM", "DISK")
	rows = append(rows, headerStyle.Render(header))

	if len(m.hosts) == 0 {
		rows = append(rows, dimStyle.Render(" No hosts found. Run a job first."))
	} else {
		// Hosts
		contentHeight := height - 4 // Account for borders and header
		rowCount := 0
		recentlyOnlineThreshold := time.Hour // Only show stats for hosts online within past hour
		for i, host := range m.hosts {
			if rowCount >= contentHeight {
				break
			}

			status := m.formatHostStatus(host)
			queue := m.queueSummaryForHost(host)
			arch := truncate(host.Arch, 16)
			if arch == "" {
				arch = "-"
			}

			// Only show stats for recently online hosts (within past hour)
			recentlyOnline := host.Status == HostStatusOnline ||
				(!host.LastCheck.IsZero() && time.Since(host.LastCheck) < recentlyOnlineThreshold)

			cpu := "-"
			ram := "-"
			disk := "-"
			if recentlyOnline {
				cpu = host.CPUUtilization()
				ram = host.RAMUtilization()
				disk = host.DiskFreeSummary()
				if disk == "" {
					disk = "-"
				}

				// Style CPU/RAM in red if >90%
				cpuPct := host.CPUUtilizationPct()
				ramPct := host.RAMUtilizationPct()
				if cpuPct > 90 {
					cpu = failedStyle.Render(cpu)
				}
				if ramPct > 90 {
					ram = failedStyle.Render(ram)
				}

				// Style disk in red if low (<5MB)
				if host.HasLowDiskSpace() {
					disk = failedStyle.Render(disk)
				}
			}

			line := fmt.Sprintf(" %-12s %-16s %-16s %-6s %-5s %-5s %-8s",
				truncate(host.Name, 12), status, arch, queue, cpu, ram, disk)

			if i == m.selectedHostIdx {
				line = selectedStyle.Width(m.width - 4).Render(line)
			} else {
				line = m.styleForHostStatus(host.Status).Render(line)
			}

			rows = append(rows, line)
			rowCount++

			// Show AI summary if enabled
			if m.showHostSummaries {
				if summary, ok := m.hostSummaries[host.Name]; ok && summary != "" {
					// Wrap summary to fit width, indent and italicize
					summaryStyle := lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("243"))
					summaryLines := wrapText(summary, m.width-10)
					for _, sl := range strings.Split(summaryLines, "\n") {
						if rowCount >= contentHeight {
							break
						}
						summaryLine := "    " + sl
						if i == m.selectedHostIdx {
							// Combine selected background with italic
							selectedSummaryStyle := summaryStyle.Background(selectedBg)
							summaryLine = selectedSummaryStyle.Width(m.width - 4).Render(summaryLine)
						} else {
							summaryLine = summaryStyle.Render(summaryLine)
						}
						rows = append(rows, summaryLine)
						rowCount++
					}
				} else if m.hostSummaryPending[host.Name] {
					// Show loading indicator
					if rowCount < contentHeight {
						pendingLine := "    " + dimStyle.Render("Generating summary...")
						rows = append(rows, pendingLine)
						rowCount++
					}
				}
			}
		}
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

// hostIndexAtRow maps a host list row (excluding header/borders) to the host index.
// It mirrors renderHostList's logic so clicks on wrapped summary rows select the owner host.
func (m Model) hostIndexAtRow(row, height int) int {
	contentHeight := height - 4
	if row < 0 || contentHeight <= 0 || row >= contentHeight {
		return -1
	}

	rowCount := 0
	for idx, host := range m.hosts {
		if rowCount >= contentHeight {
			break
		}

		startRow := rowCount
		rowCount++ // Account for the host's main row

		if m.showHostSummaries {
			if summary, ok := m.hostSummaries[host.Name]; ok && summary != "" {
				summaryLines := wrapText(summary, m.width-10)
				for range strings.Split(summaryLines, "\n") {
					if rowCount >= contentHeight {
						break
					}
					rowCount++
				}
			} else if m.hostSummaryPending[host.Name] && rowCount < contentHeight {
				rowCount++
			}
		}

		if row >= startRow && row < rowCount {
			return idx
		}
	}
	return -1
}

func (m Model) renderHostDetail(height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		lines = append(lines, dimStyle.Render("No host selected"))
	} else {
		host := m.hosts[m.selectedHostIdx]

		hostLine := fmt.Sprintf("Host: %s (%s)", host.Name, host.StatusString())
		if host.Status == HostStatusOffline && !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck).Truncate(time.Second)
			hostLine += fmt.Sprintf(" for %s", formatDuration(elapsed))
		}
		if host.Error != "" {
			hostLine += fmt.Sprintf(" - %s", host.Error)
		}
		lines = append(lines, hostLine)

		// Show static info (cached) regardless of online status
		hasStaticInfo := host.Model != "" || host.Arch != "" || host.OS != "" || host.CPUModel != "" || host.CPUs > 0 || len(host.GPUs) > 0
		if hasStaticInfo {
			lines = append(lines, "───────────────────────────────────────────────────────────────")
			// Helper to format label:value with bold label (14 chars for "Architecture:" + space)
			fmtLine := func(label, value string) string {
				return labelStyle.Render(fmt.Sprintf("%-14s", label)) + value
			}
			if host.Model != "" {
				lines = append(lines, fmtLine("Model:", host.Model))
			}
			if host.Arch != "" {
				lines = append(lines, fmtLine("Architecture:", host.Arch))
			}
			if host.OS != "" {
				lines = append(lines, fmtLine("OS Version:", host.OS))
			}
			if host.CPUModel != "" {
				lines = append(lines, fmtLine("CPU:", host.CPUModel))
			}
			if host.CPUs > 0 {
				lines = append(lines, fmtLine("CPU Cores:", fmt.Sprintf("%d", host.CPUs)))
			}

			// Memory (before GPUs, with CPU info)
			if host.MemTotal != "" {
				memInfo := host.MemTotal
				if host.MemUsed != "" {
					// Calculate utilization percentage
					usedMiB := parseMiB(host.MemUsed)
					totalMiB := parseMiB(host.MemTotal)
					if totalMiB > 0 {
						pct := (usedMiB * 100) / totalMiB
						pctStr := fmt.Sprintf("(%d%%)", pct)
						if pct > 90 {
							pctStr = failedStyle.Render(pctStr)
						}
						memInfo = fmt.Sprintf("%s used / %s total %s", host.MemUsed, host.MemTotal, pctStr)
					} else {
						memInfo = fmt.Sprintf("%s used / %s total", host.MemUsed, host.MemTotal)
					}
				}
				lines = append(lines, fmtLine("Memory:", memInfo))
			}

			// Load average (1m, 5m, 15m values)
			if host.LoadAvg != "" {
				// Parse load values - handle both comma-separated (Linux) and space-separated (macOS)
				loadStr := strings.ReplaceAll(host.LoadAvg, ",", " ")
				loads := strings.Fields(loadStr)
				if len(loads) >= 3 && host.CPUs > 0 {
					load1m := loads[0]
					load5m := loads[1]
					load15m := loads[2]
					// Calculate utilization percentage from 1-minute load
					if loadVal, err := strconv.ParseFloat(load1m, 64); err == nil {
						pct := int((loadVal / float64(host.CPUs)) * 100)
						pctStr := fmt.Sprintf("[%d%% of %d cores]", pct, host.CPUs)
						if pct > 90 {
							pctStr = failedStyle.Render(pctStr)
						}
						lines = append(lines, fmtLine("Load:", fmt.Sprintf("%s, %s, %s  %s", load1m, load5m, load15m, pctStr)))
					} else {
						lines = append(lines, fmtLine("Load:", fmt.Sprintf("%s, %s, %s", load1m, load5m, load15m)))
					}
				} else {
					lines = append(lines, fmtLine("Load:", host.LoadAvg))
				}
			}

			// GPUs (just summary, full stats are in GPUs tab)
			if len(host.GPUs) > 0 {
				gpuNames := make(map[string]int)
				for _, gpu := range host.GPUs {
					gpuNames[gpu.Name]++
				}
				if len(gpuNames) == 1 {
					for name, count := range gpuNames {
						lines = append(lines, fmtLine("GPUs:", fmt.Sprintf("%d× %s", count, name)))
					}
				} else {
					lines = append(lines, fmtLine("GPUs:", fmt.Sprintf("%d", len(host.GPUs))))
				}
			}

			// Job counts for this host
			var runningCount, queuedCount, recentFailedCount int
			oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
			for _, job := range m.allJobs {
				if job.Host != host.Name {
					continue
				}
				switch job.Status {
				case db.StatusRunning, db.StatusStarting, db.StatusPaused:
					runningCount++
				case db.StatusQueued:
					queuedCount++
				case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
					// Count as recent if ended within the last hour
					if job.EndTime != nil && *job.EndTime > oneHourAgo {
						recentFailedCount++
					}
				case db.StatusCompleted:
					// Count failed completions (non-zero exit) as recent failures
					if job.ExitCode != nil && *job.ExitCode != 0 && job.EndTime != nil && *job.EndTime > oneHourAgo {
						recentFailedCount++
					}
				}
			}
			lines = append(lines, "")
			lines = append(lines, "Jobs")
			lines = append(lines, fmt.Sprintf("  Running: %d", runningCount))
			lines = append(lines, fmt.Sprintf("  Queued:  %d", queuedCount))
			if recentFailedCount > 0 {
				lines = append(lines, fmt.Sprintf("  Failed:  %d (last hour)", recentFailedCount))
			}
		}

		// Queue status section
		if host.QueueStatus == QueueCheckChecked {
			lines = append(lines, "")
			lines = append(lines, "Queue")
			if host.QueueRunnerActive {
				lines = append(lines, "  Runner:       Active")
				if host.CurrentQueueJob != "" {
					lines = append(lines, fmt.Sprintf("  Current job:  %s", host.CurrentQueueJob))
				} else {
					lines = append(lines, "  Current job:  None")
				}
				queuedCount, _ := db.CountQueuedByHost(m.database, host.Name)
				lines = append(lines, fmt.Sprintf("  Jobs waiting: %d", queuedCount))
				if host.QueueStopPending {
					lines = append(lines, "  Stop pending: Yes")
				}
			} else {
				lines = append(lines, "  Runner:       Stopped")
			}
		}
		if host.SyncWarning != "" {
			lines = append(lines, "")
			lines = append(lines, failedStyle.Render("Sync warning: "+host.SyncWarning))
		}

	}

	// Build footer with last successful connection time
	footerText := ""
	if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts) {
		host := m.hosts[m.selectedHostIdx]
		if !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck).Truncate(time.Second)
			footerText = fmt.Sprintf("Last online: %s ago", elapsed)
		}
	}

	// Calculate available lines: height - borders(2) - title(1) - footer(1 if present)
	footerLines := 0
	if footerText != "" {
		footerLines = 1
	}
	availableLines := height - 4 - footerLines

	// Clip content if needed
	if len(lines) > availableLines && availableLines > 0 {
		lines = lines[:availableLines]
	}

	// Pad with empty lines to push footer to bottom
	for len(lines) < availableLines {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	panelContent := m.renderHostTabHeader() + "\n" + content
	if footerText != "" {
		panelContent = panelContent + "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(footerText)
	}

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderHostCPUTop(height int) string {
	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]
	hostName := host.Name

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Top CPU processes on %s\n\n", hostName))

	switch {
	case m.hostCPUTopLoading && m.hostCPUTopRequestedHost == hostName:
		b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		b.WriteString("\n")
	case m.hostCPUTopError != "" && m.hostCPUTopDataHost == hostName:
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed to fetch: %s", m.hostCPUTopError)))
		b.WriteString("\n")
	case m.hostCPUTopDataHost == hostName && len(m.hostCPUTopEntries) > 0:
		userWidth := 10
		cpuWidth := 6
		jobWidth := 8
		panelWidth := m.width - 6
		if panelWidth < 40 {
			panelWidth = 40
		}
		processWidth := panelWidth - (userWidth + cpuWidth + jobWidth + 6)
		if processWidth < 20 {
			processWidth = 20
		}

		header := fmt.Sprintf(" %-*s %*s %-*s %s", userWidth, "User", cpuWidth, "%CPU", jobWidth, "Job", "Process")
		b.WriteString(lipgloss.NewStyle().Bold(true).Render(header))
		b.WriteString("\n")

		availableLines := height - 4
		maxRows := len(m.hostCPUTopEntries)
		if availableLines > 0 && maxRows > availableLines-2 {
			maxRows = availableLines - 2
		}
		if maxRows < 0 {
			maxRows = 0
		}

		totalCPU := 0.0
		for i, proc := range m.hostCPUTopEntries {
			if i >= maxRows {
				break
			}
			jobLabel := "—"
			if proc.JobID > 0 {
				jobLabel = fmt.Sprintf("#%d", proc.JobID)
			}
			process := proc.Command
			if process == "" {
				process = fmt.Sprintf("PID %d", proc.PID)
			}
			process = truncate(shortenCommandPath(process), processWidth)
			line := fmt.Sprintf(" %-*s %*.1f %-*s %s",
				userWidth, truncate(proc.User, userWidth),
				cpuWidth, proc.CPU,
				jobWidth, jobLabel,
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
		totalLine := fmt.Sprintf(" %-*s %*.1f %-*s %s",
			userWidth, "TOTAL",
			cpuWidth, totalCPU,
			jobWidth, "",
			dimStyle.Render(coreEstimate),
		)
		b.WriteString(totalLine)
		b.WriteString("\n")

		remaining := len(m.hostCPUTopEntries) - maxRows
		if remaining > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			b.WriteString("\n")
		}

		if !m.hostCPUTopUpdated.IsZero() {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("Updated %s ago", time.Since(m.hostCPUTopUpdated).Truncate(time.Second))))
		}
	case m.hostCPUTopDataHost == hostName && len(m.hostCPUTopEntries) == 0:
		b.WriteString(dimStyle.Render("No active processes reported"))
		b.WriteString("\n")
	default:
		if m.hostCPUTopRequestedHost != "" {
			b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		} else {
			b.WriteString(dimStyle.Render("Select a host to load CPU data"))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// renderHostTabHeader renders the tab header for the host detail panel
func (m Model) renderHostTabHeader() string {
	var tabs []string
	gpuIndices := m.getHostGPUIndices()

	// Info tab
	infoLabel := "Info"
	if m.hostDetailTab == HostDetailTabInfo {
		tabs = append(tabs, activeTabStyle.Render(infoLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(infoLabel))
	}

	// CPU tab
	cpuLabel := "CPU"
	if m.hostDetailTab == HostDetailTabCPU {
		tabs = append(tabs, activeTabStyle.Render(cpuLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(cpuLabel))
	}

	// GPU Summary tab
	summaryLabel := "GPUs"
	if m.hostDetailTab == HostDetailTabGPUSummary {
		tabs = append(tabs, activeTabStyle.Render(summaryLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(summaryLabel))
	}

	// Count jobs per GPU for styling
	gpuJobStatus := m.getGPUJobStatus()

	// Individual GPU tabs (position in array, not actual GPU index)
	for pos, gpuIdx := range gpuIndices {
		label := fmt.Sprintf("%d", gpuIdx)
		gpuTab := HostDetailTabGPUBase + HostDetailTab(pos)
		if m.hostDetailTab == gpuTab {
			tabs = append(tabs, activeTabStyle.Render(label))
		} else {
			// Style based on job status
			status := gpuJobStatus[gpuIdx]
			switch {
			case status.running > 0:
				tabs = append(tabs, gpuTabRunningStyle.Render(label))
			case status.queued > 0:
				tabs = append(tabs, gpuTabQueuedStyle.Render(label))
			default:
				tabs = append(tabs, gpuTabEmptyStyle.Render(label))
			}
		}
	}

	hint := dimStyle.Render(" (Tab)")
	return strings.Join(tabs, " ") + hint
}

// getHostGPUIndices returns the sorted list of GPU indices for the selected host
// This includes both hardware GPUs and GPUs referenced by jobs
func (m Model) getHostGPUIndices() []int {
	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return nil
	}
	host := m.hosts[m.selectedHostIdx]

	knownGPUs := make(map[int]bool)

	// Add hardware GPUs
	for _, gpu := range host.GPUs {
		knownGPUs[gpu.Index] = true
	}

	// Add GPUs from jobs
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}
		for _, idx := range parseGPUIndices(job.GetGPU()) {
			knownGPUs[idx] = true
		}
	}

	var indices []int
	for idx := range knownGPUs {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}

// gpuJobCounts holds running and queued job counts for a GPU
type gpuJobCounts struct {
	running int
	queued  int
}

// getGPUJobStatus returns job counts per GPU for the selected host
func (m Model) getGPUJobStatus() map[int]gpuJobCounts {
	result := make(map[int]gpuJobCounts)

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return result
	}
	host := m.hosts[m.selectedHostIdx]

	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused
		for _, idx := range parseGPUIndices(job.GetGPU()) {
			counts := result[idx]
			if isRunning {
				counts.running++
			} else {
				counts.queued++
			}
			result[idx] = counts
		}
	}

	return result
}

// renderGPUSummary renders a compact summary of all GPUs with job counts
func (m Model) renderGPUSummary(height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]

	// Build job counts per GPU
	type gpuCounts struct {
		running int
		queued  int
	}
	gpuJobCounts := make(map[int]*gpuCounts)
	var noGPURunning, noGPUQueued int

	// Initialize with hardware GPUs
	for _, gpu := range host.GPUs {
		gpuJobCounts[gpu.Index] = &gpuCounts{}
	}

	// Count jobs per GPU
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused
		gpuIndices := parseGPUIndices(job.GetGPU())

		if len(gpuIndices) == 0 {
			if isRunning {
				noGPURunning++
			} else {
				noGPUQueued++
			}
		} else {
			for _, idx := range gpuIndices {
				if gpuJobCounts[idx] == nil {
					gpuJobCounts[idx] = &gpuCounts{}
				}
				if isRunning {
					gpuJobCounts[idx].running++
				} else {
					gpuJobCounts[idx].queued++
				}
			}
		}
	}

	// Get sorted GPU indices
	var gpuIndices []int
	for idx := range gpuJobCounts {
		gpuIndices = append(gpuIndices, idx)
	}
	sort.Ints(gpuIndices)

	// Check if we have stats to show
	hasStats := false
	if host.Status == HostStatusOnline {
		for _, gpu := range host.GPUs {
			if gpu.Temperature > 0 || gpu.Utilization > 0 || gpu.MemUsed != "" {
				hasStats = true
				break
			}
		}
	}

	lines = append(lines, fmt.Sprintf("Jobs on %s:", host.Name))
	lines = append(lines, "")

	if hasStats {
		const (
			gpuHeaderFmt = "%-4s %-5s %-7s %-6s %-6s %-22s %s"
			gpuRowFmt    = "%-4d %-5d %-7d %-6s %-6s %-22s %s"
		)
		// Combined table with job counts and GPU stats
		lines = append(lines, fmt.Sprintf(gpuHeaderFmt, "GPU", "Run", "Queue", "Temp", "Util", "Memory", "Name"))
		lines = append(lines, fmt.Sprintf(gpuHeaderFmt, "───", "───", "─────", "────", "────", "──────────────────────", "────"))

		for _, gpuIdx := range gpuIndices {
			counts := gpuJobCounts[gpuIdx]
			var gpu *GPUInfo
			for i := range host.GPUs {
				if host.GPUs[i].Index == gpuIdx {
					gpu = &host.GPUs[i]
					break
				}
			}

			temp := "-"
			util := "-"
			mem := "-"
			gpuName := ""

			if gpu != nil {
				gpuName = truncate(gpu.Name, 18)
				if gpu.Temperature > 0 {
					temp = fmt.Sprintf("%3d°C", gpu.Temperature)
				}
				if gpu.Utilization > 0 || gpu.MemUsed != "" {
					util = fmt.Sprintf("%3d%%", gpu.Utilization)
				}
				if gpu.MemUsed != "" && gpu.MemTotal != "" {
					usedMiB := parseMiB(gpu.MemUsed)
					totalMiB := parseMiB(gpu.MemTotal)
					if totalMiB > 0 {
						pct := (usedMiB * 100) / totalMiB
						mem = fmt.Sprintf("%s/%s(%d%%)", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal), pct)
					} else {
						mem = fmt.Sprintf("%s/%s", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal))
					}
				}
			}
			lines = append(lines, fmt.Sprintf(gpuRowFmt,
				gpuIdx, counts.running, counts.queued, temp, util, mem, gpuName))
		}
	} else {
		// Simple table without stats (host offline or no stats available)
		lines = append(lines, "GPU  Running  Queued  Name")
		lines = append(lines, "───  ───────  ──────  ────")

		for _, gpuIdx := range gpuIndices {
			counts := gpuJobCounts[gpuIdx]
			gpuName := ""
			for _, gpu := range host.GPUs {
				if gpu.Index == gpuIdx {
					gpuName = truncate(gpu.Name, 30)
					break
				}
			}
			lines = append(lines, fmt.Sprintf("%3d  %7d  %6d  %s", gpuIdx, counts.running, counts.queued, gpuName))
		}
	}

	// No GPU section
	if noGPURunning > 0 || noGPUQueued > 0 {
		if hasStats {
			lines = append(lines, fmt.Sprintf("  —  %3d  %5d                              (no GPU specified)", noGPURunning, noGPUQueued))
		} else {
			lines = append(lines, fmt.Sprintf("  —  %7d  %6d  (no GPU specified)", noGPURunning, noGPUQueued))
		}
	}

	return strings.Join(lines, "\n")
}

// renderGPUDetailTab renders detailed job list for a specific GPU
func (m Model) renderGPUDetailTab(gpuIdx int, height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]

	// Get GPU name
	gpuName := ""
	for _, gpu := range host.GPUs {
		if gpu.Index == gpuIdx {
			gpuName = gpu.Name
			break
		}
	}

	if gpuName != "" {
		lines = append(lines, fmt.Sprintf("GPU %d: %s", gpuIdx, gpuName))
	} else {
		lines = append(lines, fmt.Sprintf("GPU %d", gpuIdx))
	}
	lines = append(lines, "")

	// Collect jobs for this GPU
	var running, queued []*db.Job
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		jobGPUs := parseGPUIndices(job.GetGPU())
		for _, idx := range jobGPUs {
			if idx == gpuIdx {
				if job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
					running = append(running, job)
				} else {
					queued = append(queued, job)
				}
				break
			}
		}
	}

	// Running jobs
	lines = append(lines, fmt.Sprintf("Running (%d):", len(running)))
	if len(running) == 0 {
		lines = append(lines, "  (none)")
	} else {
		for _, job := range running {
			desc := truncate(job.EffectiveDescription(), 50)
			if job.Description == "" && job.GeneratedDescription != "" {
				desc = lipgloss.NewStyle().Italic(true).Render(desc)
			}
			lines = append(lines, fmt.Sprintf("  #%-4d %s", job.ID, desc))
		}
	}

	lines = append(lines, "")

	// Queued jobs
	lines = append(lines, fmt.Sprintf("Queued (%d):", len(queued)))
	if len(queued) == 0 {
		lines = append(lines, "  (none)")
	} else {
		for _, job := range queued {
			desc := truncate(job.EffectiveDescription(), 50)
			if job.Description == "" && job.GeneratedDescription != "" {
				desc = lipgloss.NewStyle().Italic(true).Render(desc)
			}
			lines = append(lines, fmt.Sprintf("  #%-4d %s", job.ID, desc))
		}
	}

	return strings.Join(lines, "\n")
}

// renderHostDetailPanel renders the host detail panel with tabs
func (m Model) renderHostDetailPanel(height int) string {
	header := m.renderHostTabHeader()

	var content string
	switch {
	case m.hostDetailTab == HostDetailTabInfo:
		// Use the existing renderHostDetail
		return m.renderHostDetail(height)
	case m.hostDetailTab == HostDetailTabCPU:
		content = m.renderHostCPUTop(height - 3)
	case m.hostDetailTab == HostDetailTabGPUSummary:
		content = m.renderGPUSummary(height - 3)
	case m.hostDetailTab.IsGPUDetailTab():
		// Convert position to actual GPU index
		gpuIndices := m.getHostGPUIndices()
		pos := m.hostDetailTab.GPUPosition()
		if pos >= 0 && pos < len(gpuIndices) {
			content = m.renderGPUDetailTab(gpuIndices[pos], height-3)
		} else {
			content = dimStyle.Render("Invalid GPU tab")
		}
	default:
		return m.renderHostDetail(height)
	}

	panelContent := header + "\n" + content
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderHostsStatusBar() string {
	help := helpStyle.Render("?:help q:quit ↑/↓:nav ←/→:jobs x:delete R:refresh")
	daemon := renderDashboardDaemonStatus()

	// Right-align the help text
	gap := m.width - lipgloss.Width(help) - lipgloss.Width(daemon) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + daemon + strings.Repeat(" ", gap) + help
}

func (m Model) formatHostStatus(host *Host) string {
	switch host.Status {
	case HostStatusOnline:
		return "● online"
	case HostStatusOffline:
		status := "○ offline"
		if !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck)
			status = fmt.Sprintf("%s %s", status, FormatCompactDuration(elapsed))
		}
		return status
	case HostStatusChecking:
		return "◐ checking"
	default:
		return "? unknown"
	}
}

// queueSummaryForHost returns a brief queue status string using database count
func (m Model) queueSummaryForHost(host *Host) string {
	switch host.QueueStatus {
	case QueueCheckUnknown, QueueCheckChecking:
		return "-"
	case QueueCheckChecked:
		// Get count from database instead of remote file
		count, err := db.CountQueuedByHost(m.database, host.Name)
		if err != nil {
			count = 0
		}
		if !host.QueueRunnerActive {
			if count > 0 {
				return fmt.Sprintf("○ %d", count) // Queue stopped but has jobs
			}
			return "○" // Queue stopped, no jobs
		}
		if host.QueueStopPending {
			return fmt.Sprintf("■ %d", count)
		}
		return fmt.Sprintf("▶ %d", count)
	default:
		return "-"
	}
}

func (m Model) styleForHostStatus(status HostStatus) lipgloss.Style {
	switch status {
	case HostStatusOnline:
		return hostOnlineStyle
	case HostStatusOffline:
		return hostOfflineStyle
	case HostStatusChecking:
		return hostCheckingStyle
	default:
		return lipgloss.NewStyle()
	}
}
