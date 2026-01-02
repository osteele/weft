package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
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
	Args: usageArgs(cobra.MinimumNArgs(1)),
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
	oplog.Log(oplog.OpCLICommand, oplog.WithDetail("kill"), oplog.WithJobID(jobID))

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
		fmt.Printf("Host %s unreachable, will remove on next sync\n", job.Host)
		if err := db.SetPendingStatus(database, job.ID, db.StatusDead); err != nil {
			return fmt.Errorf("mark pending: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("remove from queue file: %s", strings.TrimSpace(stderr))
	}

	if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDead); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	fmt.Printf("Job %d removed from queue\n", job.ID)
	return nil
}

func killRunningJob(database *sql.DB, job *db.Job) error {
	fmt.Printf("Killing job %d on %s...\n", job.ID, job.Host)

	// Use unified ops package for killing jobs
	result, err := ops.KillJob(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}

	if result.Deferred {
		fmt.Printf("Host %s unreachable, will kill on next sync\n", job.Host)
		fmt.Printf("Job %d marked for kill on next sync\n", job.ID)
	} else {
		fmt.Println(result.Message)
	}

	return nil
}
