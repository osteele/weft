package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/explain"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/queuejob"
	"github.com/osteele/weft/internal/retrypolicy"
	"github.com/osteele/weft/internal/ui/terminal"
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
  inspect   Print normalized job metadata
  diff      Compare job metadata and attempts
  anomalies List recent jobs that look worth reviewing
  churn     Group recent retry/churn clusters
  recommend Suggest Weft improvements from recent job history
  restart   Requeue a killed, dead, failed, canceled, or completed job
  retry     Alias for restart
  list      List and search job history
  watch     Watch job status changes
  move      Move a queued job to a different host
  unplace   Move a queued job back to the unplaced pool`,
}

// Job run subcommand - delegates to main run command
var jobRunCmd = &cobra.Command{
	Use:   "run <command>",
	Short: "Start a new job on a remote host",
	Long:  runCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
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

var jobPriorityClear bool

var jobPriorityCmd = &cobra.Command{
	Use:   "priority <job-id>...",
	Short: "Mark queued jobs as priority",
	Long: `Mark jobs as priority for queue ordering and cloud placement.

Priority jobs are placed before unprioritized jobs. Use --clear to return
jobs to normal priority.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobPriority,
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
	Use:     "restart [job-id]...",
	Aliases: []string{"retry"},
	Short:   "Requeue a killed, dead, failed, canceled, or completed job",
	Long:    restartCmd.Long,
	Args:    usageArgs(cobra.ArbitraryArgs),
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
	jobMoveTo      string
	jobMoveFrom    string
	jobMoveForce   bool
	jobMoveTUI     bool
	jobMovePlain   bool
	jobMoveNoTUI   bool
)

var (
	moveQueuedJobToNewInstance   = orchestration.MoveQueuedJobToNewInstance
	moveQueuedJobsToNewInstances = orchestration.MoveQueuedJobsToNewInstances
	moveNewMaxAttempts           = retrypolicy.MaxAttempts
	moveNewBackoffDelay          = retrypolicy.BackoffDelay
	moveNewWait                  = waitForNewInstanceRetry
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
  auto           Return to the unplaced pool for automatic placement
  new            Launch new instance(s) sized for the jobs (grouped by GPU affinity)
  create         Alias for 'new'
  distinct       Alias for '--each --to new' (separate new instance per job)

Flags:
  --each              With 'new'/'create': launch a separate instance per job
  --to, -t <dest>     Destination (alternative to positional final argument)
  --from, -f <src>    Select queued jobs from source instance, host, or project (e.g., wi872, cool30, myproj)
  --project <name>    Select all eligible queued jobs in the named project
  --force             Allow running/starting/paused jobs to be moved. The move uses TransferClaim:
                      the source attempt is only superseded once the destination commits. If the
                      launch fails (no offer, provider error) before the instance is created,
                      the source is untouched. On success, the source process is killed (SSH for
                      on-prem, R2 cancel marker for cloud). Currently only supported with --to new.

Examples:
  weft job move 42 cool100              # Place job 42 on cool100
  weft job move 43 wi872                # Submit job 43 to instance wi872
  weft job move 43 --to auto            # Return job 43 to automatic placement
  weft job move 44 new                  # Launch one new instance for job 44
  weft job move 44 45 46 new            # Launch instance(s) for jobs 44-46
  weft job move 44 45 --to new          # Destination via --to/-t
  weft job move 44:46 --each new        # Separate new instance per job
  weft job move 44:46 --to distinct     # Alias for --each --to new
  weft job move --from cool30 --to new  # Move queued jobs from host cool30 to new instance(s)
  weft job move --from wi872 --to wi900 # Move queued jobs from wi872 to wi900
  weft job move --from myproj --to new  # Move queued jobs from project myproj to new instance(s)
  weft job move --project myproj new    # All queued myproj jobs → new instance`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: runJobMove,
}

var jobStartCmd = &cobra.Command{
	Use:   "start <job-id>...",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobStartNowBare,
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
	Use:   "info <id>...",
	Short: "Show detailed job or instance information",
	Long: `Show full details for jobs or cloud instances.

Prefix with wj... for jobs or wi... for instances.
Unprefixed IDs inherit type only when mixed with a prefixed ID.

Examples:
  weft info wj42
  weft info wi42
  weft info wj42 43
  weft info wi42 43`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runInfo,
}

