package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queuejob"
	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage jobs",
	Long: `Manage jobs including running, monitoring, and controlling them.

All job-related operations are available under this subcommand. Common
operations (run, log, kill) also have top-level shortcuts.

Available subcommands:
  run       Start a new job on a remote host
  log       View job log output
  kill      Kill a running job
  status    Check status of one or more jobs
  describe  Set or update job description
  restart   Requeue a killed, dead, failed, or canceled job
  retry     Alias for restart
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
	Use:     "log <job-id>...",
	Aliases: []string{"logs"},
	Short:   "View log output from a job",
	Long:    logCmd.Long,
	Args:    usageArgs(cobra.MinimumNArgs(1)),
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
  weft job status 42          # Single job
  weft job status 42 43 44    # Multiple jobs
  weft job status 42...44     # Range syntax
  weft job status 42,43,44    # Comma-separated IDs`,
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
	Use:     "restart <job-id>...",
	Aliases: []string{"retry"},
	Short:   "Requeue a killed, dead, failed, or canceled job",
	Long:    restartCmd.Long,
	Args:    usageArgs(cobra.MinimumNArgs(1)),
	RunE:    runRestart,
}

// Job list subcommand
var jobListCmd = &cobra.Command{
	Use:   "list [job-id]...",
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
  weft job move 42 cool100   # Move job 42 to cool100
  weft job move 43 studio    # Move job 43 to studio`,
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runJobMove,
}

var jobStartCmd = &cobra.Command{
	Use:   "start <job-id>...",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobStartNow,
}

var jobInfoCmd = &cobra.Command{
	Use:   "info <job-id>...",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

Examples:
  weft job info 42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobInfo,
}

// Top-level info command (alias for job info)
var infoCmd = &cobra.Command{
	Use:   "info <job-id>...",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

This is an alias for 'job info'.

Examples:
  weft info 42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobInfo,
}

