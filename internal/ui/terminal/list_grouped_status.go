package terminal

import (
	"database/sql"
	"fmt"
	"log/slog"
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

// loadPlacingJobIDs returns the union of jobs with an open MoveIntent or
// PlacementIntent — the "autopilot, hands off" set the grouped UI surfaces
// as "Placing". Returns nil on error or no DB; caller renders without the
// bucket rather than failing.
func loadPlacingJobIDs(database *sql.DB) map[int64]struct{} {
	if database == nil {
		return nil
	}
	out, err := db.JobIDsWithOpenMoveOrPlacementIntents(database)
	if err != nil {
		slog.Warn("load open intents", "component", "ui.list", "error", err)
		return nil
	}
	return out
}

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
	launch    *db.Launch
	section   string
}

// recentLaunchFailures feeds the "Recent launch failures" section. recoveredIDs
// holds the subset of items whose successor is alive — those rows render dim.
type recentLaunchFailures struct {
	items             []*db.Launch
	recoveredIDs      map[int64]bool
	projectByLaunchID map[int64]string
	windowSince       time.Time
}

const (
	recentFailedLaunchMaxRows = 6
	launchFailuresSectionKey  = "launch_failures"
)

func renderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, nil, nil, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLiveState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, nil, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLaunchState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, launchStatusByID, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLaunchFailures(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, failures *recentLaunchFailures) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, launchStatusByID, nil, nil, failures, time.Now())
}

func renderJobListGroupedStatusPlainAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, placingJobIDs map[int64]struct{}, placementQueuedAtByJob map[int64]int64, launchFailures *recentLaunchFailures, now time.Time) string {
	rows := buildGroupedStatusRowsAt(jobs, width, launchLiveByID, launchStatusByID, placingJobIDs, placementQueuedAtByJob, launchFailures, now)
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
	return buildGroupedStatusRowsAt(jobs, width, launchLiveByID, launchStatusByID, nil, nil, nil, time.Now())
}

func buildGroupedStatusRowsAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, placingJobIDs map[int64]struct{}, placementQueuedAtByJob map[int64]int64, launchFailures *recentLaunchFailures, now time.Time) []groupedStatusRow {
	running := make([]*db.Job, 0)
	paused := make([]*db.Job, 0)
	placing := make([]*db.Job, 0)
	launching := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
	unplaced := make([]*db.Job, 0)
	completions := make([]*db.Job, 0)
	failedJobs := make([]*db.Job, 0)
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
		switch groupedStatusBucket(job, launchStatusByID, launchesWithActiveJob, placingJobIDs, now) {
		case "running":
			running = append(running, job)
		case "paused":
			paused = append(paused, job)
		case "placing":
			placing = append(placing, job)
		case "launching":
			launching = append(launching, job)
		case "queued":
			queued = append(queued, job)
		case "unplaced":
			unplaced = append(unplaced, job)
		case "completions":
			completions = append(completions, job)
		case "failures":
			failedJobs = append(failedJobs, job)
		case "killed_canceled":
			killedCanceled = append(killedCanceled, job)
		}
	}

	for _, s := range [][]*db.Job{running, paused, placing, launching, queued, unplaced, completions, failedJobs, killedCanceled} {
		sort.SliceStable(s, func(i, j int) bool { return s[i].ID < s[j].ID })
	}

	sections := []groupedStatusSection{
		{title: "Running", key: "running", jobs: running},
		{title: "Paused", key: "paused", jobs: paused},
		{title: "Queued", key: "queued", jobs: queued},
		{title: "Placing", key: "placing", jobs: placing},
		{title: "Launching", key: "launching", jobs: launching},
		{title: "Unplaced", key: "unplaced", jobs: unplaced},
		{title: "Completed", key: "completions", jobs: completions},
		{title: "Failed", key: "failures", jobs: failedJobs},
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
			rows = appendBlockedGroupedJobRows(rows, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, now)
		} else {
			for _, job := range section.jobs {
				rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, now, false)
			}
		}
		rows = append(rows, groupedStatusRow{text: ""})
	}

	rows = appendRecentFailedLaunchRows(rows, launchFailures, projectWidth, width, now)

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
	placementQueuedAtByJob map[int64]int64,
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
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, now, true)
		}
	}
	for _, job := range buckets[""] {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, now, false)
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
	placementQueuedAtByJob map[int64]int64,
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
	if timing := groupedStatusTimingSuffix(job, section.key, placementQueuedAtByJob, now); timing != "" {
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

func groupedStatusTimingSuffix(job *db.Job, sectionKey string, placementQueuedAtByJob map[int64]int64, now time.Time) string {
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
		if displayAt, ok := placementQueuedAtByJob[job.ID]; ok && displayAt > 0 {
			placedAt = displayAt
		}
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

func groupedStatusBucket(job *db.Job, launchStatusByID map[int64]string, launchesWithActiveJob map[int64]bool, placingJobIDs map[int64]struct{}, now time.Time) string {
	status := job.EffectiveStatus()
	// Launch-level pause overrides the job-status bucket for any non-terminal
	// job: the job is not actually progressing while the rental is paused.
	if launchStatusForJob(job, launchStatusByID) == db.LaunchStatusPaused {
		switch status {
		case db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusPendingPlacement, db.StatusPaused:
			return "paused"
		}
	}
	// An open MoveIntent / PlacementIntent surfaces as Placing so the job
	// does not flicker through Unplaced/Launching while the move runs.
	// See specs/job-move.allium § AutopilotIgnoresMovingJobs.
	if _, placing := placingJobIDs[job.ID]; placing {
		switch status {
		case db.StatusQueued, db.StatusPendingPlacement:
			return "placing"
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
	if job.Priority > 0 {
		return "!", id
	}
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
		if job != nil && groupedStatusBucket(job, nil, nil, nil, time.Now()) == "running" {
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

// Failures whose successor is alive render dim so they recede without
// being hidden — the count in the section header stays honest.
var failureDimStyle = lipgloss.NewStyle().Faint(true)

func appendRecentFailedLaunchRows(
	rows []groupedStatusRow,
	failures *recentLaunchFailures,
	projectWidth, width int,
	now time.Time,
) []groupedStatusRow {
	if failures == nil || len(failures.items) == 0 {
		return rows
	}

	total := len(failures.items)
	windowText := ""
	if !failures.windowSince.IsZero() {
		windowText = " in last " + formatProjectRecentWindow(now.Sub(failures.windowSince))
	}
	headerText := fmt.Sprintf("Recent launch failures (%d%s", total, windowText)
	if recovered := len(failures.recoveredIDs); recovered > 0 {
		headerText += fmt.Sprintf(", %d already replaced", recovered)
	}
	headerText += "):"
	rows = append(rows, groupedStatusRow{
		text:     headerText,
		isHeader: true,
		section:  launchFailuresSectionKey,
	})

	type bucket struct {
		reason string
		items  []*db.Launch
	}
	bucketIdx := make(map[string]int)
	buckets := make([]bucket, 0, 4)
	for _, f := range failures.items {
		if f == nil {
			continue
		}
		reason := strings.TrimSpace(db.HumanizeTerminationReason(f.TerminationReason))
		if reason == "" {
			reason = "unknown"
		}
		if i, ok := bucketIdx[reason]; ok {
			buckets[i].items = append(buckets[i].items, f)
			continue
		}
		bucketIdx[reason] = len(buckets)
		buckets = append(buckets, bucket{reason: reason, items: []*db.Launch{f}})
	}
	sort.SliceStable(buckets, func(i, j int) bool {
		if len(buckets[i].items) != len(buckets[j].items) {
			return len(buckets[i].items) > len(buckets[j].items)
		}
		return buckets[i].reason < buckets[j].reason
	})

	emitted := 0
emit:
	for _, b := range buckets {
		rows = append(rows, groupedStatusRow{
			text:      fmt.Sprintf("  reason: %s (%d)", b.reason, len(b.items)),
			isBlocked: true,
			section:   launchFailuresSectionKey,
		})
		for _, item := range b.items {
			if emitted >= recentFailedLaunchMaxRows {
				break emit
			}
			rows = append(rows, groupedStatusRow{
				text:    formatLaunchFailureRow(item, failures, projectWidth, width, now),
				launch:  item,
				section: launchFailuresSectionKey,
			})
			emitted++
		}
	}

	if total > emitted {
		rows = append(rows, groupedStatusRow{
			text:    fmt.Sprintf("  + %d more (weft instance list --status failed)", total-emitted),
			section: launchFailuresSectionKey,
		})
	}
	rows = append(rows, groupedStatusRow{text: ""})
	return rows
}

func formatLaunchFailureRow(
	f *db.Launch,
	failures *recentLaunchFailures,
	projectWidth, width int,
	now time.Time,
) string {
	instanceID := ids.FormatInstanceID(f.ID)
	idStyled := rentalIDStyle.Render(instanceID)

	project := ""
	if failures.projectByLaunchID != nil {
		project = strings.TrimSpace(failures.projectByLaunchID[f.ID])
	}
	if project == "" {
		project = "—"
	}
	projectCol := formatProjectColumn(project, projectWidth)

	gpuBrief := strings.TrimSpace(f.DisplayGPUBrief())
	costPart := ""
	if f.CostPerHourCents > 0 {
		costPart = fmt.Sprintf(" @ $%.2f/hr", float64(f.CostPerHourCents)/100)
	}
	hardware := strings.TrimSpace(gpuBrief + costPart)

	detail := strings.TrimSpace(f.TerminationDetail)
	if detail == "" {
		detail = strings.TrimSpace(db.HumanizeTerminationReason(f.TerminationReason))
	}

	suffixParts := make([]string, 0, 2)
	if detail != "" {
		suffixParts = append(suffixParts, detail)
	}
	if f.EndedAt != nil && *f.EndedAt > 0 {
		suffixParts = append(suffixParts, "failed "+shortRelativeTime(now.Unix()-*f.EndedAt))
	}
	suffix := ""
	if len(suffixParts) > 0 {
		suffix = " — " + strings.Join(suffixParts, " — ")
	}

	prefix := fmt.Sprintf("    - %s %s — %s ", rentalGlyphCloud, idStyled, projectCol)
	desc := hardware
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

	if failures.recoveredIDs[f.ID] {
		line = failureDimStyle.Render(line)
	}
	return line
}
