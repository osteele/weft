package cmd

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:     "restart <job-id>...",
	Aliases: []string{"retry"},
	Short:   "Restart a killed, dead, failed, canceled, or completed job",
	Long: `Restart a job by requeuing it with the same ID.

The previous run is archived and the job is reset to queued status.

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

	// Sync non-terminal jobs before restarting to get latest cloud status
	var jobsToSync []*db.Job
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			continue
		}
		jobsToSync = append(jobsToSync, job)
	}
	if len(jobsToSync) > 0 {
		quickSyncJobs(database, jobsToSync, FastSyncTimeout, FastCloudSyncTimeout)
	}

	var errors []string
	for _, jobID := range jobIDs {
		if err := restartJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
		}
	}

	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Fprintln(os.Stderr, e)
		}
		return fmt.Errorf("%d job(s) could not be restarted", len(errors))
	}
	return nil
}

func restartJob(database *sql.DB, jobID int64) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return db.ErrJobNotFound
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

	// Remove processed tag so the retried job appears in unprocessed listings
	if job.HasTag(db.ProcessedTag) {
		if err := db.RemoveJobTag(database, jobID, db.ProcessedTag); err != nil {
			return fmt.Errorf("remove processed tag: %w", err)
		}
	}

	// Cloud jobs: reset to unplaced (the original instance is gone)
	if job.IsLaunchJob() {
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

	if !job.HasInventoryHost() {
		return fmt.Errorf("job missing host")
	}

	// Requeue with same ID (archives the previous run)
	if !requeueableStatuses[effectiveStatus] {
		return fmt.Errorf("cannot retry job with status '%s'; only killed/dead/failed/canceled/completed jobs can be retried", effectiveStatus)
	}

	oldStatus := job.Status

	cfg, relayClient, err := loadCoordinatorRelay()
	if err != nil {
		return err
	}
	if relayEnabled(cfg, relayClient) {
		// Refresh here since the relay path bypasses ops.RequeueJob (which does its own refresh).
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			slog.Warn("failed to refresh metadata", "error", err)
		}
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