// Top-level show command (alias for job info)
var showCmd = &cobra.Command{
	Use:   "show <job-id>...",
	Short: "Show detailed job information",
	Long: `Show full details for a job including command, working directory,
environment variables, and timing information.

This is an alias for 'job info'.

Examples:
  weft show 42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobInfo,
}

// Top-level start command (alias for job start)
var startCmd = &cobra.Command{
	Use:   "start <job-id>...",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.

This is an alias for 'job start'.

Examples:
  weft start 42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
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
	jobRunCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory on remote host (alias: --dir)")
	jobRunCmd.Flags().StringVar(&runProject, "project", "", "Project name (default: repo root name for the working directory)")
	jobRunCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	jobRunCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	jobRunCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated). Reserved tags: 'exclusive' runs alone; 'benchmark' waits for system-wide idle; 'rental' skips local placement; 'inventory' blocks rental placement")
	addJobAddFlagAliases(jobRunCmd)

	// Copy flags from log command to job log
	jobLogCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	jobLogCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show (last N lines)")
	jobLogCmd.Flags().IntVar(&logFrom, "from", 0, "Show lines starting from line N")
	jobLogCmd.Flags().IntVar(&logTo, "to", 0, "Show lines up to line N")
	jobLogCmd.Flags().StringVar(&logGrep, "grep", "", "Filter lines matching pattern")

	// Use shared list flags helper (defined in list.go)
	addListFlags(jobListCmd)

	// Copy flags from describe command to job describe
	jobDescribeCmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	jobDescribeCmd.Flags().StringVar(&describeProject, "project", "", "Set project name")
	jobDescribeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU: device index, class, or class>=NGB (e.g., 1, a100, nvidia>=24GB) - queued jobs only")
	jobDescribeCmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs (CUDA_VISIBLE_DEVICES) - queued jobs only")
	jobDescribeCmd.Flags().IntVar(&describeGPUMem, "gpu-mem", 0, "Set GPU memory reservation in GB per device")
	jobDescribeCmd.Flags().IntVar(&describeCPU, "cpu", 0, "Set CPU allotment percent")
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
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusQueued {
		return fmt.Errorf("can only move queued jobs (job %d has status: %s)", jobID, effectiveStatus)
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

	if syncErr := syncHostAfterQueueChange(database, newHost); syncErr != nil {
		reportQueueChangeSyncFailure(newHost, syncErr)
	}

	fmt.Printf("Moved job %d: %s → %s\n", jobID, oldHost, newHost)
	fmt.Printf("Command: %s\n", job.Command)
	if job.Description != "" {
		fmt.Printf("Description: %s\n", job.Description)
	}

	return nil
}

func runJobStartNow(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: get job: %v", jobID, err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d not found", jobID))
			continue
		}
		effectiveStatus := job.EffectiveStatus()
		if effectiveStatus != db.StatusQueued {
			errorsList = append(errorsList, fmt.Sprintf("job %d is not queued (status: %s)", jobID, effectiveStatus))
			continue
		}

		deferred, err := queuejob.StartNow(database, job)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		if deferred {
			fmt.Printf("Job %d saved locally. %s is offline — it will start on the next sync.\n", jobID, job.Host)
			continue
		}

		fmt.Printf("Job %d started immediately on %s\n", jobID, job.Host)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runJobInfo(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for i, jobID := range jobIDs {
		if len(jobIDs) > 1 && i > 0 {
			fmt.Println("---")
		}
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: get job: %v", jobID, err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d not found", jobID))
			continue
		}

		// Show full job details
		fmt.Printf("Job ID:      %d\n", job.ID)
		fmt.Printf("Target:      %s\n", job.TargetDisplay())
		// Show status with waiting info
		statusStr := job.Status
		fmt.Printf("Status:      %s\n", statusStr)
		fmt.Printf("Description: %s\n", job.Description)
		fmt.Printf("Directory:   %s\n", job.DisplayWorkingDir())
		fmt.Printf("Command:     %s\n", job.Command)
		if tags := job.DisplayTags(); len(tags) > 0 {
			fmt.Printf("Tags:        %s\n", strings.Join(tags, ", "))
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
		if gpuDev := job.GPUDevice(); gpuDev != "" {
			if job.GPUClass != "" {
				fmt.Printf("GPU:         %s (class: %s)\n", gpuDev, job.GPUClass)
			} else {
				fmt.Printf("GPU:         %s\n", gpuDev)
			}
		} else if job.GPUClass != "" {
			fmt.Printf("GPU Class:   %s\n", job.GPUClass)
		}

		// Show timing info
		// Show created/queued time if different from start time
		if job.CreatedAt > 0 && job.CreatedAt != job.StartTime {
			label := "Created"
			if job.EffectiveStatus() == db.StatusQueued {
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

		// Resource usage
		if job.Metadata != nil && job.Metadata.Resource != nil {
			r := job.Metadata.Resource
			if r.UserCPUSecs != nil || r.SysCPUSecs != nil {
				userStr := "0s"
				sysStr := "0s"
				if r.UserCPUSecs != nil {
					userStr = db.FormatDuration(int64(*r.UserCPUSecs))
				}
				if r.SysCPUSecs != nil {
					sysStr = db.FormatDuration(int64(*r.SysCPUSecs))
				}
				fmt.Printf("CPU Time:    %s user, %s sys\n", userStr, sysStr)
			}
			if r.PeakRSSKB != nil {
				fmt.Printf("Peak Memory: %s\n", formatMemoryKB(*r.PeakRSSKB))
			}
			if r.MaxGPUMemMiB != nil {
				fmt.Printf("GPU Memory:  %d MiB (peak)\n", *r.MaxGPUMemMiB)
			}
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func formatUnixTime(t int64) string {
	return fmt.Sprintf("%s", time.Unix(t, 0).Format("2006-01-02 15:04:05"))
}

func formatMemoryKB(kb int64) string {
	switch {
	case kb >= 1024*1024:
		return fmt.Sprintf("%.1f GB", float64(kb)/(1024*1024))
	case kb >= 1024:
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	default:
		return fmt.Sprintf("%d KB", kb)
	}
}