// Top-level show command (alias for job info)
var showCmd = &cobra.Command{
	Use:   "show <id>...",
	Short: "Show detailed job or instance information",
	Long: `Alias for "weft info". Supports wj... (job) and wi... (instance) IDs.

Examples:
  weft show wj42
  weft show wi42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runInfo,
}

// Top-level start command (alias for job start)
var startCmd = &cobra.Command{
	Use:   "start <wj-id>...",
	Short: "Start a queued job immediately",
	Long: `Start a queued job immediately on its host, bypassing queue order.

This removes the job from the remote queue file, updates the database,
and launches the job right away.

This is an alias for 'job start'. The top-level form requires wj-prefixed IDs;
use 'weft job start' to pass bare numeric IDs.

Examples:
  weft start wj42`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobStartNowExplicitPrefix,
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

var jobDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <job-id>...",
	Short: "Explain why a job is in its current state",
	Long: `Explain the current state of one or more jobs using queue blockers,
placement reasons, lifecycle events, and failure metadata.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runJobDiagnose,
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
	jobCmd.AddCommand(jobTimeseriesCmd)
	jobTimeseriesCmd.Flags().BoolVar(&jobTimeseriesRaw, "raw", false, "Stream the JSONL file as the agent wrote it")
	jobTimeseriesCmd.Flags().BoolVar(&jobTimeseriesSummary, "summary", false, "Print high-water marks only")
	jobTimeseriesCmd.Flags().Int64Var(&jobTimeseriesRunID, "run", 0, "Specific attempt run ID (default: latest)")
	addJobMoveFlags(jobMoveCmd, &jobMoveEach, &jobMoveProject, &jobMoveTo, &jobMoveFrom)
	jobCmd.AddCommand(jobMoveCmd)
	jobCmd.AddCommand(jobDraftCmd)
	jobCmd.AddCommand(jobStartCmd)
	jobCmd.AddCommand(jobInfoCmd)
	jobPriorityCmd.Flags().BoolVar(&jobPriorityClear, "clear", false, "Clear priority instead of setting it")
	jobCmd.AddCommand(jobPriorityCmd)
	jobCmd.AddCommand(jobCancelCmd)
	jobCmd.AddCommand(jobPauseCmd)
	jobCmd.AddCommand(jobResumeCmd)
	jobCmd.AddCommand(jobCleanupCmd)
	jobCmd.AddCommand(jobMarkProcessedCmd)
	jobCmd.AddCommand(jobMarkUnprocessedCmd)
	jobCmd.AddCommand(jobPredictCmd)
	jobCmd.AddCommand(jobUnplaceCmd)
	jobCmd.AddCommand(jobDiagnoseCmd)
	jobCmd.AddCommand(jobTagCmd)
	jobTagCmd.AddCommand(jobTagAddCmd)
	jobTagCmd.AddCommand(jobTagRemoveCmd)

	// Sync flags for job info (shared across jobInfoCmd, infoCmd, showCmd)
	for _, cmd := range []*cobra.Command{jobInfoCmd, infoCmd, showCmd} {
		cmd.Flags().BoolVar(&jobInfoSync, "sync", false, "Perform full sync (30s timeout)")
		cmd.Flags().BoolVar(&jobInfoNoSync, "no-sync", false, "Skip syncing job statuses")
		cmd.Flags().BoolVar(&jobInfoAllAttempts, "all-attempts", false, "List every attempt (default: collapse same-host runs)")
	}

	// Copy flags from run command to job run
	jobRunCmd.Flags().StringVarP(&runDescription, "message", "m", "", "Job description")
	jobRunCmd.Flags().StringVarP(&runDescription, "description", "d", "", "[deprecated: use -m] Job description")
	jobRunCmd.Flags().MarkHidden("description")
	jobRunCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory on remote host (alias: --dir)")
	jobRunCmd.Flags().StringVar(&runProject, "project", "", "Project name (default: repo root name for the working directory)")
	jobRunCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	jobRunCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	jobRunCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated). Reserved tags: 'exclusive' runs alone; 'benchmark-isolation' waits for system-wide idle; 'rental' skips local placement; 'inventory' blocks rental placement")
	jobRunCmd.Flags().StringVar(&runProvider, "provider", "", "Cloud provider for rental placement (vastai or runpod)")
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
	jobDescribeCmd.Flags().StringVar(&describeProvider, "provider", "", "Cloud provider preference for rental placement (vastai or runpod)")

	// Flags for job cleanup
	jobCleanupCmd.Flags().BoolVar(&cleanupSessions, "sessions", false, "Clean finished sessions only")
	jobCleanupCmd.Flags().BoolVar(&cleanupLogs, "logs", false, "Clean log files only")
	jobCleanupCmd.Flags().IntVar(&cleanupOlderThan, "older-than", 7, "Only clean items older than N days")
	jobCleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Preview without actually deleting")

	// Flags for job predict
	jobPredictCmd.Flags().StringVar(&predictHost, "host", "", "Target host")
	jobPredictCmd.Flags().StringVar(&predictProject, "project", "", "Project name")
	jobPredictCmd.Flags().StringVar(&predictGPUClass, "gpu-class", "", "GPU class")
}

func runJobMove(cmd *cobra.Command, args []string) error {
	return runJobMoveOrPlace(args, jobMoveProject, jobMoveEach, false, jobMoveTo, jobMoveFrom, jobMoveTUI, jobMovePlain || jobMoveNoTUI, jobMoveForce)
}

func runJobPriority(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDsForJobCommand(args)
	if err != nil {
		return err
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	level := 1
	if jobPriorityClear {
		level = 0
	}
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
		}
		if job == nil {
			return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
		}
		if err := db.SetJobPriority(database, jobID, level); err != nil {
			return fmt.Errorf("set priority for %s: %w", ids.FormatJobID(jobID), err)
		}
		if level > 0 && job.EffectiveStatus() == db.StatusQueued {
			if job.HasInventoryHost() {
				_ = db.SetQueuedAtBefore(database, jobID, job.Host)
				if result, moveErr := ops.RequestQueuePriority(database, job, ops.DefaultOptions()); moveErr == nil && !result.Deferred {
					_ = syncHostAfterQueueChange(database, job.Host)
				}
			} else if job.Host != "" {
				_ = db.SetQueuedAtBefore(database, jobID, job.Host)
			}
		}
		action := "priority"
		if level == 0 {
			action = "normal priority"
		}
		fmt.Printf("Job %s set to %s\n", ids.FormatJobID(jobID), action)
	}
	return nil
}

