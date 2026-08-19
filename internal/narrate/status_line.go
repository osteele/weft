package narrate

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/status"
)

// StatusLine is the deterministic per-entry header. It carries counts that
// operators expect to see every wake — running/queued/instances/run rate,
// project mix, and the unprocessed-results inbox — and is computed
// directly from the snapshot, with no model call or token spend.
type StatusLine struct {
	Now                  time.Time `json:"now"`
	RunningJobs          int       `json:"running_jobs"`
	QueuedJobs           int       `json:"queued_jobs"`
	StartingJobs         int       `json:"starting_jobs"`
	PendingPlacement     int       `json:"pending_placement"`
	ActiveInstances      int       `json:"active_instances"`
	GraceInstances       int       `json:"grace_instances"`
	LaunchingInst        int       `json:"launching_instances"`
	RunRateUSDPerHour    float64   `json:"run_rate_usd_per_hour"`
	BudgetUSDPerHour     float64   `json:"budget_usd_per_hour"` // 0 = no configured target
	Projects             []string  `json:"projects"`            // sorted, deduped names of projects with active jobs
	AutopilotState       string    `json:"autopilot_state"`
	UnprocessedCompleted int       `json:"unprocessed_completed"` // unprocessed jobs in terminal "completed" state
	UnprocessedFailed    int       `json:"unprocessed_failed"`    // unprocessed jobs in failed/dead/killed/canceled
	CompletedProjects    []string  `json:"completed_projects,omitempty"`
	FailedProjects       []string  `json:"failed_projects,omitempty"`
}

// UnprocessedCounts collects unprocessed terminal-job counts for the
// status line. Caller may compute these from any source; narrate.go
// queries the DB.
type UnprocessedCounts struct {
	Completed         int      `json:"completed"`
	Failed            int      `json:"failed"`
	CompletedProjects []string `json:"completed_projects,omitempty"`
	FailedProjects    []string `json:"failed_projects,omitempty"`
}

// LoadUnprocessedCounts queries terminal jobs in the unprocessed inbox and
// splits the count into successes versus failures. Optionally scoped to a
// project. Bounded to the last 14 days so old terminal jobs do not dominate
// status surfaces.
func LoadUnprocessedCounts(database *sql.DB, project string) (UnprocessedCounts, error) {
	jobs, err := loadUnprocessedJobs(database, project)
	if err != nil {
		return UnprocessedCounts{}, err
	}
	return unprocessedCountsFromJobs(jobs), nil
}

// LoadUnprocessedJobViews returns the bounded unprocessed terminal-job inbox as
// narrate job views, using the same project scope and age window as
// LoadUnprocessedCounts.
func LoadUnprocessedJobViews(database *sql.DB, project string) ([]JobView, error) {
	jobs, err := loadUnprocessedJobs(database, project)
	if err != nil {
		return nil, err
	}
	return unprocessedJobViews(database, jobs), nil
}

// LoadUnprocessedCountsAndViews runs the unprocessed-inbox query once and
// derives both the status-line counts and the job views from the same job
// list, for callers that need both. The inbox query is the expensive part
// (a 14-day terminal-job scan), so callers needing counts and views must use
// this rather than calling LoadUnprocessedCounts and LoadUnprocessedJobViews
// separately.
func LoadUnprocessedCountsAndViews(database *sql.DB, project string) (UnprocessedCounts, []JobView, error) {
	jobs, err := loadUnprocessedJobs(database, project)
	if err != nil {
		return UnprocessedCounts{}, nil, err
	}
	return unprocessedCountsFromJobs(jobs), unprocessedJobViews(database, jobs), nil
}

func unprocessedCountsFromJobs(jobs []*db.Job) UnprocessedCounts {
	var counts UnprocessedCounts
	completedProjects := map[string]struct{}{}
	failedProjects := map[string]struct{}{}
	for _, j := range jobs {
		if !isUnprocessedTerminalJob(j) {
			continue
		}
		if isFailedJob(j) {
			counts.Failed++
			if p := strings.TrimSpace(j.Project); p != "" {
				failedProjects[p] = struct{}{}
			}
			continue
		}
		if j.EffectiveStatus() == status.Completed {
			counts.Completed++
			if p := strings.TrimSpace(j.Project); p != "" {
				completedProjects[p] = struct{}{}
			}
		}
	}
	counts.CompletedProjects = sortedStringKeys(completedProjects)
	counts.FailedProjects = sortedStringKeys(failedProjects)
	return counts
}

