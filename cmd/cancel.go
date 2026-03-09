package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:     "cancel <job-id>...",
	Aliases: []string{"remove"},
	Short:   "Cancel one or more jobs (queued or running)",
	Long: `Cancel jobs by removing them from the queue or killing them if running.

For queued jobs: removes from both the remote queue file and the local database.
For running jobs: kills the job process.

Examples:
  weft cancel 123
  weft cancel 123 124 125`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runCancel,
}

func init() {
	rootCmd.AddCommand(cancelCmd)
}

func runCancel(cmd *cobra.Command, args []string) error {
	service, err := core.NewService()
	if err != nil {
		return fmt.Errorf("initialize core service: %w", err)
	}
	defer service.Close()

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	var errors []string
	var cancelled int

	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("cancel"), oplog.WithJobID(jobID))

		if msg, err := killOrCancelCloudJob(service.Database(), jobID, db.StatusCanceled); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		} else if msg != "" {
			fmt.Println(msg)
			cancelled++
			continue
		}

		result, err := service.KillJob(jobID, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		message := result.Outcome.Message
		if message == "" {
			message = fmt.Sprintf("Job %d canceled", jobID)
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