func addJobMoveFlags(cmd *cobra.Command, each *bool, project *string, destination *string, from *string) {
	cmd.Flags().BoolVar(each, "each", false, "With 'new'/'create'/'distinct': launch a separate instance per job")
	cmd.Flags().StringVar(project, "project", "", "Select all eligible queued jobs in the named project")
	cmd.Flags().BoolVar(&jobMoveForce, "force", false, "Allow running/starting/paused jobs to be moved. Source attempt is superseded atomically inside the launch; the source process is killed on success (in-progress work is lost). Only supported with --to new.")
	cmd.Flags().BoolVar(&jobMoveTUI, "tui", false, "Force launch progress TUI for move-to-new")
	cmd.Flags().BoolVar(&jobMovePlain, "plain", false, "Force plain text output for move-to-new")
	cmd.Flags().BoolVar(&jobMoveNoTUI, "no-tui", false, "Disable launch progress TUI for move-to-new (alias for --plain)")
	_ = cmd.Flags().MarkHidden("no-tui")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
	cmd.MarkFlagsMutuallyExclusive("tui", "no-tui")
	if destination != nil {
		cmd.Flags().StringVarP(destination, "to", "t", "", "Destination host, instance (wi<N>), or 'new'/'create'/'distinct'")
	}
	if from != nil {
		cmd.Flags().StringVarP(from, "from", "f", "", "Select queued jobs from source instance, host, or project (e.g., wi<N>, cool30, myproj)")
	}
}

func runJobMoveOrPlace(args []string, project string, each bool, unplacedOnly bool, destinationFlag string, from string, forceTUI bool, forcePlain bool, force bool) error {
	dest, jobArgs, err := resolveMoveDestination(args, destinationFlag)
	if err != nil {
		return err
	}
	dest, each = normalizeMoveDestination(dest, each)

	if err := validateMoveSelectors(jobArgs, project, from); err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	eligible, err := resolveEligibleJobs(database, jobArgs, project, from, unplacedOnly, force)
	if err != nil {
		return err
	}

	switch {
	case strings.EqualFold(dest, "auto") || strings.EqualFold(dest, "unplaced"):
		if each {
			return usageErrorf("--each is only valid with 'new', 'create', or 'distinct' destination")
		}
		if force {
			return usageErrorf("--force is not supported with destination 'auto' / 'unplaced'")
		}
		return moveJobsToAuto(database, eligible)
	case strings.EqualFold(dest, "new") || strings.EqualFold(dest, "create"):
		useTUI, err := resolveTUI(forceTUI, forcePlain)
		if err != nil {
			return err
		}
		return moveJobsToNewInstances(database, eligible, each, useTUI, force)
	default:
		if each {
			return usageErrorf("--each is only valid with 'new', 'create', or 'distinct' destination")
		}
		if force {
			return usageErrorf("--force is currently only supported with --to new")
		}
		if instanceID, parseErr := ids.ParseInstanceID(dest); parseErr == nil {
			return moveJobsToInstance(database, eligible, instanceID)
		}
		return moveJobsToHost(database, eligible, dest)
	}
}

func normalizeMoveDestination(dest string, each bool) (string, bool) {
	if strings.EqualFold(strings.TrimSpace(dest), "distinct") {
		return "new", true
	}
	return dest, each
}

func resolveMoveDestination(args []string, destinationFlag string) (string, []string, error) {
	destinationFlag = strings.TrimSpace(destinationFlag)
	if destinationFlag != "" {
		if len(args) == 0 {
			return destinationFlag, nil, nil
		}
		return destinationFlag, args, nil
	}

	if len(args) == 0 {
		return "", nil, usageErrorf("provide destination as final argument or --to")
	}
	return strings.TrimSpace(args[len(args)-1]), args[:len(args)-1], nil
}

func validateMoveSelectors(jobArgs []string, project string, from string) error {
	selectors := 0
	if len(jobArgs) > 0 {
		selectors++
	}
	if strings.TrimSpace(project) != "" {
		selectors++
	}
	if strings.TrimSpace(from) != "" {
		selectors++
	}
	if selectors == 0 {
		return usageErrorf("provide job IDs, --project, or --from")
	}
	if selectors > 1 {
		return usageErrorf("choose exactly one selector mode: job IDs, --project, or --from")
	}
	return nil
}

// resolveEligibleJobs fetches jobs by ID list and/or --project, filtering to
// queued jobs. When unplacedOnly is true (place command), already-placed jobs
// are also skipped. When force is true, running/starting/paused jobs are also
// admitted; the move pipeline will atomically supersede the source attempt via
// TransferClaim and then SSH-kill the source process via
// orchestration.SnapshotForcedSources / TerminateForcedSources.
func resolveEligibleJobs(database *sql.DB, jobArgs []string, project string, from string, unplacedOnly bool, force bool) ([]*db.Job, error) {
	jobIDs, err := ParseJobIDs(jobArgs)
	if err != nil {
		return nil, fmt.Errorf("parse job IDs: %w", err)
	}
	return orchestration.ResolveEligibleJobsWithForce(database, jobIDs, project, from, unplacedOnly, force, orchestration.JobMoveCallbacks{
		OnWarning: func(message string) {
			fmt.Fprintln(os.Stderr, message)
		},
	})
}

