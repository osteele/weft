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

	useTUI, err := resolveCampaignTUI(projectWatchTUI, projectWatchPlain)
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

	var tracker *terminal.TransitionTracker
	if opts.EmitsEvents() {
		tracker = terminal.NewTransitionTracker()
	}

	first := true
	var lastWarnings []string
	for {
		warnings := terminal.SyncProjectWatchData(database, projectWatchSync, projectWatchNoSync)
		if !slices.Equal(warnings, lastWarnings) {
			for _, warning := range warnings {
				fmt.Fprintln(stderr, warning)
			}
			lastWarnings = warnings
		}
		groups, err := terminal.LoadProjectWatchGroups(database, projectWatchRecent)
		if err != nil {
			return err
		}
		groups = terminal.FilterProjectGroups(groups, project)
		if first && len(groups) == 0 && project != "" {
			return errNoJobsForProject(database, project)
		}
		first = false
		now := time.Now()

		jobs := flattenProjectGroupJobs(groups)

		switch opts.SnapshotMode() {
		case terminal.SnapshotText:
			if _, err := io.WriteString(stdout, terminal.RenderProjectWatchPlain(groups, terminal.ListOutputWidth(), now, projectWatchRecent)); err != nil {
				return err
			}
		case terminal.SnapshotJSON:
			if err := opts.EmitSnapshotJSON(jobs, now); err != nil {
				return err
			}
		case terminal.SnapshotNone:
		}

		var anyTerminalTransition bool
		if tracker != nil {
			events := tracker.Diff(jobs, now)
			anyTerminalTransition, err = opts.EmitTransitions(events)
			if err != nil {
				return err
			}
		}

		if opts.UntilAnyTerminal && anyTerminalTransition {
			return nil
		}
		if !opts.Follow && !opts.UntilAnyTerminal && !projectGroupsHaveActive(groups) {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(terminal.TerminalSyncInterval):
		}
	}
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
	by := make(map[int64]*db.Job)
	for _, g := range groups {
		for _, bucket := range [][]*db.Job{g.Running, g.Queued, g.Unplaced, g.Recent} {
			for _, j := range bucket {
				if j == nil {
					continue
				}
				by[j.ID] = j
			}
		}
	}
	out := make([]*db.Job, 0, len(by))
	for _, j := range by {
		out = append(out, j)
	}
	return out
}
