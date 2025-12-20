package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running jobs",
	Long: `Kill running jobs by their IDs.

Examples:
  remote-jobs kill 42
  remote-jobs kill 42 43 44`,
	Args: cobra.MinimumNArgs(1),
	RunE: runKill,
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errors []string
	for _, arg := range args {
		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}

		if err := killJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}

func killJob(database *sql.DB, jobID int64) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("not found")
	}

	// Handle queued jobs: remove from queue file
	if job.Status == db.StatusQueued {
		return removeQueuedJob(database, job)
	}

	// Handle running/starting jobs: kill tmux session
	if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
		return killRunningJob(database, job)
	}

	// Job already terminated
	return fmt.Errorf("job already %s", job.Status)
}

func removeQueuedJob(database *sql.DB, job *db.Job) error {
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	fmt.Printf("Removing queued job %d from %s on %s...\n", job.ID, queueName, job.Host)

	// Try to remove from queue file
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", job.ID, queueFile)
	_, stderr, err := ssh.Run(job.Host, removeCmd)

	if err != nil && ssh.IsConnectionError(stderr) {
		// Host unreachable - add deferred operation
		fmt.Printf("Host %s unreachable, will remove on next sync\n", job.Host)
		if err := db.AddDeferredOperation(database, job.Host, db.OpRemoveQueued, job.ID, queueName); err != nil {
			return fmt.Errorf("add deferred operation: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("remove from queue file: %s", strings.TrimSpace(stderr))
	}

	// Mark job as dead in database
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	fmt.Printf("Job %d removed from queue\n", job.ID)
	return nil
}

func killRunningJob(database *sql.DB, job *db.Job) error {
	fmt.Printf("Killing job %d on %s...\n", job.ID, job.Host)

	// Queue-runner jobs (SessionName == "") don't have individual tmux sessions
	// They run under the queue runner's session, so we need to kill the PID directly
	if job.SessionName == "" {
		return killQueueRunnerJob(database, job)
	}

	// Regular jobs have their own tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	if err := ssh.TmuxKillSession(job.Host, tmuxSession); err != nil {
		// Check if connection error
		if ssh.IsConnectionError(err.Error()) {
			// Host unreachable - add deferred operation
			fmt.Printf("Host %s unreachable, will kill on next sync\n", job.Host)
			if err := db.AddDeferredOperation(database, job.Host, db.OpKillJob, job.ID, ""); err != nil {
				return fmt.Errorf("add deferred operation: %w", err)
			}
			// Mark job as dead in database anyway
			if err := db.MarkDeadByID(database, job.ID); err != nil {
				fmt.Printf("Warning: failed to update database: %v\n", err)
			}
			fmt.Printf("Job %d marked for kill on next sync\n", job.ID)
			return nil
		}
		return fmt.Errorf("kill session: %v", err)
	}

	// Mark job as dead in database
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		fmt.Printf("Warning: failed to update database: %v\n", err)
	}

	fmt.Printf("Job %d killed\n", job.ID)
	return nil
}

func killQueueRunnerJob(database *sql.DB, job *db.Job) error {
	// Find and kill the PID for this queue-runner job
	pidPattern := session.PidFilePattern(job.ID)

	// Try to read PID and kill the process
	killCmd := fmt.Sprintf(`
		pid=$(cat %s 2>/dev/null | head -1)
		if [ -n "$pid" ] && kill -0 $pid 2>/dev/null; then
			kill $pid 2>/dev/null && echo "killed" || echo "failed"
		else
			echo "not_running"
		fi
	`, pidPattern)

	stdout, stderr, err := ssh.Run(job.Host, killCmd)

	if err != nil && ssh.IsConnectionError(stderr) {
		// Host unreachable - add deferred operation
		fmt.Printf("Host %s unreachable, will kill on next sync\n", job.Host)
		if err := db.AddDeferredOperation(database, job.Host, db.OpKillJob, job.ID, ""); err != nil {
			return fmt.Errorf("add deferred operation: %w", err)
		}
		// Mark job as dead in database anyway
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			fmt.Printf("Warning: failed to update database: %v\n", err)
		}
		fmt.Printf("Job %d marked for kill on next sync\n", job.ID)
		return nil
	} else if err != nil {
		return fmt.Errorf("kill process: %s", strings.TrimSpace(stderr))
	}

	result := strings.TrimSpace(stdout)
	if result == "not_running" {
		fmt.Printf("Job %d is not running (already finished)\n", job.ID)
	} else if result == "killed" {
		fmt.Printf("Job %d killed\n", job.ID)
	} else {
		fmt.Printf("Warning: unexpected result: %s\n", result)
	}

	// Mark job as dead in database
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		fmt.Printf("Warning: failed to update database: %v\n", err)
	}

	return nil
}
