package terminal

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
)

// FormatMoveExitSummary prints the receipt shown after the move launch TUI exits.
func FormatMoveExitSummary(database *sql.DB, result orchestration.BulkResult) string {
	return FormatMoveExitSummaryAt(database, result, time.Now(), listOutputWidth())
}

func FormatMoveExitSummaryAt(database *sql.DB, result orchestration.BulkResult, now time.Time, width int) string {
	if len(result.PlacedJobIDs) == 0 && len(result.UnplacedJobs) == 0 && len(result.InstanceIDs) == 0 {
		return ""
	}
	if width <= 0 {
		width = 120
	}

	var b strings.Builder
	writeSummaryLine(&b, width, fmt.Sprintf("weft move ended - %s", now.Format("Mon Jan 2 15:04")))
	writeMoveResultHeader(&b, width, result)
	if len(result.InstanceIDs) > 0 {
		writeSummaryLine(&b, width, "Instances:")
		for _, line := range moveExitInstanceLines(database, result.InstanceIDs) {
			writeSummaryLine(&b, width, line)
		}
	}
	return b.String()
}

// writeMoveResultHeader renders the success / shortfall summary lines.
//
// Full success: a single "Moved: w… -> N new instances" line, matching the
// pre-shortfall output.
//
// Shortfall: a "Placed M of N distinct instances" header followed by indented
// "Moved:" (placed jobs) and "Unplaced:" (failed jobs, grouped by reason)
// lines so the user can see at a glance which requested instances didn't
// land and why.
func writeMoveResultHeader(b *strings.Builder, width int, result orchestration.BulkResult) {
	placedCount := len(result.PlacedJobIDs)
	unplacedCount := len(result.UnplacedJobs)
	if placedCount == 0 && unplacedCount == 0 {
		return
	}

	if unplacedCount == 0 {
		// Full success — keep today's output verbatim so the common case
		// stays familiar.
		writeSummaryLine(b, width, fmt.Sprintf("Moved: %s -> %s",
			formatJobIDList(result.PlacedJobIDs),
			pluralize(len(result.InstanceIDs), "new instance", "new instances")))
		return
	}

	// Partial: lead with the shape mismatch so the user notices immediately.
	requested := result.RequestedCount
	if requested == 0 {
		requested = placedCount + unplacedCount
	}
	header := fmt.Sprintf("Placed %d of %d %s", placedCount, requested,
		distinctInstancesNoun(result.RequestedEach, requested))
	writeSummaryLine(b, width, header)
	if placedCount > 0 {
		writeSummaryLine(b, width, fmt.Sprintf("  Moved:    %s", formatJobIDList(result.PlacedJobIDs)))
	}
	for _, line := range formatUnplacedLines(result.UnplacedJobs) {
		writeSummaryLine(b, width, "  "+line)
	}
}

func formatJobIDList(jobIDs []int64) string {
	if len(jobIDs) == 0 {
		return ""
	}
	sorted := append([]int64(nil), jobIDs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = ids.FormatJobID(id)
	}
	return strings.Join(parts, ", ")
}

// formatUnplacedLines groups unplaced jobs by reason so a swarm of "no
// offer accepted" failures collapses into a single line. Returns one
// "Unplaced: …" line per distinct reason, preserving the order the reasons
// were first encountered (which roughly tracks the per-group event order).
func formatUnplacedLines(unplaced []orchestration.UnplacedJob) []string {
	if len(unplaced) == 0 {
		return nil
	}
	type group struct {
		reason string
		jobIDs []int64
	}
	order := []string{}
	groups := map[string]*group{}
	for _, u := range unplaced {
		reason := strings.TrimSpace(u.Reason)
		if reason == "" {
			reason = "no offer accepted"
		}
		if existing, ok := groups[reason]; ok {
			existing.jobIDs = append(existing.jobIDs, u.JobID)
			continue
		}
		groups[reason] = &group{reason: reason, jobIDs: []int64{u.JobID}}
		order = append(order, reason)
	}
	lines := make([]string, 0, len(order))
	for _, reason := range order {
		g := groups[reason]
		lines = append(lines, fmt.Sprintf("Unplaced: %s — %s",
			formatJobIDList(g.jobIDs), truncateReason(reason)))
	}
	return lines
}

func truncateReason(reason string) string {
	const limit = 120
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit-1] + "…"
}

// distinctInstancesNoun chooses between "distinct instances" (when the
// caller asked for --to distinct / --each) and a generic "new instances"
// label. The "distinct" framing names the user's explicit request so the
// shortfall message reads as "you asked for distinct, you got fewer".
func distinctInstancesNoun(requestedEach bool, count int) string {
	if requestedEach {
		if count == 1 {
			return "distinct instance"
		}
		return "distinct instances"
	}
	if count == 1 {
		return "new instance"
	}
	return "new instances"
}

type moveExitInstanceRow struct {
	instance string
	gpu      string
	cpu      string
	disk     string
	cost     string
	status   string
	job      string
}

func moveExitInstanceLines(database *sql.DB, instanceIDs []int64) []string {
	rows := make([]moveExitInstanceRow, 0, len(instanceIDs))
	widths := moveExitInstanceRow{}
	for _, instanceID := range instanceIDs {
		row := moveExitInstanceRowFor(database, instanceID)
		rows = append(rows, row)
		widths.instance = wider(widths.instance, row.instance)
		widths.gpu = wider(widths.gpu, row.gpu)
		widths.cpu = wider(widths.cpu, row.cpu)
		widths.disk = wider(widths.disk, row.disk)
		widths.cost = wider(widths.cost, row.cost)
		widths.status = wider(widths.status, row.status)
	}

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("  %-*s  %-*s  %*s  %*s  %*s  %-*s  %s",
			len(widths.instance), row.instance,
			len(widths.gpu), row.gpu,
			len(widths.cpu), row.cpu,
			len(widths.disk), row.disk,
			len(widths.cost), row.cost,
			len(widths.status), row.status,
			row.job,
		))
	}
	return lines
}

func moveExitInstanceRowFor(database *sql.DB, instanceID int64) moveExitInstanceRow {
	row := moveExitInstanceRow{
		instance: ids.FormatInstanceID(instanceID),
		gpu:      "-",
		cpu:      "-",
		disk:     "-",
		cost:     "-",
		status:   "-",
		job:      "-",
	}
	if database != nil {
		if launch, err := db.GetLaunch(database, instanceID); err == nil && launch != nil {
			if gpu := strings.TrimSpace(launch.DisplayGPUBrief()); gpu != "" {
				row.gpu = gpu
			}
			if launch.CPUCores > 0 {
				row.cpu = fmt.Sprintf("%dc", launch.CPUCores)
			}
			if launch.DiskGB > 0 {
				row.disk = fmt.Sprintf("%dGB", launch.DiskGB)
			}
			if launch.CostPerHourCents > 0 {
				row.cost = fmt.Sprintf("$%.2f/hr", float64(launch.CostPerHourCents)/100.0)
			}
			if status := strings.TrimSpace(launch.Status); status != "" {
				row.status = status
			}
		}
		row.job = moveExitInstanceJobLabel(database, instanceID)
	}
	return row
}

func moveExitInstanceJobLabel(database *sql.DB, instanceID int64) string {
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		return "-"
	}
	current := 0
	first := ""
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID != instanceID {
			continue
		}
		current++
		if first == "" {
			first = ids.FormatJobID(job.ID)
		}
	}
	if first == "" {
		return "-"
	}
	if current > 1 {
		return first + "+"
	}
	return first
}

func wider(a, b string) string {
	if len(b) > len(a) {
		return b
	}
	return a
}
