package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/spf13/cobra"
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Manage job tags",
	Long: `Manage job tags stored in the local database.

Tags are free-form labels that help group jobs and track processing state.`,
}

var tagAddCmd = &cobra.Command{
	Use:   "add <job-id> <tag>",
	Short: "Add a tag to a job",
	Args:  usageArgs(cobra.ExactArgs(2)),
	RunE:  runTagAdd,
}

var tagRemoveCmd = &cobra.Command{
	Use:     "rm <job-id> <tag>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a tag from a job",
	Args:    usageArgs(cobra.ExactArgs(2)),
	RunE:    runTagRemove,
}

func init() {
	rootCmd.AddCommand(tagCmd)
	tagCmd.AddCommand(tagAddCmd)
	tagCmd.AddCommand(tagRemoveCmd)
}

func runTagAdd(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}
	tag := args[1]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if err := db.AddJobTag(database, jobID, tag); err != nil {
		return err
	}

	fmt.Printf("Added tag %q to job %d\n", tag, jobID)
	return nil
}

func runTagRemove(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}
	tag := args[1]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if err := db.RemoveJobTag(database, jobID, tag); err != nil {
		return err
	}

	fmt.Printf("Removed tag %q from job %d\n", tag, jobID)
	return nil
}
