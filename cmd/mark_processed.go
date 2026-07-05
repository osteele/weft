package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/localmutate"
	"github.com/spf13/cobra"
)

var markProcessedCmd = &cobra.Command{
	Use:   "mark-processed <job-id>...",
	Short: "Mark a job as processed",
	Long:  "Mark a job as processed by adding the reserved processed tag.",
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runMarkProcessed,
}

var markUnprocessedCmd = &cobra.Command{
	Use:   "mark-unprocessed <job-id>...",
	Short: "Mark a job as unprocessed",
	Long:  "Mark a job as unprocessed by removing the reserved processed tag.",
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runMarkUnprocessed,
}

func init() {
	rootCmd.AddCommand(markProcessedCmd)
	rootCmd.AddCommand(markUnprocessedCmd)
}

func runMarkProcessed(_ *cobra.Command, args []string) error {
	return setProcessedTag(args, true)
}

func runMarkUnprocessed(_ *cobra.Command, args []string) error {
	return setProcessedTag(args, false)
}

var setProcessedTagMutationFunc = localmutate.SetProcessedTag

func setProcessedTag(args []string, processed bool) error {
	jobIDs, err := ParseJobIDsForJobCommand(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	verb := "processed"
	if !processed {
		verb = "unprocessed"
	}

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := setProcessedTagSingleWriter(database, jobID, processed); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		fmt.Printf("Job %s marked as %s\n", ids.FormatJobID(jobID), verb)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func setProcessedTagSingleWriter(database *sql.DB, jobID int64, processed bool) error {
	return setProcessedTagMutationFunc(context.Background(), database, jobID, processed)
}