func unprocessedJobViews(database *sql.DB, jobs []*db.Job) []JobView {
	views := make([]JobView, 0, len(jobs))
	for _, job := range jobs {
		if !isUnprocessedTerminalJob(job) {
			continue
		}
		views = append(views, jobToView(database, job, nil, jobview.PlacementStatus{}))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views
}

func loadUnprocessedJobs(database *sql.DB, project string) ([]*db.Job, error) {
	const maxAgeDays = 14
	jobs, err := db.ListJobsWithMaxAge(database, "", "", 0, maxAgeDays, nil, "unprocessed")
	if err != nil {
		return nil, err
	}
	if project != "" {
		jobs = db.FilterJobsByProject(jobs, project)
	}
	return jobs, nil
}

func isFailedJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead:
		return true
	case db.StatusCompleted:
		return job.ExitCode != nil && *job.ExitCode != 0
	default:
		return false
	}
}

func isUnprocessedTerminalJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	return isFailedJob(job) || job.EffectiveStatus() == status.Completed
}

func sortedStringKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

var (
	statusCompletedStyle = "\x1b[1;32m"
	statusFailedStyle    = "\x1b[1;31m"
	statusResetStyle     = "\x1b[0m"
)

// BuildStatusLine summarizes a snapshot. budgetCentsPerHour is the
// configured autopilot run-rate target (0 if unset). unprocessed is the
// inbox of terminal jobs the user hasn't acknowledged yet.
func BuildStatusLine(s *Snapshot, budgetCentsPerHour int, unprocessed UnprocessedCounts) StatusLine {
	sl := StatusLine{
		Now:                  s.Time,
		AutopilotState:       s.Autopilot.State,
		UnprocessedCompleted: unprocessed.Completed,
		UnprocessedFailed:    unprocessed.Failed,
		CompletedProjects:    append([]string(nil), unprocessed.CompletedProjects...),
		FailedProjects:       append([]string(nil), unprocessed.FailedProjects...),
	}
	if budgetCentsPerHour > 0 {
		sl.BudgetUSDPerHour = float64(budgetCentsPerHour) / 100.0
	}
	projectSet := map[string]struct{}{}
	for _, j := range s.Jobs {
		switch j.Status {
		case status.Running:
			sl.RunningJobs++
		case status.Queued:
			sl.QueuedJobs++
		case status.Starting:
			sl.StartingJobs++
		case status.PendingPlacement:
			sl.PendingPlacement++
		}
		if p := strings.TrimSpace(j.Project); p != "" {
			projectSet[p] = struct{}{}
		}
	}
	for _, inst := range s.Instances {
		switch inst.Status {
		case db.LaunchStatusRunning:
			sl.ActiveInstances++
			sl.RunRateUSDPerHour += float64(inst.CostPerHourCents) / 100.0
		case db.LaunchStatusGrace:
			sl.GraceInstances++
		case db.LaunchStatusLaunching:
			sl.LaunchingInst++
		}
	}
	for p := range projectSet {
		sl.Projects = append(sl.Projects, p)
	}
	sort.Strings(sl.Projects)
	sort.Strings(sl.CompletedProjects)
	sort.Strings(sl.FailedProjects)
	return sl
}

