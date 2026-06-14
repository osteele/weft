package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <job-id>...",
	Short: "Pause one or more running jobs",
	Long: `Pause running jobs by their IDs (sends SIGSTOP).

Examples:
  weft pause 42
  weft pause 42 43 44`,
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

	jobIDs, err := ParseJobIDsForJobCommand(args)
	if err != nil {
		return err
	}

	var errors []string
	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("pause"), oplog.WithJobID(jobID))

		if isCloud, err := isCloudJob(service.Database(), jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		} else if isCloud {
			errors = append(errors, fmt.Sprintf("job %s: pause is not supported for rental jobs", ids.FormatJobID(jobID)))
			continue
		}

		result, err := service.PauseJob(jobID, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		message := result.Outcome.Message
		if message == "" {
			message = fmt.Sprintf("Job %s paused", ids.FormatJobID(jobID))
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
