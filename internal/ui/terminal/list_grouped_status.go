package terminal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/jobview"
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
	launch    *db.Launch
	section   string
	// expandToggle, when non-empty, marks a selectable row that toggles an
	// in-place disclosure. The value identifies what it toggles (e.g.
	// failedInstancesSectionKey).
	expandToggle string
}

type groupedStatusRenderOptions struct {
	launchLiveByID         map[int64]*db.LaunchLiveState
	launchStatusByID       map[int64]string
	placingJobIDs          map[int64]struct{}
	placementQueuedAtByJob map[int64]int64
	placementStatusByJob   map[int64]jobview.PlacementStatus
	overloadedHostsByName  map[string]bool
	failedInstances        *recentFailedInstances
	launchByID             map[int64]*db.Launch
	now                    time.Time
	launchSpinner          string
	launchingETA           groupedStatusLaunchingETA
	// blockedDetail carries the structured launch/reuse breakdown for unplaced
	// jobs, keyed by job ID. expandedBlocked records which of those rows the
	// user has opened for an in-place per-avenue disclosure.
	blockedDetail   map[int64]*blockreason.Structured
	expandedBlocked map[int64]bool
	// interactive is true for the live TUI render and false for plain text
	// output. The failed-instances section uses it to decide whether to
	// collapse the FYI buckets behind an expand toggle (TUI) or render all
	// buckets unconditionally (plain output).
	interactive bool
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

// recentFailedInstances feeds the "Recent failed instances" section.
//   - chainTerminalByLaunchID maps each failed launch to the status of the
//     terminal launch in its relaunch chain (db.LaunchChainTerminalStatuses).
//   - jobOutcomeByLaunchID maps each failed launch to the current state of the
//     job it carried; absence means the instance never carried a job (a dud).
type recentFailedInstances struct {
	items                   []*db.Launch
	projectByLaunchID       map[int64]string
	jobOutcomeByLaunchID    map[int64]db.LaunchJobOutcome
	chainTerminalByLaunchID map[int64]string
}

const failedInstancesSectionKey = "failed_instances"

func renderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, nil, nil, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLiveState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, nil, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLaunchState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, launchStatusByID, nil, nil, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithFailedInstances(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, failures *recentFailedInstances) string {
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

func renderJobListGroupedStatusPlainAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, placingJobIDs map[int64]struct{}, placementQueuedAtByJob map[int64]int64, failedInstances *recentFailedInstances, now time.Time) string {
	rows := buildGroupedStatusRowsAt(jobs, width, launchLiveByID, launchStatusByID, placingJobIDs, placementQueuedAtByJob, failedInstances, now)
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

func buildGroupedStatusRowsAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string, placingJobIDs map[int64]struct{}, placementQueuedAtByJob map[int64]int64, failedInstances *recentFailedInstances, now time.Time) []groupedStatusRow {
	return buildGroupedStatusRowsWithOptions(jobs, width, groupedStatusRenderOptions{
		launchLiveByID:         launchLiveByID,
		launchStatusByID:       launchStatusByID,
		placingJobIDs:          placingJobIDs,
		placementQueuedAtByJob: placementQueuedAtByJob,
		failedInstances:        failedInstances,
		now:                    now,
	})
}

func buildGroupedStatusRowsWithOptions(jobs []*db.Job, width int, opts groupedStatusRenderOptions) []groupedStatusRow {
	now := opts.now
	if now.IsZero() {
		now = time.Now()
	}
	opts.now = now
	if len(opts.placementStatusByJob) > 0 {
		jobs = jobview.ExpandJobsForOpenMoves(jobs, opts.placementStatusByJob)
	}
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
	launchesEverReady := jobview.LaunchesEverReady(opts.launchByID)

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch groupedStatusBucketWithOptions(job, launchesWithActiveJob, launchesEverReady, opts) {
		case string(jobview.BucketRunning):
			running = append(running, job)
		case string(jobview.BucketPaused):
			paused = append(paused, job)
		case string(jobview.BucketPlacing):
			placing = append(placing, job)
		case string(jobview.BucketLaunching):
			launching = append(launching, job)
		case string(jobview.BucketQueued):
			queued = append(queued, job)
		case string(jobview.BucketUnplaced):
			unplaced = append(unplaced, job)
		case string(jobview.BucketCompletions):
			completions = append(completions, job)
		case string(jobview.BucketFailures):
			failedJobs = append(failedJobs, job)
		case string(jobview.BucketKilledCanceled):
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
			rows = appendBlockedGroupedJobRows(rows, section, projectWidth, width, opts, now)
		} else if section.key == "launching" {
			rows = appendLaunchingGroupedJobRows(rows, section, projectWidth, width, opts)
		} else {
			for _, job := range section.jobs {
				rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, false, opts.overloadedHostsByName)
			}
		}
		rows = append(rows, groupedStatusRow{text: ""})
	}

	// The interactive TUI renders recent abnormal instance terminations as a
	// severity-gated footer element (buildInstanceHealthFooter), not as a list
	// section. Non-interactive/plain output (exit summaries, `weft jobs list
	// --group-by status`) keeps the full inline section.
	if !opts.interactive {
		rows = appendRecentFailedInstanceRows(rows, opts.failedInstances, projectWidth, width, now)
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

func buildListGroupedRows(jobs []*db.Job, mode listGroupMode, width int, layout jobListLayout) []groupedStatusRow {
	if len(jobs) == 0 {
		return nil
	}
	groups := map[string][]*db.Job{}
	order := make([]string, 0)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		key := listGenericGroupKey(job, mode)
		if _, ok := groups[key]; !ok {
			groups[key] = nil
			order = append(order, key)
		}
		groups[key] = append(groups[key], job)
	}
	sort.Strings(order)
	rows := make([]groupedStatusRow, 0, len(jobs)+len(order)*3)
	for groupIdx, key := range order {
		if groupIdx > 0 {
			rows = append(rows, groupedStatusRow{text: ""})
		}
		groupJobs := groups[key]
		rows = append(rows, groupedStatusRow{
			text:     fmt.Sprintf("%s (%d):", key, len(groupJobs)),
			isHeader: true,
			section:  string(mode),
		})
		header := "  " + formatJobListHeader(layout)
		rows = append(rows, groupedStatusRow{text: header, section: string(mode)})
		for _, job := range groupJobs {
			rows = append(rows, groupedStatusRow{
				text:    "  " + formatJobListRow(layout, job),
				job:     job,
				section: string(mode),
			})
		}
	}
	if width > 0 {
		for i := range rows {
			rows[i].text = truncateDisplayWidth(rows[i].text, width)
		}
	}
	return rows
}

