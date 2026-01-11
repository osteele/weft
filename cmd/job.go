package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/queuejob"
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
	Args:  usageArgs(cobra.MinimumNArgs(2)),
	RunE:  runRun,
}

// Job log subcommand - delegates to main log command
var jobLogCmd = &cobra.Command{
	Use:     "log <job-id>",
	Aliases: []string{"logs"},
	Short:   "View log output from a remote job",
	Long:    logCmd.Long,
	Args:    usageArgs(cobra.ExactArgs(1)),
	RunE:    runLog,
}

// Job kill subcommand - delegates to main kill command
var jobKillCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running jobs",
	Long:  killCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
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
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runStatus,
}

// Job describe subcommand
var jobDescribeCmd = &cobra.Command{
	Use:   "describe <job-id>",
	Short: "Set or update job metadata",
	Long:  describeCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runDescribe,
}

// Job restart subcommand
var jobRestartCmd = &cobra.Command{
	Use:   "restart <job-id>",
	Short: "Restart a job using saved metadata",
	Long:  restartCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
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
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runJobMove,
}

var jobStartCmd = &cobra.Command{
	Use:   "start <job-id>",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobStartNow,
}

var jobInfoCmd = &cobra.Command{
	Use:   "info <job-id>",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

Examples:
  remote-jobs job info 42`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobInfo,
}

// Top-level info command (alias for job info)
var infoCmd = &cobra.Command{
	Use:   "info <job-id>",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

This is an alias for 'job info'.

Examples:
  remote-jobs info 42`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobInfo,
}

// Top-level show command (alias for job info)
var showCmd = &cobra.Command{
	Use:   "show <job-id>",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

This is an alias for 'job info'.

Examples:
  remote-jobs show 42`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobInfo,
}

// Top-level start command (alias for job start)
var startCmd = &cobra.Command{
	Use:   "start <job-id>",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.

This is an alias for 'job start'.

Examples:
  remote-jobs start 42`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobStartNow,
}

func init() {
	// Register job command with root
	rootCmd.AddCommand(jobCmd)

	// Register top-level aliases
	rootCmd.AddCommand(infoCmd)
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(startCmd)

	// Register subcommands
	jobCmd.AddCommand(jobRunCmd)
	jobCmd.AddCommand(jobLogCmd)
	jobCmd.AddCommand(jobKillCmd)
	jobCmd.AddCommand(jobStatusCmd)
	jobCmd.AddCommand(jobDescribeCmd)
	jobCmd.AddCommand(jobRestartCmd)
	jobCmd.AddCommand(jobListCmd)
	jobCmd.AddCommand(jobMoveCmd)
	jobCmd.AddCommand(jobDraftCmd)
	jobCmd.AddCommand(jobStartCmd)
	jobCmd.AddCommand(jobInfoCmd)

	// Copy flags from run command to job run
	jobRunCmd.Flags().StringVarP(&runDescription, "message", "m", "", "Job description")
	jobRunCmd.Flags().StringVarP(&runDescription, "description", "d", "", "[deprecated: use -m] Job description")
	jobRunCmd.Flags().MarkHidden("description")
	jobRunCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory on remote host")
	jobRunCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	jobRunCmd.Flags().BoolVarP(&runImmediate, "immediate", "i", false, "Start job immediately instead of queuing")
	jobRunCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	jobRunCmd.Flags().StringVar(&runTimeout, "timeout", "", "Kill job after duration (e.g., \"2h\", \"30m\", \"1h30m\")")
	jobRunCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated)")

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
	jobListCmd.Flags().BoolVar(&listQueued, "queued", false, "Show only queued jobs (waiting in queue)")
	jobListCmd.Flags().StringVarP(&listStatus, "status", "s", "", "Filter by status (running, completed, queued, dead, processed, unprocessed)")
	jobListCmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	jobListCmd.Flags().BoolVar(&listAllHosts, "all-hosts", false, "Include jobs from hosts not synced recently")
	jobListCmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	jobListCmd.Flags().StringSliceVar(&listTags, "tag", nil, "Filter by tag (can be repeated)")
	jobListCmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	jobListCmd.Flags().Int64Var(&listShow, "show", 0, "Show detailed info for a specific job ID")
	jobListCmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	jobListCmd.Flags().BoolVar(&listSync, "sync", false, "Sync job statuses from remote hosts before listing")
	jobListCmd.Flags().BoolVar(&listNoSync, "no-sync", false, "Skip syncing job statuses before listing")
	jobListCmd.Flags().BoolVarP(&listAll, "all", "a", false, "Include jobs older than 7 days")

	// Copy flags from describe command to job describe
	jobDescribeCmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	jobDescribeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU (CUDA_VISIBLE_DEVICES) - queued jobs only")
	jobDescribeCmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs (CUDA_VISIBLE_DEVICES) - queued jobs only")
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

	// Update host in database first
	if err := db.UpdateJobHost(database, jobID, newHost); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	// Set pending status to queued to trigger reconciliation
	if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
		return fmt.Errorf("set pending status: %w", err)
	}

	fmt.Printf("Moved job %d: %s → %s\n", jobID, oldHost, newHost)
	fmt.Printf("Command: %s\n", job.Command)
	if job.Description != "" {
		fmt.Printf("Description: %s\n", job.Description)
	}

	return nil
}