func moveJobsToHost(database *sql.DB, jobs []*db.Job, host string) error {
	_, err := orchestration.MoveJobsToHost(database, jobs, host, orchestration.JobMoveCallbacks{
		OnWarning: func(message string) {
			fmt.Fprintln(os.Stderr, message)
		},
		OnMoved: func(jobID int64, _ string) {
			fmt.Printf("Moved job %s → %s\n", ids.FormatJobID(jobID), host)
		},
	})
	if err != nil {
		return err
	}
	if syncErr := syncHostAfterQueueChange(database, host); syncErr != nil {
		reportQueueChangeSyncFailure(host, syncErr)
	}
	return nil
}

func moveJobsToAuto(database *sql.DB, jobs []*db.Job) error {
	errorsList := unplaceJobs(database, jobs, ops.DefaultOptions(), true, unplaceJobCallbacks{
		OnAlreadyUnplaced: func(job *db.Job) {
			fmt.Printf("Job %s already auto\n", ids.FormatJobID(job.ID))
		},
		OnUnplaced: func(job *db.Job, _ ops.Result) {
			fmt.Printf("Moved job %s → auto\n", ids.FormatJobID(job.ID))
		},
	})
	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func moveJobsToInstance(database *sql.DB, jobs []*db.Job, instanceID int64) error {
	_, err := orchestration.MoveJobsToInstance(database, jobs, instanceID, orchestration.JobMoveCallbacks{
		OnWarning: func(message string) {
			fmt.Fprintln(os.Stderr, message)
		},
		OnMoved: func(jobID int64, target string) {
			fmt.Printf("Moved job %s → %s\n", ids.FormatJobID(jobID), target)
		},
	})
	return err
}

func moveJobsToNewInstances(database *sql.DB, jobs []*db.Job, separateEach bool, useLaunchTUI bool, force bool) error {
	if !verbose {
		restore := logging.Suppress()
		defer restore()
	}

	maxAttempts := moveNewMaxAttempts()
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = moveJobsToNewInstancesOnce(database, jobs, separateEach, useLaunchTUI, force)
		if err == nil {
			return nil
		}
		if attempt >= maxAttempts-1 || !orchestration.IsRetryableNewInstanceLaunchError(err) {
			break
		}
		delay, ok := moveNewBackoffDelay(attempt)
		if !ok {
			break
		}
		fmt.Fprintf(os.Stderr, "Move-to-new attempt %d/%d failed: %v\n", attempt+1, maxAttempts, err)
		fmt.Fprintf(os.Stderr, "Retrying move-to-new launch in %s (next attempt %d/%d)\n", delay, attempt+2, maxAttempts)
		if waitErr := moveNewWait(context.Background(), delay); waitErr != nil {
			return waitErr
		}
	}
	if err != nil && maxAttempts > 1 && orchestration.IsRetryableNewInstanceLaunchError(err) {
		return fmt.Errorf("move-to-new launch failed after %d attempts: %w", maxAttempts, err)
	}
	return err
}

func moveJobsToNewInstancesOnce(database *sql.DB, jobs []*db.Job, separateEach bool, useLaunchTUI bool, force bool) error {
	// Keep single-job move-to-new on the same implementation path as TUI move,
	// so behavior and instrumentation stay consistent across interfaces.
	if len(jobs) == 1 && !separateEach {
		res, err := moveQueuedJobToNewInstance(database, jobs[0].ID, force)
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
		fmt.Printf("Moved job %s → %s\n", ids.FormatJobID(jobs[0].ID), target)
		return nil
	}
	var launchTUI *terminal.LaunchProgressTUI
	result, err := moveQueuedJobsToNewInstances(database, jobs, separateEach, force, orchestration.BulkCallbacks{
		OnStatus: func(message string) {
			if shouldPrintMoveLaunchStatus(useLaunchTUI, launchTUI != nil) {
				fmt.Println(message)
			}
		},
		OnWarning: func(message string) {
			if launchTUI != nil {
				launchTUI.SendEvent(campaign.LaunchEvent{
					Kind:  campaign.LaunchEventCampaignStatus,
					Phase: message,
				})
				return
			}
			fmt.Fprintln(os.Stderr, message)
		},
		OnEvent: func(event campaign.LaunchEvent) {
			if launchTUI != nil {
				launchTUI.SendEvent(event)
			}
		},
		OnCampaignCreated: func(campaignID int64, expectedWorkers int) {
			if useLaunchTUI && launchTUI == nil {
				launchTUI = terminal.StartLaunchProgressTUI(campaignID, expectedWorkers)
				return
			}
			if launchTUI != nil {
				launchTUI.SetCampaign(campaignID, expectedWorkers)
			}
		},
	})
	usedLaunchTUI := launchTUI != nil
	if launchTUI != nil {
		if err == nil {
			if stopErr := launchTUI.Complete(result.InstanceIDs); stopErr != nil {
				return stopErr
			}
		} else {
			_ = launchTUI.Stop()
		}
	}
	if err == nil && !usedLaunchTUI {
		for _, id := range result.InstanceIDs {
			fmt.Printf("Launched instance %s\n", ids.FormatInstanceID(id))
		}
	}
	if err == nil && usedLaunchTUI {
		fmt.Fprint(os.Stdout, terminal.FormatMoveExitSummary(database, result))
	}
	return err
}

