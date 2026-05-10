package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/status"
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
		return terminal.WatchJobsPlain(database, jobIDs, watchPlainOptions())
	}

	useTUI, err := resolveTUIMode(watchTUI, watchPlain, hasTerminalIO(), inAgentContext())
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
	return watchJobsPlainAll(database, watchPlainOptions())
}

func openJobWatchDatabase(useTUI bool) (*sql.DB, error) {
	if jobWatchNeedsWritableDB(useTUI) {
		return db.Open()
	}
	return db.OpenForReading()
}

func jobWatchNeedsWritableDB(useTUI bool) bool {
	return useTUI
}

// watchJobsPlainAll polls all jobs matching the current list filters,
// printing their status periodically.
func watchJobsPlainAll(database *sql.DB, opts terminal.WatchPlainOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	step := func(_ time.Time) (terminal.WatchPlainStep, error) {
		printWarnings(syncListData(database))
		jobs, err := collectJobsForList(database, nil)
		if err != nil {
			return terminal.WatchPlainStep{}, err
		}
		hasActive := false
		for _, job := range jobs {
			if !status.IsTerminal(job.EffectiveStatus()) {
				hasActive = true
				break
			}
		}
		return terminal.WatchPlainStep{
			Jobs:       jobs,
			RenderText: func() error { return printJobs(database, jobs) },
			HasActive:  hasActive,
		}, nil
	}
	return terminal.RunWatchPlainLoop(ctx, opts, step)
}
