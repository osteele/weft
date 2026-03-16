package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var resumeCmd = &cobra.Command{
	Use:   "resume <job-id>...",
	Short: "Resume one or more paused jobs",
	Long: `Resume paused jobs by their IDs (sends SIGCONT).

Examples:
  weft resume 42
  weft resume 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runResume,
}

func init() {
	resumeCmd.Deprecated = "use 'weft job resume' instead"
	rootCmd.AddCommand(resumeCmd)
}

func runResume(cmd *cobra.Command, args []string) error {
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
	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("resume"), oplog.WithJobID(jobID))

		if isCloud, err := isCloudJob(service.Database(), jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		} else if isCloud {
			errors = append(errors, fmt.Sprintf("job %d: resume is not supported for rental jobs", jobID))
			continue
		}

		result, err := service.ResumeJob(jobID, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		message := result.Outcome.Message
		if message == "" {
			message = fmt.Sprintf("Job %d resumed", jobID)
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
