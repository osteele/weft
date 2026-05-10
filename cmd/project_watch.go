package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var projectWatchCmd = &cobra.Command{
	Use:   "watch [project-name]",
	Short: "Watch active and recent jobs grouped by project",
	Long: `Watch active and recent jobs grouped by project.

Defaults to the current directory's project. Pass a project name to override.

In an interactive terminal this defaults to a read-only TUI. Otherwise it
polls the database, printing a grouped plain-text snapshot every refresh
interval until all jobs reach a terminal state. Use --follow to keep printing
even when nothing is active.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectWatch,
}

var (
	projectWatchTUI    bool
	projectWatchPlain  bool
	projectWatchSync   bool
	projectWatchNoSync bool
	projectWatchRecent time.Duration
)

func init() {
	projectCmd.AddCommand(projectWatchCmd)
	addProjectWatchFlags(projectWatchCmd)
}

func addProjectWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&projectWatchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&projectWatchPlain, "plain", false, "Force plain text output")
	cmd.Flags().BoolVar(&projectWatchSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	cmd.Flags().BoolVar(&projectWatchNoSync, "no-sync", false, "Skip syncing job statuses before displaying")
	cmd.Flags().BoolVarP(&watchFollow, "follow", "f", false, "Keep printing snapshots even when nothing is active")
	cmd.Flags().DurationVar(&projectWatchRecent, "recent", 24*time.Hour, "Window for recent terminal jobs")
	addWatchEventFlags(cmd)
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func openJobsDB() (*sql.DB, error) {
	database, err := db.OpenForReading()
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return database, nil
}

func runProjectWatch(cmd *cobra.Command, args []string) error {
	project, err := resolveProjectArg(args)
	if err != nil {
		return err
	}

	useTUI, err := resolveTUI(projectWatchTUI, projectWatchPlain)
	if err != nil {
		return err
	}

	database, err := openJobsDB()
	if err != nil {
		return err
	}
	defer database.Close()

	if useTUI {
		cfg, _ := config.Load()
		return terminal.RunProjectWatchTUI(database, cfg, projectWatchRecent, !projectWatchNoSync, project, watchAuto)
	}

	opts := watchPlainOptions()
	opts.Stdout = cmd.OutOrStdout()
	opts.Stderr = cmd.ErrOrStderr()
	return runProjectWatchPlain(cmd, database, project, opts)
}

func runProjectWatchPlain(cmd *cobra.Command, database *sql.DB, project string, opts terminal.WatchPlainOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	first := true
	var lastWarnings []string

	step := func(now time.Time) (terminal.WatchPlainStep, error) {
		warnings := terminal.SyncProjectWatchData(database, projectWatchSync, projectWatchNoSync)
		if !slices.Equal(warnings, lastWarnings) {
			for _, warning := range warnings {
				fmt.Fprintln(stderr, warning)
			}
			lastWarnings = warnings
		}
		groups, err := terminal.LoadProjectWatchGroups(database, projectWatchRecent)
		if err != nil {
			return terminal.WatchPlainStep{}, err
		}
		groups = terminal.FilterProjectGroups(groups, project)
		if first && len(groups) == 0 && project != "" {
			return terminal.WatchPlainStep{}, errNoJobsForProject(database, project)
		}
		first = false

		return terminal.WatchPlainStep{
			Jobs: flattenProjectGroupJobs(groups),
			RenderText: func() error {
				_, err := io.WriteString(stdout, terminal.RenderProjectWatchPlain(groups, terminal.ListOutputWidth(), now, projectWatchRecent))
				return err
			},
			HasActive: projectGroupsHaveActive(groups),
		}, nil
	}
	return terminal.RunWatchPlainLoop(ctx, opts, step)
}

func projectGroupsHaveActive(groups []terminal.ProjectGroup) bool {
	for _, g := range groups {
		if len(g.Running) > 0 || len(g.Queued) > 0 || len(g.Unplaced) > 0 {
			return true
		}
	}
	return false
}

// flattenProjectGroupJobs collects every *db.Job referenced by the project
// groups (Running/Queued/Unplaced/Recent), deduplicating by ID. Recent is
// included so already-terminal jobs are baselined as terminal and don't fire
// spurious transitions.
func flattenProjectGroupJobs(groups []terminal.ProjectGroup) []*db.Job {
	all := make([][]*db.Job, 0, 4*len(groups))
	for _, g := range groups {
		all = append(all, g.Running, g.Queued, g.Unplaced, g.Recent)
	}
	return terminal.DedupeJobsByID(all...)
}