// HeaderLines renders the multi-line header. The first line is prefixed
// by the timestamp; subsequent lines are hanging-indented under it. width
// is the available terminal column width (used to abbreviate project names
// when they overflow). 0 disables width-based truncation.
func (sl StatusLine) HeaderLines(width int) []string {
	tsPrefix := sl.Now.Local().Format("15:04:05") + " | "
	indent := strings.Repeat(" ", len(tsPrefix))
	bodyWidth := 0
	if width > 0 {
		bodyWidth = width - runewidth.StringWidth(indent)
		if bodyWidth < 1 {
			bodyWidth = 1
		}
	}

	// Top row: instances + run rate (+ budget) | autopilot
	var topParts []string
	switch {
	case sl.ActiveInstances > 0 && sl.BudgetUSDPerHour > 0:
		topParts = append(topParts, fmt.Sprintf("%d instances running ($%.2f/hr out of $%.2f/hr budget)",
			sl.ActiveInstances, sl.RunRateUSDPerHour, sl.BudgetUSDPerHour))
	case sl.ActiveInstances > 0:
		topParts = append(topParts, fmt.Sprintf("%d instances running ($%.2f/hr)",
			sl.ActiveInstances, sl.RunRateUSDPerHour))
	case sl.LaunchingInst > 0:
		topParts = append(topParts, fmt.Sprintf("%d instances launching", sl.LaunchingInst))
	default:
		topParts = append(topParts, "no rentals")
	}
	if sl.LaunchingInst > 0 && sl.ActiveInstances > 0 {
		topParts = append(topParts, fmt.Sprintf("+%d launching", sl.LaunchingInst))
	}
	if sl.GraceInstances > 0 {
		topParts = append(topParts, fmt.Sprintf("%d in grace", sl.GraceInstances))
	}
	apState := sl.AutopilotState
	if apState == "" {
		apState = "never"
	}
	topParts = append(topParts, "autopilot:"+apState)
	top := tsPrefix + strings.Join(topParts, " | ")
	lines := []string{fitDisplayWidth(top, width)}

	// Second row: jobs counters + projects
	jobParts, noAttributionJobParts := sl.jobSegments()
	row2 := strings.Join(jobParts, " | ")
	if width > 0 && displayWidth(indent)+displayWidth(row2) > width {
		row2 = strings.Join(noAttributionJobParts, " | ")
	}
	if len(jobParts) == 0 && len(sl.Projects) == 0 {
		return lines
	}
	projectLines := []string{}
	if len(sl.Projects) > 0 {
		inlineBudget := 0
		if width > 0 {
			inlineBudget = bodyWidth - displayWidth(row2)
			if row2 != "" {
				inlineBudget -= len(" | ")
			}
		}
		if inline, ok := renderProjectsInline(sl.Projects, inlineBudget, sl.projectStyles()); ok {
			if row2 != "" {
				row2 += " | "
			}
			row2 += inline
		} else {
			row2 = strings.Join(noAttributionJobParts, " | ")
			projectLines = renderProjectLines(sl.Projects, bodyWidth, sl.projectStyles())
		}
	}
	if row2 != "" {
		lines = append(lines, indent+fitDisplayWidth(row2, bodyWidth))
	}
	for _, line := range projectLines {
		lines = append(lines, indent+fitDisplayWidth(line, bodyWidth))
	}
	return lines
}

const minInlineProjectWidth = 16

func renderProjectsInline(projects []string, budget int, styles map[string]string) (string, bool) {
	if budget <= 0 {
		return "", false
	}
	projects = compactSortedProjects(projects)
	if len(projects) == 0 {
		return "", false
	}
	full := renderProjectNames(projects, styles)
	if displayWidth(full) <= budget {
		return full, true
	}
	for cap := 20; cap >= minInlineProjectWidth; cap -= 2 {
		parts := make([]string, 0, len(projects))
		for _, project := range projects {
			parts = append(parts, styleProjectName(project, abbreviateProject(project, cap), styles))
		}
		joined := strings.Join(parts, " ")
		if displayWidth(joined) <= budget {
			return joined, true
		}
	}
	return "", false
}

