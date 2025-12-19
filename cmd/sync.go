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

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync job statuses from all remote hosts",
	Long: `Sync job statuses by checking all hosts with running jobs.

Automatically finds hosts with running jobs and updates their status
in the local database. Connection failures are silently ignored.

Examples:
  remote-jobs sync              # Sync all hosts
  remote-jobs sync --verbose    # Show progress`,
	RunE: runSync,
}

var syncVerbose bool

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all unique hosts with running or queued jobs
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	if len(hosts) == 0 {
		fmt.Println("No active jobs to sync")
		return nil
	}

	var totalUpdated, hostsReached, hostsUnreachable int

	for _, host := range hosts {
		if syncVerbose {
			fmt.Printf("Checking %s...\n", host)
		}

		updated, err := syncHost(database, host)
		if err != nil {
			// Check if it's a connection error
			if ssh.IsConnectionError(err.Error()) {
				hostsUnreachable++
				if syncVerbose {
					fmt.Printf("  %s: unreachable\n", host)
				}
				continue
			}
			// Non-connection error - log warning but continue
			fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			continue
		}

		hostsReached++
		totalUpdated += updated
		if syncVerbose && updated > 0 {
			fmt.Printf("  %s: %d job(s) updated\n", host, updated)
		}
	}

	// Print summary
	if hostsUnreachable > 0 {
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) unreachable)\n",
			totalUpdated, hostsReached, hostsUnreachable)
	} else {
		fmt.Printf("Synced %d job(s) on %d host(s)\n", totalUpdated, hostsReached)
	}

	return nil
}

// syncHost syncs all active jobs (running and queued) for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (int, error) {
	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	var updated int
	for _, job := range jobs {
		changed, err := syncJob(database, job)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	return updated, nil
}

// syncJob checks and updates a single job's status, returning true if status changed
func syncJob(database *sql.DB, job *db.Job) (bool, error) {
	// Jobs without a session name were started by the queue runner
	// They don't have individual tmux sessions, so use pattern-based file lookup
	if job.SessionName == "" {
		return syncQueueRunnerJob(database, job)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		return false, err
	}

	if exists {
		// Job still running, no change
		return false, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		// Job completed
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// No status file - job died unexpectedly
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// syncQueueRunnerJob checks and updates a queue runner job's status using pattern-based file lookup
func syncQueueRunnerJob(database *sql.DB, job *db.Job) (bool, error) {
	const timeout = 5 * time.Second

	// Check if status file exists (job completed) using glob pattern
	// Queue runner creates files with its own timestamp, not the database start_time
	statusPattern := session.StatusFilePattern(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(stdout) != "" {
		// Job completed - read exit code
		exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Check if job is in queue's .current file (actively running right now)
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	// Use || true to avoid exit code 1 when file doesn't exist
	currentCmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, currentCmd, timeout)
	if err != nil {
		return false, err
	}

	currentJobID := strings.TrimSpace(stdout)
	if currentJobID == fmt.Sprintf("%d", job.ID) {
		// Job is currently running
		return false, nil
	}

	// Check if job is still in the queue file (waiting to run)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	grepCmd := fmt.Sprintf("grep -q '^%d	' %s 2>/dev/null && echo yes || echo no", job.ID, queueFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, grepCmd, timeout)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(stdout) == "yes" {
		// Job is still in queue, waiting to run
		return false, nil
	}

	// Check if the job's process is still running (via PID file)
	pidPattern := session.PidFilePattern(job.ID)
	pidCmd := fmt.Sprintf("pid=$(cat %s 2>/dev/null); [ -n \"$pid\" ] && ps -p $pid > /dev/null 2>&1 && echo running || echo not_running", pidPattern)
	stdout, _, err = ssh.RunWithTimeout(job.Host, pidCmd, timeout)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(stdout) == "running" {
		// Process is still running, don't mark as dead
		return false, nil
	}

	// Job is not current, not in queue, process not running, and has no status file - it's dead
	// (Either it died mid-execution, or was removed from queue)
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}
