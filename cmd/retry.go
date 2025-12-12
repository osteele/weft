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
	Short: "Retry pending jobs",
	Long: `Retry pending jobs that couldn't start (e.g., due to connection failures).

Examples:
  remote-jobs retry --list               # List pending jobs
  remote-jobs retry 42                   # Retry job #42
  remote-jobs retry 42 --host studio     # Retry on different host
  remote-jobs retry --all                # Retry all pending jobs
  remote-jobs retry --all --host cool30  # Retry pending jobs for cool30
  remote-jobs retry --delete 42          # Remove pending job`,
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

	retryCmd.Flags().BoolVar(&retryList, "list", false, "List pending jobs")
	retryCmd.Flags().BoolVar(&retryAll, "all", false, "Retry all pending jobs")
	retryCmd.Flags().StringVar(&retryHost, "host", "", "Filter by host or override host for retry")
	retryCmd.Flags().Int64Var(&retryDelete, "delete", 0, "Delete a pending job")
}

func runRetry(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Handle list mode
	if retryList {
		return listPendingJobs(database, retryHost)
	}

	// Handle delete mode
	if retryDelete > 0 {
		return deletePendingJob(database, retryDelete)
	}

	// Handle all mode
	if retryAll {
		return retryAllPending(database, retryHost)
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

func listPendingJobs(database *sql.DB, host string) error {
	jobs, err := db.ListPending(database, host)
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No pending jobs")
		return nil
	}

	fmt.Printf("Pending jobs:\n\n")
	for _, job := range jobs {
		fmt.Printf("ID %d on %s\n", job.ID, job.Host)
		fmt.Printf("  Command: %s\n", job.Command)
		fmt.Printf("  Directory: %s\n", job.WorkingDir)
		if job.Description != "" {
			fmt.Printf("  Description: %s\n", job.Description)
		}
		fmt.Println()
	}

	return nil
}

func deletePendingJob(database *sql.DB, id int64) error {
	job, err := db.GetPendingJob(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("pending job %d not found", id)
	}

	if err := db.DeletePending(database, id); err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	fmt.Printf("Deleted pending job %d on %s\n", id, job.Host)
	return nil
}

func retryAllPending(database *sql.DB, host string) error {
	jobs, err := db.ListPending(database, host)
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No pending jobs to retry")
		return nil
	}

	var successes, failures int
	for _, job := range jobs {
		fmt.Printf("Retrying job %d on %s...\n", job.ID, job.Host)
		if err := startPendingJob(database, job, ""); err != nil {
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
	job, err := db.GetPendingJob(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("pending job %d not found", id)
	}

	return startPendingJob(database, job, overrideHost)
}

func startPendingJob(database *sql.DB, job *db.Job, overrideHost string) error {
	host := job.Host
	if overrideHost != "" {
		host = overrideHost
	}

	// Delete the pending entry
	if err := db.DeletePending(database, job.ID); err != nil {
		return fmt.Errorf("delete pending: %w", err)
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
		db.UpdateJobFailed(database, newJobID, fmt.Sprintf("create log dir: %s", stderr))
		return fmt.Errorf("create log directory: %s", stderr)
	}

	// Save metadata
	metadata := session.FormatMetadata(newJobID, job.WorkingDir, job.Command, host, job.Description, newJob.StartTime)
	metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	ssh.RunWithRetry(host, metadataCmd)

	// Create the wrapped command with better error capture
	wrappedCommand := fmt.Sprintf(
		`echo "=== START $(date) ===" > '%s'; `+
			`echo "job_id: %d" >> '%s'; `+
			`echo "cd: %s" >> '%s'; `+
			`echo "cmd: %s" >> '%s'; `+
			`echo "===" >> '%s'; `+
			`cd '%s' && (%s) 2>&1 | tee -a '%s'; `+
			`EXIT_CODE=\${PIPESTATUS[0]}; `+
			`echo "=== END exit=\$EXIT_CODE $(date) ===" >> '%s'; `+
			`echo \$EXIT_CODE > '%s'`,
		logFile,
		newJobID, logFile,
		job.WorkingDir, logFile,
		job.Command, logFile,
		logFile,
		job.WorkingDir, job.Command, logFile,
		logFile,
		statusFile)

	// Start tmux session
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", tmuxSession, wrappedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		db.UpdateJobFailed(database, newJobID, fmt.Sprintf("start tmux: %s", stderr))
		return fmt.Errorf("start session: %s", stderr)
	}

	// Mark job as running
	if err := db.UpdateJobRunning(database, newJobID); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}

	fmt.Printf("✓ Job started on %s\n", host)
	fmt.Printf("Job ID: %d\n", newJobID)

	return nil
}