func renderProjectLines(projects []string, width int, styles map[string]string) []string {
	projects = compactSortedProjects(projects)
	if len(projects) == 0 {
		return nil
	}
	if width <= 0 {
		return []string{renderProjectNames(projects, styles)}
	}
	lines := make([]string, 0, len(projects))
	current := ""
	currentWidth := 0
	for _, project := range projects {
		label := project
		if displayWidth(label) > width {
			label = fitDisplayWidth(abbreviateProject(label, width), width)
		}
		part := styleProjectName(project, label, styles)
		partWidth := displayWidth(part)
		if current == "" {
			current = part
			currentWidth = partWidth
			continue
		}
		if currentWidth+1+partWidth <= width {
			current += " " + part
			currentWidth += 1 + partWidth
			continue
		}
		lines = append(lines, current)
		current = part
		currentWidth = partWidth
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func fitDisplayWidth(s string, width int) string {
	if width <= 0 || displayWidth(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	s = ansi.Strip(s)
	var b strings.Builder
	currentWidth := 0
	for _, r := range s {
		runeWidth := runewidth.RuneWidth(r)
		if currentWidth+runeWidth+1 > width {
			break
		}
		b.WriteRune(r)
		currentWidth += runeWidth
	}
	b.WriteString("…")
	return b.String()
}

func displayWidth(s string) int {
	return runewidth.StringWidth(ansi.Strip(s))
}

func (sl StatusLine) jobSegments() ([]string, []string) {
	var withProjects []string
	var withoutProjects []string
	hasJobNoun := false
	if sl.RunningJobs > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.RunningJobs, "running", nil)
		withoutProjects = append(withoutProjects, withProjects[len(withProjects)-1])
	}
	if sl.StartingJobs > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.StartingJobs, "starting", nil)
		withoutProjects = append(withoutProjects, withProjects[len(withProjects)-1])
	}
	if sl.QueuedJobs > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.QueuedJobs, "queued", nil)
		withoutProjects = append(withoutProjects, withProjects[len(withProjects)-1])
	}
	if sl.PendingPlacement > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.PendingPlacement, "pending-placement", nil)
		withoutProjects = append(withoutProjects, withProjects[len(withProjects)-1])
	}
	if sl.UnprocessedCompleted > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.UnprocessedCompleted, "completed", sl.CompletedProjects)
		withoutProjects, _ = appendJobStateSegment(withoutProjects, len(withoutProjects) > 0, sl.UnprocessedCompleted, "completed", nil)
	}
	if sl.UnprocessedFailed > 0 {
		withProjects, hasJobNoun = appendJobStateSegment(withProjects, hasJobNoun, sl.UnprocessedFailed, "failed", sl.FailedProjects)
		withoutProjects, _ = appendJobStateSegment(withoutProjects, len(withoutProjects) > 0, sl.UnprocessedFailed, "failed", nil)
	}
	return withProjects, withoutProjects
}

func appendJobStateSegment(parts []string, hasJobNoun bool, count int, state string, projects []string) ([]string, bool) {
	if count <= 0 {
		return parts, hasJobNoun
	}
	suffix := formatProjectSuffix(projects)
	var segment string
	if !hasJobNoun {
		segment = fmt.Sprintf("%s %s%s", pluralizeJobs(count), state, suffix)
	} else {
		segment = fmt.Sprintf("%d %s%s", count, state, suffix)
	}
	return append(parts, styleJobStateSegment(state, segment)), true
}

func formatProjectSuffix(projects []string) string {
	projects = compactSortedProjects(projects)
	if len(projects) == 0 {
		return ""
	}
	return " (" + strings.Join(projects, ", ") + ")"
}

