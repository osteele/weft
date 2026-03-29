package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/spf13/cobra"
)

var projectWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch active and recent jobs grouped by project",
	Long: `Watch active and recent jobs grouped by project.

In an interactive terminal this defaults to a read-only TUI. Otherwise it
prints a grouped plain-text snapshot.`,
	RunE: runProjectWatch,
}

var (
	projectWatchTUI    bool
	projectWatchPlain  bool
	projectWatchSync   bool
	projectWatchNoSync bool
	projectWatchRecent time.Duration
)

const projectWatchSyncInterval = 30 * time.Second

func init() {
	projectCmd.AddCommand(projectWatchCmd)
	addProjectWatchFlags(projectWatchCmd)
}

func addProjectWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&projectWatchTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&projectWatchPlain, "plain", false, "Force plain text output")
	cmd.Flags().BoolVar(&projectWatchSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	cmd.Flags().BoolVar(&projectWatchNoSync, "no-sync", false, "Skip syncing job statuses before displaying")
	cmd.Flags().DurationVar(&projectWatchRecent, "recent", 24*time.Hour, "Window for recent terminal jobs")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func runProjectWatch(cmd *cobra.Command, args []string) error {
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
		return runProjectWatchTUI(database, projectWatchRecent, !projectWatchNoSync)
	}

	for _, warning := range syncProjectWatchData(database) {
		fmt.Fprintln(cmd.ErrOrStderr(), warning)
	}
	groups, err := loadProjectWatchGroups(database, projectWatchRecent)
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), renderProjectWatchPlain(groups, listOutputWidth(), time.Now(), projectWatchRecent))
	return err
}

func runProjectWatchTUI(database *sql.DB, recentWindow time.Duration, syncEnabled bool) error {
	cfg, _ := config.Load()
	router := newProjectWatchRouterModel(database, cfg, recentWindow, syncEnabled, watchAuto)

	restore := logging.Suppress()
	defer restore()

	finalModel, err := tea.NewProgram(router, tea.WithAltScreen()).Run()

	// Clean up syncWorker from whichever model was active at exit
	if r, ok := finalModel.(watchRouterModel); ok {
		if w, ok := r.active.(watchModel); ok && w.syncWorker != nil {
			w.syncWorker.Stop()
		}
	}
	if err != nil {
		return fmt.Errorf("run project watch TUI: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Data loading (shared by TUI and plain modes)
// ---------------------------------------------------------------------------

func syncProjectWatchTUIData(database *sql.DB, full bool) []string {
	timeout := FastSyncTimeout
	if full {
		timeout = NormalSyncTimeout
	}
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailedWithOptions(database, nil, timeout, false, full)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}

func loadProjectWatchGroups(database *sql.DB, recentWindow time.Duration) ([]projectGroup, error) {
	activeJobs := make([]*db.Job, 0)
	running, err := db.ListAllRunning(database)
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	activeJobs = append(activeJobs, running...)

	for _, status := range []string{db.StatusStarting, db.StatusPaused, db.StatusQueued, db.StatusPendingPlacement} {
		jobs, err := db.ListJobs(database, status, "", 0, nil, "")
		if err != nil {
			return nil, fmt.Errorf("list %s jobs: %w", status, err)
		}
		activeJobs = append(activeJobs, jobs...)
	}

	recentJobs, err := db.ListRecentTerminalJobs(database, time.Now().Add(-recentWindow).Unix())
	if err != nil {
		return nil, fmt.Errorf("list recent terminal jobs: %w", err)
	}

	// Load unplaced jobs to separate them from the Queued bucket
	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	unplacedSet := make(map[int64]bool, len(unplacedJobs))
	for _, job := range unplacedJobs {
		unplacedSet[job.ID] = true
	}

	groups := groupProjectActivity(activeJobs, recentJobs)
	classifyUnplacedJobs(groups, unplacedSet)
	if err := attachProjectLaunches(database, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// classifyUnplacedJobs moves jobs from the Queued bucket to the Unplaced bucket
// if they are in the unplaced set.
func classifyUnplacedJobs(groups []projectGroup, unplacedSet map[int64]bool) {
	for i := range groups {
		var queued []*db.Job
		for _, job := range groups[i].Queued {
			if unplacedSet[job.ID] {
				groups[i].Unplaced = append(groups[i].Unplaced, job)
			} else {
				queued = append(queued, job)
			}
		}
		groups[i].Queued = queued
	}
}

func attachProjectLaunches(database *sql.DB, groups []projectGroup) error {
	cache := make(map[int64]*db.Launch)
	for i := range groups {
		seen := make(map[int64]struct{})
		appendJobInstance := func(job *db.Job) error {
			if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
				return nil
			}
			instanceID := *job.LaunchID
			if _, ok := seen[instanceID]; ok {
				return nil
			}
			seen[instanceID] = struct{}{}

			inst, ok := cache[instanceID]
			if !ok {
				var err error
				inst, err = db.GetLaunch(database, instanceID)
				if err != nil {
					return fmt.Errorf("get cloud instance %d: %w", instanceID, err)
				}
				cache[instanceID] = inst
			}
			if inst != nil {
				groups[i].CloudInsts = append(groups[i].CloudInsts, inst)
			}
			return nil
		}

		for _, bucket := range [][]*db.Job{groups[i].Running, groups[i].Queued, groups[i].Recent} {
			for _, job := range bucket {
				if err := appendJobInstance(job); err != nil {
					return err
				}
			}
		}
		sort.SliceStable(groups[i].CloudInsts, func(a, b int) bool {
			return groups[i].CloudInsts[a].ID < groups[i].CloudInsts[b].ID
		})
	}
	return nil
}

func syncProjectWatchData(database *sql.DB) []string {
	if projectWatchNoSync {
		return nil
	}

	var warnings []string
	if projectWatchSync {
		completed, unreachable, syncWarnings := performSyncWithTimeoutForHostsDetailed(database, nil, DefaultSyncTimeout, false)
		warnings = append(warnings, syncWarnings...)
		if !completed {
			if note := buildStaleDataNote(database, unreachable); note != "" {
				warnings = append(warnings, note)
			}
		}
		return warnings
	}

	completed, unreachable, syncWarnings := performSyncWithTimeoutForHostsDetailed(database, nil, FastSyncTimeout, false)
	warnings = append(warnings, syncWarnings...)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}
