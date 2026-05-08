package terminal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

const listExitSummaryMaxRunningRows = 5

func listTUIExitSummary(model tea.Model) string {
	list, ok := model.(listTUIModel)
	if !ok {
		return ""
	}
	return list.exitSummaryAt(time.Now(), listOutputWidth())
}

func (m listTUIModel) exitSummaryAt(now time.Time, width int) string {
	if width <= 0 {
		width = 120
	}
	if m.groupedRows == nil {
		m.rebuildGroupedRows()
	}
	var b strings.Builder
	writeSummaryLine(&b, width, fmt.Sprintf("weft uj ended - %s", now.Format("Mon Jan 2 15:04")))
	if title := strings.TrimSpace(m.title); title != "" {
		writeSummaryLine(&b, width, "View: "+title)
	}
	if len(m.args) > 0 {
		writeSummaryLine(&b, width, "Filter: "+strings.Join(m.args, " "))
	}
	writeSummaryLine(&b, width, "")

	jobs := m.groupedJobsWithAutoReasons()
	launchesWithActiveJob := m.launchesWithActiveJob()
	counts := map[string]int{}
	running := make([]*db.Job, 0)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		bucket := groupedStatusBucket(job, m.launchStatusByID, launchesWithActiveJob, m.placingJobIDs, now)
		counts[bucket]++
		if bucket == "running" {
			running = append(running, job)
		}
	}
	sort.SliceStable(running, func(i, j int) bool { return running[i].ID < running[j].ID })

	writeSummaryLine(&b, width, "Running:")
	if len(running) == 0 {
		writeSummaryLine(&b, width, "  none")
	} else {
		limit := min(len(running), listExitSummaryMaxRunningRows)
		for _, job := range running[:limit] {
			writeSummaryLine(&b, width, m.exitSummaryJobLine(job, now))
		}
		if len(running) > limit {
			writeSummaryLine(&b, width, fmt.Sprintf("  ... %d more running", len(running)-limit))
		}
	}
	writeSummaryLine(&b, width, "")

	queueParts := make([]string, 0, 5)
	for _, item := range []struct {
		key   string
		label string
	}{
		{"queued", "queued"},
		{"placing", "placing"},
		{"launching", "launching"},
		{"unplaced", "unplaced"},
		{"failures", "failed"},
	} {
		if n := counts[item.key]; n > 0 {
			queueParts = append(queueParts, pluralize(n, item.label+" job", item.label+" jobs"))
		}
	}
	if len(queueParts) == 0 {
		queueParts = append(queueParts, "no queued work")
	}
	writeSummaryLine(&b, width, "Queue: "+strings.Join(queueParts, " - "))

	if line := m.exitSummarySystemLine(); line != "" {
		writeSummaryLine(&b, width, line)
	}
	if autoLine := strings.TrimSpace(m.groupedAutoPilotStatusText(len(running))); autoLine != "" {
		writeSummaryLine(&b, width, autoLine)
	}
	if failureLine := m.exitSummaryRecentFailureLine(); failureLine != "" {
		writeSummaryLine(&b, width, failureLine)
	}
	if detail := m.exitSummarySelectedLine(); detail != "" {
		writeSummaryLine(&b, width, "")
		writeSummaryLine(&b, width, detail)
	}

	writeSummaryLine(&b, width, "")
	writeSummaryLine(&b, width, "Next: weft uj | weft list | weft log <job>")
	return b.String()
}

func (m listTUIModel) exitSummaryJobLine(job *db.Job, now time.Time) string {
	cols := []string{
		"  " + ids.FormatJobID(job.ID),
		campaign.JobProjectLabel(job),
	}
	if target := m.exitSummaryJobTarget(job); target != "" {
		cols = append(cols, target)
	}
	if progress := groupedStatusProgressSuffix(job, "running", m.launchLiveByID); progress != "" {
		cols = append(cols, progress)
	}
	if eta := groupedStatusETASuffix(job, "running", m.launchLiveByID, now); eta != "" {
		cols = append(cols, eta)
	}
	if desc := strings.TrimSpace(job.EffectiveDescription()); desc != "" {
		cols = append(cols, desc)
	}
	return strings.Join(cols, "  ")
}

func (m listTUIModel) exitSummaryJobTarget(job *db.Job) string {
	if job == nil {
		return ""
	}
	if host := strings.TrimSpace(job.Host); host != "" {
		return host
	}
	if job.LaunchID == nil {
		return ""
	}
	parts := []string{ids.FormatInstanceID(*job.LaunchID)}
	if launch := m.launchByID[*job.LaunchID]; launch != nil {
		if gpu := strings.TrimSpace(launch.DisplayGPUBrief()); gpu != "" {
			parts = append(parts, gpu)
		}
	}
	return strings.Join(parts, " ")
}

func (m listTUIModel) exitSummarySystemLine() string {
	base, globalRunning, burn := sharedTUIStatusTextWithCount(m.database)
	if base == "" {
		return ""
	}
	prefix := "System: "
	if visibleRunning := countVisibleRunningJobs(m.groupedJobsWithAutoReasons()); visibleRunning >= 0 && globalRunning != visibleRunning {
		prefix = "System (global): "
	}
	line := prefix + base
	if seg := formatCostRateSegment(burn, m.autoRunRateTargetCents); seg != "" {
		line += " - " + seg
	}
	return line
}

func (m listTUIModel) exitSummaryRecentFailureLine() string {
	failures := m.recentLaunchFailures
	if failures == nil && m.database != nil {
		failures = loadRecentLaunchFailures(m.database, recentLaunchFailureWindow, time.Now())
	}
	if failures == nil || len(failures.items) == 0 {
		return ""
	}
	current := 0
	var latest *db.Launch
	for _, item := range failures.items {
		if item == nil {
			continue
		}
		if !failures.recoveredIDs[item.ID] {
			current++
		}
		if latest == nil || item.CreatedAt > latest.CreatedAt {
			latest = item
		}
	}
	if latest == nil {
		return ""
	}
	detail := strings.TrimSpace(latest.TerminationDetail)
	if detail == "" {
		detail = strings.TrimSpace(latest.DisplayTerminationReason())
	}
	if detail == "" {
		detail = "unknown"
	}
	return fmt.Sprintf("Recent launch issues: %d current - latest %s: %s", current, ids.FormatInstanceID(latest.ID), detail)
}

func (m listTUIModel) exitSummarySelectedLine() string {
	job := m.currentSelectedJob()
	if job == nil {
		return ""
	}
	return fmt.Sprintf("Selected: %s - %s - %s", ids.FormatJobID(job.ID), campaign.JobProjectLabel(job), strings.TrimSpace(job.EffectiveDescription()))
}

func writeSummaryLine(b *strings.Builder, width int, line string) {
	if line != "" && width > 0 {
		line = truncateDisplayWidth(line, width)
	}
	b.WriteString(line)
	b.WriteString("\n")
}