func shouldPrintMoveLaunchStatus(useLaunchTUI bool, launchTUIStarted bool) bool {
	return !useLaunchTUI && !launchTUIStarted
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

	jobs, errorsList := loadJobsForUnplace(database, jobIDs)
	errorsList = append(errorsList, unplaceJobs(database, jobs, ops.OptionsForMode(ops.TimeoutNormal), false, unplaceJobCallbacks{
		OnUnplaced: func(_ *db.Job, result ops.Result) {
			fmt.Println(result.Message)
		},
	})...)
	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func loadJobsForUnplace(database *sql.DB, jobIDs []int64) ([]*db.Job, []string) {
	jobs := make([]*db.Job, 0, len(jobIDs))
	var errorsList []string
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s not found", ids.FormatJobID(jobID)))
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, errorsList
}

type unplaceJobCallbacks struct {
	OnAlreadyUnplaced func(job *db.Job)
	OnUnplaced        func(job *db.Job, result ops.Result)
}

func unplaceJobs(database *sql.DB, jobs []*db.Job, opts ops.ExecuteOptions, allowAlreadyUnplaced bool, callbacks unplaceJobCallbacks) []string {
	var errorsList []string
	for _, job := range jobs {
		if job == nil {
			errorsList = append(errorsList, "job is nil")
			continue
		}
		if job.TargetKind() == db.JobTargetUnplaced && allowAlreadyUnplaced {
			if callbacks.OnAlreadyUnplaced != nil {
				callbacks.OnAlreadyUnplaced(job)
			}
			continue
		}
		result, err := ops.UnplaceQueuedJob(database, job, opts)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(job.ID), err))
			continue
		}
		if callbacks.OnUnplaced != nil {
			callbacks.OnUnplaced(job, result)
		}
	}
	return errorsList
}

