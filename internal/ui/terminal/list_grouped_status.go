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

type groupedStatusRenderOptions struct {
	launchLiveByID         map[int64]*db.LaunchLiveState
	launchStatusByID       map[int64]string
	placingJobIDs          map[int64]struct{}
	placementQueuedAtByJob map[int64]int64
	launchFailures         *recentLaunchFailures
	launchByID             map[int64]*db.Launch
	now                    time.Time
	launchSpinner          string
	launchingETA           groupedStatusLaunchingETA
}

type groupedStatusLaunchingETA struct {
	totalP50               time.Duration
	totalSamples           int
	stageByName            map[string]groupedStatusLaunchingStageETA
	stageEnteredAtByLaunch map[int64]int64
}

type groupedStatusLaunchingStageETA struct {
	p50     time.Duration
	samples int
	oldest  int64
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

func renderJobListGroupedStatusPlainWithOptions(jobs []*db.Job, width int, opts groupedStatusRenderOptions) string {
	rows := buildGroupedStatusRowsWithOptions(jobs, width, opts)
	if len(rows) == 0 {
		return "None\n"
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, row.text)
	}
	return strings.Join(lines, "\n") + "\n"
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

func groupedStatusLaunchIDs(jobs []*db.Job) []int64 {
	launchIDs := make([]int64, 0, len(jobs))
	seen := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		if _, ok := seen[*job.LaunchID]; ok {
			continue
		}
		seen[*job.LaunchID] = struct{}{}
		launchIDs = append(launchIDs, *job.LaunchID)
	}
	return launchIDs
}

func buildGroupedStatusRowsAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, placingJobIDs map[int64]struct{}, placementQueuedAtByJob map[int64]int64, launchFailures *recentLaunchFailures, now time.Time) []groupedStatusRow {
	return buildGroupedStatusRowsWithOptions(jobs, width, groupedStatusRenderOptions{
		launchLiveByID:         launchLiveByID,
		launchStatusByID:       launchStatusByID,
		placingJobIDs:          placingJobIDs,
		placementQueuedAtByJob: placementQueuedAtByJob,
		launchFailures:         launchFailures,
		now:                    now,
	})
}

func buildGroupedStatusRowsWithOptions(jobs []*db.Job, width int, opts groupedStatusRenderOptions) []groupedStatusRow {
	now := opts.now
	if now.IsZero() {
		now = time.Now()
	}
	opts.now = now
	running := make([]*db.Job, 0)
	paused := make([]*db.Job, 0)
	placing := make([]*db.Job, 0)
	launching := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
	unplaced := make([]*db.Job, 0)
	completions := make([]*db.Job, 0)
	failedJobs := make([]*db.Job, 0)
	killedCanceled := make([]*db.Job, 0)

	launchesWithActiveJob := computeLaunchesWithActiveJob(jobs, opts.launchLiveByID)

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch groupedStatusBucket(job, opts.launchStatusByID, launchesWithActiveJob, opts.placingJobIDs, now) {
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
			rows = appendBlockedGroupedJobRows(rows, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, now)
		} else if section.key == "launching" {
			rows = appendLaunchingGroupedJobRows(rows, section, projectWidth, width, opts)
		} else {
			for _, job := range section.jobs {
				rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, false)
			}
		}
		rows = append(rows, groupedStatusRow{text: ""})
	}

	rows = appendRecentFailedLaunchRows(rows, opts.launchFailures, projectWidth, width, now)

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

func computeLaunchesWithActiveJob(jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState) map[int64]bool {
	out := make(map[int64]bool)
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil {
			continue
		}
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			out[*job.LaunchID] = true
		}
	}
	for launchID, live := range launchLiveByID {
		if live == nil {
			continue
		}
		verb, phaseJobID, ok := campaign.ParsePhaseJobID(live.InstancePhase)
		if ok && verb == campaign.PhaseRunning && phaseJobID > 0 {
			out[launchID] = true
		}
	}
	return out
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
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, true)
		}
	}
	for _, job := range buckets[""] {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, launchLiveByID, placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, false)
	}
	return rows
}

