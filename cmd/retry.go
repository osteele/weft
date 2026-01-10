package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/spf13/cobra"
)

var retryCmd = &cobra.Command{
	Use:   "retry <job-id>...",
	Short: "Clone a job and queue it again",
	Long: `Retry a job by cloning its host, directory, command, description, and environment variables.

The retried job receives a new ID, enters the queue state, and does not inherit any of the original job's dependencies.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runRetry,
}

var jobRetryCmd = &cobra.Command{
	Use:   "retry <job-id>...",
	Short: "Alias for 'remote-jobs retry'",
	Long:  "Alias for 'remote-jobs retry'.",
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runRetry,
}

func init() {
	rootCmd.AddCommand(retryCmd)
	jobCmd.AddCommand(jobRetryCmd)
}

func runRetry(cmd *cobra.Command, args []string) error {
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
		if err := retryJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}

func retryJob(database *sql.DB, originalID int64) error {
	job, err := db.GetJobByID(database, originalID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job not found")
	}
	if job.Host == "" {
		return fmt.Errorf("job %d missing host", originalID)
	}
	if job.Command == "" {
		return fmt.Errorf("job %d missing command", originalID)
	}

	queue := job.QueueName
	if queue == "" {
		queue = defaultQueueName
	}

	workingDir := job.WorkingDir
	if workingDir == "" {
		if dir := job.EffectiveWorkingDir(); dir != "" {
			workingDir = dir
		} else {
			workingDir = "~"
		}
	}

	result, err := queueJob(database, queueJobOptions{
		Host:        job.Host,
		WorkingDir:  workingDir,
		Command:     job.Command,
		Description: job.Description,
		EnvVars:     job.EnvVars,
		Tags:        job.Tags,
		GPU:         job.GPU,
		QueueName:   queue,
		AutoStart:   true,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Retried job %d as %d on %s (queue '%s')\n", originalID, result.JobID, job.Host, queue)
	if len(job.EnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(job.EnvVars, ", "))
	}
	if result.Deferred {
		fmt.Printf("Host offline; job will sync once reachable.\n")
	}
	return rewireDependentJobs(database, originalID, result.JobID)
}

func rewireDependentJobs(database *sql.DB, oldID, newID int64) error {
	jobs, err := db.ListQueuedJobsWithDependency(database, oldID)
	if err != nil {
		return fmt.Errorf("list dependent jobs: %w", err)
	}
	for _, depJob := range jobs {
		newSpec, changed := db.ReplaceDepSpecID(depJob.DepSpec, oldID, newID)
		if !changed {
			continue
		}
		if err := db.SetJobDepSpec(database, depJob.ID, newSpec); err != nil {
			return fmt.Errorf("update job %d dependencies: %w", depJob.ID, err)
		}
		depJob.DepSpec = newSpec
		if err := ops.UpdateQueuedJobEntry(depJob, depJob.QueueName, depJob.EnvVars, newSpec); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update queue entry for job %d: %v\n", depJob.ID, err)
			continue
		}
		fmt.Printf("Updated job %d to depend on retried job %d\n", depJob.ID, newID)
	}
	return nil
}