func runJobDiagnose(_ *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobs, errorsList := loadJobsForUnplace(database, jobIDs)
	hydrateQueueBlockedReasons(jobs)
	for i, job := range jobs {
		if i > 0 {
			fmt.Println("---")
		}
		x := explain.ForJob(database, job, time.Now())
		fmt.Println(explain.DiagnoseText(x))
	}
	if len(errorsList) > 0 {
		return fmt.Errorf("%s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runJobStartNowBare(cmd *cobra.Command, args []string) error {
	return runJobStartNowWithParser(cmd, args, ParseJobIDs)
}

func runJobStartNowExplicitPrefix(cmd *cobra.Command, args []string) error {
	return runJobStartNowWithParser(cmd, args, ParseJobIDsForJobCommand)
}

func runJobStartNowWithParser(cmd *cobra.Command, args []string, parser func([]string) ([]int64, error)) error {
	jobIDs, err := parser(args)
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
			errorsList = append(errorsList, fmt.Sprintf("job %s: get job: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s not found", ids.FormatJobID(jobID)))
			continue
		}
		effectiveStatus := job.EffectiveStatus()
		if effectiveStatus != db.StatusQueued {
			errorsList = append(errorsList, fmt.Sprintf("job %s is not queued (status: %s)", ids.FormatJobID(jobID), effectiveStatus))
			continue
		}

		deferred, err := queuejob.StartNow(database, job)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		if deferred {
			fmt.Printf("Job %s saved locally. %s is offline — it will start on the next sync.\n", ids.FormatJobID(jobID), job.Host)
			continue
		}

		fmt.Printf("Job %s started immediately on %s\n", ids.FormatJobID(jobID), job.Host)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

var (
	jobInfoSync        bool
	jobInfoNoSync      bool
	jobInfoAllAttempts bool
)

var quickSyncJobsFunc = targetedSyncJobs
var runJobInfoFromInfoFunc = runJobInfo
var runInstanceStatusFromInfoFunc = runInstanceStatus

func runInfo(cmd *cobra.Command, args []string) error {
	// info/show is a read-only router. Route to instances only when an explicit
	// wi prefix is present; bare numerics (and wj IDs) resolve to jobs, since an
	// instance is always wi-prefixed and a bare number is never an instance.
	sawJob, sawInstance := idPrefixesSeen(args)
	if sawJob && sawInstance {
		return usageErrorf("cannot mix job and instance IDs in one command; use only wj... or only wi...")
	}
	if sawInstance {
		return runInstanceStatusFromInfoFunc(cmd, args)
	}
	return runJobInfoFromInfoFunc(cmd, args)
}

func runJobInfo(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
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
		if len(jobsToSync) > 0 && liveSyncNeededForJobs(database, jobsToSync, jobInfoSync, jobInfoNoSync) {
			timeout := FastSyncTimeout
			cloudTimeout := FastCloudSyncTimeout
			if jobInfoSync {
				timeout = NormalSyncTimeout
				cloudTimeout = NormalCloudSyncTimeout
			}
			doneNotice := func() {}
			if targets := remoteLiveTargets(jobsToSync); targets != "" {
				doneNotice = remoteWaitNotice(cmd, "Refreshing live state from %s; use --no-sync for cached DB state.", targets)
			}
			outcome := quickSyncJobsFunc(database, jobsToSync, timeout, FastSyncHostTimeout, cloudTimeout)
			doneNotice()
			if !outcome.completed() {
				if note := buildTargetedStaleDataNote(database, outcome); note != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), note)
				}
			}
		}
	}

	var errorsList []string
	printed := 0
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: get job: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s not found", ids.FormatJobID(jobID)))
			continue
		}
		if printed > 0 {
			fmt.Println("---")
		}
		printed++
		hydrateQueueBlockedReasons([]*db.Job{job})
		display := queueblock.Display(job, nil)

		// Show full job details
		fmt.Printf("Job ID:      %s\n", ids.FormatJobID(job.ID))
		fmt.Printf("Host:        %s\n", job.TargetDisplay())
		// Show status with waiting info. Tombstoned annotation comes
		// inline so the operator notices it before reading further —
		// otherwise a tombstoned job looks identical to an active queued
		// one and the autopilot's "ignored" decision is invisible.
		tombstone := ""
		if job.Tombstoned {
			tombstone = " [tombstoned, ignored by autopilot]"
		}
		if display.Blocked {
			fmt.Printf("Status:      %s%s\n", display.Status, tombstone)
			// A placement-avenue failure carries a structured launch/reuse
			// breakdown — print every avenue, not just the truncated head of
			// the joined flat string. Single-cause blockers fall through to
			// the merged flat reasons.
			if s := blockreason.ForJob(job); s.IsPlacementFailure() {
				fmt.Printf("Reason:      %s\n", s.Summary)
				for _, line := range s.DetailLines() {
					fmt.Printf("             %s\n", line)
				}
			} else {
				reasons := mergeBlockedReasons(display.Reason, job.PlacementReasons)
				for i, reason := range reasons {
					if i == 0 {
						fmt.Printf("Reason:      %s\n", reason)
					} else {
						fmt.Printf("             %s\n", reason)
					}
				}
			}
		} else {
			statusText := job.EffectiveStatus()
			if statusText == db.StatusQueued && job.TargetKind() == db.JobTargetUnplaced {
				// Bare "queued" reads as "queued on <last attempt's
				// instance>" when an attempt table follows. Spell out
				// that the job is not placed and is waiting for the
				// autopilot to assign a target.
				statusText = "queued (unplaced — awaiting placement)"
			}
			fmt.Printf("Status:      %s%s\n", statusText, tombstone)
		}
		printPlacementLines(queuedPlacementLines(database, job), 12)
		if job.Priority > 0 {
			fmt.Printf("Priority:    %d\n", job.Priority)
		}
		// If the autopilot has paused this job's scope via the runaway
		// breaker, surface the trip details here. The canned blocked
		// reason ("paused: repeated launch failures without progress")
		// gives no actionable info; the structured trip metrics tell
		// the operator which threshold actually fired.
		if job.EffectiveStatus() == "queued" {
			if info, err := campaign.LookupRunawayBreakerForJob(database, job); err == nil && info != nil {
				age := time.Since(info.TrippedAt).Truncate(time.Second)
				fmt.Printf("Blocked by:  runaway-breaker (%s), tripped %s ago\n", info.ScopeLabel(), age)
				fmt.Printf("             %s\n", info.MetricsLine())
				fmt.Printf("             reset: weft autopilot blocked --unblock\n")
			}
		}
		if x := explain.ForJob(database, job, time.Now()); x.SuggestedAction != "" && x.SuggestedAction != "none" {
			fmt.Printf("Explain:     %s\n", x.SuggestedAction)
		}
		fmt.Printf("Description: %s\n", job.Description)
		fmt.Printf("Directory:   %s\n", job.DisplayWorkingDir())
		fmt.Printf("Command:     %s\n", job.Command)
		if len(job.EnvVars) > 0 {
			fmt.Printf("Env Vars:    %s\n", formatEnvVarsForDisplay(job.EnvVars))
		}
		if job.Metadata != nil {
			printDiskPreview(os.Stdout, job.Metadata.Disk)
		}
		if tags := job.DisplayTags(); len(tags) > 0 {
			fmt.Printf("Tags:        %s\n", strings.Join(tags, ", "))
		}
		now := time.Now()
		elapsed := estimate.JobElapsedDuration(job, now)
		if elapsed > 0 {
			fmt.Printf("Elapsed:     %s\n", db.FormatDuration(int64(elapsed.Seconds())))
		}
		estimateTotal, hasEstimate := estimate.JobTimeEstimate(job, database, now)
		if hasEstimate {
			fmt.Printf("Est. Time:   %s\n", estimateTotal.FormatWithBounds())
			if elapsed > 0 {
				eta := estimate.Remaining(estimateTotal, elapsed)
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
		// Torch-derived GPU-runtime constraints apply only to GPU jobs; a
		// CPU-only job must not display an inert "Arch cap" that reads as
		// the placement blocker.
		if job.RequestsGPU() {
			if job.MaxComputeCap != "" && job.MaxComputeCap != placement.MaxComputeCapAny {
				fmt.Printf("Arch cap:    sm_%s (excludes GPUs with higher compute capability)\n", job.MaxComputeCap)
			}
			if line := driverFloorLine(job); line != "" {
				fmt.Printf("Driver floor: %s\n", line)
			}
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
		if reason := humanizeFailureReason(job.FailureReason); reason != "" {
			fmt.Printf("Reason:      %s\n", reason)
		}
		if job.FailureReason == "killed_stdout_silence" {
			fmt.Printf("Hint:        Emit a periodic progress line so the silence watchdog sees output.\n")
			fmt.Printf("             Weft parses any of these formats and displays the parsed progress:\n")
			fmt.Printf("               Progress: 42%%\n")
			fmt.Printf("               Progress: 9/14\n")
			fmt.Printf("               Progress: 9 of 14\n")
			fmt.Printf("             You can also write checkpoints to the job's output dir so the run can be resumed.\n")
		}
		if job.ErrorMessage != "" {
			fmt.Printf("Error:       %s\n", job.ErrorMessage)
		}
		printJobLocalDiagnostics(database, job)
		if rentalSummary, ok := estimate.RentalCostSummary(database, job, now); ok {
			fmt.Printf("Cost:        $%.2f (%s)\n", rentalSummary.Cost, rentalSummary.Basis)
		}
		if progress := jobProgressSummary(database, job); progress != "" {
			fmt.Printf("Progress:    %s\n", progress)
		}

		printAttemptsSection(cmd, database, job)

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
			var stats *db.GPUTelemetryStats
			if job.LatestRunID != nil {
				if summary, telErr := db.GetTimeseriesSummaryByRun(database, *job.LatestRunID); telErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: telemetry: %v\n", telErr)
				} else if summary != nil {
					stats = db.GPUTelemetryStatsFromTimeseriesSummary(summary)
				}
			}
			if stats == nil {
				samples, telErr := db.GetTimeseries(database, job.ID)
				if telErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: telemetry: %v\n", telErr)
				} else if len(samples) > 0 {
					stats = db.ComputeGPUTelemetryStats(samples)
				}
			}
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

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func formatUnixTime(t int64) string {
	return fmt.Sprintf("%s", time.Unix(t, 0).Format("2006-01-02 15:04:05"))
}

// printAttemptsSection prints a per-attempt history table for jobs with
// multiple attempts.
func printAttemptsSection(cmd *cobra.Command, database *sql.DB, job *db.Job) {
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: list attempts: %v\n", err)
		return
	}
	if len(attempts) == 0 {
		return
	}
	now := time.Now().Unix()
	fmt.Println()
	fmt.Printf("Attempts:    %d\n", len(attempts))
	if latest := attempts[0]; latest.StartTime != nil {
		fmt.Printf("Latest:      #%d started %s on %s\n", latest.AttemptNumber, formatUnixTime(*latest.StartTime), attemptTarget(latest))
	} else if latest := attempts[0]; isAttemptPreflightRejected(latest) {
		fmt.Printf("Latest:      #%d preflight-rejected on %s\n", latest.AttemptNumber, attemptTarget(latest))
	}
	if previous := previousInstanceList(attempts); previous != "" {
		fmt.Printf("Previous:    %s\n", previous)
	}
	if len(attempts) < 2 {
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  #\tWhen\tTarget\tStatus\tExit\tDuration\tOutcome")
	if jobInfoAllAttempts {
		for _, a := range attempts {
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				a.AttemptNumber,
				attemptWhen(a),
				attemptTarget(a),
				attemptStatus(a),
				attemptExit(a),
				attemptDuration(a, now),
				attemptOutcome(a),
			)
		}
	} else {
		sessions := groupAttemptsByTarget(attempts)
		folded := 0
		for _, s := range sessions {
			latest := attempts[s.indices[0]]
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				attemptRangeLabel(attempts, s),
				attemptWhen(latest),
				attemptTarget(latest),
				attemptStatus(latest),
				attemptExit(latest),
				attemptDuration(latest, now),
				attemptOutcomeWithFold(attempts, s),
			)
			if len(s.indices) > 1 {
				folded += len(s.indices) - 1
			}
		}
		if folded > 0 {
			fmt.Fprintf(tw, "\n")
			fmt.Fprintf(tw, "  (%d earlier attempts folded; pass --all-attempts to expand)\n", folded)
		}
	}
	tw.Flush()
}

