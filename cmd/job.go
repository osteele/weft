package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/queuejob"
	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:     "job",
	Aliases: []string{"jobs"},
	Short:   "Manage jobs",
	Long: `Manage jobs including running, monitoring, and controlling them.

All job-related operations are available under this subcommand. Common
operations (run, log, kill) also have top-level shortcuts.

Available subcommands:
  run       Start a new job on a remote host
  log       View job log output
  kill      Kill a running job
  status    Check status of one or more jobs
  describe  Set or update job description
  info      Show detailed job information
  show      Alias for info
  restart   Requeue a killed, dead, failed, canceled, or completed job
  retry     Alias for restart
  list      List and search job history
  watch     Watch job status changes
  move      Move a queued job to a different host
  unplace   Move a queued job back to the unplaced pool`,
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
	Args:    validateLogArgs,
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
	Short:   "Requeue a killed, dead, failed, canceled, or completed job",
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
var (
	jobMoveEach    bool
	jobMoveProject string
)

var jobMoveCmd = &cobra.Command{
	Use:   "move <job-id>... <destination>",
	Short: "Move queued jobs to a host, instance, or new instance(s)",
	Long: `Move one or more queued jobs to a different destination.

The last argument is the destination; all preceding arguments are job IDs.
Job IDs support ranges (123:127), comma lists (123,124), and wj prefix.
Jobs that are not queued are skipped with a warning.

Destinations:
  <hostname>     Move to an on-prem inventory host (e.g., cool100, studio)
  wi<N>          Submit to an existing cloud instance (e.g., wi872)
  new            Launch new instance(s) sized for the jobs (grouped by GPU affinity)
  create         Alias for 'new'

Flags:
  --each              With 'new'/'create': launch a separate instance per job
  --project <name>    Select all eligible queued jobs in the named project

Examples:
  weft job move 42 cool100              # Place job 42 on cool100
  weft job move 43 wi872                # Submit job 43 to instance wi872
  weft job move 44 new                  # Launch one new instance for job 44
  weft job move 44 45 46 new            # Launch instance(s) for jobs 44-46
  weft job move 44:46 --each new        # Separate new instance per job
  weft job move --project myproj new    # All queued myproj jobs → new instance`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobMove,
}

var (
	jobPlaceEach    bool
	jobPlaceProject string
)

var jobPlaceCmd = &cobra.Command{
	Use:   "place <job-id>... <destination>",
	Short: "Place unplaced queued jobs on a host, instance, or new instance(s)",
	Long: `Place one or more unplaced queued jobs on a destination.

Like 'move', but only acts on jobs that are currently unplaced.
Already-placed jobs are skipped with a warning. If all jobs are
already placed, it's an error.

Accepts the same destinations, --each, and --project flags as 'move'.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobPlace,
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
	Use:     "info <job-id>...",
	Aliases: []string{"show"},
	Short:   "Show detailed job information",
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

var jobCancelCmd = &cobra.Command{
	Use:     "cancel <job-id>...",
	Aliases: []string{"remove"},
	Short:   cancelCmd.Short,
	Long:    cancelCmd.Long,
	Args:    usageArgs(cobra.MinimumNArgs(1)),
	RunE:    runCancel,
}

var jobPauseCmd = &cobra.Command{
	Use:   "pause <job-id>...",
	Short: pauseCmd.Short,
	Long:  pauseCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runPause,
}

var jobResumeCmd = &cobra.Command{
	Use:   "resume <job-id>...",
	Short: resumeCmd.Short,
	Long:  resumeCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runResume,
}

var jobCleanupCmd = &cobra.Command{
	Use:   "cleanup <host>",
	Short: cleanupCmd.Short,
	Long:  cleanupCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runCleanup,
}

var jobMarkProcessedCmd = &cobra.Command{
	Use:   "mark-processed <job-id>...",
	Short: markProcessedCmd.Short,
	Long:  markProcessedCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runMarkProcessed,
}

var jobMarkUnprocessedCmd = &cobra.Command{
	Use:   "mark-unprocessed <job-id>...",
	Short: markUnprocessedCmd.Short,
	Long:  markUnprocessedCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runMarkUnprocessed,
}

var jobPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: pruneCmd.Short,
	Long:  pruneCmd.Long,
	RunE:  runPrune,
}

var jobPredictCmd = &cobra.Command{
	Use:   "predict <command>",
	Short: "Predict job duration and resource usage",
	Long:  predictCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runPredict,
}

var jobUnplaceCmd = &cobra.Command{
	Use:   "unplace <job-id>...",
	Short: "Move a queued job back to the unplaced pool",
	Long: `Remove the host or instance assignment from a queued job, returning it
to the unplaced pool where it can be re-placed on a different host or instance.

Works for both on-prem (inventory host) and cloud (rental instance) jobs.
Only queued jobs can be unplaced — running jobs must be killed first.

Examples:
  weft job unplace 42
  weft job unplace 42 43 44
  weft job unplace 42...44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobUnplace,
}