type launchingJobBucket struct {
	key      string
	launchID int64
	launch   *db.Launch
	jobs     []*db.Job
}

func appendLaunchingGroupedJobRows(
	rows []groupedStatusRow,
	section groupedStatusSection,
	projectWidth, width int,
	opts groupedStatusRenderOptions,
) []groupedStatusRow {
	buckets := make(map[int64]*launchingJobBucket)
	order := make([]int64, 0)
	ungrouped := make([]*db.Job, 0)
	for _, job := range section.jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			ungrouped = append(ungrouped, job)
			continue
		}
		launchID := *job.LaunchID
		bucket := buckets[launchID]
		if bucket == nil {
			bucket = &launchingJobBucket{
				key:      fmt.Sprintf("launch:%d", launchID),
				launchID: launchID,
				launch:   opts.launchByID[launchID],
			}
			buckets[launchID] = bucket
			order = append(order, launchID)
		}
		bucket.jobs = append(bucket.jobs, job)
	}

	sort.SliceStable(order, func(i, j int) bool {
		left := buckets[order[i]]
		right := buckets[order[j]]
		leftCreated := launchingBucketCreatedAt(left)
		rightCreated := launchingBucketCreatedAt(right)
		if leftCreated != rightCreated {
			return leftCreated < rightCreated
		}
		return left.launchID < right.launchID
	})

	for _, launchID := range order {
		bucket := buckets[launchID]
		sort.SliceStable(bucket.jobs, func(i, j int) bool { return bucket.jobs[i].ID < bucket.jobs[j].ID })
		if len(bucket.jobs) == 1 {
			rows = appendGroupedStatusJobRow(rows, bucket.jobs[0], section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, opts.launchSpinner, opts.launchingETA, false)
			continue
		}
		rows = append(rows, launchingInstanceHeaderRow(bucket, section.key, width, opts))
		for _, job := range bucket.jobs {
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, "", groupedStatusLaunchingETA{}, true)
		}
	}
	for _, job := range ungrouped {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, opts.launchSpinner, opts.launchingETA, false)
	}
	return rows
}

func launchingBucketCreatedAt(bucket *launchingJobBucket) int64 {
	if bucket == nil {
		return 0
	}
	if bucket.launch != nil && bucket.launch.CreatedAt > 0 {
		return bucket.launch.CreatedAt
	}
	for _, job := range bucket.jobs {
		if job != nil && job.QueuedAt > 0 {
			return job.QueuedAt
		}
	}
	return 0
}