func runJobStartNow(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}
	if job.Status != db.StatusQueued {
		return fmt.Errorf("job %d is not queued (status: %s)", jobID, job.Status)
	}

	deferred, err := queuejob.StartNow(database, job)
	if err != nil {
		return err
	}

	if deferred {
		fmt.Printf("Host %s unreachable. Job %d will start when the next sync reaches that host.\n", job.Host, jobID)
		fmt.Printf("Run 'remote-jobs sync %s' once the host is reachable to trigger the start.\n", job.Host)
		return nil
	}

	fmt.Printf("Job %d started immediately on %s\n", jobID, job.Host)
	return nil
}

func runJobInfo(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Show full job details
	fmt.Printf("Job ID:      %d\n", job.ID)
	fmt.Printf("Host:        %s\n", job.Host)
	// Show status with waiting info
	statusStr := job.Status
	fmt.Printf("Status:      %s\n", statusStr)
	fmt.Printf("Description: %s\n", job.Description)
	fmt.Printf("Directory:   %s\n", job.WorkingDir)
	fmt.Printf("Command:     %s\n", job.Command)
	if len(job.Tags) > 0 {
		fmt.Printf("Tags:        %s\n", strings.Join(job.Tags, ", "))
	}

	// Show effective command/directory if different
	effectiveCmd := job.EffectiveCommand()
	effectiveDir := job.EffectiveWorkingDir()
	if effectiveCmd != job.Command {
		fmt.Printf("  (effective: %s)\n", effectiveCmd)
	}
	if effectiveDir != job.WorkingDir {
		fmt.Printf("  (effective dir: %s)\n", effectiveDir)
	}

	// Show GPU if present
	if gpu := job.GetGPU(); gpu != "" {
		fmt.Printf("GPU:         %s\n", gpu)
	}

	// Show queue info if queued
	if job.QueueName != "" {
		fmt.Printf("Queue:       %s\n", job.QueueName)
	}

	// Show timing info
	// Show created/queued time if different from start time
	if job.CreatedAt > 0 && job.CreatedAt != job.StartTime {
		label := "Created"
		if job.Status == db.StatusQueued {
			label = "Queued"
		}
		fmt.Printf("%-12s %s\n", label+":", formatUnixTime(job.CreatedAt))
	}
	if job.StartTime > 0 {
		fmt.Printf("Started:     %s\n", formatUnixTime(job.StartTime))
	}
	if job.EndTime != nil {
		fmt.Printf("Ended:       %s\n", formatUnixTime(*job.EndTime))
		if job.StartTime > 0 {
			duration := *job.EndTime - job.StartTime
			fmt.Printf("Duration:    %s\n", db.FormatDuration(duration))
		}
	}
	if job.ExitCode != nil {
		fmt.Printf("Exit Code:   %d\n", *job.ExitCode)
	}
	if job.ErrorMessage != "" {
		fmt.Printf("Error:       %s\n", job.ErrorMessage)
	}

	return nil
}

func formatUnixTime(t int64) string {
	return fmt.Sprintf("%s", time.Unix(t, 0).Format("2006-01-02 15:04:05"))
}