var jobTagCmd = &cobra.Command{
	Use:   "tag",
	Short: tagCmd.Short,
	Long:  tagCmd.Long,
}

var jobTagAddCmd = &cobra.Command{
	Use:   "add <job-id>... <tag>",
	Short: tagAddCmd.Short,
	Args:  usageArgs(cobra.MinimumNArgs(2)),
	RunE:  runTagAdd,
}

var jobTagRemoveCmd = &cobra.Command{
	Use:     "rm <job-id>... <tag>",
	Aliases: []string{"remove", "delete"},
	Short:   tagRemoveCmd.Short,
	Args:    usageArgs(cobra.MinimumNArgs(2)),
	RunE:    runTagRemove,
}

func init() {
	// Register job command with root
	rootCmd.AddCommand(jobCmd)

	// Register top-level aliases
	rootCmd.AddCommand(infoCmd)
	showCmd.Deprecated = "use 'weft job info' instead"
	startCmd.Deprecated = "use 'weft job start' instead"
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(startCmd)

	// Register subcommands
	jobCmd.AddCommand(jobRunCmd)
	jobCmd.AddCommand(jobLogCmd)
	jobCmd.AddCommand(jobKillCmd)
	addStatusFlags(jobStatusCmd)
	jobCmd.AddCommand(jobStatusCmd)
	jobCmd.AddCommand(jobDescribeCmd)
	addRestartFlags(jobRestartCmd)
	jobCmd.AddCommand(jobRestartCmd)
	jobCmd.AddCommand(jobListCmd)
	jobCmd.AddCommand(jobWatchCmd)
	jobMoveCmd.Flags().BoolVar(&jobMoveEach, "each", false, "With 'new'/'create': launch a separate instance per job")
	jobMoveCmd.Flags().StringVar(&jobMoveProject, "project", "", "Select all eligible queued jobs in the named project")
	jobCmd.AddCommand(jobMoveCmd)
	jobPlaceCmd.Flags().BoolVar(&jobPlaceEach, "each", false, "With 'new'/'create': launch a separate instance per job")
	jobPlaceCmd.Flags().StringVar(&jobPlaceProject, "project", "", "Select all eligible unplaced queued jobs in the named project")
	jobCmd.AddCommand(jobPlaceCmd)
	jobCmd.AddCommand(jobDraftCmd)
	jobCmd.AddCommand(jobStartCmd)
	jobCmd.AddCommand(jobInfoCmd)
	jobCmd.AddCommand(jobCancelCmd)
	jobCmd.AddCommand(jobPauseCmd)
	jobCmd.AddCommand(jobResumeCmd)
	jobCmd.AddCommand(jobCleanupCmd)
	jobCmd.AddCommand(jobMarkProcessedCmd)
	jobCmd.AddCommand(jobMarkUnprocessedCmd)
	jobCmd.AddCommand(jobPruneCmd)
	jobCmd.AddCommand(jobPredictCmd)
	jobCmd.AddCommand(jobUnplaceCmd)
	jobCmd.AddCommand(jobTagCmd)
	jobTagCmd.AddCommand(jobTagAddCmd)
	jobTagCmd.AddCommand(jobTagRemoveCmd)

	// Sync flags for job info (shared across jobInfoCmd, infoCmd, showCmd)
	for _, cmd := range []*cobra.Command{jobInfoCmd, infoCmd, showCmd} {
		cmd.Flags().BoolVar(&jobInfoSync, "sync", false, "Perform full sync (30s timeout)")
		cmd.Flags().BoolVar(&jobInfoNoSync, "no-sync", false, "Skip syncing job statuses")
	}

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
	addLogFlags(jobLogCmd)

	// Use shared list flags helper (defined in list.go)
	addListFlags(jobListCmd)

	// Watch flags
	addJobWatchFlags(jobWatchCmd)

	// Copy flags from describe command to job describe
	jobDescribeCmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	jobDescribeCmd.Flags().StringVar(&describeProject, "project", "", "Set project name")
	jobDescribeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command (queued jobs only)")
	jobDescribeCmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU: device index, class, or class>=NGB (e.g., 1, a100, nvidia>=24GB) - queued jobs only")
	jobDescribeCmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs (CUDA_VISIBLE_DEVICES) - queued jobs only")
	jobDescribeCmd.Flags().IntVar(&describeGPUMem, "gpu-mem", 0, "Set GPU memory reservation in GB per device")
	jobDescribeCmd.Flags().IntVar(&describeCPU, "cpu", 0, "Set CPU allotment percent")

	// Flags for job cleanup
	jobCleanupCmd.Flags().BoolVar(&cleanupSessions, "sessions", false, "Clean finished sessions only")
	jobCleanupCmd.Flags().BoolVar(&cleanupLogs, "logs", false, "Clean log files only")
	jobCleanupCmd.Flags().IntVar(&cleanupOlderThan, "older-than", 7, "Only clean items older than N days")
	jobCleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Preview without actually deleting")

	// Flags for job prune
	jobPruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "", "Only remove jobs older than this duration (e.g., 7d, 24h, 30m)")
	jobPruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Preview without actually deleting")
	jobPruneCmd.Flags().BoolVar(&pruneDeadOnly, "dead-only", false, "Only remove dead jobs (not completed)")
	jobPruneCmd.Flags().BoolVar(&pruneKeepFiles, "keep-files", false, "Don't delete remote log files")

	// Flags for job predict
	jobPredictCmd.Flags().StringVar(&predictHost, "host", "", "Target host")
	jobPredictCmd.Flags().StringVar(&predictProject, "project", "", "Project name")
	jobPredictCmd.Flags().StringVar(&predictGPUClass, "gpu-class", "", "GPU class")
}

