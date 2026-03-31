package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
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

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectJobsCmd)
	projectCmd.AddCommand(projectLaunchCmd)

	addListQueryFlags(projectListCmd)
	addListQueryFlags(projectJobsCmd)
	addCampaignLaunchFlags(projectLaunchCmd)
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

func errNoJobsForProject(project string) error {
	return fmt.Errorf("no jobs found for project %q; is the current directory a known project?", project)
}

// filterLaunchJobsByProject applies campaignLaunchProject to a job list.
func filterLaunchJobsByProject(jobs []*db.Job) []*db.Job {
	if campaignLaunchProject == "" {
		return jobs
	}
	return db.FilterJobsByProject(jobs, campaignLaunchProject)
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
	campaignLaunchProject = project
	return runCampaignLaunch(cmd, nil)
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

	for _, warning := range syncListData(database) {
		fmt.Fprintln(cmd.ErrOrStderr(), warning)
	}

	jobs, err := collectJobsForList(database, nil)
	if err != nil {
		return err
	}
	if len(jobs) == 0 && listProject != "" {
		return errNoJobsForProject(listProject)
	}
	return writeListPlainOutput(renderProjectListPlain(groupJobsByProject(jobs), listOutputWidth()))
}

func runProjectJobs(cmd *cobra.Command, args []string) error {
	// If --project not set, check if first arg is a project name (non-numeric)
	if listProject == "" && len(args) > 0 {
		if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
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

	for _, warning := range syncListData(database) {
		fmt.Fprintln(cmd.ErrOrStderr(), warning)
	}

	jobs, err := collectJobsForList(database, args)
	if err != nil {
		return err
	}
	if len(jobs) == 0 && listProject != "" {
		return errNoJobsForProject(listProject)
	}
	return writeListPlainOutput(renderProjectJobsPlain(groupJobsByProject(jobs), listOutputWidth()))
}
