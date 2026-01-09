package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:     "cancel <job-id>...",
	Aliases: []string{"remove"},
	Short:   "Cancel one or more queued jobs",
	Long: `Cancel queued jobs before they start.

This removes jobs from both the remote queue file and the local database.
Only works for jobs that haven't started yet (status: queued).

For running jobs, use 'remote-jobs kill' instead.

Examples:
  remote-jobs cancel 123
  remote-jobs cancel 123 124 125`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runCancel,
}

func init() {
	rootCmd.AddCommand(cancelCmd)
}

func runCancel(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errors []string
	var cancelled int

	for _, arg := range args {
		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d not found", jobID))
			continue
		}

		if job.Status != db.StatusQueued {
			errors = append(errors, fmt.Sprintf("job %d has status '%s', can only cancel queued jobs (use 'kill' for running jobs)", jobID, job.Status))
			continue
		}

		result, err := ops.CancelQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutNormal))
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		if result.Deferred {
			fmt.Printf("Job %d: cancel pending (host unreachable)\n", jobID)
		} else {
			fmt.Printf("Job %d canceled\n", jobID)
		}
		cancelled++
	}

	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Printf("Error: %s\n", e)
		}
		if cancelled > 0 {
			fmt.Printf("\n%d job(s) cancelled, %d error(s)\n", cancelled, len(errors))
		}
		return fmt.Errorf("%d error(s)", len(errors))
	}

	return nil
}
