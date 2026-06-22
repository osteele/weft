package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:     "cancel <job-id>...",
	Aliases: []string{"remove"},
	Short:   "Cancel one or more jobs (queued or running)",
	Long: `Cancel jobs by removing them from the queue or killing them if running.

For queued jobs: removes from both the remote queue file and the local database.
For running jobs: kills the job process.

Accepts bare numeric IDs (123) or wj-prefixed IDs (wj123). Instance IDs (wi...)
are rejected, since cancel operates only on jobs.

Examples:
  weft cancel 123
  weft cancel wj123 wj124 125`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runCancelExplicitPrefix,
}

func init() {
	rootCmd.AddCommand(cancelCmd)
}

func runCancelExplicitPrefix(cmd *cobra.Command, args []string) error {
	return runCancelWithParser(cmd, args, ParseJobIDsForJobCommand)
}

func runCancel(cmd *cobra.Command, args []string) error {
	return runCancelWithParser(cmd, args, ParseJobIDs)
}

func runCancelWithParser(cmd *cobra.Command, args []string, parser func([]string) ([]int64, error)) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobIDs, err := parser(args)
	if err != nil {
		return err
	}

	var errors []string
	var cancelled int

	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("cancel"), oplog.WithJobID(jobID))

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if handled, err := cancelSkyJob(cmd, database, job); handled {
			if err != nil {
				errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
				continue
			}
			cancelled++
			continue
		}

		result, err := orchestration.KillOrCancelJob(database, jobID, "canceled", ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		message := result.Message
		if message == "" {
			message = fmt.Sprintf("Job %s canceled", ids.FormatJobID(jobID))
		}
		if verified, status, verifyErr := cancelDurablyVisible(database, jobID); verifyErr == nil && !verified {
			message = fmt.Sprintf("Cancel requested for job %s (current status: %s; waiting for sync confirmation)", ids.FormatJobID(jobID), status)
		}
		fmt.Println(message)
		cancelled++
	}

	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Printf("Error: %s\n", e)
		}
		if cancelled > 0 {
			fmt.Printf("\n%d job(s) canceled, %d error(s)\n", cancelled, len(errors))
		}
		return fmt.Errorf("%d error(s)", len(errors))
	}

	return nil
}

func cancelDurablyVisible(database *sql.DB, jobID int64) (bool, string, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return false, "", err
	}
	if job == nil {
		return false, "", nil
	}
	status := job.EffectiveStatus()
	return status == db.StatusCanceled, status, nil
}
