package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/ui/dashboard"
)

type groupedStatusSection struct {
	title string
	key   string
	jobs  []*db.Job
}

const stalePendingPlacementNoLaunchWindow = 120 * time.Second

type groupedStatusRow struct {
	text      string
	isHeader  bool
	isBlocked bool
	job       *db.Job
	section   string
}

func renderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLiveState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLaunchState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, launchStatusByID, time.Now())
}

func renderJobListGroupedStatusPlainAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, now time.Time) string {
	rows := buildGroupedStatusRowsAt(jobs, width, launchLiveByID, launchStatusByID, now)
	if len(rows) == 0 {
		return "None\n"
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, row.text)
	}
	return strings.Join(lines, "\n") + "\n"
}

func buildGroupedStatusRows(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string) []groupedStatusRow {
	return buildGroupedStatusRowsAt(jobs, width, launchLiveByID, launchStatusByID, time.Now())
}

func buildGroupedStatusRowsAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, now time.Time) []groupedStatusRow {
	running := make([]*db.Job, 0)
	launching := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
	unplaced := make([]*db.Job, 0)
	completions := make([]*db.Job, 0)
	failures := make([]*db.Job, 0)
	killedCanceled := make([]*db.Job, 0)

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch groupedStatusBucket(job, launchStatusByID, now) {
		case "running":
			running = append(running, job)
		case "launching":
			launching = append(launching, job)
		case "queued":
			queued = append(queued, job)
		case "unplaced":
			unplaced = append(unplaced, job)
		case "completions":
			completions = append(completions, job)
		case "failures":
			failures = append(failures, job)
		case "killed_canceled":
			killedCanceled = append(killedCanceled, job)
		}
	}

	sections := []groupedStatusSection{
		{title: "Running", key: "running", jobs: running},
		{title: "Queued", key: "queued", jobs: queued},
		{title: "Launching", key: "launching", jobs: launching},
		{title: "Unplaced", key: "unplaced", jobs: unplaced},
		{title: "Completed", key: "completions", jobs: completions},
		{title: "Failed", key: "failures", jobs: failures},
		{title: "Killed/Canceled", key: "killed_canceled", jobs: killedCanceled},
	}

	// Compute uniform project column width across all jobs.
	projectWidth := computeProjectColumnWidth(jobs, width)

	rows := make([]groupedStatusRow, 0, len(jobs)+8)
	for _, section := range sections {
		if len(section.jobs) == 0 {
			continue
		}
		rows = append(rows, groupedStatusRow{
			text:     fmt.Sprintf("%s (%d):", section.title, len(section.jobs)),
			isHeader: true,
			section:  section.key,
		})
		for _, job := range section.jobs {
			project, desc := groupedStatusJobParts(job)
			projectCol := formatProjectColumn(project, projectWidth)

			// Build suffix parts (progress, ETA, timing, outcome).
			var suffixParts []string
			if progressText := groupedStatusProgressSuffix(job, section.key, launchLiveByID); progressText != "" {
				suffixParts = append(suffixParts, progressText)
			}
			if etaText := groupedStatusETASuffix(job, section.key, launchLiveByID, now); etaText != "" {
				suffixParts = append(suffixParts, etaText)
			}
			if timing := groupedStatusTimingSuffix(job, section.key, now); timing != "" {
				suffixParts = append(suffixParts, timing)
			}
			if suffix := groupedStatusOutcomeSuffix(job, section.title); suffix != "" {
				suffixParts = append(suffixParts, suffix)
			}
			suffix := ""
			if len(suffixParts) > 0 {
				suffix = " — " + strings.Join(suffixParts, " — ")
			}

			prefix := fmt.Sprintf("- wj%d — %s ", job.ID, projectCol)
			line := prefix + desc + suffix
			if width > 0 {
				prefixWidth := lipgloss.Width(prefix)
				suffixWidth := lipgloss.Width(suffix)
				descWidth := width - prefixWidth - suffixWidth
				if descWidth < 0 {
					descWidth = 0
				}
				desc = truncateDisplayWidth(desc, descWidth)
				line = prefix + desc
				if suffix != "" {
					padding := descWidth - lipgloss.Width(desc)
					if padding < 0 {
						padding = 0
					}
					line += strings.Repeat(" ", padding) + suffix
				}
			}
			rows = append(rows, groupedStatusRow{
				text:    line,
				job:     job,
				section: section.key,
			})
			if blocked := groupedStatusBlockedSuffix(job, section.key); blocked != "" {
				rows = append(rows, groupedStatusRow{
					text:      "    " + blocked,
					isBlocked: true,
					job:       job,
					section:   section.key,
				})
			}
		}
		rows = append(rows, groupedStatusRow{text: ""})
	}

	if len(rows) == 0 {
		return nil
	}

	// Remove trailing blank line.
	if rows[len(rows)-1].text == "" {
		rows = rows[:len(rows)-1]
	}

	if width > 0 {
		for i := range rows {
			rows[i].text = truncateDisplayWidth(rows[i].text, width)
		}
	}

	return rows
}

