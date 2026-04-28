package terminal

import (
	"fmt"
	"sort"
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
	paused := make([]*db.Job, 0)
	launching := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
	unplaced := make([]*db.Job, 0)
	completions := make([]*db.Job, 0)
	failures := make([]*db.Job, 0)
	killedCanceled := make([]*db.Job, 0)

	// Launch-active set is derived from actual job statuses rather than
	// LaunchLiveState.JobProgressID, which can point at a job that has
	// since finished or fallen back to queued.
	launchesWithActiveJob := make(map[int64]bool)
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil {
			continue
		}
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			launchesWithActiveJob[*job.LaunchID] = true
		}
	}

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch groupedStatusBucket(job, launchStatusByID, launchesWithActiveJob, now) {
		case "running":
			running = append(running, job)
		case "paused":
			paused = append(paused, job)
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

	for _, s := range [][]*db.Job{running, paused, launching, queued, unplaced, completions, failures, killedCanceled} {
		sort.SliceStable(s, func(i, j int) bool { return s[i].ID < s[j].ID })
	}

	sections := []groupedStatusSection{
		{title: "Running", key: "running", jobs: running},
		{title: "Paused", key: "paused", jobs: paused},
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
		if section.key == "unplaced" || section.key == "queued" {
			rows = appendBlockedGroupedJobRows(rows, section, projectWidth, width, launchLiveByID, now)
		} else {
			for _, job := range section.jobs {
				rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, now, false)
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

// appendBlockedGroupedJobRows renders an Unplaced/Queued section, grouping
// jobs by their blocked reason. Each distinct reason becomes a subheader
// ("  blocked: <reason> (N)") followed by indented job rows. Jobs with no
// blocked reason are emitted last without a subheader at the normal indent.
func appendBlockedGroupedJobRows(
	rows []groupedStatusRow,
	section groupedStatusSection,
	projectWidth, width int,
	launchLiveByID map[int64]*db.LaunchLiveState,
	now time.Time,
) []groupedStatusRow {
	order := make([]string, 0, len(section.jobs))
	buckets := make(map[string][]*db.Job, len(section.jobs))
	for _, job := range section.jobs {
		reason := groupedStatusBlockedReason(job, section.key)
		if _, ok := buckets[reason]; !ok {
			order = append(order, reason)
		}
		buckets[reason] = append(buckets[reason], job)
	}
	for _, reason := range order {
		if reason == "" {
			continue
		}
		jobs := buckets[reason]
		rows = append(rows, groupedStatusRow{
			text:      fmt.Sprintf("  blocked: %s (%d)", reason, len(jobs)),
			isBlocked: true,
			section:   section.key,
		})
		for _, job := range jobs {
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, now, true)
		}
	}
	for _, job := range buckets[""] {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, now, false)
	}
	return rows
}

func groupedStatusBlockedReason(job *db.Job, sectionKey string) string {
	if job == nil {
		return ""
	}
	if sectionKey != "queued" && sectionKey != "unplaced" {
		return ""
	}
	return campaign.SanitizeBlockedReason(job.QueueBlockedReason)
}

func appendGroupedStatusJobRow(
	rows []groupedStatusRow,
	job *db.Job,
	section groupedStatusSection,
	projectWidth, width int,
	launchLiveByID map[int64]*db.LaunchLiveState,
	now time.Time,
	indented bool,
) []groupedStatusRow {
	project, desc := groupedStatusJobParts(job)
	projectCol := formatProjectColumn(project, projectWidth)

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
	if outcome := groupedStatusOutcomeSuffix(job, section.title); outcome != "" {
		suffixParts = append(suffixParts, outcome)
	}
	suffix := ""
	if len(suffixParts) > 0 {
		suffix = " — " + strings.Join(suffixParts, " — ")
	}

	indent := ""
	if indented {
		indent = "  "
	}
	glyph, jobID := groupedStatusPlacementMarker(job)
	prefix := fmt.Sprintf("%s- %s %s — %s ", indent, glyph, jobID, projectCol)
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
	return append(rows, groupedStatusRow{
		text:    line,
		job:     job,
		section: section.key,
	})
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
			return fmt.Sprintf("instance %s starting", formatRentalInstanceLabel(job))
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
	var placedAt int64
	label := "placed"
	if sectionKey == "unplaced" {
		// Unplaced jobs aren't actually queued, and QueuedAt is reset to
		// "now" on every autopilot retry that closes and recreates the
		// open attempt. Prefer CreatedAt so the displayed age reflects
		// the job's true age rather than the latest retry.
		placedAt = job.CreatedAt
		if placedAt == 0 {
			placedAt = job.QueuedAt
		}
		if placedAt == 0 {
			placedAt = job.StartTime
		}
		label = "created"
	} else {
		placedAt = job.QueuedAt
		if placedAt == 0 {
			placedAt = job.CreatedAt
		}
		if placedAt == 0 {
			placedAt = job.StartTime
		}
	}
	if placedAt <= 0 {
		return ""
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

func groupedStatusBucket(job *db.Job, launchStatusByID map[int64]string, launchesWithActiveJob map[int64]bool, now time.Time) string {
	status := job.EffectiveStatus()
	// Launch-level pause overrides the job-status bucket for any non-terminal
	// job: the job is not actually progressing while the rental is paused.
	if launchStatusForJob(job, launchStatusByID) == db.LaunchStatusPaused {
		switch status {
		case db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusPendingPlacement, db.StatusPaused:
			return "paused"
		}
	}
	switch status {
	case db.StatusRunning, db.StatusStarting:
		return "running"
	case db.StatusPaused:
		return "paused"
	case db.StatusPendingPlacement:
		if job.TargetKind() == db.JobTargetUnplaced {
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
		case db.LaunchStatusRunning:
			// Instance VM is up. If a sibling job on this launch is
			// genuinely Running/Starting, this job is queued behind it;
			// otherwise the agent hasn't dispatched any job yet, so
			// classify as still launching.
			if job.LaunchID != nil && launchesWithActiveJob[*job.LaunchID] {
				return "queued"
			}
			return "launching"
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

// groupedStatusPlacementMarker returns a one-cell glyph and the styled job ID
// indicating whether the job is on a rental instance ("☁", cyan) or on-prem
// (" ", default). The glyph slot is always one cell wide so that ids align
// down the column regardless of placement.
var (
	rentalIDStyle    = lipgloss.NewStyle().Foreground(tuiAccentColor)
	rentalGlyphCloud = rentalIDStyle.Render("☁")
)

func groupedStatusPlacementMarker(job *db.Job) (glyph, jobID string) {
	id := ids.FormatJobID(job.ID)
	if job.IsRentalJob() {
		return rentalGlyphCloud, rentalIDStyle.Render(id)
	}
	return " ", id
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
		if job != nil && groupedStatusBucket(job, nil, nil, time.Now()) == "running" {
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