func runJobMove(cmd *cobra.Command, args []string) error {
	return runJobMoveOrPlace(args, jobMoveProject, jobMoveEach, false)
}

func runJobPlace(cmd *cobra.Command, args []string) error {
	return runJobMoveOrPlace(args, jobPlaceProject, jobPlaceEach, true)
}

func runJobMoveOrPlace(args []string, project string, each bool, unplacedOnly bool) error {
	dest := args[len(args)-1]
	jobArgs := args[:len(args)-1]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	eligible, err := resolveEligibleJobs(database, jobArgs, project, unplacedOnly)
	if err != nil {
		return err
	}

	switch {
	case strings.EqualFold(dest, "new") || strings.EqualFold(dest, "create"):
		return moveJobsToNewInstances(database, eligible, each)
	default:
		if each {
			return usageErrorf("--each is only valid with 'new' or 'create' destination")
		}
		if instanceID, parseErr := ids.ParseInstanceID(dest); parseErr == nil {
			return moveJobsToInstance(database, eligible, instanceID)
		}
		return moveJobsToHost(database, eligible, dest)
	}
}

// resolveEligibleJobs fetches jobs by ID list and/or --project, filtering to
// queued jobs. When unplacedOnly is true (place command), already-placed jobs
// are also skipped.
func resolveEligibleJobs(database *sql.DB, jobArgs []string, project string, unplacedOnly bool) ([]*db.Job, error) {
	var jobs []*db.Job
	if project != "" {
		all, err := db.ListJobs(database, db.StatusQueued, "", 0, nil, "")
		if err != nil {
			return nil, fmt.Errorf("list queued jobs: %w", err)
		}
		for _, job := range all {
			if job.Project == project {
				jobs = append(jobs, job)
			}
		}
	}
	if len(jobArgs) > 0 {
		jobIDs, err := ParseJobIDs(jobArgs)
		if err != nil {
			return nil, fmt.Errorf("parse job IDs: %w", err)
		}
		for _, jobID := range jobIDs {
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				return nil, fmt.Errorf("get job %s: %w", FormatJobID(jobID), err)
			}
			if job != nil {
				jobs = append(jobs, job)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: job %s not found, skipping\n", FormatJobID(jobID))
			}
		}
	}
	if len(jobs) == 0 && project == "" {
		return nil, usageErrorf("provide job IDs or --project")
	}

	var eligible []*db.Job
	for _, job := range jobs {
		if job.EffectiveStatus() != db.StatusQueued {
			fmt.Fprintf(os.Stderr, "Warning: job %s has status %s, skipping\n", FormatJobID(job.ID), job.EffectiveStatus())
			continue
		}
		if unplacedOnly && job.TargetKind() != db.JobTargetUnplaced {
			fmt.Fprintf(os.Stderr, "Warning: job %s is already placed, skipping\n", FormatJobID(job.ID))
			continue
		}
		eligible = append(eligible, job)
	}
	if len(eligible) == 0 {
		if unplacedOnly {
			return nil, fmt.Errorf("no eligible unplaced queued jobs")
		}
		return nil, fmt.Errorf("no eligible queued jobs to move")
	}
	return eligible, nil
}

