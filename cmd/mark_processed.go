package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/spf13/cobra"
)

var markProcessedCmd = &cobra.Command{
	Use:   "mark-processed <job-id>",
	Short: "Mark a job as processed",
	Long:  "Mark a job as processed by adding the reserved processed tag.",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runMarkProcessed,
}

func init() {
	rootCmd.AddCommand(markProcessedCmd)
}

func runMarkProcessed(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if err := db.AddJobTag(database, jobID, db.ProcessedTag); err != nil {
		return err
	}

	fmt.Printf("Job %d marked as processed\n", jobID)
	return nil
}
