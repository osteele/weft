package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
		fmt.Printf("ID %d: %s on %s\n", job.ID, job.SessionName, job.Host)
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

	fmt.Printf("Deleted pending job %d (%s on %s)\n", id, job.SessionName, job.Host)
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
		fmt.Printf("Retrying job %d (%s on %s)...\n", job.ID, job.SessionName, job.Host)
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

	// Check if session already exists
	exists, err := ssh.TmuxSessionExists(host, job.SessionName)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	if exists {
		return fmt.Errorf("session '%s' already exists on %s", job.SessionName, host)
	}

	// Delete the pending entry
	if err := db.DeletePending(database, job.ID); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}

	// Create file paths
	logFile := session.LogFile(job.SessionName)
	statusFile := session.StatusFile(job.SessionName)
	metadataFile := session.MetadataFile(job.SessionName)

	// Archive any existing log file
	archiveCmd := fmt.Sprintf("if [ -f '%s' ]; then mv '%s' '%s.$(date +%%Y%%m%%d-%%H%%M%%S).log'; fi",
		logFile, logFile, strings.TrimSuffix(logFile, ".log"))
	ssh.RunWithRetry(host, archiveCmd)

	// Remove old status file
	ssh.RunWithRetry(host, fmt.Sprintf("rm -f '%s'", statusFile))

	// Save metadata
	startTime := time.Now().Unix()
	metadata := session.FormatMetadata(job.WorkingDir, job.Command, host, job.Description, startTime)

	metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	ssh.RunWithRetry(host, metadataCmd)

	// Create the wrapped command
	wrappedCommand := fmt.Sprintf(
		"cd '%s' && %s 2>&1 | tee '%s'; EXIT_CODE=$?; echo $EXIT_CODE > '%s'",
		job.WorkingDir, job.Command, logFile, statusFile)

	// Start tmux session
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", job.SessionName, wrappedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return fmt.Errorf("start session: %s", stderr)
	}

	fmt.Printf("✓ Session '%s' started on %s\n", job.SessionName, host)

	// Record in database
	jobID, err := db.RecordStart(database, host, job.SessionName, job.WorkingDir, job.Command, startTime, job.Description)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record job: %v\n", err)
	} else {
		fmt.Printf("Job ID: %d\n", jobID)
	}

	return nil
}
