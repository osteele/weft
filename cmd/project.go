package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var projectCmd = &cobra.Command{
	Use:     "project [project-name]",
	Aliases: []string{"projects"},
	Short:   "View jobs grouped by project",
	Long: `View jobs grouped by project.

Defaults to the current directory's project. Pass a project name to override.

Available subcommands:
  list    List projects with summary stats (default)
  jobs    List jobs grouped by project
  watch   Watch active and recent jobs grouped by project
  launch  Launch cloud instances for unplaced jobs in this project`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectList,
}

var projectListCmd = &cobra.Command{
	Use:   "list [project-name]",
	Short: "List projects with summary stats",
	Long: `List projects with job counts and last activity time.

Defaults to the current directory's project. Pass a project name to override.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectList,
}

var projectJobsCmd = &cobra.Command{
	Use:   "jobs [job-id]...",
	Short: "List jobs grouped by project",
	Long: `List jobs grouped by project.

This behaves like job list, but renders grouped project blocks instead of a
single flat table. Only projects with matching jobs are shown.`,
	RunE: runProjectJobs,
}

var projectLaunchCmd = &cobra.Command{
	Use:     "launch",
	Aliases: []string{"run", "start"},
	Short:   "Launch cloud instances for unplaced jobs in this project",
	Long: `Launches cloud instances for unplaced jobs filtered to a specific project.

The project defaults to the current directory's project name.
Pass a project name as the first argument to override.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProjectLaunch,
}

var (
	projectSpentSince string
)

var projectSpentCmd = &cobra.Command{
	Use:   "spent",
	Short: "Show spend per project since a cutoff",
	Long: `Show cloud spend per project since a cutoff time.

Examples:
  weft project spent --since 2026-04-01
  weft project spent --since "36h ago"
  weft project spent --since 7d`,
	Args: cobra.NoArgs,
	RunE: runProjectSpent,
}

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectJobsCmd)
	projectCmd.AddCommand(projectLaunchCmd)
	projectCmd.AddCommand(projectSpentCmd)

	addListQueryFlags(projectListCmd)
	addListQueryFlags(projectJobsCmd)
	addInstanceLaunchFlags(projectLaunchCmd)
	projectSpentCmd.Flags().StringVar(&projectSpentSince, "since", "", "Cutoff time as YYYY-MM-DD or duration ago (for example: \"24h ago\", \"7d\")")
	_ = projectSpentCmd.MarkFlagRequired("since")
}

// resolveProjectArg returns the project name from args[0] if provided,
// otherwise derives it from the current working directory.
func resolveProjectArg(args []string) (string, error) {
	explicit := ""
	if len(args) > 0 {
		explicit = strings.TrimSpace(args[0])
	}
	project, err := workdir.ResolveProjectName(explicit, "")
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("could not determine project from current directory; pass a project name as an argument")
	}
	return project, nil
}

func errNoJobsForProject(database *sql.DB, project string) error {
	hasAny, err := db.ProjectHasAnyJobs(database, project)
	if err == nil && hasAny {
		return fmt.Errorf("no jobs found for project %q matching the current filters", project)
	}
	return fmt.Errorf("no jobs found for project %q; is the current directory a known project?", project)
}

// filterLaunchJobsByProject applies instanceLaunchProject to a job list.
func filterLaunchJobsByProject(jobs []*db.Job) []*db.Job {
	if instanceLaunchProject == "" {
		return jobs
	}
	return db.FilterJobsByProject(jobs, instanceLaunchProject)
}

// defaultProjectFilter sets listProject from args or cwd if not already set via --project.
func defaultProjectFilter(args []string) error {
	if listProject != "" {
		return nil
	}
	project, err := resolveProjectArg(args)
	if err != nil {
		return err
	}
	listProject = project
	return nil
}

func runProjectLaunch(cmd *cobra.Command, args []string) error {
	project, err := resolveProjectArg(args)
	if err != nil {
		return err
	}
	instanceLaunchProject = project
	return runInstanceLaunch(cmd, nil)
}

func runProjectList(cmd *cobra.Command, args []string) error {
	if err := defaultProjectFilter(args); err != nil {
		return err
	}

	database, err := openJobsDB()
	if err != nil {
		return err
	}
	defer database.Close()

	writeWarnings(cmd.ErrOrStderr(), syncListDataContext(cmd.Context(), database))

	jobs, err := collectJobsForList(database, nil)
	if err != nil {
		return err
	}
	if len(jobs) == 0 && listProject != "" {
		return errNoJobsForProject(database, listProject)
	}
	return terminal.WriteListPlainOutput(terminal.RenderProjectListPlain(terminal.GroupJobsByProject(jobs), terminal.ListOutputWidth()))
}

func runProjectJobs(cmd *cobra.Command, args []string) error {
	// If --project not set, check if first arg is a project name (non-numeric)
	if listProject == "" && len(args) > 0 {
		if _, err := ids.ParseJobID(args[0]); err != nil {
			listProject = args[0]
			args = args[1:]
		}
	}
	if err := defaultProjectFilter(nil); err != nil {
		return err
	}

	database, err := openJobsDB()
	if err != nil {
		return err
	}
	defer database.Close()

	writeWarnings(cmd.ErrOrStderr(), syncListDataContext(cmd.Context(), database))

	jobs, err := collectJobsForList(database, args)
	if err != nil {
		return err
	}
	if len(jobs) == 0 && listProject != "" {
		return errNoJobsForProject(database, listProject)
	}
	return terminal.WriteListPlainOutput(terminal.RenderProjectJobsPlain(terminal.GroupJobsByProject(jobs), terminal.ListOutputWidth()))
}

func runProjectSpent(cmd *cobra.Command, _ []string) error {
	since, err := parseSinceCutoff(projectSpentSince, time.Now())
	if err != nil {
		return err
	}

	database, err := openJobsDB()
	if err != nil {
		return err
	}
	defer database.Close()

	writeWarnings(cmd.ErrOrStderr(), syncListDataContext(cmd.Context(), database))

	rows, err := db.ListProjectSpendSince(database, since.Unix())
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No project spend found since %s\n", since.Format(time.RFC3339))
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROJECT\tSPENT\tRUNS\n")
	total := 0.0
	totalRuns := 0
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t$%.2f\t%d\n", row.Project, row.SpentUSD, row.RunCount)
		total += row.SpentUSD
		totalRuns += row.RunCount
	}
	fmt.Fprintf(w, "TOTAL\t$%.2f\t%d\n", total, totalRuns)
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "\nSince %s\n", since.Format(time.RFC3339))
	return err
}