func moveJobsToHost(database *sql.DB, jobs []*db.Job, host string) error {
	moved := 0
	for _, job := range jobs {
		if err := unplaceIfNeeded(database, job); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: unplace job %s failed: %v\n", FormatJobID(job.ID), err)
			continue
		}
		if err := db.UpdateJobHost(database, job.ID, host); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: move job %s failed: %v\n", FormatJobID(job.ID), err)
			continue
		}
		if err := db.SetPendingStatus(database, job.ID, db.StatusQueued); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: set pending status for job %s: %v\n", FormatJobID(job.ID), err)
		}
		moved++
		fmt.Printf("Moved job %s → %s\n", FormatJobID(job.ID), host)
	}
	if moved == 0 {
		return fmt.Errorf("all %d job(s) failed to move to %s", len(jobs), host)
	}
	if syncErr := syncHostAfterQueueChange(database, host); syncErr != nil {
		reportQueueChangeSyncFailure(host, syncErr)
	}
	return nil
}

func moveJobsToInstance(database *sql.DB, jobs []*db.Job, instanceID int64) error {
	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return fmt.Errorf("R2 client: %w", err)
	}

	var ready []*db.Job
	for _, job := range jobs {
		if err := unplaceIfNeeded(database, job); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: unplace job %s failed: %v\n", FormatJobID(job.ID), err)
			continue
		}
		ready = append(ready, job)
	}
	if len(ready) == 0 {
		return fmt.Errorf("all jobs failed to unplace")
	}

	if err := campaign.SubmitJobsToInstance(context.Background(), database, r2Client, instanceID, ready); err != nil {
		return fmt.Errorf("submit to instance %s: %w", ids.FormatInstanceID(instanceID), err)
	}
	for _, job := range ready {
		fmt.Printf("Moved job %s → instance %s\n", FormatJobID(job.ID), ids.FormatInstanceID(instanceID))
	}
	return nil
}

func moveJobsToNewInstances(database *sql.DB, jobs []*db.Job, separateEach bool) error {
	// Keep single-job move-to-new on the same implementation path as TUI move,
	// so behavior and instrumentation stay consistent across interfaces.
	if len(jobs) == 1 && !separateEach {
		res, err := orchestration.MoveQueuedJobToNewInstance(database, jobs[0].ID)
		if err != nil {
			return err
		}
		target := res.TargetDesc
		if strings.TrimSpace(target) == "" && res.InstanceID > 0 {
			target = fmt.Sprintf("instance %s", ids.FormatInstanceID(res.InstanceID))
		}
		if strings.TrimSpace(target) == "" {
			target = "new instance"
		}
		fmt.Printf("Moved job %s → %s\n", FormatJobID(jobs[0].ID), target)
		return nil
	}
	_, err := orchestration.MoveQueuedJobsToNewInstances(database, jobs, separateEach, orchestration.BulkCallbacks{
		OnStatus: func(message string) {
			fmt.Println(message)
		},
		OnWarning: func(message string) {
			fmt.Fprintln(os.Stderr, message)
		},
	})
	return err
}

func unplaceIfNeeded(database *sql.DB, job *db.Job) error {
	if job.TargetKind() == db.JobTargetUnplaced {
		return nil
	}
	_, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
	return err
}

func countGroupJobs(groups []campaign.InstanceGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.Jobs)
	}
	return n
}

func runJobUnplace(cmd *cobra.Command, args []string) error {
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
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d not found", jobID))
			continue
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutNormal))
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		fmt.Println(result.Message)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
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

var (
	jobInfoSync   bool
	jobInfoNoSync bool
)