// computeProjectColumnWidth determines a uniform project column width
// based on the longest project name, capped to a fraction of terminal width.
func computeProjectColumnWidth(jobs []*db.Job, termWidth int) int {
	maxLen := 0
	for _, job := range jobs {
		if job == nil {
			continue
		}
		name := strings.TrimSpace(campaign.JobProjectLabel(job))
		if len(name) > maxLen {
			maxLen = len(name)
		}
	}
	if maxLen == 0 {
		return 0
	}
	// Cap at 30% of terminal width, min 8, max 30.
	cap := 30
	if termWidth > 0 {
		cap = termWidth * 30 / 100
	}
	if cap < 8 {
		cap = 8
	}
	if cap > 30 {
		cap = 30
	}
	if maxLen < cap {
		return maxLen
	}
	return cap
}

// formatProjectColumn abbreviates and pads a project name to exactly width chars.
func formatProjectColumn(project string, width int) string {
	if width <= 0 || project == "" {
		return project
	}
	abbreviated := dashboard.AbbreviateProject(project, width)
	if len(abbreviated) < width {
		abbreviated += strings.Repeat(" ", width-len(abbreviated))
	}
	return abbreviated
}

func groupedStatusProgressSuffix(job *db.Job, sectionKey string, launchLiveByID map[int64]*db.LaunchLiveState) string {
	if job == nil || sectionKey != "running" {
		return ""
	}
	if job.LaunchID != nil && launchLiveByID != nil {
		if live := launchLiveByID[*job.LaunchID]; live != nil && live.JobProgressID == job.ID {
			if pctText := strings.TrimSpace(progress.FormatPhaseProgress(live.JobProgressPhase, live.JobProgressPct)); pctText != "" {
				return "running " + pctText
			}
		}
	}
	switch job.EffectiveStatus() {
	case db.StatusStarting:
		return "starting"
	default:
		return ""
	}
}

func groupedStatusTimingSuffix(job *db.Job, sectionKey string, now time.Time) string {
	if job == nil {
		return ""
	}
	if sectionKey == "running" && job.StartTime > 0 {
		return "started " + shortRelativeTime(now.Unix()-job.StartTime)
	}
	if sectionKey == "launching" {
		if job.LaunchID != nil && *job.LaunchID > 0 {
			return fmt.Sprintf("instance %s starting", ids.FormatInstanceID(*job.LaunchID))
		}
		return "instance starting"
	}
	if sectionKey == "completions" && job.EndTime != nil && *job.EndTime > 0 {
		return "completed " + shortRelativeTime(now.Unix()-*job.EndTime)
	}
	if sectionKey == "unplaced" && job.EndTime != nil && *job.EndTime > 0 {
		reason := strings.ToLower(strings.TrimSpace(queueblock.Display(job, nil).Reason))
		if strings.Contains(reason, "retry budget exceeded") || strings.Contains(reason, "max attempts") {
			return "retry rejected " + shortRelativeTime(now.Unix()-*job.EndTime)
		}
		return "retry pending " + shortRelativeTime(now.Unix()-*job.EndTime)
	}
	placedAt := job.QueuedAt
	if placedAt == 0 {
		placedAt = job.CreatedAt
	}
	if placedAt == 0 {
		placedAt = job.StartTime
	}
	if placedAt <= 0 {
		return ""
	}
	label := "placed"
	if sectionKey == "unplaced" {
		label = "queued"
	}
	return label + " " + shortRelativeTime(now.Unix()-placedAt)
}

func groupedStatusETASuffix(job *db.Job, sectionKey string, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) string {
	if job == nil || sectionKey != "running" {
		return ""
	}
	remaining, ok := estimateRunningJobRemaining(job, launchLiveByID, now)
	if !ok || remaining.Mean <= 0 {
		return ""
	}
	return "ETA " + remaining.FormatWithBounds()
}

