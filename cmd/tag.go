package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Manage job tags",
	Long: `Manage job tags stored in the local database.

Tags are free-form labels that help group jobs and track processing state.`,
}

var tagAddCmd = &cobra.Command{
	Use:   "add <job-id>... <tag>",
	Short: "Add a tag to a job",
	Args:  usageArgs(cobra.MinimumNArgs(2)),
	RunE:  runTagAdd,
}

var tagRemoveCmd = &cobra.Command{
	Use:     "rm <job-id>... <tag>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a tag from a job",
	Args:    usageArgs(cobra.MinimumNArgs(2)),
	RunE:    runTagRemove,
}

func init() {
	tagCmd.Deprecated = "use 'weft job tag' instead"
	rootCmd.AddCommand(tagCmd)
	tagCmd.AddCommand(tagAddCmd)
	tagCmd.AddCommand(tagRemoveCmd)
}

func runTagAdd(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args[:len(args)-1])
	if err != nil {
		return err
	}
	tag := args[len(args)-1]
	displayTag := db.CanonicalizeTag(tag)

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := db.AddJobTag(database, jobID, tag); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		fmt.Printf("Added tag %q to job %d\n", displayTag, jobID)
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: reload: %v", jobID, err))
			continue
		}
		if job != nil && job.HasTagHostConflict() {
			oldHost := job.Host
			if result, err := ops.UnplaceQueuedJob(database, job, ops.DefaultOptions()); err != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %d: unplace: %v", jobID, err))
			} else if result.Success {
				fmt.Printf("  Unplaced job %d from %s (rental tag conflicts with inventory host)\n", jobID, oldHost)
			}
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runTagRemove(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args[:len(args)-1])
	if err != nil {
		return err
	}
	tag := args[len(args)-1]
	displayTag := db.CanonicalizeTag(tag)

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := db.RemoveJobTag(database, jobID, tag); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		fmt.Printf("Removed tag %q from job %d\n", displayTag, jobID)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}