// attemptSession groups consecutive same-target attempts.
type attemptSession struct {
	indices []int
	target  string
}

// groupAttemptsByTarget folds contiguous runs of same-target attempts (as
// returned newest-first by ListAttempts). No-target attempts inherit from
// older neighbors so the (queued, canceled-superseded) intent pair lands
// in the same session.
func groupAttemptsByTarget(attempts []db.JobAttempt) []attemptSession {
	if len(attempts) == 0 {
		return nil
	}
	targets := make([]string, len(attempts))
	for i, a := range attempts {
		t := attemptTarget(a)
		if t == "-" {
			t = ""
		}
		targets[i] = t
	}
	for i := len(targets) - 2; i >= 0; i-- {
		if targets[i] == "" {
			targets[i] = targets[i+1]
		}
	}
	for i := 1; i < len(targets); i++ {
		if targets[i] == "" {
			targets[i] = targets[i-1]
		}
	}
	var sessions []attemptSession
	for i, t := range targets {
		if i == 0 || t != targets[i-1] || t == "" {
			sessions = append(sessions, attemptSession{target: t})
		}
		s := &sessions[len(sessions)-1]
		s.indices = append(s.indices, i)
	}
	return sessions
}

func attemptRangeLabel(attempts []db.JobAttempt, s attemptSession) string {
	if len(s.indices) == 0 {
		return "-"
	}
	if len(s.indices) == 1 {
		return strconv.Itoa(attempts[s.indices[0]].AttemptNumber)
	}
	first := attempts[s.indices[0]].AttemptNumber
	last := attempts[s.indices[len(s.indices)-1]].AttemptNumber
	return fmt.Sprintf("%d-%d", last, first)
}

