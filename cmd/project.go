package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"projects"},
	Short:   "View jobs grouped by project",
	Long: `View jobs grouped by project.

Available subcommands:
  list   List projects with summary stats (default)
  jobs   List jobs grouped by project
  watch  Watch active and recent jobs grouped by project`,
	RunE: runProjectList,
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List projects with summary stats",
	Long: `List all projects with job counts and last activity time.

Shows one row per project with aggregate statistics.`,
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

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectJobsCmd)

	addListQueryFlags(projectListCmd)
	addListQueryFlags(projectJobsCmd)
}

func runProjectList(cmd *cobra.Command, args []string) error {
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
	return writeListPlainOutput(renderProjectListPlain(groupJobsByProject(jobs), listOutputWidth()))
}

func runProjectJobs(cmd *cobra.Command, args []string) error {
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
	return writeListPlainOutput(renderProjectJobsPlain(groupJobsByProject(jobs), listOutputWidth()))
}
