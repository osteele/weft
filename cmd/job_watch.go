package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var jobWatchCmd = &cobra.Command{
	Use:   "watch [job-id]...",
	Short: "Watch job status changes",
	Long: `Watch jobs with live updates.

Without arguments, shows all recent jobs and updates as their status changes.
With job IDs, watches those specific jobs until they reach a terminal state.

In an interactive terminal, displays a TUI with live database monitoring.
In non-interactive mode (or with --plain), prints periodic status updates.`,
	RunE: runJobWatch,
}

func addJobWatchFlags(cmd *cobra.Command) {
	configureWatchFlags(cmd)
	addListQueryFlags(cmd)
}

func runJobWatch(cmd *cobra.Command, args []string) error {
	// If args look like job IDs, watch those specific jobs
	if len(args) > 0 {
		jobIDs, err := ParseJobIDs(args)
		if err != nil {
			return err
		}
		database, err := db.OpenForReading()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		return terminal.WatchJobsPlain(database, jobIDs, watchFollow)
	}

	useTUI, err := resolveCampaignTUIMode(watchTUI, watchPlain, hasCampaignTerminalIO(), inCampaignAgentContext())
	if err != nil {
		return err
	}

	database, err := openJobWatchDatabase(useTUI)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if useTUI {
		jobs, err := collectJobsForList(database, nil)
		if err != nil {
			return err
		}
		autoMode := watchAuto
		if listGroupBy == "status" && !cmd.Flags().Changed("auto") {
			autoMode = true
		}
		return terminal.RunListTUI(database, nil, jobs, buildListTitle(nil), !listNoSync, listGroupBy == "status", autoMode)
	}
	return watchJobsPlainAll(database, watchFollow)
}

func openJobWatchDatabase(useTUI bool) (*sql.DB, error) {
	if jobWatchNeedsWritableDB(useTUI) {
		return db.Open()
	}
	return db.OpenForReading()
}

func jobWatchNeedsWritableDB(useTUI bool) bool {
	return useTUI && listGroupBy == "status"
}

// watchJobsPlainAll polls all jobs matching the current list filters,
// printing their status periodically.
func watchJobsPlainAll(database *sql.DB, follow bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	for {
		printWarnings(syncListData(database))

		jobs, err := collectJobsForList(database, nil)
		if err != nil {
			return err
		}

		if err := printJobs(database, jobs); err != nil {
			return err
		}

		hasActive := false
		for _, job := range jobs {
			if !isTerminalStatus(job.EffectiveStatus()) {
				hasActive = true
				break
			}
		}

		if !follow && !hasActive {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}
