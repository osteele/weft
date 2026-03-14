package cmd

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:     "restart <job-id>...",
	Aliases: []string{"retry"},
	Short:   "Restart a killed, dead, failed, canceled, or completed job",
	Long: `Restart a job by re-running it.

For killed/dead/failed/canceled jobs, the job is requeued with its original ID.
For completed jobs, a new job is created with the same command and metadata.

Examples:
  weft restart 42
  weft retry 42
  weft restart 42 43 44`,
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

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	var errors []string
	for i, jobID := range jobIDs {
		if i > 0 {
			fmt.Println("---")
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
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued {
		return fmt.Errorf("job is already queued")
	}
	if effectiveStatus == db.StatusRunning || effectiveStatus == db.StatusStarting {
		return fmt.Errorf("job is currently %s; kill it first if you want to retry", effectiveStatus)
	}

	if job.Command == "" {
		return fmt.Errorf("job missing command")
	}

	// Cloud jobs: reset to unplaced (the original instance is gone)
	if job.IsCloudJob() {
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			return err
		}
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return err
		}
		fmt.Printf("Reset job %d to queued (cloud instance no longer available)\n", jobID)
		fmt.Printf("  Use 'weft campaign launch' to run on a new instance\n")
		return nil
	}

	if job.Host == "" {
		return fmt.Errorf("job missing host")
	}

	// Completed jobs: create a new job with the same metadata
	if effectiveStatus == db.StatusCompleted {
		cfg, relayClient, err := loadCoordinatorRelay()
		if err != nil {
			return err
		}
		if relayEnabled(cfg, relayClient) {
			params := ops.QueueJobParams{
				Host:        job.Host,
				WorkingDir:  job.WorkingDir,
				Command:     job.Command,
				Description: job.Description,
				Project:     job.Project,
				EnvVars:     job.EnvVars,
				Tags:        job.Tags,
				GPU:         job.GPU,
				GPUClass:    job.GPUClass,
				GPUMemGB:    job.GPUMemGB,
				DepSpec:     job.DepSpec,
				Inputs:      job.Inputs,
				Outputs:     job.Outputs,
				OutputDirs:  job.OutputDirs,
				Produces:    job.Produces,
				Needs:       job.Needs,
			}
			newJobID, ack, err := relaySubmitJob(database, cfg, relayClient, params)
			if err != nil {
				return err
			}
			fmt.Printf("Created new job %d from completed job %d via coordinator relay\n", newJobID, jobID)
			if ack != nil && ack.Message != "" {
				fmt.Printf("  relay: %s\n", ack.Message)
			}
			return nil
		}
		result, err := ops.RestartJob(database, ops.RestartJobParams{
			OriginalJob:  job,
			EnvVars:      job.EnvVars,
			Tags:         job.Tags,
			DepSpec:      job.DepSpec,
			CPUAllotment: job.CPUAllotment,
			Project:      job.Project,
		}, ops.DefaultOptions())
		if err != nil {
			return err
		}
		if result.Deferred {
			fmt.Printf("Job saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", job.Host)
		}
		fmt.Printf("Created new job %d from completed job %d on %s\n", result.JobID, jobID, job.Host)
		if job.Description != "" {
			fmt.Printf("  Description: %s\n", job.Description)
		}
		return nil
	}

	// Killed/dead/failed/canceled jobs: requeue with same ID
	if !requeueableStatuses[effectiveStatus] {
		return fmt.Errorf("cannot retry job with status '%s'; only killed/dead/failed/canceled/completed jobs can be retried", effectiveStatus)
	}

	oldStatus := job.Status

	cfg, relayClient, err := loadCoordinatorRelay()
	if err != nil {
		return err
	}
	if relayEnabled(cfg, relayClient) {
		if err := db.RequeueByID(database, jobID); err != nil {
			return fmt.Errorf("update status to queued: %w", err)
		}
		ack, err := relayRequeueJob(cfg, relayClient, job)
		if err != nil {
			return err
		}
		fmt.Printf("Restarted job %d via coordinator relay\n", jobID)
		fmt.Printf("  Status: %s → queued\n", oldStatus)
		if ack != nil && ack.Message != "" {
			fmt.Printf("  relay: %s\n", ack.Message)
		}
		return nil
	}

	result, err := ops.RequeueJob(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	if result.Deferred {
		fmt.Printf("Job saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", job.Host)
	}

	fmt.Printf("Restarted job %d on %s\n", jobID, job.Host)
	fmt.Printf("  Status: %s → queued\n", oldStatus)
	if job.Description != "" {
		fmt.Printf("  Description: %s\n", job.Description)
	}
	if len(job.EnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(job.EnvVars, ", "))
	}
	return nil
}
