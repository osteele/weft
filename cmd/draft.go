package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/spf13/cobra"
)

var jobDraftCmd = &cobra.Command{
	Use:   "draft <job-id>",
	Short: "Move a job into draft status and stop any remote execution",
	Long: `Mark a job as draft so it won't run on remote hosts.

For running jobs, this kills the remote process. For queued jobs, the entry
is removed from the remote queue. If the host is unreachable, the draft
status will be reconciled during the next sync.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobDraft,
}

func runJobDraft(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || jobID <= 0 {
		return usageErrorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	result, err := ops.DraftJob(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}

	if result.Deferred {
		fmt.Printf("Job %d draft pending (host unavailable)\n", jobID)
	} else {
		fmt.Printf("Job %d marked as draft\n", jobID)
	}
	return nil
}