func listGenericGroupKey(job *db.Job, mode listGroupMode) string {
	switch mode {
	case listGroupProject:
		return projectGroupLabel(job)
	case listGroupHost:
		if job == nil {
			return "(no host/instance)"
		}
		if host := strings.TrimSpace(formatJobListHost(job)); host != "" && host != "-" && host != "—" {
			return host
		}
		return "(unassigned)"
	default:
		return listGroupModeLabel(mode)
	}
}

func computeLaunchesWithActiveJob(jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState) map[int64]bool {
	return jobview.LaunchesWithActiveJob(jobs, launchLiveByID)
}

// blockedDetailStyle dims the per-avenue disclosure lines so they recede
// beneath the job row they belong to.
var blockedDetailStyle = lipgloss.NewStyle().Faint(true)
var moveAttemptDimStyle = lipgloss.NewStyle().Faint(true)

// appendBlockedGroupedJobRows renders an Unplaced/Queued section, grouping
// jobs by their blocked or waiting reason. Each distinct reason becomes a
// subheader ("  blocked: <reason> (N)" or "  waiting: <reason> (N)") followed
// by indented job rows. Jobs with no reason are emitted last without a
// subheader at the normal indent.
//
// When every placement-failure job in the section shares the same launch
// blocker, that blocker is hoisted to a single section-level line and the
// per-job subheaders carry only the reuse-side headline. A job with a
// structured launch/reuse breakdown gains a disclosure marker; when expanded,
// its full per-avenue detail is emitted as dim, non-selectable rows.
func appendBlockedGroupedJobRows(
	rows []groupedStatusRow,
	section groupedStatusSection,
	projectWidth, width int,
	opts groupedStatusRenderOptions,
	now time.Time,
) []groupedStatusRow {
	rows = appendActiveIncidentsRows(rows, section, opts.blockedDetail, section.key)
	sharedLaunch, sharedCount := commonLaunchBlocker(section.jobs, opts.blockedDetail)
	if sharedLaunch != "" {
		scope := "for all"
		if sharedCount < len(section.jobs) {
			scope = fmt.Sprintf("for %d of %d", sharedCount, len(section.jobs))
		}
		rows = append(rows, groupedStatusRow{
			text:      fmt.Sprintf("  launch blocked %s: %s", scope, sharedLaunch),
			isBlocked: true,
			section:   section.key,
		})
	}
	order := make([]blockedReasonBucketKey, 0, len(section.jobs))
	buckets := make(map[blockedReasonBucketKey][]*db.Job, len(section.jobs))
	for _, job := range section.jobs {
		key := blockedBucketKey(job, section.key, opts.blockedDetail, sharedLaunch != "")
		if _, ok := buckets[key]; !ok {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], job)
	}
	for _, key := range order {
		if key.reason == "" {
			continue
		}
		jobs := buckets[key]
		rows = append(rows, groupedStatusRow{
			text:      groupedStatusBlockedBucketHeader(key, jobs),
			isBlocked: true,
			section:   section.key,
		})
		for _, job := range jobs {
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, true, opts.overloadedHostsByName)
			rows = appendBlockedDisclosureRows(rows, job, opts, section.key, width)
		}
	}
	for _, job := range buckets[blockedReasonBucketKey{}] {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, nil, now, "", groupedStatusLaunchingETA{}, false, opts.overloadedHostsByName)
		rows = appendBlockedDisclosureRows(rows, job, opts, section.key, width)
	}
	return rows
}

func groupedStatusBlockedBucketHeader(key blockedReasonBucketKey, jobs []*db.Job) string {
	text := fmt.Sprintf("  %s: %s (%d)", key.kind, key.reason, len(jobs))
	if len(jobs) == 0 {
		return text
	}
	for _, job := range jobs {
		if job == nil || !job.DisplayMoveDim {
			return text
		}
	}
	return moveAttemptDimStyle.Render(text)
}

