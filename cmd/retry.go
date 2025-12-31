package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var retryCmd = &cobra.Command{
	Use:   "retry [job-id]",
	Short: "Retry draft jobs",
	Long: `Retry draft jobs that couldn't start (e.g., due to connection failures).

Examples:
  remote-jobs retry --list               # List draft jobs
  remote-jobs retry 42                   # Retry job #42
  remote-jobs retry 42 --host studio     # Retry on different host
  remote-jobs retry --all                # Retry all draft jobs
  remote-jobs retry --all --host cool30  # Retry draft jobs for cool30
  remote-jobs retry --delete 42          # Remove draft job`,
	RunE: runRetry,
}

var (
	retryList   bool
	retryAll    bool
	retryHost   string
	retryDelete int64
)

func init() {
	rootCmd.AddCommand(retryCmd)

	retryCmd.Flags().BoolVar(&retryList, "list", false, "List draft jobs")
	retryCmd.Flags().BoolVar(&retryAll, "all", false, "Retry all draft jobs")
	retryCmd.Flags().StringVar(&retryHost, "host", "", "Filter by host or override host for retry")
	retryCmd.Flags().Int64Var(&retryDelete, "delete", 0, "Delete a draft job")
}

func runRetry(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Handle list mode
	if retryList {
		return listDraftJobs(database, retryHost)
	}

	// Handle delete mode
	if retryDelete > 0 {
		return deleteDraftJob(database, retryDelete)
	}

	// Handle all mode
	if retryAll {
		return retryAllDraft(database, retryHost)
	}

	// Handle single job retry
	if len(args) == 0 {
		return fmt.Errorf("job ID required (or use --list, --all, --delete)")
	}

	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	return retrySingleJob(database, jobID, retryHost)
}

func listDraftJobs(database *sql.DB, host string) error {
	jobs, err := db.ListDraft(database, host)
	if err != nil {
		return fmt.Errorf("list draft: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No draft jobs")
		return nil
	}

	fmt.Printf("Draft jobs:\n\n")
	for _, job := range jobs {
		fmt.Printf("ID %d on %s\n", job.ID, job.Host)
		fmt.Printf("  Command: %s\n", job.EffectiveCommand())
		fmt.Printf("  Directory: %s\n", job.EffectiveWorkingDir())
		if job.Description != "" {
			fmt.Printf("  Description: %s\n", job.Description)
		}
		fmt.Println()
	}

	return nil
}

func deleteDraftJob(database *sql.DB, id int64) error {
	job, err := db.GetDraftJob(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("draft job %d not found", id)
	}

	if err := db.DeleteDraft(database, id); err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	fmt.Printf("Deleted draft job %d on %s\n", id, job.Host)
	return nil
}

func retryAllDraft(database *sql.DB, host string) error {
	jobs, err := db.ListDraft(database, host)
	if err != nil {
		return fmt.Errorf("list draft: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No draft jobs to retry")
		return nil
	}

	var successes, failures int
	for _, job := range jobs {
		fmt.Printf("Retrying job %d on %s...\n", job.ID, job.Host)
		if err := startDraftJob(database, job, ""); err != nil {
			fmt.Fprintf(os.Stderr, "  Failed: %v\n", err)
			failures++
		} else {
			successes++
		}
	}

	fmt.Printf("\nCompleted: %d succeeded, %d failed\n", successes, failures)
	return nil
}

func retrySingleJob(database *sql.DB, id int64, overrideHost string) error {
	job, err := db.GetDraftJob(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("draft job %d not found", id)
	}

	return startDraftJob(database, job, overrideHost)
}

func startDraftJob(database *sql.DB, job *db.Job, overrideHost string) error {
	host := job.Host
	if overrideHost != "" {
		host = overrideHost
	}

	// Delete the draft entry
	if err := db.DeleteDraft(database, job.ID); err != nil {
		return fmt.Errorf("delete draft: %w", err)
	}

	// Create new job record to get ID
	newJobID, err := db.RecordJobStarting(database, host, job.WorkingDir, job.Command, job.Description)
	if err != nil {
		return fmt.Errorf("create job record: %w", err)
	}

	// Get the new job to access start time
	newJob, err := db.GetJobByID(database, newJobID)
	if err != nil || newJob == nil {
		return fmt.Errorf("get new job: %w", err)
	}

	// Generate file paths from job ID
	tmuxSession := session.TmuxSessionName(newJobID)
	logFile := session.LogFile(newJobID, newJob.StartTime)
	statusFile := session.StatusFile(newJobID, newJob.StartTime)
	metadataFile := session.MetadataFile(newJobID, newJob.StartTime)
	pidFile := session.PidFile(newJobID, newJob.StartTime)

	// Check if session already exists (shouldn't with new unique IDs)
	exists, err := ssh.TmuxSessionExists(host, tmuxSession)
	if err != nil {
		db.UpdateJobFailed(database, newJobID, err.Error())
		return fmt.Errorf("check session: %w", err)
	}
	if exists {
		db.UpdateJobFailed(database, newJobID, "session already exists")
		return fmt.Errorf("session '%s' already exists on %s", tmuxSession, host)
	}

	// Create log directory on remote
	mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
	if _, stderr, err := ssh.RunWithRetry(host, mkdirCmd); err != nil {
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, newJobID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	// Save metadata
	metadata := session.FormatMetadata(newJobID, job.WorkingDir, job.Command, host, job.Description, newJob.StartTime)
	// Don't quote path - it contains ~ which needs shell expansion
	metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	ssh.RunWithRetry(host, metadataCmd)

	// Create the wrapped command using the common builder (tested for tilde expansion)
	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      newJobID,
		WorkingDir: job.WorkingDir,
		Command:    job.Command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
	})

	// Escape single quotes for embedding in single-quoted string
	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

	// Start tmux session - use single quotes to prevent shell expansion
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, newJobID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	// Mark job as running
	if err := db.UpdateJobRunning(database, newJobID); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}

	fmt.Printf("✓ Job started on %s\n", host)
	fmt.Printf("Job ID: %d\n", newJobID)

	return nil
}
