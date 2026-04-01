package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
	"github.com/osteele/weft/internal/ui/terminal"
)

// watchAndReport runs the appropriate watch mode (TUI or plain) and prints
// an exit report when all instances reach terminal state.
func watchAndReport(database *sql.DB, useTUI bool, mode terminal.Mode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary, autoMode bool) error {
	var finalIDs []int64
	var err error
	if useTUI {
		finalIDs, err = terminal.WatchInstances(database, mode, instanceIDs, estimateSummary, autoMode)
	} else {
		err = terminal.WatchInstancesPlain(database, mode, instanceIDs, estimateSummary)
		finalIDs = instanceIDs
	}
	printWatchExitReport(database, finalIDs)
	return err
}

// printWatchExitReport prints a concise summary after campaign watch completes.
// It shows instance status, jobs, and total cost.
func printWatchExitReport(database *sql.DB, instanceIDs []int64) {
	if len(instanceIDs) == 0 {
		return
	}

	now := time.Now()
	var totalCost float64
	allTerminal := true
	allCompleted := true
	hasInstances := false
	seenJobs := make(map[int64]bool)
	watchedSet := make(map[int64]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		watchedSet[id] = true
	}

	type jobRow struct {
		id              int64
		status          string
		instanceID      int64
		project         string
		fullDescription string // untruncated; truncated at render time
	}

	var instances []exitReportInstanceRow
	var jobs []jobRow

	// Collect instances and jobs from the watched session
	for _, id := range instanceIDs {
		ci, err := db.GetLaunch(database, id)
		if err != nil || ci == nil {
			continue
		}
		hasInstances = true
		if !campaign.IsInstanceTerminal(ci.Status) {
			allTerminal = false
			allCompleted = false
		} else if ci.Status != db.LaunchStatusCompleted {
			allCompleted = false
		}

		row := buildInstanceRow(ci, now)
		totalCost += row.cost
		instances = append(instances, row)

		// Collect jobs for this instance
		instanceJobs, err := db.GetLaunchJobsIncludingAttempts(database, id)
		if err != nil {
			continue
		}
		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, id)
		for _, j := range instanceJobs {
			if j == nil || seenJobs[j.ID] {
				continue
			}
			seenJobs[j.ID] = true
			jobs = append(jobs, jobRow{
				id:              j.ID,
				status:          campaign.AttemptDisplayStatus(j, outcomes),
				instanceID:      id,
				project:         campaign.JobProjectLabel(j),
				fullDescription: j.EffectiveDescription(),
			})
		}
	}

	if !hasInstances {
		return
	}

	// Collect historical instance attempts for the watched jobs (last 5 not already shown)
	jobIDs := make([]int64, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.id
	}
	historicalIDs := collectHistoricalInstanceIDs(database, jobIDs, watchedSet)
	const maxHistorical = 5
	omitted := 0
	if len(historicalIDs) > maxHistorical {
		omitted = len(historicalIDs) - maxHistorical
		historicalIDs = historicalIDs[len(historicalIDs)-maxHistorical:]
	}
	var historicalInstances []exitReportInstanceRow
	for _, id := range historicalIDs {
		ci, err := db.GetLaunch(database, id)
		if err != nil || ci == nil {
			continue
		}
		historicalInstances = append(historicalInstances, buildInstanceRow(ci, now))
	}

	// Sort current instances by ID
	slices.SortFunc(instances, func(a, b exitReportInstanceRow) int {
		return int(a.id - b.id)
	})

	// Check for other running instances not in this watch session
	var otherRunning []*db.Launch
	if allRunning, err := db.ListRunningLaunches(database); err == nil {
		for _, ci := range allRunning {
			if !watchedSet[ci.ID] {
				otherRunning = append(otherRunning, ci)
			}
		}
	}

	// Header
	fmt.Println()
	switch {
	case !allTerminal || len(otherRunning) > 0:
		fmt.Println("Rental summary:")
	case allCompleted:
		fmt.Println("All rentals completed.")
	default:
		fmt.Println("All rentals terminated.")
	}
	fmt.Println()

	// Instance table — current campaign
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "  INSTANCE\tGPU\tSTATUS\tUPTIME\tCOST\tREASON\n")
	for _, inst := range instances {
		fmt.Fprint(w, inst.line)
	}
	w.Flush()

	// Historical attempts from prior campaigns
	if len(historicalInstances) > 0 || omitted > 0 {
		fmt.Println()
		hw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		total := len(historicalInstances) + omitted
		fmt.Fprintf(hw, "  Prior attempts (%d):\n", total)
		if omitted > 0 {
			fmt.Fprintf(hw, "  ...\t\t\t\t\t(%d earlier)\n", omitted)
		}
		for _, inst := range historicalInstances {
			fmt.Fprint(hw, inst.line)
		}
		hw.Flush()
	}

	// Other running instances not in this watch session
	if len(otherRunning) > 0 {
		fmt.Println()
		if len(otherRunning) == 1 {
			ci := otherRunning[0]
			fmt.Printf("  1 other instance still running (ID %d, %s)\n", ci.ID, ci.DisplayGPUSpec())
		} else {
			ids := make([]string, len(otherRunning))
			for i, ci := range otherRunning {
				ids[i] = fmt.Sprintf("%d", ci.ID)
			}
			fmt.Printf("  %d other instances still running (IDs %s)\n", len(otherRunning), strings.Join(ids, ", "))
		}
	}

	// Job table
	if len(jobs) > 0 {
		// Compute project column width and description budget
		projectWidth := 0
		for _, j := range jobs {
			if n := len(j.project); n > projectWidth {
				projectWidth = n
			}
		}
		if projectWidth > 30 {
			projectWidth = 30
		}

		// Calculate description width from terminal width
		// Fixed columns: indent(2) + JOB(~5) + STATUS(~10) + INSTANCE(~5) + PROJECT(projectWidth)
		// Plus tab separators (4 gaps × ~4 chars each ≈ 16)
		termWidth := terminal.ListOutputWidth()
		fixedWidth := 2 + 5 + 10 + 5 + projectWidth + 20 // columns + padding
		descWidth := termWidth - fixedWidth
		if descWidth < 30 {
			descWidth = 30
		}

		fmt.Println()
		w = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  JOB\tSTATUS\tINSTANCE\tPROJECT\tDESCRIPTION\n")
		for _, j := range jobs {
			fmt.Fprintf(w, "  %d\t%s\t%d\t%s\t%s\n",
				j.id, j.status, j.instanceID, truncate(j.project, projectWidth), truncate(j.fullDescription, descWidth))
		}
		w.Flush()
	}

	// Total cost
	if totalCost > 0 {
		fmt.Printf("\n  Total cost: $%.2f\n", totalCost)
	}
}