// activeIncidentSummary describes one fingerprint affecting ≥2 jobs in the
// section. Sample is one job's launch reason picked deterministically (lowest
// job ID) so the rollup row can carry the human-readable upstream message
// alongside the fingerprint identifier.
type activeIncidentSummary struct {
	fingerprint string
	count       int
	sample      string
	sampleJobID int64
}

// collectActiveIncidents scans the section for fingerprints shared across ≥2
// jobs and returns them in deterministic order (highest count first, ties
// broken by fingerprint). Single-job fingerprints don't qualify — a single
// blocked job is not an "incident", it's a job-specific failure.
func collectActiveIncidents(jobs []*db.Job, detail map[int64]*blockreason.Structured) []activeIncidentSummary {
	if len(detail) == 0 {
		return nil
	}
	type bucket struct {
		count       int
		sample      string
		sampleJobID int64
	}
	by := make(map[string]*bucket)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		d := detail[job.ID]
		if d == nil {
			continue
		}
		fp := strings.TrimSpace(d.Fingerprint)
		if fp == "" {
			continue
		}
		b, ok := by[fp]
		if !ok {
			b = &bucket{}
			by[fp] = b
		}
		b.count++
		candidate := strings.TrimSpace(d.Launch)
		if candidate == "" {
			candidate = strings.TrimSpace(d.Summary)
		}
		// Only adopt this job as the sample when it actually carries a
		// non-empty message — otherwise a lower-ID job with neither Launch
		// nor Summary would wipe a meaningful sample picked up earlier.
		if candidate == "" {
			continue
		}
		if b.sample == "" || job.ID < b.sampleJobID {
			b.sample = candidate
			b.sampleJobID = job.ID
		}
	}
	out := make([]activeIncidentSummary, 0, len(by))
	for fp, b := range by {
		if b.count < 2 {
			continue
		}
		out = append(out, activeIncidentSummary{
			fingerprint: fp,
			count:       b.count,
			sample:      b.sample,
			sampleJobID: b.sampleJobID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].fingerprint < out[j].fingerprint
	})
	return out
}

// appendActiveIncidentsRows emits one rollup row per active incident
// (fingerprint shared by ≥2 jobs in the section). The rollup precedes the
// "launch blocked for all" hoist and the per-bucket subheaders, so a user
// scanning the section sees the systemic failures first. Each row reads
// "  ⚠ vastai/search-offers/400/bad-field:driver_vers — N jobs: <message>".
// Per-job rows still appear under their normal buckets — the rollup is
// additive and doesn't replace anything.
func appendActiveIncidentsRows(rows []groupedStatusRow, section groupedStatusSection, detail map[int64]*blockreason.Structured, sectionKey string) []groupedStatusRow {
	incidents := collectActiveIncidents(section.jobs, detail)
	if len(incidents) == 0 {
		return rows
	}
	for _, inc := range incidents {
		text := fmt.Sprintf("  ⚠ %s — %s", inc.fingerprint, plural(inc.count, "job", "jobs"))
		if sample := strings.TrimSpace(inc.sample); sample != "" {
			text += ": " + sample
		}
		rows = append(rows, groupedStatusRow{
			text:      text,
			isBlocked: true,
			section:   sectionKey,
		})
	}
	return rows
}

// plural is the inline plural-helper twin of internal/blockreason.plural — TUI
// rendering shouldn't reach into another package for a one-liner.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// commonLaunchBlocker returns the launch-side blocker shared by the
// placement-failure jobs in the section, along with the number of section
// jobs covered by that blocker. Returns ("", 0) when fewer than two
// placement-failure jobs share a blocker — a single matching job is a
// job-specific failure, not a section-wide hoist. Single-cause jobs
// (preconditions) are ignored: they carry no launch blocker and do not
// prevent hoisting.
//
// Equality is computed on Fingerprint when the participating jobs all
// carry one — that lets a systemic upstream failure (e.g. every vastai
// search-offers call 400-ing on the same field) hoist as a shared blocker
// even when the per-job Launch strings differ on cosmetic prefix. Falls
// back to exact-Launch-string equality when fingerprints are absent or
// disagree, preserving today's behavior for unclassified errors.
//
// The returned count lets the caller distinguish "for all" from "for N of
// M" so the hoist line doesn't overstate its scope when only a subset of
// the section's jobs share the blocker (the rest may be deferred for
// unrelated reasons that don't persist a Structured breakdown).
func commonLaunchBlocker(jobs []*db.Job, detail map[int64]*blockreason.Structured) (string, int) {
	if len(detail) == 0 {
		return "", 0
	}
	fingerprint := ""
	launch := ""
	launchAgreed := true
	useFingerprint := true
	covered := 0
	for _, job := range jobs {
		if job == nil {
			continue
		}
		d := detail[job.ID]
		if d == nil || !d.IsPlacementFailure() {
			continue
		}
		l := strings.TrimSpace(d.Launch)
		if l == "" {
			return "", 0
		}
		covered++
		if launch == "" {
			launch = l
		} else if l != launch {
			launchAgreed = false
		}
		fp := strings.TrimSpace(d.Fingerprint)
		if fp == "" {
			useFingerprint = false
		} else if fingerprint == "" {
			fingerprint = fp
		} else if fp != fingerprint {
			useFingerprint = false
		}
		// Once neither the launch strings agree nor the fingerprints all
		// match, no hoist is possible. Bail early so we don't return the
		// last-seen Launch as a falsely shared blocker.
		if !launchAgreed && !useFingerprint {
			return "", 0
		}
	}
	if covered < 2 {
		return "", 0
	}
	if launchAgreed && launch != "" {
		return launch, covered
	}
	if useFingerprint && fingerprint != "" {
		return fingerprint, covered
	}
	return "", 0
}