func groupedStatusBlockedSuffix(job *db.Job, sectionKey string) string {
	if job == nil || strings.TrimSpace(job.QueueBlockedReason) == "" {
		return ""
	}
	if sectionKey != "queued" && sectionKey != "unplaced" {
		return ""
	}
	return "blocked: " + strings.TrimSpace(job.QueueBlockedReason)
}

func groupedStatusBucket(job *db.Job, launchStatusByID map[int64]string, now time.Time) string {
	status := job.EffectiveStatus()
	switch status {
	case db.StatusRunning, db.StatusStarting:
		return "running"
	case db.StatusPendingPlacement:
		if groupedStatusPendingPlacementLooksStale(job, now) {
			return "unplaced"
		}
		switch launchStatusForJob(job, launchStatusByID) {
		case db.LaunchStatusRunning, db.LaunchStatusGrace, db.LaunchStatusCompleted:
			return "queued"
		case db.LaunchStatusFailed, db.LaunchStatusCancelled:
			return "unplaced"
		}
		return "launching"
	case db.StatusQueued:
		if job.TargetKind() == db.JobTargetUnplaced {
			return "unplaced"
		}
		switch launchStatusForJob(job, launchStatusByID) {
		case db.LaunchStatusPlanned, db.LaunchStatusLaunching:
			return "launching"
		case db.LaunchStatusFailed, db.LaunchStatusCancelled:
			return "unplaced"
		}
		return "queued"
	case db.StatusKilled, db.StatusCanceled:
		return "killed_canceled"
	case db.StatusFailed, db.StatusDead:
		return "failures"
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return "failures"
		}
		return "completions"
	default:
		return ""
	}
}

// groupedStatusJobParts returns the project name and description separately.
func groupedStatusJobParts(job *db.Job) (project, desc string) {
	project = strings.TrimSpace(campaign.JobProjectLabel(job))
	desc = strings.TrimSpace(job.EffectiveDescription())
	if desc == "" && project == "" {
		desc = job.EffectiveCommand()
	}
	scope := groupedStatusScopeLabel(job)
	if scope != "" {
		desc += fmt.Sprintf(" (%s)", scope)
	}
	return project, desc
}

func groupedStatusJobLabel(job *db.Job) string {
	project, desc := groupedStatusJobParts(job)
	if project != "" && desc != "" {
		return project + " " + desc
	}
	if project != "" {
		return project
	}
	return desc
}

func groupedStatusScopeLabel(job *db.Job) string {
	switch {
	case job == nil:
		return ""
	case job.UsesRentalPlacement():
		return "rental"
	case job.UsesInventoryPlacement():
		return "inventory"
	default:
		return ""
	}
}

// countVisibleRunningJobs counts jobs that would appear in the "Running" section.
func countVisibleRunningJobs(jobs []*db.Job) int {
	n := 0
	for _, job := range jobs {
		if job != nil && groupedStatusBucket(job, nil, time.Now()) == "running" {
			n++
		}
	}
	return n
}

func groupedStatusOutcomeSuffix(job *db.Job, sectionTitle string) string {
	switch sectionTitle {
	case "Completed":
		return "completed ok"
	case "Failed":
		status := job.EffectiveStatus()
		if status == db.StatusCompleted && job.ExitCode != nil {
			return fmt.Sprintf("completed (exit %d)", *job.ExitCode)
		}
		return status
	case "Killed/Canceled":
		return job.EffectiveStatus()
	default:
		return ""
	}
}

func launchStatusForJob(job *db.Job, launchStatusByID map[int64]string) string {
	if job == nil || job.LaunchID == nil || launchStatusByID == nil {
		return ""
	}
	return strings.TrimSpace(launchStatusByID[*job.LaunchID])
}

func groupedStatusPendingPlacementLooksStale(job *db.Job, now time.Time) bool {
	if job == nil || job.EffectiveStatus() != db.StatusPendingPlacement {
		return false
	}
	if job.LaunchID != nil && *job.LaunchID > 0 {
		return false
	}
	if job.PendingAt == nil || *job.PendingAt <= 0 {
		return false
	}
	if now.IsZero() {
		return false
	}
	pendingAt := time.Unix(*job.PendingAt, 0)
	return now.Sub(pendingAt) >= stalePendingPlacementNoLaunchWindow
}