func attemptOutcomeWithFold(attempts []db.JobAttempt, s attemptSession) string {
	latest := attemptOutcome(attempts[s.indices[0]])
	if len(s.indices) == 1 {
		return latest
	}
	counts := map[string]int{}
	for _, idx := range s.indices[1:] {
		oc := attempts[idx].CloudOutcome
		if oc == "" {
			oc = "queued"
		}
		counts[oc]++
	}
	var parts []string
	for k, n := range counts {
		parts = append(parts, fmt.Sprintf("%dx %s", n, k))
	}
	slices.Sort(parts)
	return latest + " (+" + strings.Join(parts, ", ") + ")"
}

func previousInstanceList(attempts []db.JobAttempt) string {
	var out []string
	seen := map[int64]struct{}{}
	for i, a := range attempts {
		if i == 0 || a.LaunchID == nil {
			continue
		}
		if _, ok := seen[*a.LaunchID]; ok {
			continue
		}
		seen[*a.LaunchID] = struct{}{}
		out = append(out, ids.FormatInstanceID(*a.LaunchID))
	}
	return strings.Join(out, ", ")
}

func attemptWhen(a db.JobAttempt) string {
	switch {
	case a.StartTime != nil:
		return formatUnixTime(*a.StartTime)
	case a.QueuedAt != nil:
		return formatUnixTime(*a.QueuedAt)
	}
	return "-"
}

func attemptTarget(a db.JobAttempt) string {
	if a.LaunchID != nil {
		return ids.FormatInstanceID(*a.LaunchID)
	}
	return dashIfEmpty(a.Host)
}

func attemptExit(a db.JobAttempt) string {
	if a.ExitCode == nil {
		return "-"
	}
	return strconv.Itoa(*a.ExitCode)
}

// attemptDuration returns elapsed wall time. In-flight attempts (started but
// not ended) report time since start so info doesn't show a misleading dash
// for the row that is currently consuming time/cost.
func attemptDuration(a db.JobAttempt, now int64) string {
	if a.StartTime == nil {
		return "-"
	}
	end := now
	if a.EndTime != nil {
		end = *a.EndTime
	}
	d := end - *a.StartTime
	if d < 0 {
		return "-"
	}
	return db.FormatDuration(d)
}

// attemptOutcome surfaces the most informative outcome label, falling back
// from cloud_outcome → failure_reason → error_message when the more
// structured fields are absent (typical for on-prem failed attempts).
func attemptOutcome(a db.JobAttempt) string {
	switch {
	case a.CloudOutcome != "":
		return a.CloudOutcome
	case a.FailureReason != "":
		return a.FailureReason
	case a.ErrorMessage != "":
		return a.ErrorMessage
	}
	return "-"
}

// isAttemptPreflightRejected detects an attempt the runner refused to start
// (e.g. source provenance mismatch). Such attempts have no start_time, no
// exit_code, and a populated failure_reason — distinguishing them from
// genuine exit-1 failures that ran for 0 seconds.
func isAttemptPreflightRejected(a db.JobAttempt) bool {
	return a.Status == db.StatusFailed && a.StartTime == nil && a.ExitCode == nil && a.FailureReason != ""
}

// attemptStatus returns the display label for an attempt's status, mapping
// preflight rejections to a distinct label rather than the underlying
// "failed" stored on the row.
func attemptStatus(a db.JobAttempt) string {
	if isAttemptPreflightRejected(a) {
		return "preflight-rejected"
	}
	return dashIfEmpty(a.Status)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

// driverFloorLine renders the job's effective NVIDIA driver/CUDA floor with
// its provenance, e.g. ">=525 (CUDA >=12.0, from torch 2.9.1+cu128)".
// Returns "" when no floor applies. This is the constraint that actually
// rejects hosts in placement (the arch cap above rarely does), so weft info
// must show it.
func driverFloorLine(job *db.Job) string {
	rf := placement.RuntimeFloorForJob(job)
	driver, cuda := rf.Req.MinDriverVersion, rf.Req.MinCUDAVersion
	if driver <= 0 && cuda == "" {
		return ""
	}
	// A non-empty CUDA floor always carries its origin (MergeInferred and
	// ApplyExplicit set them together).
	detail := ""
	if cuda != "" {
		detail = fmt.Sprintf("(CUDA >=%s, from %s)", cuda, rf.CUDAOrigin)
	}
	if driver > 0 {
		return strings.TrimSpace(fmt.Sprintf(">=%d %s", driver, detail))
	}
	return detail
}
