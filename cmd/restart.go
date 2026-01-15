package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <job-id>...",
	Short: "Requeue a killed, dead, failed, or canceled job",
	Long: `Restart a job by changing its status back to queued.

The job keeps its original ID and all metadata. Only jobs with status
killed, dead, failed, or canceled can be restarted.

Examples:
  remote-jobs restart 42
  remote-jobs restart 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errors []string
	for i, arg := range args {
		if i > 0 {
			fmt.Println("---")
		}
		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}
		if err := restartJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}

func restartJob(database *sql.DB, jobID int64) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job not found")
	}

	// Validate job can be retried
	if !requeueableStatuses[job.Status] {
		if job.Status == db.StatusQueued {
			return fmt.Errorf("job is already queued")
		}
		if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
			return fmt.Errorf("job is currently %s; kill it first if you want to retry", job.Status)
		}
		return fmt.Errorf("cannot retry job with status '%s'; only killed/dead/failed/canceled jobs can be retried", job.Status)
	}

	if job.Host == "" {
		return fmt.Errorf("job missing host")
	}
	if job.Command == "" {
		return fmt.Errorf("job missing command")
	}

	oldStatus := job.Status
	queueName := defaultQueueName

	// Change status to queued
	if err := db.MarkQueuedByID(database, jobID); err != nil {
		return fmt.Errorf("update status to queued: %w", err)
	}

	// Push to remote queue
	entry := ops.QueueEntry{
		JobID:       job.ID,
		WorkingDir:  job.EffectiveWorkingDir(),
		Command:     job.Command,
		Description: job.Description,
		EnvVars:     job.EnvVars,
		DepSpec:     job.DepSpec,
	}
	deferred := false
	if err := ops.AppendQueueEntry(job.Host, queueName, entry, ops.AppendQueueEntryOptions{}); err != nil {
		// Best effort - job is queued locally, sync will eventually push it
		fmt.Printf("Note: could not immediately push to remote queue (will sync later): %v\n", err)
		deferred = true
	}

	fmt.Printf("Restarted job %d on %s\n", jobID, job.Host)
	fmt.Printf("  Status: %s → queued\n", oldStatus)
	if job.Description != "" {
		fmt.Printf("  Description: %s\n", job.Description)
	}
	if len(job.EnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(job.EnvVars, ", "))
	}
	if deferred {
		fmt.Printf("Host offline; job will sync once reachable.\n")
	}
	return nil
}
