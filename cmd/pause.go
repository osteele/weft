package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/core"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/spf13/cobra"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <job-id>...",
	Short: "Pause one or more running jobs",
	Long: `Pause running jobs by their IDs (sends SIGSTOP).

Examples:
  remote-jobs pause 42
  remote-jobs pause 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runPause,
}

func init() {
	rootCmd.AddCommand(pauseCmd)
}

func runPause(cmd *cobra.Command, args []string) error {
	service, err := core.NewService()
	if err != nil {
		return fmt.Errorf("initialize core service: %w", err)
	}
	defer service.Close()

	var errors []string
	for _, arg := range args {
		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}

		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("pause"), oplog.WithJobID(jobID))

		result, err := service.PauseJob(jobID, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		message := result.Outcome.Message
		if message == "" {
			message = fmt.Sprintf("Job %d paused", jobID)
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
