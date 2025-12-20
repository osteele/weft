package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage remote jobs",
	Long: `Manage remote jobs including running, monitoring, and controlling them.

All job-related operations are available under this subcommand. Common
operations (run, log, kill) also have top-level shortcuts.

Available subcommands:
  run       Start a new job on a remote host
  log       View job log output
  kill      Kill a running job
  status    Check status of one or more jobs
  describe  Set or update job description
  restart   Restart a job using saved metadata
  list      List and search job history
  move      Move a queued job to a different host`,
}

// Job run subcommand - delegates to main run command
var jobRunCmd = &cobra.Command{
	Use:   "run <host> <command>",
	Short: "Start a new job on a remote host",
	Long:  runCmd.Long,
	Args:  cobra.MinimumNArgs(2),
	RunE:  runRun,
}

// Job log subcommand - delegates to main log command
var jobLogCmd = &cobra.Command{
	Use:     "log <job-id>",
	Aliases: []string{"logs"},
	Short:   "View log output from a remote job",
	Long:    logCmd.Long,
	Args:    cobra.ExactArgs(1),
	RunE:    runLog,
}

// Job kill subcommand - delegates to main kill command
var jobKillCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running jobs",
	Long:  killCmd.Long,
	Args:  cobra.MinimumNArgs(1),
	RunE:  runKill,
}

// Job status subcommand - delegates to main status command
var jobStatusCmd = &cobra.Command{
	Use:   "status <job-id>...",
	Short: "Check status of one or more jobs",
	Long: `Check the status of one or more jobs by ID.

Shows job metadata including command, host, status, exit code, and timing.
Supports checking multiple jobs at once.

Examples:
  remote-jobs job status 42          # Single job
  remote-jobs job status 42 43 44    # Multiple jobs`,
	Args: cobra.MinimumNArgs(1),
	RunE: runStatus,
}

// Job describe subcommand
var jobDescribeCmd = &cobra.Command{
	Use:   "describe <job-id> <description>",
	Short: "Set or update the description of a job",
	Long:  describeCmd.Long,
	Args:  cobra.ExactArgs(2),
	RunE:  runDescribe,
}

// Job restart subcommand
var jobRestartCmd = &cobra.Command{
	Use:   "restart <job-id>",
	Short: "Restart a job using saved metadata",
	Long:  restartCmd.Long,
	Args:  cobra.ExactArgs(1),
	RunE:  runRestart,
}

// Job list subcommand
var jobListCmd = &cobra.Command{
	Use:   "list",
	Short: "List and search job history",
	Long:  listCmd.Long,
	RunE:  runList,
}

// Job move subcommand
var jobMoveCmd = &cobra.Command{
	Use:   "move <job-id> <new-host>",
	Short: "Move a queued job to a different host",
	Long: `Move a queued job to a different host.

This command only works for jobs with status=queued that haven't started yet.
It updates the host in the database and removes/adds the job from/to queue files.

Examples:
  remote-jobs job move 42 cool100   # Move job 42 to cool100
  remote-jobs job move 43 studio    # Move job 43 to studio`,
	Args: cobra.ExactArgs(2),
	RunE: runJobMove,
}

func init() {
	// Register job command with root
	rootCmd.AddCommand(jobCmd)

	// Register subcommands
	jobCmd.AddCommand(jobRunCmd)
	jobCmd.AddCommand(jobLogCmd)
	jobCmd.AddCommand(jobKillCmd)
	jobCmd.AddCommand(jobStatusCmd)
	jobCmd.AddCommand(jobDescribeCmd)
	jobCmd.AddCommand(jobRestartCmd)
	jobCmd.AddCommand(jobListCmd)
	jobCmd.AddCommand(jobMoveCmd)

	// Copy flags from run command to job run
	jobRunCmd.Flags().StringVarP(&runDescription, "description", "d", "", "Job description")
	jobRunCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory on remote host")
	jobRunCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	jobRunCmd.Flags().BoolVar(&runQueue, "queue", false, "Queue job for later instead of running now")
	jobRunCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID (replaces retry)")
	jobRunCmd.Flags().StringVar(&runTimeout, "timeout", "", "Kill job after duration (e.g., \"2h\", \"30m\", \"1h30m\")")

	// Copy flags from log command to job log
	jobLogCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	jobLogCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show (last N lines)")
	jobLogCmd.Flags().IntVar(&logFrom, "from", 0, "Show lines starting from line N")
	jobLogCmd.Flags().IntVar(&logTo, "to", 0, "Show lines up to line N")
	jobLogCmd.Flags().StringVar(&logGrep, "grep", "", "Filter lines matching pattern")

	// Copy flags from list command to job list
	jobListCmd.Flags().BoolVar(&listRunning, "running", false, "Show only running jobs")
	jobListCmd.Flags().BoolVar(&listCompleted, "completed", false, "Show only completed jobs")
	jobListCmd.Flags().BoolVar(&listDead, "dead", false, "Show only dead jobs")
	jobListCmd.Flags().BoolVar(&listPending, "pending", false, "Show only pending jobs")
	jobListCmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	jobListCmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	jobListCmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	jobListCmd.Flags().Int64Var(&listShow, "show", 0, "Show detailed info for a specific job ID")
	jobListCmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	jobListCmd.Flags().BoolVar(&listSync, "sync", false, "Sync job statuses from remote hosts before listing")
}

func runJobMove(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}
	newHost := args[1]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get the job
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Check status
	if job.Status != db.StatusQueued {
		return fmt.Errorf("can only move queued jobs (job %d has status: %s)", jobID, job.Status)
	}

	oldHost := job.Host
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	// Update host in database first
	if err := db.UpdateJobHost(database, jobID, newHost); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	// Remove from old host's queue file
	oldQueueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", jobID, oldQueueFile)
	_, stderr, err := ssh.Run(oldHost, removeCmd)

	if err != nil && ssh.IsConnectionError(stderr) {
		// Old host unreachable - defer removal
		fmt.Printf("Old host %s unreachable, will remove on next sync\n", oldHost)
		if err := db.AddDeferredOperation(database, oldHost, db.OpMoveFromQueue, jobID, queueName); err != nil {
			return fmt.Errorf("add deferred operation for old host: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("remove from old host queue: %s", strings.TrimSpace(stderr))
	}

	// Add to new host's queue file
	newQueueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	queueLine := fmt.Sprintf("%d\t%s\t%s\t%s", jobID, job.WorkingDir, job.Command, job.Description)
	addCmd := fmt.Sprintf("mkdir -p ~/.cache/remote-jobs/queue && echo '%s' >> %s",
		ssh.EscapeForSingleQuotes(queueLine), newQueueFile)
	_, stderr, err = ssh.Run(newHost, addCmd)

	if err != nil && ssh.IsConnectionError(stderr) {
		// New host unreachable - job will need to be manually re-queued
		fmt.Printf("Warning: new host %s unreachable, job updated in database but not added to queue\n", newHost)
		fmt.Printf("Run 'remote-jobs sync %s' when host is reachable to complete the move\n", newHost)
	} else if err != nil {
		return fmt.Errorf("add to new host queue: %s", strings.TrimSpace(stderr))
	}

	fmt.Printf("Moved job %d: %s → %s\n", jobID, oldHost, newHost)
	fmt.Printf("Command: %s\n", job.Command)
	if job.Description != "" {
		fmt.Printf("Description: %s\n", job.Description)
	}

	return nil
}
