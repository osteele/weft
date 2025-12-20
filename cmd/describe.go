package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/spf13/cobra"
)

var describeCmd = &cobra.Command{
	Use:   "describe <job-id> [description]",
	Short: "Set or update the description of a job",
	Long: `Set or update the description of an existing job.

Examples:
  remote-jobs describe 42 "Training GPT-2 with lr=0.001"
  remote-jobs describe 42 ""  # Clear description`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runDescribe,
}

func init() {
	// Removed: Describe command is now only available as `job describe`
	// rootCmd.AddCommand(describeCmd)
}

func runDescribe(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	// Description is optional second argument
	description := ""
	if len(args) > 1 {
		description = args[1]
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Check job exists
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Update description
	if err := db.UpdateJobDescription(database, jobID, description); err != nil {
		return fmt.Errorf("update description: %w", err)
	}

	if description == "" {
		fmt.Printf("Cleared description for job %d\n", jobID)
	} else {
		fmt.Printf("Updated description for job %d: %s\n", jobID, description)
	}

	return nil
}
