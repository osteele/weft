package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// FormatMoveExitSummary prints the receipt shown after the move launch TUI exits.
func FormatMoveExitSummary(database *sql.DB, jobs []*db.Job, instanceIDs []int64) string {
	return FormatMoveExitSummaryAt(database, jobs, instanceIDs, time.Now(), listOutputWidth())
}

func FormatMoveExitSummaryAt(database *sql.DB, jobs []*db.Job, instanceIDs []int64, now time.Time, width int) string {
	if len(jobs) == 0 && len(instanceIDs) == 0 {
		return ""
	}
	if width <= 0 {
		width = 120
	}

	var b strings.Builder
	writeSummaryLine(&b, width, fmt.Sprintf("weft move ended - %s", now.Format("Mon Jan 2 15:04")))
	if len(jobs) > 0 {
		writeSummaryLine(&b, width, fmt.Sprintf("Moved: %s -> %s", campaign.FormatJobIDs(jobs, 8), pluralize(len(instanceIDs), "new instance", "new instances")))
	}
	if len(instanceIDs) > 0 {
		writeSummaryLine(&b, width, "Instances:")
		for _, line := range moveExitInstanceLines(database, instanceIDs) {
			writeSummaryLine(&b, width, line)
		}
	}
	writeSummaryLine(&b, width, "")
	writeSummaryLine(&b, width, "Next: weft watch instance | weft uj | weft log <job>")
	return b.String()
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