func compactSortedProjects(projects []string) []string {
	set := map[string]struct{}{}
	for _, project := range projects {
		project = strings.TrimSpace(project)
		if project != "" {
			set[project] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for project := range set {
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

func styleJobStateSegment(state, segment string) string {
	switch state {
	case "completed":
		return renderStatusStyle(statusCompletedStyle, segment)
	case "failed":
		return renderStatusStyle(statusFailedStyle, segment)
	default:
		return segment
	}
}

func (sl StatusLine) projectStyles() map[string]string {
	styles := make(map[string]string)
	for _, project := range compactSortedProjects(sl.FailedProjects) {
		styles[project] = statusFailedStyle
	}
	for _, project := range compactSortedProjects(sl.CompletedProjects) {
		styles[project] = statusCompletedStyle
	}
	return styles
}

func renderProjectNames(projects []string, styles map[string]string) string {
	parts := make([]string, 0, len(projects))
	for _, project := range projects {
		parts = append(parts, styleProjectName(project, project, styles))
	}
	return strings.Join(parts, " ")
}

func styleProjectName(project, label string, styles map[string]string) string {
	style, ok := styles[project]
	if !ok {
		return label
	}
	return renderStatusStyle(style, label)
}

func renderStatusStyle(style, s string) string {
	if style == "" || s == "" {
		return s
	}
	return style + s + statusResetStyle
}

func pluralizeJobs(count int) string {
	if count == 1 {
		return "1 job"
	}
	return fmt.Sprintf("%d jobs", count)
}

// Equal reports whether two status lines are observationally identical.
// Run rate is compared with cent precision to avoid floating-point churn.
func (sl StatusLine) Equal(other StatusLine) bool {
	if sl.RunningJobs != other.RunningJobs ||
		sl.QueuedJobs != other.QueuedJobs ||
		sl.StartingJobs != other.StartingJobs ||
		sl.PendingPlacement != other.PendingPlacement ||
		sl.ActiveInstances != other.ActiveInstances ||
		sl.GraceInstances != other.GraceInstances ||
		sl.LaunchingInst != other.LaunchingInst ||
		sl.AutopilotState != other.AutopilotState ||
		sl.UnprocessedCompleted != other.UnprocessedCompleted ||
		sl.UnprocessedFailed != other.UnprocessedFailed ||
		!equalStringSlices(sl.CompletedProjects, other.CompletedProjects) ||
		!equalStringSlices(sl.FailedProjects, other.FailedProjects) {
		return false
	}
	if int(sl.RunRateUSDPerHour*100) != int(other.RunRateUSDPerHour*100) {
		return false
	}
	if int(sl.BudgetUSDPerHour*100) != int(other.BudgetUSDPerHour*100) {
		return false
	}
	if len(sl.Projects) != len(other.Projects) {
		return false
	}
	for i, p := range sl.Projects {
		if other.Projects[i] != p {
			return false
		}
	}
	return true
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abbreviateProject(name string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if len(name) <= maxWidth {
		return name
	}
	parts := strings.Split(name, "-")
	if len(parts) == 1 {
		return truncateWithEllipsis(name, maxWidth)
	}
	segments := append([]string(nil), parts...)
	for joinedProjectLen(segments) > maxWidth {
		maxIdx := longestProjectSegmentIndex(segments)
		if len(segments[maxIdx]) <= 1 {
			break
		}
		next := abbreviateProjectSegment(parts[maxIdx], len(segments[maxIdx])-1)
		if next == "" || next == segments[maxIdx] {
			next = segments[maxIdx][:len(segments[maxIdx])-1]
		}
		segments[maxIdx] = next
	}
	candidate := strings.Join(segments, "-")
	if len(candidate) <= maxWidth {
		return candidate
	}
	initials := initialString(parts)
	if len(initials) <= maxWidth {
		return initials
	}
	return truncateWithEllipsis(initials, maxWidth)
}

var projectSegmentAbbreviations = map[string][]string{
	"attention":   {"attent", "attn"},
	"encoder":     {"enc"},
	"injection":   {"inject", "inj"},
	"performance": {"perf", "per"},
	"probes":      {"prob"},
	"structural":  {"struct"},
	"structure":   {"struct"},
	"structures":  {"struct"},
}

func abbreviateProjectSegment(segment string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(segment) <= maxLen {
		return segment
	}
	lower := strings.ToLower(segment)
	if candidates, ok := projectSegmentAbbreviations[lower]; ok {
		for _, candidate := range candidates {
			if len(candidate) <= maxLen {
				return candidate
			}
		}
	}
	if strings.HasSuffix(lower, "tion") && maxLen >= 5 {
		stem := trimTrailingVowel(segment[:len(segment)-len("ion")])
		if len(stem) <= maxLen {
			return stem
		}
	}
	return trimTrailingVowel(segment[:maxLen])
}

func trimTrailingVowel(s string) string {
	if len(s) <= 3 {
		return s
	}
	switch s[len(s)-1] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return s[:len(s)-1]
	default:
		return s
	}
}

func longestProjectSegmentIndex(segments []string) int {
	maxIdx := 0
	for i := 1; i < len(segments); i++ {
		if len(segments[i]) >= len(segments[maxIdx]) {
			maxIdx = i
		}
	}
	return maxIdx
}

func joinedProjectLen(segments []string) int {
	if len(segments) == 0 {
		return 0
	}
	total := len(segments) - 1
	for _, segment := range segments {
		total += len(segment)
	}
	return total
}

func initialString(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteByte(p[0])
		}
	}
	return b.String()
}

func truncateWithEllipsis(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if len(s) <= maxWidth {
		return s
	}
	if maxWidth == 1 {
		return "…"
	}
	return s[:maxWidth-1] + "…"
}