func launchingInstanceHeaderRow(bucket *launchingJobBucket, sectionKey string, width int, opts groupedStatusRenderOptions) groupedStatusRow {
	parts := groupedStatusLaunchingSuffixParts(bucket.jobs[0], opts.launchLiveByID, opts.launchByID, opts.now, opts.launchingETA)
	label := ids.FormatInstanceID(bucket.launchID)
	if glyph := groupedStatusLaunchingGlyph(bucket.jobs[0], opts.launchLiveByID, opts.launchSpinner); glyph != "" {
		label = glyph + " " + label
	}
	text := fmt.Sprintf("  %s", label)
	if len(parts) > 0 {
		text += " — " + strings.Join(parts, " · ")
	}
	text += fmt.Sprintf(" (%d jobs)", len(bucket.jobs))
	if width > 0 {
		text = truncateDisplayWidth(text, width)
	}
	return groupedStatusRow{
		text:    text,
		launch:  bucket.launch,
		section: sectionKey,
	}
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
	launchByID map[int64]*db.Launch,
	now time.Time,
	launchSpinner string,
	launchingETA groupedStatusLaunchingETA,
	indented bool,
) []groupedStatusRow {
	project, desc := groupedStatusJobParts(job)
	projectCol := formatProjectColumn(project, projectWidth)

	var suffixParts []string
	suffixJoin := " — "
	if section.key == "launching" && indented {
		suffixParts = nil
	} else if section.key == "launching" {
		suffixParts = groupedStatusLaunchingSuffixParts(job, launchLiveByID, launchByID, now, launchingETA)
		suffixJoin = " · "
	} else {
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
	}
	suffix := ""
	if len(suffixParts) > 0 {
		suffix = " — " + strings.Join(suffixParts, suffixJoin)
	}

	indent := ""
	if indented {
		indent = "  "
	}
	glyph, jobID := groupedStatusPlacementMarker(job)
	if section.key == "launching" {
		if launchGlyph := groupedStatusLaunchingGlyph(job, launchLiveByID, launchSpinner); launchGlyph != "" {
			glyph = launchGlyph
		}
	}
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

func groupedStatusLaunchingSuffixParts(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, launchByID map[int64]*db.Launch, now time.Time, eta groupedStatusLaunchingETA) []string {
	if job == nil {
		return nil
	}
	launch := launchForJob(job, launchByID)
	live := launchLiveStateForLaunchingJob(job, launchLiveByID)
	parts := []string{groupedStatusLaunchingPhase(live)}
	if elapsed := groupedStatusLaunchingElapsed(job, launch, now); elapsed != "" {
		parts = append(parts, elapsed)
	}
	if etaText := groupedStatusLaunchingETAText(launch, live, now, eta); etaText != "" {
		parts = append(parts, etaText)
	}
	return parts
}

func groupedStatusLaunchingPhase(live *db.LaunchLiveState) string {
	if live == nil {
		return "provisioning"
	}
	stage := strings.TrimSpace(live.BootstrapStage)
	if stage != "" {
		return groupedStatusBootstrapStageLabel(stage)
	}
	phase := strings.TrimSpace(live.InstancePhase)
	if strings.EqualFold(phase, "created") {
		return "provisioned, awaiting boot"
	}
	if phase == "" {
		return "provisioning"
	}
	return normalizeLaunchingStageLabel(phase)
}

func groupedStatusBootstrapStageLabel(stage string) string {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "image_pull", "image_pulling":
		return "image pulling"
	case "deps_install", "deps_installing":
		return "deps installing"
	case "agent_starting":
		return "agent starting"
	case "ready":
		return "agent ready"
	default:
		return normalizeLaunchingStageLabel(stage)
	}
}

func normalizeLaunchingStageLabel(stage string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(stage)), "_", " ")
}

func groupedStatusLaunchingElapsed(job *db.Job, launch *db.Launch, now time.Time) string {
	startedAt := int64(0)
	if launch != nil {
		if origin := launch.BootstrapOrigin(); origin != nil && *origin > 0 {
			startedAt = *origin
		} else if launch.CreatedAt > 0 {
			startedAt = launch.CreatedAt
		}
	} else if job != nil && job.QueuedAt > 0 {
		startedAt = job.QueuedAt
	}
	if startedAt <= 0 {
		return ""
	}
	return shortRelativeTime(now.Unix() - startedAt)
}

func groupedStatusLaunchingETAText(launch *db.Launch, live *db.LaunchLiveState, now time.Time, eta groupedStatusLaunchingETA) string {
	if launch != nil && launch.BootstrapDeadlineExceeded(now) {
		return "overdue"
	}
	stageETA, stageEnteredAt := groupedStatusLaunchingStageETAForLaunch(launch, live, now, eta)
	if stageETA.samples >= 5 && stageETA.p50 > 0 && stageEnteredAt > 0 && now.Sub(time.Unix(stageETA.oldest, 0)) >= db.BootstrapStageStatsMinSpan() {
		elapsed := now.Sub(time.Unix(stageEnteredAt, 0))
		return groupedStatusRemainingText(elapsed, stageETA.p50)
	}
	if launch == nil || eta.totalSamples < 5 || eta.totalP50 <= 0 {
		return ""
	}
	origin := launch.CreatedAt
	if bootstrapOrigin := launch.BootstrapOrigin(); bootstrapOrigin != nil && *bootstrapOrigin > 0 {
		origin = *bootstrapOrigin
	}
	if origin <= 0 {
		return ""
	}
	elapsed := now.Sub(time.Unix(origin, 0))
	return groupedStatusRemainingText(elapsed, eta.totalP50)
}

