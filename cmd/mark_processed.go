package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var markProcessedCmd = &cobra.Command{
	Use:   "mark-processed <job-id>...",
	Short: "Mark a job as processed",
	Long:  "Mark a job as processed by adding the reserved processed tag.",
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runMarkProcessed,
}

func init() {
	markProcessedCmd.Deprecated = "use 'weft job mark-processed' instead"
	rootCmd.AddCommand(markProcessedCmd)
}

func runMarkProcessed(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := db.AddJobTag(database, jobID, db.ProcessedTag); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		fmt.Printf("Job %d marked as processed\n", jobID)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}