var quickSyncJobsFunc = quickSyncJobs

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

	// Quick sync for non-terminal jobs before display
	if !jobInfoNoSync {
		var jobsToSync []*db.Job
		for _, jobID := range jobIDs {
			job, err := db.GetJobByID(database, jobID)
			if err != nil || job == nil {
				continue
			}
			jobsToSync = append(jobsToSync, job)
		}
		if len(jobsToSync) > 0 {
			timeout := FastSyncTimeout
			cloudTimeout := FastCloudSyncTimeout
			if jobInfoSync {
				timeout = NormalSyncTimeout
				cloudTimeout = NormalCloudSyncTimeout
			}
			quickSyncJobsFunc(database, jobsToSync, timeout, cloudTimeout)
		}
	}

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
		hydrateQueueBlockedReasons([]*db.Job{job})
		display := queueblock.Display(job, nil)

		// Show full job details
		fmt.Printf("Job ID:      %d\n", job.ID)
		fmt.Printf("Host:        %s\n", job.TargetDisplay())
		// Show status with waiting info
		if display.Blocked {
			fmt.Printf("Status:      %s\n", display.Status)
			fmt.Printf("Reason:      %s\n", display.Reason)
		} else {
			fmt.Printf("Status:      %s\n", job.EffectiveStatus())
		}
		fmt.Printf("Description: %s\n", job.Description)
		fmt.Printf("Directory:   %s\n", job.DisplayWorkingDir())
		fmt.Printf("Command:     %s\n", job.Command)
		if tags := job.DisplayTags(); len(tags) > 0 {
			fmt.Printf("Tags:        %s\n", strings.Join(tags, ", "))
		}
		now := time.Now()
		elapsed := jobElapsedDuration(job, now)
		if elapsed > 0 {
			fmt.Printf("Elapsed:     %s\n", db.FormatDuration(int64(elapsed.Seconds())))
		}
		estimateTotal, hasEstimate := jobTimeEstimate(job, database)
		if hasEstimate {
			fmt.Printf("Est. Time:   %s\n", estimateTotal.FormatWithBounds())
			if elapsed > 0 {
				eta := estimateRemaining(estimateTotal, elapsed)
				fmt.Printf("ETA:         %s\n", eta.FormatWithBounds())
			}
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
		if rentalSummary, ok := rentalCostSummary(database, job, now); ok {
			fmt.Printf("Cost:        $%.2f (%s)\n", rentalSummary.Cost, rentalSummary.Basis)
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

		// Telemetry summary for benchmark jobs
		if job.HasTag(db.TagBenchmark) {
			samples, telErr := db.GetTimeseries(database, job.ID)
			if telErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: telemetry: %v\n", telErr)
			} else if len(samples) > 0 {
				stats := db.ComputeGPUTelemetryStats(samples)
				if stats != nil {
					fmt.Println()
					fmt.Println("Telemetry:")
					if stats.TempMax > 0 {
						fmt.Printf("  GPU Temp:  %d°C peak, %.0f°C mean\n", stats.TempMax, stats.TempMean)
					}
					if stats.UtilMax > 0 {
						fmt.Printf("  GPU Util:  %.0f%% mean\n", stats.UtilMean)
					}
					if stats.Throttled {
						fmt.Printf("  ⚠ Thermal throttling likely (temp > %d°C)\n", db.ThermalThrottleThresholdC)
					}
				}
			}
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

type rentalCostInfo struct {
	Cost  float64
	Basis string
}

func jobElapsedDuration(job *db.Job, now time.Time) time.Duration {
	if job == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if job.StartTime <= 0 {
		return 0
	}
	start := time.Unix(job.StartTime, 0)
	end := now
	if job.EndTime != nil && *job.EndTime > 0 {
		end = time.Unix(*job.EndTime, 0)
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func jobSetupDuration(job *db.Job, timings *db.JobPhaseTimings, now time.Time) time.Duration {
	if job == nil || timings == nil || timings.SetupStart == nil || *timings.SetupStart <= 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	start := time.Unix(*timings.SetupStart, 0)
	switch {
	case timings.SetupEnd != nil && *timings.SetupEnd > 0:
		end := time.Unix(*timings.SetupEnd, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	case timings.RunStart != nil && *timings.RunStart > 0:
		end := time.Unix(*timings.RunStart, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	}
	if job.EndTime != nil && *job.EndTime > 0 {
		end := time.Unix(*job.EndTime, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	}
	if now.After(start) {
		return now.Sub(start)
	}
	return 0
}

func jobRunElapsedDuration(job *db.Job, timings *db.JobPhaseTimings, now time.Time) time.Duration {
	if job == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if timings != nil && timings.RunStart != nil && *timings.RunStart > 0 {
		start := time.Unix(*timings.RunStart, 0)
		end := now
		if timings.RunEnd != nil && *timings.RunEnd > 0 {
			end = time.Unix(*timings.RunEnd, 0)
		} else if job.EndTime != nil && *job.EndTime > 0 {
			end = time.Unix(*job.EndTime, 0)
		}
		if end.After(start) {
			return end.Sub(start)
		}
		return 0
	}
	return jobElapsedDuration(job, now)
}

func predictionToEstimate(predictedSeconds *float64) (estimate.Estimate, bool) {
	if predictedSeconds == nil || *predictedSeconds <= 0 {
		return estimate.Estimate{}, false
	}
	meanSeconds := *predictedSeconds
	return estimate.FromSeconds(meanSeconds, meanSeconds*0.5, meanSeconds*2.0), true
}

func jobTimeEstimate(job *db.Job, database *sql.DB) (estimate.Estimate, bool) {
	if job == nil {
		return estimate.Estimate{}, false
	}
	timeEstimate, ok := predictionToEstimate(nil)
	if job.PlacementMeta != nil {
		timeEstimate, ok = predictionToEstimate(job.PlacementMeta.PredictedDurationS)
	}
	if !ok {
		timeEstimate = estimate.DefaultJobDuration
		ok = true
	}
	// For rentals, include observed setup overhead when available.
	if job.LaunchID != nil && *job.LaunchID > 0 {
		if timings, err := db.GetJobPhaseTimings(database, job.ID); err == nil && timings != nil {
			setup := jobSetupDuration(job, timings, time.Now())
			if setup > 0 {
				timeEstimate = estimate.Estimate{
					Mean:  timeEstimate.Mean + setup,
					Lower: timeEstimate.Lower + setup,
					Upper: timeEstimate.Upper + setup,
				}
			}
		}
	}
	return timeEstimate, ok
}

func estimateRemaining(total estimate.Estimate, elapsed time.Duration) estimate.Estimate {
	remaining := estimate.Estimate{
		Mean:  max(0, total.Mean-elapsed),
		Lower: max(0, total.Lower-elapsed),
		Upper: max(0, total.Upper-elapsed),
	}
	if remaining.Upper < remaining.Lower {
		remaining.Upper = remaining.Lower
	}
	if remaining.Mean < remaining.Lower {
		remaining.Mean = remaining.Lower
	}
	if remaining.Mean > remaining.Upper {
		remaining.Mean = remaining.Upper
	}
	return remaining
}

func launchCostSoFar(launch *db.Launch, now time.Time) float64 {
	if launch == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if launch.LaunchedAt != nil && launch.CostPerHourCents > 0 {
		start := time.Unix(*launch.LaunchedAt, 0)
		end := now
		if launch.EndedAt != nil && *launch.EndedAt > 0 {
			end = time.Unix(*launch.EndedAt, 0)
		}
		if end.Before(start) {
			end = start
		}
		return end.Sub(start).Hours() * (float64(launch.CostPerHourCents) / 100.0)
	}
	if launch.ActualSpendCents > 0 {
		return float64(launch.ActualSpendCents) / 100.0
	}
	return 0
}

func rentalCostSummary(database *sql.DB, job *db.Job, now time.Time) (rentalCostInfo, bool) {
	if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
		return rentalCostInfo{}, false
	}
	launch, err := db.GetLaunch(database, *job.LaunchID)
	if err != nil || launch == nil {
		return rentalCostInfo{}, false
	}

	currentJobs, err := db.GetLaunchJobs(database, *job.LaunchID)
	if err != nil {
		return rentalCostInfo{}, false
	}
	isOnlyJob := len(currentJobs) == 1 && currentJobs[0] != nil && currentJobs[0].ID == job.ID

	if isOnlyJob {
		return rentalCostInfo{
			Cost:  launchCostSoFar(launch, now),
			Basis: "instance total",
		}, true
	}

	ratePerHour := float64(launch.CostPerHourCents) / 100.0
	if ratePerHour <= 0 {
		return rentalCostInfo{}, false
	}
	timings, _ := db.GetJobPhaseTimings(database, job.ID)
	setup := jobSetupDuration(job, timings, now)
	run := jobRunElapsedDuration(job, timings, now)
	billable := setup + run
	if billable <= 0 {
		return rentalCostInfo{}, false
	}
	cost := math.Max(0, billable.Hours()*ratePerHour)
	return rentalCostInfo{
		Cost:  cost,
		Basis: "shared instance: setup + run",
	}, true
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
