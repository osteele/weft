package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <job-id>...",
	Short: "Move one or more jobs to draft state",
	Long: `Move jobs to draft state so they will not be placed or run.

Examples:
  weft pause wj42
  weft pause wj42 wj43 wj44
  weft pause job wj42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runPause,
}

var pauseJobCmd = &cobra.Command{
	Use:   "job <job-id>...",
	Short: pauseCmd.Short,
	Long:  pauseCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runPause,
}

var unpauseCmd = &cobra.Command{
	Use:   "unpause <job-id>...",
	Short: "Queue one or more draft jobs",
	Long: `Move draft jobs back to queued state so they can be placed or run.

Examples:
  weft unpause wj42
  weft unpause job wj42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runUnpause,
}

var unpauseJobCmd = &cobra.Command{
	Use:   "job <job-id>...",
	Short: unpauseCmd.Short,
	Long:  unpauseCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runUnpause,
}

func init() {
	rootCmd.AddCommand(pauseCmd)
	pauseCmd.AddCommand(pauseJobCmd)
	rootCmd.AddCommand(unpauseCmd)
	unpauseCmd.AddCommand(unpauseJobCmd)
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

		result, err := service.DraftJob(jobID, ops.TimeoutNormal)
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

func runUnpause(cmd *cobra.Command, args []string) error {
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
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("unpause"), oplog.WithJobID(jobID))

		job, err := service.Job(jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errors = append(errors, fmt.Sprintf("job %s: not found", ids.FormatJobID(jobID)))
			continue
		}
		if job.EffectiveStatus() != db.StatusDraft {
			fmt.Printf("Job %s is %s; not draft\n", ids.FormatJobID(jobID), job.EffectiveStatus())
			continue
		}

		result, err := service.RequestStatus(jobID, db.StatusQueued, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		message := result.Outcome.Message
		if message == "" {
			message = fmt.Sprintf("Job %s queued", ids.FormatJobID(jobID))
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
