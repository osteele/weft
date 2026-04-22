package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:     "cancel <wj-id>...",
	Aliases: []string{"remove"},
	Short:   "Cancel one or more jobs (queued or running)",
	Long: `Cancel jobs by removing them from the queue or killing them if running.

For queued jobs: removes from both the remote queue file and the local database.
For running jobs: kills the job process.

The top-level form requires wj-prefixed IDs. Use 'weft job cancel' to pass
bare numeric IDs.

Examples:
  weft cancel wj123
  weft cancel wj123 wj124 wj125`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runCancelExplicitPrefix,
}

func init() {
	rootCmd.AddCommand(cancelCmd)
}

func runCancelExplicitPrefix(cmd *cobra.Command, args []string) error {
	return runCancelWithParser(cmd, args, ParseJobIDsWithExplicitPrefix)
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

		result, err := orchestration.KillOrCancelJob(database, jobID, "canceled", ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		message := result.Message
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
