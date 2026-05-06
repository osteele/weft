package narrate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/status"
	"github.com/osteele/weft/internal/ui/dashboard"
)

// StatusLine is the deterministic per-entry header. It carries counts that
// operators expect to see every wake — running/queued/instances/run rate,
// project mix, and the unprocessed-results inbox — and is computed
// directly from the snapshot, with no model call or token spend.
type StatusLine struct {
	Now                  time.Time
	RunningJobs          int
	QueuedJobs           int
	StartingJobs         int
	PendingPlacement     int
	ActiveInstances      int
	GraceInstances       int
	LaunchingInst        int
	RunRateUSDPerHour    float64
	BudgetUSDPerHour     float64  // 0 = no configured target
	Projects             []string // sorted, deduped names of projects with active jobs
	AutopilotState       string
	UnprocessedCompleted int // unprocessed jobs in terminal "completed" state
	UnprocessedFailed    int // unprocessed jobs in failed/dead/killed/canceled
}

// UnprocessedCounts collects unprocessed terminal-job counts for the
// status line. Caller may compute these from any source; narrate.go
// queries the DB.
type UnprocessedCounts struct {
	Completed int
	Failed    int
}

// BuildStatusLine summarizes a snapshot. budgetCentsPerHour is the
// configured autopilot run-rate target (0 if unset). unprocessed is the
// inbox of terminal jobs the user hasn't acknowledged yet.
func BuildStatusLine(s *Snapshot, budgetCentsPerHour int, unprocessed UnprocessedCounts) StatusLine {
	sl := StatusLine{
		Now:                  s.Time,
		AutopilotState:       s.Autopilot.State,
		UnprocessedCompleted: unprocessed.Completed,
		UnprocessedFailed:    unprocessed.Failed,
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
	return sl
}

// HeaderLines renders the multi-line header. The first line is prefixed
// by the timestamp; subsequent lines are hanging-indented under it. width
// is the available terminal column width (used to abbreviate project names
// when they overflow). 0 disables width-based truncation.
func (sl StatusLine) HeaderLines(width int) []string {
	tsPrefix := sl.Now.Local().Format("15:04:05") + " | "
	indent := strings.Repeat(" ", len(tsPrefix))

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

	// Second row: jobs counters + projects
	var jobParts []string
	if sl.RunningJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d running", sl.RunningJobs))
	}
	if sl.StartingJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d starting", sl.StartingJobs))
	}
	if sl.QueuedJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d queued", sl.QueuedJobs))
	}
	if sl.PendingPlacement > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d pending-placement", sl.PendingPlacement))
	}
	if sl.UnprocessedCompleted > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d unprocessed completed", sl.UnprocessedCompleted))
	}
	if sl.UnprocessedFailed > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d unprocessed failed", sl.UnprocessedFailed))
	}
	if len(jobParts) == 0 && len(sl.Projects) == 0 {
		return []string{top}
	}
	row2 := strings.Join(jobParts, " | ")
	if len(sl.Projects) > 0 {
		// Width budget for the project list = available width minus indent
		// minus what jobParts already occupies plus the " | " separator.
		used := len(indent) + len(row2)
		if row2 != "" {
			used += len(" | ")
		}
		budget := width - used
		if width <= 0 {
			budget = 0 // disabled
		}
		if row2 != "" {
			row2 += " | "
		}
		row2 += renderProjects(sl.Projects, budget)
	}
	return []string{top, indent + row2}
}

// renderProjects fits as many project names as possible into budget,
// abbreviating individual names via dashboard.AbbreviateProject if the
// full set doesn't fit. budget <= 0 disables truncation (return all
// full names).
func renderProjects(projects []string, budget int) string {
	if budget <= 0 {
		return strings.Join(projects, " ")
	}
	full := strings.Join(projects, " ")
	if len(full) <= budget {
		return full
	}
	// Try increasingly aggressive abbreviation.
	for cap := 18; cap >= 4; cap -= 2 {
		var parts []string
		for _, p := range projects {
			parts = append(parts, dashboard.AbbreviateProject(p, cap))
		}
		joined := strings.Join(parts, " ")
		if len(joined) <= budget {
			return joined
		}
	}
	// Last resort: drop trailing names with an ellipsis marker.
	out := []string{}
	used := 0
	for _, p := range projects {
		ab := dashboard.AbbreviateProject(p, 4)
		add := len(ab)
		if len(out) > 0 {
			add++
		}
		if used+add+2 > budget { // +2 for trailing " …"
			break
		}
		out = append(out, ab)
		used += add
	}
	return strings.Join(out, " ") + " …"
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
		sl.UnprocessedFailed != other.UnprocessedFailed {
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