// blockedBucketKey is the subheader grouping key for a blocked job. Once the
// shared launch blocker is hoisted, placement-failure jobs group by their
// reuse-side headline; everything else keeps the resolved one-line reason.
//
// When the job carries a Fingerprint, it overrides the flat string as the
// bucket key so jobs with cosmetically different filter prefixes but the
// same systemic upstream cause collapse into one bucket. The fingerprint is
// rendered in human-readable form (the launch reason or the fingerprint
// itself) — the raw fingerprint string is the bucket identity, but display
// stays close to today's compact reason text.
type blockedReasonBucketKey struct {
	kind   blockreason.Kind
	reason string
}

func blockedBucketKey(job *db.Job, sectionKey string, detail map[int64]*blockreason.Structured, launchHoisted bool) blockedReasonBucketKey {
	if job != nil {
		if d := detail[job.ID]; d != nil {
			if launchHoisted && d.IsPlacementFailure() {
				// When there are no actual reuse rejections to enumerate, the
				// "no running instances to reuse" headline is non-actionable
				// noise — the section hoist already says why these jobs are
				// blocked. Falling through to "" lets the renderer place the
				// jobs inline under the hoist without a redundant subheader.
				if len(d.Reuse) == 0 {
					return blockedReasonBucketKey{}
				}
				return blockedReasonBucketKey{kind: blockreason.KindBlocked, reason: d.ReuseHeadline()}
			}
			if fp := strings.TrimSpace(d.Fingerprint); fp != "" {
				return blockedReasonBucketKey{kind: blockreason.KindBlocked, reason: fp}
			}
		}
	}
	return groupedStatusBlockedReason(job, sectionKey)
}

// appendBlockedDisclosureRows marks an expandable blocked job row with a
// disclosure triangle and, when the row is expanded, emits its full
// per-avenue breakdown as dim, non-selectable detail rows.
func appendBlockedDisclosureRows(
	rows []groupedStatusRow,
	job *db.Job,
	opts groupedStatusRenderOptions,
	sectionKey string,
	width int,
) []groupedStatusRow {
	if job == nil || len(rows) == 0 {
		return rows
	}
	d := opts.blockedDetail[job.ID]
	if d == nil || !d.IsPlacementFailure() {
		return rows
	}
	expanded := opts.expandedBlocked[job.ID]
	last := len(rows) - 1
	rows[last].text = applyDisclosureMarker(rows[last].text, expanded)
	if !expanded {
		return rows
	}
	const indent = "        "
	const continuation = indent + "  "
	for _, line := range d.DetailLines() {
		// Wrap long lines (e.g. multi-line vastai stderr surfaced via
		// LaunchDetail) to the terminal width so the user can read the full
		// underlying message in expanded form. wrapDisplayWidth strips leading
		// whitespace via strings.Fields, so wrap the bare line against the
		// indent-adjusted width and prepend the indent to each row.
		var wrapped []string
		if width > len(indent) {
			wrapped = wrapDisplayWidth(line, width-len(indent))
		} else {
			wrapped = []string{line}
		}
		for i, w := range wrapped {
			prefix := indent
			if i > 0 {
				prefix = continuation
			}
			rows = append(rows, groupedStatusRow{
				text:    blockedDetailStyle.Render(prefix + w),
				section: sectionKey,
			})
		}
	}
	return rows
}

