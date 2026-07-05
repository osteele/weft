package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var jobDraftCmd = &cobra.Command{
	Use:   "draft <job-id>...",
	Short: "Toggle a job between draft and queued status",
	Long: `Mark a job as draft so it won't run on remote hosts, or queue an
already-draft job so it can run.

For running jobs, this kills the remote process. For queued jobs, the entry
is removed from the remote queue. If the host is unreachable, the draft
status will be reconciled during the next sync.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobDraft,
}

func runJobDraft(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	service, err := core.NewService()
	if err != nil {
		return fmt.Errorf("initialize core service: %w", err)
	}
	defer service.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		job, err := service.Job(jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: not found", ids.FormatJobID(jobID)))
			continue
		}
		var result core.OperationResult
		if job.EffectiveStatus() == db.StatusDraft {
			result, err = service.RequestStatus(jobID, db.StatusQueued, ops.TimeoutNormal)
		} else {
			result, err = service.DraftJob(jobID, ops.TimeoutNormal)
		}
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if result.Outcome.Message != "" {
			fmt.Println(result.Outcome.Message)
			continue
		}
		fmt.Printf("Job %s marked as draft\n", ids.FormatJobID(jobID))
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}
