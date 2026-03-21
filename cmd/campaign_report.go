package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/tui"
)

// watchAndReport runs the appropriate watch mode (TUI or plain) and prints
// an exit report when all instances reach terminal state.
func watchAndReport(database *sql.DB, useTUI bool, instanceIDs []int64) error {
	var finalIDs []int64
	var err error
	if useTUI {
		finalIDs, err = watchInstances(database, instanceIDs)
	} else {
		err = watchInstancesPlain(database, instanceIDs)
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
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	var totalCost float64
	allTerminal := true
	hasInstances := false
	seenJobs := make(map[int64]bool)

	type jobRow struct {
		id          int64
		status      string
		instanceID  int64
		description string
	}
	var jobs []jobRow

	// Instance table header (deferred until we know we have data)
	var instanceLines []string
	for _, id := range instanceIDs {
		ci, err := db.GetCloudInstance(database, id)
		if err != nil || ci == nil {
			continue
		}
		hasInstances = true
		if !campaign.IsInstanceTerminal(ci.Status) {
			allTerminal = false
		}

		obs := observeCloudInstance(ci, nil, now)
		uptimeStr := "—"
		if obs.Uptime != nil {
			uptimeStr = tui.FormatCompactDuration(*obs.Uptime)
		}
		costStr := "—"
		if obs.Cost != nil {
			costStr = fmt.Sprintf("$%.2f", *obs.Cost)
			totalCost += *obs.Cost
		}
		reason := ci.TerminationReason
		if reason == "" {
			reason = "—"
		}

		instanceLines = append(instanceLines, fmt.Sprintf("  %d\t%s\t%s\t%s\t%s\t%s\n",
			id, ci.DisplayGPUSpec(), ci.Status, uptimeStr, costStr, reason))

		// Collect jobs for this instance
		instanceJobs, err := db.GetCloudInstanceJobsIncludingAttempts(database, id)
		if err != nil {
			continue
		}
		outcomes, _ := db.GetAttemptOutcomesByInstance(database, id)
		for _, j := range instanceJobs {
			if j == nil || seenJobs[j.ID] {
				continue
			}
			seenJobs[j.ID] = true
			jobs = append(jobs, jobRow{
				id:          j.ID,
				status:      campaign.JobDisplayStatus(j, outcomes),
				instanceID:  id,
				description: truncate(j.EffectiveDescription(), 60),
			})
		}
	}

	if !hasInstances {
		return
	}

	fmt.Println()
	if allTerminal {
		fmt.Println("Campaign complete.")
	} else {
		fmt.Println("Campaign summary:")
	}
	fmt.Println()

	// Instance table
	fmt.Fprintf(w, "  INSTANCE\tGPU\tSTATUS\tUPTIME\tCOST\tREASON\n")
	for _, line := range instanceLines {
		fmt.Fprint(w, line)
	}
	w.Flush()

	// Job table
	if len(jobs) > 0 {
		fmt.Println()
		w = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  JOB\tSTATUS\tINSTANCE\tDESCRIPTION\n")
		for _, j := range jobs {
			fmt.Fprintf(w, "  %d\t%s\t%d\t%s\n",
				j.id, j.status, j.instanceID, j.description)
		}
		w.Flush()
	}

	// Total cost
	if totalCost > 0 {
		fmt.Printf("\n  Total cost: $%.2f\n", totalCost)
	}
}
