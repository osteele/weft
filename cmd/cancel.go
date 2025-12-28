package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
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

		// Determine queue name
		jobQueueName := job.QueueName
		if jobQueueName == "" {
			jobQueueName = "default"
		}

		// Remove from remote queue file
		queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", jobQueueName)
		removeCmd := fmt.Sprintf("grep -v '^%d\\t' %s > %s.tmp 2>/dev/null && mv %s.tmp %s || true",
			jobID, queueFile, queueFile, queueFile, queueFile)

		_, stderr, err := ssh.Run(job.Host, removeCmd)
		if err != nil {
			if ssh.IsConnectionError(stderr) {
				// Host unreachable - add deferred operation
				if err := db.AddDeferredOperation(database, job.Host, db.OpRemoveQueued, jobID, jobQueueName, ""); err != nil {
					errors = append(errors, fmt.Sprintf("job %d: failed to add deferred operation: %v", jobID, err))
					continue
				}
				if err := db.MarkDeadByID(database, jobID); err != nil {
					errors = append(errors, fmt.Sprintf("job %d: failed to mark as dead: %v", jobID, err))
					continue
				}
				fmt.Printf("Job %d: host %s unreachable, will cancel on next sync\n", jobID, job.Host)
				cancelled++
				continue
			}
			errors = append(errors, fmt.Sprintf("job %d: failed to remove from remote queue: %s", jobID, strings.TrimSpace(stderr)))
			continue
		}

		// Delete from local database
		if err := db.DeleteJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: delete failed: %v", jobID, err))
			continue
		}

		fmt.Printf("Job %d cancelled\n", jobID)
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
