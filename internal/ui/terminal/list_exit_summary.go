package terminal

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
	var b strings.Builder
	writeSummaryLine(&b, width, m.exitSummaryHeader())
	b.WriteString("\n")

	switch m.effectiveGroupMode() {
	case listGroupUngrouped:
		b.WriteString(renderJobListPlain(m.jobs, width))
	case listGroupStatus:
		b.WriteString(renderJobListGroupedStatusPlainWithOptions(m.groupedJobsWithAutoReasons(), width, groupedStatusRenderOptions{
			launchLiveByID:         m.launchLiveByID,
			launchStatusByID:       m.launchStatusByID,
			placingJobIDs:          m.placingJobIDs,
			placementQueuedAtByJob: m.placementQueuedAtByJob,
			placementStatusByJob:   m.placementStatusByJob,
			launchByID:             m.launchByID,
			now:                    now,
			launchSpinner:          m.launchSpinner.View(),
			launchingETA: groupedStatusLaunchingETA{
				totalP50:               m.launchBootstrapP50,
				totalSamples:           m.launchBootstrapSamples,
				stageByName:            m.launchStageETAByName,
				stageEnteredAtByLaunch: m.launchStageEnteredAtByID,
			},
		}))
	case listGroupProject:
		b.WriteString(renderProjectJobsPlain(groupJobsByProject(m.jobs), width))
	default:
		rows := buildListGroupedRows(m.jobs, m.effectiveGroupMode(), width, newJobListLayout(width, m.jobs, nil, false))
		if len(rows) == 0 {
			b.WriteString("No jobs found\n")
			break
		}
		for _, row := range rows {
			writeSummaryLine(&b, width, row.text)
		}
	}
	return b.String()
}

func (m listTUIModel) exitSummaryOrder() string {
	switch m.effectiveGroupMode() {
	case listGroupStatus:
		return "job id"
	case listGroupProject:
		return "project, current list order"
	case listGroupHost:
		return "host/instance, current list order"
	default:
		return "current list order"
	}
}

func (m listTUIModel) exitSummaryHeader() string {
	parts := []string{m.exitSummaryViewName()}
	if group := m.exitSummaryGroupClause(); group != "" {
		parts = append(parts, group)
	}
	for _, filter := range m.exitSummaryFilterClauses() {
		parts = append(parts, filter)
	}
	parts = append(parts, "ordered by "+m.exitSummaryOrder())
	return strings.Join(parts, " · ")
}

func (m listTUIModel) exitSummaryViewName() string {
	switch {
	case m.unprocessedView && m.statusView != "":
		return fmt.Sprintf("Unprocessed %s jobs", m.statusView)
	case m.unprocessedView:
		return "Unprocessed jobs"
	case m.statusView != "":
		return fmt.Sprintf("%s jobs", titleCaseStatus(m.statusView))
	default:
		return "Jobs"
	}
}

func (m listTUIModel) exitSummaryGroupClause() string {
	mode := m.effectiveGroupMode()
	if mode == listGroupUngrouped {
		return ""
	}
	return "grouped by " + listGroupModeLabel(mode)
}

func (m listTUIModel) exitSummaryFilterClauses() []string {
	parts := make([]string, 0, 2)
	if m.projectFilter != "" {
		parts = append(parts, "project "+m.projectFilter)
	}
	return parts
}

func titleCaseStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}
	status = strings.ReplaceAll(status, "_", " ")
	return strings.ToUpper(status[:1]) + status[1:]
}

func writeSummaryLine(b *strings.Builder, width int, line string) {
	if line != "" && width > 0 {
		line = truncateDisplayWidth(line, width)
	}
	b.WriteString(line)
	b.WriteString("\n")
}
