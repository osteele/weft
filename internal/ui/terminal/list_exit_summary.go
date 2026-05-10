package terminal

import (
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
	if m.groupedRows == nil {
		m.rebuildGroupedRows()
	}
	failures := m.recentLaunchFailures
	if failures == nil && m.database != nil {
		failures = loadRecentLaunchFailures(m.database, recentLaunchFailureWindow, now)
	}
	return renderJobListGroupedStatusPlainWithOptions(m.groupedJobsWithAutoReasons(), width, groupedStatusRenderOptions{
		launchLiveByID:         m.launchLiveByID,
		launchStatusByID:       m.launchStatusByID,
		placingJobIDs:          m.placingJobIDs,
		placementQueuedAtByJob: m.placementQueuedAtByJob,
		placementStatusByJob:   m.placementStatusByJob,
		launchFailures:         failures,
		launchByID:             m.launchByID,
		now:                    now,
		launchSpinner:          m.launchSpinner.View(),
		launchingETA: groupedStatusLaunchingETA{
			totalP50:               m.launchBootstrapP50,
			totalSamples:           m.launchBootstrapSamples,
			stageByName:            m.launchStageETAByName,
			stageEnteredAtByLaunch: m.launchStageEnteredAtByID,
		},
	})
}

func writeSummaryLine(b *strings.Builder, width int, line string) {
	if line != "" && width > 0 {
		line = truncateDisplayWidth(line, width)
	}
	b.WriteString(line)
	b.WriteString("\n")
}