type exitReportInstanceRow struct {
	id   int64
	line string
	cost float64
}

// buildInstanceRow creates a formatted instance row for the exit report.
func buildInstanceRow(ci *db.Launch, now time.Time) exitReportInstanceRow {
	obs := terminal.ObserveLaunch(ci, nil, now)
	uptimeStr := "—"
	if obs.Uptime != nil {
		uptimeStr = dashboard.FormatCompactDuration(*obs.Uptime)
	}
	costStr := "—"
	var cost float64
	if obs.Cost != nil {
		costStr = fmt.Sprintf("$%.2f", *obs.Cost)
		cost = *obs.Cost
	}
	reason := ci.DisplayTerminationReason()
	if reason == "" {
		reason = "—"
	}

	return exitReportInstanceRow{
		id:   ci.ID,
		line: fmt.Sprintf("  %d\t%s\t%s\t%s\t%s\t%s\n", ci.ID, ci.DisplayGPUSpec(), ci.Status, uptimeStr, costStr, reason),
		cost: cost,
	}
}

// collectHistoricalInstanceIDs finds cloud instance IDs from prior attempts
// for the given jobs, excluding instances already in the watched set.
// Returns IDs sorted ascending.
func collectHistoricalInstanceIDs(database *sql.DB, jobIDs []int64, watchedSet map[int64]bool) []int64 {
	seen := make(map[int64]bool)
	for _, jobID := range jobIDs {
		attempts, err := db.GetLaunchAttempts(database, jobID)
		if err != nil {
			continue
		}
		for _, a := range attempts {
			if !watchedSet[a.LaunchID] && !seen[a.LaunchID] {
				seen[a.LaunchID] = true
			}
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