// applyDisclosureMarker swaps the two-space indent of an expandable blocked
// job row for a disclosure triangle, preserving display width.
func applyDisclosureMarker(text string, expanded bool) string {
	marker := "▸ "
	if expanded {
		marker = "▾ "
	}
	if strings.HasPrefix(text, "  ") {
		return marker + text[2:]
	}
	return marker + text
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
			rows = appendGroupedStatusJobRow(rows, bucket.jobs[0], section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, opts.launchSpinner, opts.launchingETA, false, opts.overloadedHostsByName)
			continue
		}
		rows = append(rows, launchingInstanceHeaderRow(bucket, section.key, width, opts))
		for _, job := range bucket.jobs {
			rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, "", groupedStatusLaunchingETA{}, true, opts.overloadedHostsByName)
		}
	}
	for _, job := range ungrouped {
		rows = appendGroupedStatusJobRow(rows, job, section, projectWidth, width, opts.launchLiveByID, opts.placementQueuedAtByJob, opts.launchByID, opts.now, opts.launchSpinner, opts.launchingETA, false, opts.overloadedHostsByName)
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

func groupedStatusBlockedReason(job *db.Job, sectionKey string) blockedReasonBucketKey {
	if job == nil {
		return blockedReasonBucketKey{}
	}
	if sectionKey != "queued" && sectionKey != "unplaced" {
		return blockedReasonBucketKey{}
	}
	resolved := blockreason.Resolve(job, blockreason.Options{Compact: true})
	if resolved.Reason == "" {
		return blockedReasonBucketKey{}
	}
	kind := resolved.Kind
	if kind == blockreason.KindNone {
		kind = blockreason.KindBlocked
	}
	return blockedReasonBucketKey{kind: kind, reason: resolved.Reason}
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
	overloadedHostsByName map[string]bool,
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
		if moveText := groupedStatusMoveSuffix(job); moveText != "" {
			suffixParts = append(suffixParts, moveText)
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
	if job != nil && job.TargetKind() == db.JobTargetInventoryHost && overloadedHostsByName[strings.TrimSpace(job.Host)] {
		line = tuiFailedStyle.Render(line)
	}
	if job != nil && job.DisplayMoveDim {
		line = moveAttemptDimStyle.Render(line)
	}
	return append(rows, groupedStatusRow{
		text:    line,
		job:     job,
		section: section.key,
	})
}

func groupedStatusMoveSuffix(job *db.Job) string {
	if job == nil || strings.TrimSpace(job.DisplayMoveSource) == "" || strings.TrimSpace(job.DisplayMoveTarget) == "" {
		return ""
	}
	path := strings.TrimSpace(job.DisplayMoveSource) + " -> " + strings.TrimSpace(job.DisplayMoveTarget)
	if job.DisplayMoveDim {
		return "move target " + path + " (non-authoritative)"
	}
	return "move pending " + path
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

// bootstrapDeadlineWarnThreshold is the window before a launch's bootstrap
// deadline within which we surface a "terminate in <d>" countdown in place
// of the stage ETA. Picked to give a clear heads-up without being noisy for
// healthy boots that finish in under ten minutes.
const bootstrapDeadlineWarnThreshold = 10 * time.Minute

func groupedStatusLaunchingETAText(launch *db.Launch, live *db.LaunchLiveState, now time.Time, eta groupedStatusLaunchingETA) string {
	if launch != nil && launch.BootstrapDeadlineExceeded(now) {
		return "overdue"
	}
	if launch != nil && launch.BootstrapDeadlineUnix != nil {
		remaining := time.Unix(*launch.BootstrapDeadlineUnix, 0).Sub(now)
		if remaining > 0 && remaining <= bootstrapDeadlineWarnThreshold {
			return "terminate in " + remaining.Truncate(time.Second).String()
		}
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
	if sectionKey == "failures" && job.EndTime != nil && *job.EndTime > 0 {
		return "failed " + shortRelativeTime(now.Unix()-*job.EndTime)
	}
	if sectionKey == "unplaced" && job.EndTime != nil && *job.EndTime > 0 {
		reason := strings.ToLower(strings.TrimSpace(queueblock.Display(job, nil).Reason))
		if strings.Contains(reason, "retry budget exceeded") || strings.Contains(reason, "max attempts") {
			return "retry rejected " + shortRelativeTime(now.Unix()-*job.EndTime)
		}
		// Runaway-breaker pause: the autopilot has stepped back from
		// re-placing this job because the project's recent attempts
		// produced too many infra failures (or, separately, too many
		// launch failures) without forward progress. "retry pending"
		// implies a backoff timer is counting down — misleading when
		// the pause is conditional on a successful auto-probe rather
		// than a clock. See internal/campaign/relaunch.go § runaway.
		if strings.Contains(reason, "paused: repeated") || strings.Contains(reason, "retry blocked") {
			return "breaker paused " + shortRelativeTime(now.Unix()-*job.EndTime)
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

func groupedStatusBucket(job *db.Job, launchStatusByID map[int64]string, launchesWithActiveJob map[int64]bool, launchesEverReady map[int64]bool, placingJobIDs map[int64]struct{}, now time.Time) string {
	_, placing := placingJobIDs[job.ID]
	return string(jobview.ClassifyBucket(job, jobview.ClassifyInput{
		LaunchStatusByID:     launchStatusByID,
		LaunchesWithActive:   launchesWithActiveJob,
		LaunchEverReady:      launchesEverReady,
		HasOpenPlacingIntent: placing,
	}))
}

func groupedStatusBucketWithOptions(job *db.Job, launchesWithActiveJob map[int64]bool, launchesEverReady map[int64]bool, opts groupedStatusRenderOptions) string {
	if job != nil && job.DisplayMoveDim {
		return groupedStatusBucket(job, opts.launchStatusByID, launchesWithActiveJob, launchesEverReady, nil, opts.now)
	}
	if opts.placementStatusByJob != nil && job != nil {
		if ps, ok := opts.placementStatusByJob[job.ID]; ok && ps.Bucket != "" {
			return string(ps.Bucket)
		}
	}
	return groupedStatusBucket(job, opts.launchStatusByID, launchesWithActiveJob, launchesEverReady, opts.placingJobIDs, opts.now)
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
		if job != nil && groupedStatusBucket(job, nil, nil, nil, nil, time.Now()) == "running" {
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

// failureOutcome classifies how the relaunch series of a failed instance
// turned out. The user cares about the fate of the work, not which instance
// IDs died.
type failureOutcome int

const (
	// failureSeriesFailed: the relaunch chain is exhausted and the job ended
	// in failure (e.g. retry budget exhausted).
	failureSeriesFailed failureOutcome = iota
	// failureAwaitingPlacement: the chain is exhausted but the job is
	// re-queued and waiting for placement — the bucket that needs attention.
	failureAwaitingPlacement
	// failureReplacedRunning: the failed instance's affected work has a live
	// successor, or the job is running again.
	failureReplacedRunning
	// failureSeriesSucceeded: the chain/job ultimately completed.
	failureSeriesSucceeded
	// failureDud: the instance never carried a job — it failed before it
	// could do work.
	failureDud
)

// failureOutcomeOrder lists buckets in attention-first display order.
var failureOutcomeOrder = []failureOutcome{
	failureSeriesFailed, failureAwaitingPlacement, failureReplacedRunning,
	failureSeriesSucceeded, failureDud,
}

func failureGroupLabel(o failureOutcome) string {
	switch o {
	case failureSeriesFailed:
		return "failed"
	case failureAwaitingPlacement:
		return "awaiting placement"
	case failureReplacedRunning:
		return "replaced/running"
	case failureSeriesSucceeded:
		return "succeeded"
	default:
		return "dud"
	}
}

// failureCountPhrase renders a count for one bucket, e.g. "3 failed" or
// "2 duds".
func failureCountPhrase(o failureOutcome, n int) string {
	if o == failureDud {
		return pluralize(n, "dud", "duds")
	}
	return fmt.Sprintf("%d %s", n, failureGroupLabel(o))
}

// classifyFailureOutcome follows the relaunch chain to its terminal launch and,
// when that chain is exhausted, consults the job's current status — the chain
// alone cannot distinguish an abandoned job from one re-queued for a fresh
// attempt.
func classifyFailureOutcome(f *db.Launch, failures *recentFailedInstances) failureOutcome {
	if _, hasJob := failures.jobOutcomeByLaunchID[f.ID]; !hasJob {
		return failureDud
	}
	switch failures.chainTerminalByLaunchID[f.ID] {
	case db.LaunchStatusCompleted:
		return failureSeriesSucceeded
	case db.LaunchStatusFailed, db.LaunchStatusCancelled, "":
		// Chain exhausted (or unknown) — fall through to the job's status.
	default:
		// running / paused / grace / launching — the chain is still live.
		return failureReplacedRunning
	}
	switch failures.jobOutcomeByLaunchID[f.ID].Status {
	case db.StatusCompleted:
		return failureSeriesSucceeded
	case db.StatusRunning, db.StatusStarting:
		return failureReplacedRunning
	case db.StatusQueued, db.StatusPendingPlacement, "orphaned":
		// "orphaned" is a job_status-view value: the job's instance died and
		// it is waiting to be re-placed.
		return failureAwaitingPlacement
	default:
		return failureSeriesFailed
	}
}

func launchEndedAt(f *db.Launch) int64 {
	if f != nil && f.EndedAt != nil {
		return *f.EndedAt
	}
	return 0
}

// formatFailureSpan describes the time range a roll-up of failed launches
// covers: a single item as "<age> ago", multiple as "last <span>". Both forms
// use shortRelativeTime's coarse rounding so the output stays clean.
func formatFailureSpan(items []*db.Launch, now time.Time) string {
	var oldest int64
	count := 0
	for _, f := range items {
		e := launchEndedAt(f)
		if e <= 0 {
			continue
		}
		count++
		if oldest == 0 || e < oldest {
			oldest = e
		}
	}
	if count == 0 {
		return ""
	}
	age := shortRelativeTime(now.Unix() - oldest)
	if count == 1 {
		return age
	}
	return "last " + strings.TrimSuffix(age, " ago")
}

// dominantFailureFactor reports a host/provider attribute shared by a strong
// majority (>=60%, at least 3) of the failed instances, most specific first.
// A shared machine or data center points at a localized provider fault; a
// shared GPU or provider points at a bad offer class. Empty when the failures
// have no common factor — likely independent transient noise.
func dominantFailureFactor(items []*db.Launch) string {
	dims := []struct {
		label string
		get   func(*db.Launch) string
	}{
		{"machine", func(l *db.Launch) string { return strings.TrimSpace(l.MachineID) }},
		{"data center", func(l *db.Launch) string { return strings.TrimSpace(l.DataCenter) }},
		{"GPU", func(l *db.Launch) string { return strings.TrimSpace(l.ResolvedGPUName) }},
		{"provider", func(l *db.Launch) string { return strings.TrimSpace(l.Provider) }},
	}
	n := len(items)
	for _, d := range dims {
		counts := make(map[string]int)
		for _, it := range items {
			if v := d.get(it); v != "" {
				counts[v]++
			}
		}
		best, bestN := "", 0
		for v, c := range counts {
			if c > bestN {
				best, bestN = v, c
			}
		}
		if bestN >= 3 && bestN*5 >= n*3 {
			return d.label + " " + best
		}
	}
	return ""
}

func failureClusterFactor(items []*db.Launch) string {
	if len(items) < 3 {
		return ""
	}
	clusterItems := make([]*db.Launch, 0, len(items))
	for _, f := range items {
		if db.IsTransientInstanceTermination(f.TerminationReason) {
			continue
		}
		clusterItems = append(clusterItems, f)
	}
	if len(clusterItems) < 3 {
		return ""
	}
	return dominantFailureFactor(clusterItems)
}

// recentFailedInstanceSummary is the classified view of recent abnormal
// instance terminations, shared by the non-interactive inline section
// (appendRecentFailedInstanceRows) and the interactive footer element
// (buildInstanceHealthFooter). present is false when nothing abnormal remains
// after filtering normal terminations.
type recentFailedInstanceSummary struct {
	present       bool
	byOutcome     map[failureOutcome][]*db.Launch
	outcomeOf     map[int64]failureOutcome
	total         int
	wastedCents   int
	clusterFactor string
	headerSpan    string
	newestAge     string
	summaryParts  []string
}

func summarizeRecentFailedInstances(failures *recentFailedInstances, now time.Time) recentFailedInstanceSummary {
	if failures == nil {
		return recentFailedInstanceSummary{}
	}
	renderable := make([]*db.Launch, 0, len(failures.items))
	for _, f := range failures.items {
		if f == nil || db.IsNormalInstanceTermination(f.TerminationReason) {
			continue
		}
		renderable = append(renderable, f)
	}
	total := len(renderable)
	if total == 0 {
		return recentFailedInstanceSummary{}
	}

	byOutcome := make(map[failureOutcome][]*db.Launch)
	outcomeOf := make(map[int64]failureOutcome, total)
	wastedCents := 0
	var newest int64
	for _, f := range renderable {
		o := classifyFailureOutcome(f, failures)
		byOutcome[o] = append(byOutcome[o], f)
		outcomeOf[f.ID] = o
		if f.ActualSpendCents > 0 {
			wastedCents += f.ActualSpendCents
		}
		if e := launchEndedAt(f); e > newest {
			newest = e
		}
	}
	for o := range byOutcome {
		items := byOutcome[o]
		sort.SliceStable(items, func(i, j int) bool {
			return launchEndedAt(items[i]) > launchEndedAt(items[j])
		})
	}

	summaryParts := make([]string, 0, len(failureOutcomeOrder))
	for _, o := range failureOutcomeOrder {
		if n := len(byOutcome[o]); n > 0 {
			summaryParts = append(summaryParts, failureCountPhrase(o, n))
		}
	}

	newestAge := ""
	if newest > 0 {
		newestAge = shortRelativeTime(now.Unix() - newest)
	}

	return recentFailedInstanceSummary{
		present:       true,
		byOutcome:     byOutcome,
		outcomeOf:     outcomeOf,
		total:         total,
		wastedCents:   wastedCents,
		clusterFactor: failureClusterFactor(renderable),
		headerSpan:    formatFailureSpan(renderable, now),
		newestAge:     newestAge,
		summaryParts:  summaryParts,
	}
}

// instanceHealthFooterView is the interactive-TUI footer rendering of recent
// abnormal instance terminations: a single, severity-gated line in the footer's
// System zone (empty when the fleet is quiet) that replaces the inline list
// section. The full grouped breakdown lives in the `f` diagnose overlay.
type instanceHealthFooterView struct {
	lines []string
}

// instanceHealthHeadline summarizes the attention buckets (failed / awaiting
// placement / dud) for the collapsed token. When every abnormal termination
// recovered, it reports the calmer "N instances recovered" with attention=false
// so the caller can render it dim rather than as a warning.
func instanceHealthHeadline(s recentFailedInstanceSummary) (text string, attention bool) {
	parts := make([]string, 0, 3)
	for _, o := range []failureOutcome{failureSeriesFailed, failureAwaitingPlacement, failureDud} {
		if n := len(s.byOutcome[o]); n > 0 {
			parts = append(parts, failureCountPhrase(o, n))
		}
	}
	if len(parts) == 0 {
		return pluralize(s.total, "instance recovered", "instances recovered"), false
	}
	return "Instances: " + strings.Join(parts, " · "), true
}

func buildInstanceHealthFooter(failures *recentFailedInstances, width int, now time.Time) instanceHealthFooterView {
	s := summarizeRecentFailedInstances(failures, now)
	if !s.present {
		return instanceHealthFooterView{}
	}

	parts := make([]string, 0, 3)
	style := tuiWarnStyle
	if s.clusterFactor != "" {
		// A run sharing a machine/provider/GPU is a systemic fault, not bad luck.
		style = tuiFailedStyle
		parts = append(parts, fmt.Sprintf("⚠ Clustered instance failures — %s (%d)", s.clusterFactor, s.total))
	} else if headline, attention := instanceHealthHeadline(s); attention {
		parts = append(parts, "⚠ "+headline)
	} else {
		style = tuiDimStyle
		parts = append(parts, headline)
	}
	if s.newestAge != "" {
		parts = append(parts, "latest "+s.newestAge)
	}
	if s.wastedCents > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f wasted", float64(s.wastedCents)/100))
	}

	// The action hint is parenthesized so it reads as an aside, not another data
	// field in the · -separated row. The key is sourced from the binding so it
	// tracks any rebind; "diagnose" is a friendlier label than the binding action.
	hint := "  (" + listKeyInstanceFailures.keys + ":diagnose)"
	line := strings.Join(parts, " · ") + hint
	return instanceHealthFooterView{lines: []string{style.Render(truncateDisplayWidth(line, width))}}
}

// appendRecentFailedInstanceRows renders the full inline "Recent failed
// instances" section for non-interactive/plain output (exit summaries and
// `weft jobs list --group-by status`). The live TUI renders this data as a
// severity-gated footer element instead (buildInstanceHealthFooter), so this
// path is never used interactively.
func appendRecentFailedInstanceRows(
	rows []groupedStatusRow,
	failures *recentFailedInstances,
	projectWidth, width int,
	now time.Time,
) []groupedStatusRow {
	s := summarizeRecentFailedInstances(failures, now)
	if !s.present {
		return rows
	}

	headerText := "Recent failed instances"
	if s.headerSpan != "" {
		headerText += " — " + s.headerSpan
	}
	headerText += fmt.Sprintf(" (%d):", s.total)
	rows = append(rows, groupedStatusRow{
		text:     headerText,
		isHeader: true,
		section:  failedInstancesSectionKey,
	})

	// Summary line: full breakdown, overall span, wasted spend.
	summary := "  " + strings.Join(s.summaryParts, " · ")
	if s.headerSpan != "" {
		summary += " — " + s.headerSpan
	}
	if s.wastedCents > 0 {
		summary += fmt.Sprintf(" · $%.2f wasted", float64(s.wastedCents)/100)
	}
	rows = append(rows, groupedStatusRow{
		text:      summary,
		isBlocked: true,
		section:   failedInstancesSectionKey,
	})

	// Cluster line: a run of failures sharing a machine/provider/GPU is
	// almost never independent bad luck. Exclude transient provider-side
	// terminations (e.g. CLI timeouts) so a slow-API hour doesn't masquerade
	// as a systemic provider failure — the rows still appear above for
	// cost/postmortem accounting, they just don't contribute to clustering.
	if s.clusterFactor != "" {
		rows = append(rows, groupedStatusRow{
			text:      "  ⚠ clustered failures — common factor: " + s.clusterFactor,
			isBlocked: true,
			section:   failedInstancesSectionKey,
		})
	}

	appendGroup := func(rows []groupedStatusRow, o failureOutcome) []groupedStatusRow {
		items := s.byOutcome[o]
		if len(items) == 0 {
			return rows
		}
		header := fmt.Sprintf("  %s (%d)", failureGroupLabel(o), len(items))
		if span := formatFailureSpan(items, now); span != "" {
			header += " — " + span
		}
		rows = append(rows, groupedStatusRow{
			text:      header + ":",
			isBlocked: true,
			section:   failedInstancesSectionKey,
		})
		for _, item := range items {
			rows = append(rows, groupedStatusRow{
				text:    formatLaunchFailureRow(item, failures, s.outcomeOf[item.ID], projectWidth, width, now),
				launch:  item,
				section: failedInstancesSectionKey,
			})
		}
		return rows
	}

	// Attention buckets first, then the FYI buckets (succeeded, dud).
	for _, o := range failureOutcomeOrder {
		rows = appendGroup(rows, o)
	}

	rows = append(rows, groupedStatusRow{text: ""})
	return rows
}

// formatLaunchFailureRow renders one failed-instance row. The failure reason is
// the triage-critical content, so it gets priority over the width budget; the
// short age suffix is dropped first when space is tight.
func formatLaunchFailureRow(
	f *db.Launch,
	failures *recentFailedInstances,
	outcome failureOutcome,
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

	detail := strings.TrimSpace(f.TerminationDetail)
	if detail == "" {
		detail = strings.TrimSpace(db.HumanizeTerminationReason(f.TerminationReason))
	}
	if detail == "" {
		detail = "unknown failure"
	}
	if outcome == failureReplacedRunning {
		detail += ", relaunching"
	}

	suffix := ""
	if f.EndedAt != nil && *f.EndedAt > 0 {
		suffix = " — " + shortRelativeTime(now.Unix()-*f.EndedAt)
	}

	prefix := fmt.Sprintf("    - %s %s  %s  ", rentalGlyphCloud, idStyled, projectCol)
	line := prefix + detail + suffix

	if width > 0 {
		prefixWidth := lipgloss.Width(prefix)
		detailWidth := width - prefixWidth - lipgloss.Width(suffix)
		if detailWidth < 24 {
			// Drop the age suffix so the failure reason stays readable.
			suffix = ""
			detailWidth = width - prefixWidth
		}
		if detailWidth < 0 {
			detailWidth = 0
		}
		line = prefix + truncateDisplayWidth(detail, detailWidth) + suffix
	}

	switch outcome {
	case failureReplacedRunning, failureSeriesSucceeded, failureDud:
		line = failureDimStyle.Render(line)
	}
	return line
}