func groupedStatusLaunchingStageETAForLaunch(launch *db.Launch, live *db.LaunchLiveState, now time.Time, eta groupedStatusLaunchingETA) (groupedStatusLaunchingStageETA, int64) {
	if launch == nil || live == nil || eta.stageByName == nil || eta.stageEnteredAtByLaunch == nil {
		return groupedStatusLaunchingStageETA{}, 0
	}
	stage := strings.TrimSpace(live.BootstrapStage)
	if stage == "" {
		return groupedStatusLaunchingStageETA{}, 0
	}
	enteredAt := eta.stageEnteredAtByLaunch[launch.ID]
	if enteredAt <= 0 {
		return groupedStatusLaunchingStageETA{}, 0
	}
	return eta.stageByName[stage], enteredAt
}

func groupedStatusRemainingText(elapsed time.Duration, p50 time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	remaining := p50 - elapsed
	if remaining < 0 {
		remaining = 0
	}
	return "~" + groupedStatusDurationText(remaining) + " remaining"
}

func groupedStatusLaunchingGlyph(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState, spinner string) string {
	if spinner == "" {
		return ""
	}
	live := launchLiveStateForLaunchingJob(job, launchLiveByID)
	if live != nil && strings.EqualFold(strings.TrimSpace(live.BootstrapStage), "ready") {
		return "✓"
	}
	return spinner
}

func groupedStatusDurationText(d time.Duration) string {
	text := shortRelativeTime(int64(d.Seconds()))
	return strings.TrimSuffix(text, " ago")
}

func launchForJob(job *db.Job, launchByID map[int64]*db.Launch) *db.Launch {
	if job == nil || job.LaunchID == nil || launchByID == nil {
		return nil
	}
	return launchByID[*job.LaunchID]
}

func launchLiveStateForLaunchingJob(job *db.Job, launchLiveByID map[int64]*db.LaunchLiveState) *db.LaunchLiveState {
	if job == nil || job.LaunchID == nil || launchLiveByID == nil {
		return nil
	}
	return launchLiveByID[*job.LaunchID]
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
		if sectionKey == "placing" {
			label = "placing"
		}
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

// groupedStatusPlacementMarker returns a one-cell glyph and the styled job ID.
// On-prem jobs get a host marker; interruptible rentals get a cloud marker;
// other rental and unplaced jobs leave the slot blank so ids align down the
// column regardless of placement.
var (
	onPremPlacementStyle     = lipgloss.NewStyle().Foreground(tuiOnPremColor)
	interruptibleRentalStyle = lipgloss.NewStyle().Foreground(tuiInterruptColor)
	onPremGlyphHost          = onPremPlacementStyle.Render("⌂")
	rentalGlyphCloud         = onPremPlacementStyle.Render("☁")
	interruptibleGlyphCloud  = interruptibleRentalStyle.Render("☁")
)

func groupedStatusPlacementMarker(job *db.Job) (glyph, jobID string) {
	id := ids.FormatJobID(job.ID)
	if job.Priority > 0 {
		return "!", id
	}
	if job.UsesInventoryPlacement() {
		return onPremGlyphHost, onPremPlacementStyle.Render(id)
	}
	if job.UsesRentalPlacement() && job.UsesPreemptiblePlacement() {
		return interruptibleGlyphCloud, interruptibleRentalStyle.Render(id)
	}
	return " ", id
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
	idStyled := onPremPlacementStyle.Render(instanceID)

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
