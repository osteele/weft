package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "View jobs grouped by project",
	Long: `View jobs grouped by project.

Available subcommands:
  jobs   List jobs grouped by project
  watch  Watch active and recent jobs grouped by project`,
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
	projectCmd.AddCommand(projectJobsCmd)

	addListQueryFlags(projectJobsCmd)
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
