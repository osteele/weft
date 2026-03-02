package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuefile"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/slack"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

const (
	defaultQueueName = queuefile.DefaultQueueName
)

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage job queues for sequential execution on remote hosts",
	Long: `Manage job queues that run sequentially on remote hosts.

Jobs added to a queue run one after another without requiring the local
machine to stay connected. The queue runner runs in the background on
the remote host.

Subcommands:
  add     Add a job to the queue
  edit    Alias for 'weft edit'
  remove  Remove a queued job before it starts
  start   Start the queue runner
  stop    Stop the queue runner after current job
  list    List jobs in the queue
  status  Show queue runner status
  update  Update the queue runner script on a host`,
}

var queueAddCmd = &cobra.Command{
	Use:   "add [host] <command>",
	Short: "Add a job to the queue",
	Long: `Add a job to a remote queue for sequential execution.

The job will be executed when the queue runner reaches it. Jobs run
in FIFO order.

Examples:
  weft queue add cool30 'python train.py --epochs 100'
  weft queue add --host cool30 'python train.py --epochs 100'
  weft queue add -m "Training run 1" cool30 'python train.py'
  weft queue add -e CUDA_VISIBLE_DEVICES=0 cool30 'python train.py'
  weft queue add --after 42 cool30 'python eval.py'  # Run after job 42 completes`,
	Args: usageArgs(cobra.RangeArgs(1, 2)),
	RunE: runQueueAdd,
}

var queueStartCmd = &cobra.Command{
	Use:   "start [host]",
	Short: "Start the queue runner on a remote host",
	Long: `Start the queue runner on a remote host.

The queue runner processes jobs from the queue file sequentially.
It continues running even when you disconnect.

This command is idempotent - safe to call multiple times.

Examples:
  weft queue start cool30
  weft queue start --host cool30`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runQueueStart,
}

var queueStopCmd = &cobra.Command{
	Use:   "stop [host]",
	Short: "Stop the queue runner after current job",
	Long: `Stop the queue runner after the current job completes.

This sends a stop signal that the runner will detect after the current
job finishes. The runner will exit gracefully.

Examples:
  weft queue stop cool30
  weft queue stop --host cool30`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runQueueStop,
}

var queueListCmd = &cobra.Command{
	Use:   "list [host]",
	Short: "List jobs in the queue",
	Long: `Show jobs waiting in the queue and the currently running job.

Examples:
  weft queue list cool30
  weft queue list --host cool30`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runQueueList,
}

var queueStatusCmd = &cobra.Command{
	Use:   "status [host]",
	Short: "Show queue runner status",
	Long: `Show the status of the queue runner on a remote host.

Displays whether the runner is active, current job (if any), and queue depth.

Examples:
  weft queue status cool30
  weft queue status --host cool30`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runQueueStatus,
}

var queueUpdateCmd = &cobra.Command{
	Use:   "update [host]",
	Short: "Update the queue runner script on a remote host",
	Long: `Update the queue runner script on a remote host.

If the script was updated and the runner is currently running, the command
will attempt a short restart so the new version is picked up.

Examples:
  weft queue update cool30
  weft queue update --host cool30`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runQueueUpdate,
}

var queueRemoveCmd = &cobra.Command{
	Use:   "remove <job-id>...",
	Short: "Remove one or more queued jobs",
	Long: `Remove jobs from the queue before they start.

This removes jobs from both the remote queue file and the local database.
Only works for jobs that haven't started yet (status: queued).

Examples:
  weft queue remove 123
  weft queue remove 123 124 125`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runQueueRemove,
}

var queueFrontCmd = &cobra.Command{
	Use:   "front <job-id>",
	Short: "Move a queued job to the front of the queue",
	Long: `Move a queued job to the front of the queue so it runs next.

The job will run immediately after the currently running job completes.
Only works for jobs that haven't started yet (status: queued).

Examples:
  weft queue front 123`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueFront,
}

var editCmd = &cobra.Command{
	Use:   "edit <job-id>",
	Short: "Edit queued job metadata",
	Long: `Edit a queued job's description, command, directory, environment variables, or dependencies.

Examples:
  weft edit 1595 --depends-on 1599
  weft edit 1595 --command "python eval.py"
  weft edit 1595 --env FOO=bar --env BAZ=qux`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runEdit,
}

var queueEditCmd = &cobra.Command{
	Use:   "edit <job-id>",
	Short: "Alias for 'weft edit'",
	Long:  "Alias for 'weft edit'. All flags are shared with the top-level command.",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runEdit,
}

var (
	queueDir_           string
	queueDescription    string
	queueEnvVars        []string
	queueTags           []string
	queueAfter          int64
	queueAfterAny       int64
	queueNoStart        bool
	queueDraft          bool
	queueWait           bool
	queueNoWait         bool
	queueEditDepends    []string
	queueEditDependsAny []string
	queueEditClearDeps  bool
	editMessage         string
	editCommand         string
	editDirectory       string
	editEnvVars         []string
	editClearEnv        bool
	editStatus          string
	editRetry           bool
	editGPUClass        string
	queueHost           string // Shared --host flag for queue subcommands
)

// allowedStatusTransitions defines which status transitions are valid for the edit command.
// These are statuses that can be transitioned TO queued.
var requeueableStatuses = map[string]bool{
	db.StatusKilled:   true,
	db.StatusDead:     true,
	db.StatusFailed:   true,
	db.StatusCanceled: true,
}

// addQueueHostFlag adds the --host flag to a queue subcommand
func addQueueHostFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&queueHost, "host", "", "Target host")
}

// resolveQueueHost resolves host from --host flag or positional argument.
// For commands that take host as first positional arg.
func resolveQueueHost(args []string) (string, error) {
	if queueHost != "" {
		return queueHost, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return "", fmt.Errorf("host is required (provide as argument or use --host)")
}

func init() {
	rootCmd.AddCommand(queueCmd)
	rootCmd.AddCommand(editCmd)
	queueCmd.AddCommand(queueAddCmd)
	queueCmd.AddCommand(queueStartCmd)
	queueCmd.AddCommand(queueStopCmd)
	queueCmd.AddCommand(queueListCmd)
	queueCmd.AddCommand(queueStatusCmd)
	queueCmd.AddCommand(queueUpdateCmd)
	queueCmd.AddCommand(queueRemoveCmd)
	queueCmd.AddCommand(queueFrontCmd)
	queueCmd.AddCommand(queueEditCmd)
	addEditFlags(editCmd)
	addEditFlags(queueEditCmd)

	// Add --host flag to queue subcommands
	addQueueHostFlag(queueAddCmd)
	addQueueHostFlag(queueStartCmd)
	addQueueHostFlag(queueStopCmd)
	addQueueHostFlag(queueListCmd)
	addQueueHostFlag(queueStatusCmd)
	addQueueHostFlag(queueUpdateCmd)

	queueAddCmd.Flags().StringVarP(&queueDir_, "directory", "C", "", "Working directory (default: current directory path)")
	queueAddCmd.Flags().StringVarP(&queueDescription, "message", "m", "", "Description of the job")
	queueAddCmd.Flags().StringVarP(&queueDescription, "description", "d", "", "[deprecated: use -m] Description of the job")
	queueAddCmd.Flags().MarkHidden("description")
	queueAddCmd.Flags().StringSliceVarP(&queueEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	queueAddCmd.Flags().StringSliceVar(&queueTags, "tag", nil, "Tag to attach to the job (can be repeated). Special: 'exclusive' makes job run alone; 'benchmark' waits for system-wide idle")
	queueAddCmd.Flags().Int64Var(&queueAfter, "after", 0, "Start job after another job succeeds (job ID)")
	queueAddCmd.Flags().Int64Var(&queueAfter, "depends-on", 0, "Alias for --after; start job after another job succeeds (job ID)")
	queueAddCmd.Flags().Int64Var(&queueAfterAny, "after-any", 0, "Start job after another job completes, success or failure (job ID)")
	queueAddCmd.Flags().BoolVar(&queueNoStart, "no-start", false, "Don't auto-start the queue runner")
	queueAddCmd.Flags().BoolVar(&queueDraft, "draft", false, "Create the job in draft status without syncing to the remote queue")
	queueAddCmd.Flags().BoolVar(&queueWait, "wait", false, "Wait for job to complete before returning")
	queueAddCmd.Flags().BoolVar(&queueNoWait, "no-wait", false, "Don't wait for job (default behavior, for explicit acknowledgment)")

}

func runQueueAdd(cmd *cobra.Command, args []string) error {
	var host, command string
	if queueHost != "" {
		// --host flag used, all args are the command
		host = queueHost
		command = strings.Join(args, " ")
	} else if len(args) >= 2 {
		// Positional: first arg is host, rest is command
		host = args[0]
		command = strings.Join(args[1:], " ")
	} else {
		return fmt.Errorf("requires host (via --host or first argument) and command")
	}

	// Validate command against blocked patterns
	cfg, _ := config.Load()
	setUsageHintsFromConfig(cfg)
	if err := cfg.ValidateCommand(command); err != nil {
		return err
	}

	// Print recommendations for common patterns
	printCommandRecommendations(command)

	// Set defaults
	workingDir := queueDir_
	if workingDir == "" {
		var err error
		workingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return fmt.Errorf("get working dir: %w", err)
		}
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if queueAfter > 0 && queueAfterAny > 0 {
		return fmt.Errorf("cannot use both --after/--depends-on and --after-any")
	}
	if queueWait && queueNoWait {
		return fmt.Errorf("--wait and --no-wait cannot be used together")
	}
	if queueWait && queueDraft {
		return fmt.Errorf("--wait cannot be combined with --draft")
	}

	var deps []queueDependency
	if queueAfter > 0 {
		if err := ensureSameHostDependency(database, queueAfter, host); err != nil {
			return err
		}
		deps = append(deps, queueDependency{JobID: queueAfter, AllowFailure: false})
	}
	if queueAfterAny > 0 {
		if err := ensureSameHostDependency(database, queueAfterAny, host); err != nil {
			return err
		}
		deps = append(deps, queueDependency{JobID: queueAfterAny, AllowFailure: true})
	}

	if queueDraft {
		gpu := extractGPUFromEnvVars(queueEnvVars)
		depSpec := encodeQueueDependencies(deps)
		jobID, err := db.RecordDraftJob(database, host, workingDir, command, queueDescription, gpu, depSpec)
		if err != nil {
			return fmt.Errorf("record draft job: %w", err)
		}
		backend, err := ops.ResolveBackend(host, 5*time.Second)
		if err != nil {
			return fmt.Errorf("resolve backend: %w", err)
		}
		if err := db.SetJobBackend(database, jobID, backend); err != nil {
			return fmt.Errorf("set job backend: %w", err)
		}
		if err := db.SetJobEnvVars(database, jobID, queueEnvVars); err != nil {
			db.DeleteJob(database, jobID)
			return fmt.Errorf("record draft env vars: %w", err)
		}
		if len(queueTags) > 0 {
			if err := db.SetJobTags(database, jobID, queueTags); err != nil {
				db.DeleteJob(database, jobID)
				return fmt.Errorf("record draft tags: %w", err)
			}
		}
		fmt.Printf("Draft job #%d saved for %s\n\n", jobID, host)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if queueDescription != "" {
			fmt.Printf("  Description: %s\n", queueDescription)
		}
		if len(queueEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", strings.Join(queueEnvVars, ", "))
		}
		if len(queueTags) > 0 {
			fmt.Printf("  Tags: %s\n", strings.Join(queueTags, ", "))
		}
		if queueAfter > 0 {
			fmt.Printf("  After job: %d (will wait for success when queued)\n", queueAfter)
		}
		if queueAfterAny > 0 {
			fmt.Printf("  After job: %d (will wait for completion when queued)\n", queueAfterAny)
		}
		return nil
	}

	result, err := queueJob(database, queueJobOptions{
		Host:         host,
		WorkingDir:   workingDir,
		Command:      command,
		Description:  queueDescription,
		EnvVars:      queueEnvVars,
		Tags:         queueTags,
		Dependencies: deps,
		AutoStart:    !queueNoStart,
	})
	if err != nil {
		return err
	}
	jobID := result.JobID

	fmt.Printf("Job %d added to queue on %s\n\n", jobID, host)
	fmt.Printf("  Working dir: %s\n", workingDir)
	fmt.Printf("  Command: %s\n", command)
	if queueDescription != "" {
		fmt.Printf("  Description: %s\n", queueDescription)
	}
	if len(queueEnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(queueEnvVars, ", "))
	}
	if len(queueTags) > 0 {
		fmt.Printf("  Tags: %s\n", strings.Join(queueTags, ", "))
	}
	if queueAfter > 0 {
		fmt.Printf("  After job: %d (will wait for success)\n", queueAfter)
	}
	if queueAfterAny > 0 {
		fmt.Printf("  After job: %d (will wait for completion)\n", queueAfterAny)
	}

	// Handle --wait: block until job completes
	if queueWait {
		return waitForQueuedJobCompletion(database, result.JobID, result.Deferred)
	}

	// Auto-start queue runner unless --no-start is specified
	if result.Deferred {
		fmt.Printf("\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
		return nil
	}

	if !queueNoStart {
		backend, err := ops.ResolveBackend(host, 5*time.Second)
		if err == nil && backend == db.BackendSlurm {
			return nil
		}
		_, err = ensureQueueRunnerStarted(host, defaultQueueName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nWarning: failed to start queue runner: %v\n", err)
		}
	}

	return nil
}

// ensureQueueRunnerStarted checks if the queue runner is running and starts it if not.
// It also ensures the agent binary is up-to-date before starting.
// Returns (true, nil) if the runner was started, (false, nil) if already running,
// or (false, error) if starting failed.
func ensureQueueRunnerStarted(host, queue string) (bool, error) {
	queue = defaultQueueName

	// Deploy agent binary if out of date
	if spec := inventory.FindHost(host); spec != nil {
		agentdeploy.EnsureAgentUpToDate(host, *spec) //nolint:errcheck // best-effort
	}

	// Deploy notify script if Slack is configured
	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)

	// Build environment variables for the runner
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host)
	return runner.EnsureStarted(envVars)
}

func runQueueStart(cmd *cobra.Command, args []string) error {
	host, err := resolveQueueHost(args)
	if err != nil {
		return err
	}

	_, err = ensureQueueRunnerStarted(host, defaultQueueName)
	return err
}

func runQueueStop(cmd *cobra.Command, args []string) error {
	host, err := resolveQueueHost(args)
	if err != nil {
		return err
	}

	runner := queuerunner.NewRunner(host)
	return runner.SendStopSignal()
}

func runQueueList(cmd *cobra.Command, args []string) error {
	host, err := resolveQueueHost(args)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	completed, unreachable := performFastSyncForHosts(database, []string{host}, false)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
	}

	// Query jobs from database - this is the source of truth
	// Get queued jobs for this host/queue
	queuedJobs, err := db.ListQueued(database, host, defaultQueueName)
	if err != nil {
		return fmt.Errorf("list queued jobs: %w", err)
	}

	// Get running jobs for this host (running jobs may have been started from this queue)
	runningJobs, err := db.ListRunning(database, host)
	if err != nil {
		return fmt.Errorf("list running jobs: %w", err)
	}

	// Filter running jobs to only include those from this queue
	var jobs []*db.Job
	for _, job := range runningJobs {
		jobs = append(jobs, job)
	}
	jobs = append(jobs, queuedJobs...)

	if len(jobs) == 0 {
		fmt.Printf("No jobs in queue on %s\n", host)
		return nil
	}

	// Print in tabular format matching `list` command
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tHOST\tSTATUS\tCOMMAND / DESCRIPTION")

	for _, job := range jobs {
		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		if len(display) > 40 {
			display = display[:39] + "…"
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
			job.ID, host, job.Status, display)
	}

	return w.Flush()
}

func runQueueStatus(cmd *cobra.Command, args []string) error {
	host, err := resolveQueueHost(args)
	if err != nil {
		return err
	}

	runnerSession := fmt.Sprintf("weft-queue-%s", defaultQueueName)

	// Check if runner is active
	exists, err := ssh.TmuxSessionExists(host, runnerSession)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}

	fmt.Printf("Queue on %s:\n\n", host)

	if exists {
		fmt.Println("Runner: ACTIVE")
	} else {
		fmt.Println("Runner: STOPPED")
	}

	// Get currently running job
	currentFile := fmt.Sprintf("%s/%s.current", queuerunner.QueueDir(), defaultQueueName)
	currentID, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", currentFile))
	currentID = strings.TrimSpace(currentID)

	if currentID != "" {
		fmt.Printf("Current job: %s\n", currentID)
	} else {
		fmt.Println("Current job: (none)")
	}

	// Get queue depth
	stateFile := fmt.Sprintf("%s/%s.state.json", queuerunner.QueueDir(), defaultQueueName)
	countOutput, _, _ := ssh.Run(host, fmt.Sprintf("jq -r '.pending | length // 0' %s 2>/dev/null || echo 0", stateFile))
	countOutput = strings.TrimSpace(countOutput)
	fmt.Printf("Jobs waiting: %s\n", countOutput)

	// Check for stop signal
	stopFile := fmt.Sprintf("%s/%s.stop", queuerunner.QueueDir(), defaultQueueName)
	stopExists, _, _ := ssh.Run(host, fmt.Sprintf("test -f %s && echo yes || echo no", stopFile))
	if strings.TrimSpace(stopExists) == "yes" {
		fmt.Println("\nSTOP signal pending - runner will exit after current job")
	}

	return nil
}

func runQueueUpdate(cmd *cobra.Command, args []string) error {
	host, err := resolveQueueHost(args)
	if err != nil {
		return err
	}

	fmt.Printf("The queue runner is now a Go binary. Use 'weft sync %s' to deploy the latest agent binary.\n", host)
	return nil
}

func runQueueRemove(cmd *cobra.Command, args []string) error {
	// Open database
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	var errors []string
	for _, jobID := range jobIDs {
		// Get job from database
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: database error: %v", jobID, err))
			continue
		}
		if job == nil {
			errors = append(errors, fmt.Sprintf("job %d not found", jobID))
			continue
		}

		// Check if job is queued (not yet started)
		effectiveStatus := job.EffectiveStatus()
		if effectiveStatus != db.StatusQueued {
			errors = append(errors, fmt.Sprintf("job %d has status '%s', can only remove queued jobs", jobID, effectiveStatus))
			continue
		}

		result, err := ops.CancelQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutNormal))
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		if result.Deferred {
			fmt.Printf("Job %d marked for removal on next sync\n", jobID)
		} else {
			fmt.Printf("Job %d removed from queue on %s\n", jobID, job.Host)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}

func runQueueFront(cmd *cobra.Command, args []string) error {
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
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusQueued {
		return fmt.Errorf("job %d has status '%s', can only move queued jobs", jobID, effectiveStatus)
	}

	result, err := ops.RequestQueuePriority(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	if result.Deferred {
		fmt.Printf("Job %d saved locally. %s is offline — it will move to the front automatically when the host is reachable.\n", jobID, job.Host)
		_ = syncHostAfterQueueChange(database, job.Host)
		return nil
	}
	if result.Moved {
		fmt.Printf("Job %d moved to front of queue on %s\n", jobID, job.Host)
	} else {
		fmt.Printf("Job %d is already at the front of queue on %s\n", jobID, job.Host)
	}
	if syncErr := syncHostAfterQueueChange(database, job.Host); syncErr != nil {
		reportQueueChangeSyncFailure(job.Host, syncErr)
	}
	return nil
}

func runEdit(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	statusChanged := cmd.Flags().Changed("status") || editRetry
	dependsChanged := cmd.Flags().Changed("depends-on") || cmd.Flags().Changed("depends-on-any")
	envChanged := cmd.Flags().Changed("env") || editClearEnv
	gpuClassChanged := cmd.Flags().Changed("gpu-class")
	fieldChanged := cmd.Flags().Changed("message") || cmd.Flags().Changed("command") ||
		cmd.Flags().Changed("directory") || envChanged || dependsChanged || queueEditClearDeps || statusChanged || gpuClassChanged
	if !fieldChanged {
		return usageErrorf("no changes specified; use --message/--command/--directory/--env/--status/--retry/--gpu-class or dependency flags")
	}
	if queueEditClearDeps && dependsChanged {
		return fmt.Errorf("cannot combine --clear-depends with --depends-on flags")
	}
	if editClearEnv && cmd.Flags().Changed("env") {
		return fmt.Errorf("cannot combine --env and --clear-env")
	}
	if editRetry && cmd.Flags().Changed("status") {
		return fmt.Errorf("cannot combine --retry with --status")
	}

	// Validate status flag if provided (--retry is equivalent to --status=queued)
	if cmd.Flags().Changed("status") {
		if editStatus != db.StatusQueued {
			return fmt.Errorf("only --status=queued is supported")
		}
	}
	if editRetry {
		editStatus = db.StatusQueued
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

	// Handle status change (requeue)
	effectiveStatus := job.EffectiveStatus()
	if statusChanged && editStatus == db.StatusQueued {
		if effectiveStatus == db.StatusQueued {
			return fmt.Errorf("job %d is already queued", jobID)
		}
		if !requeueableStatuses[effectiveStatus] {
			return fmt.Errorf("cannot change job %d from '%s' to 'queued'; only killed/dead/failed/canceled jobs can be requeued", jobID, effectiveStatus)
		}
	} else if effectiveStatus != db.StatusQueued {
		return fmt.Errorf("job %d has status '%s', can only edit queued jobs", jobID, effectiveStatus)
	}

	var updates []string

	// Handle status change first (requeue), since other field updates require job to be queued
	var wasRequeued bool
	var oldStatus string
	if statusChanged && editStatus == db.StatusQueued {
		oldStatus = job.Status
		if err := db.RequeueByID(database, jobID); err != nil {
			return fmt.Errorf("update status to queued: %w", err)
		}
		job.Status = db.StatusQueued
		wasRequeued = true
		updates = append(updates, fmt.Sprintf("status: %s → queued", oldStatus))
	}

	if cmd.Flags().Changed("message") {
		if err := db.UpdateJobDescription(database, jobID, editMessage); err != nil {
			return fmt.Errorf("update description: %w", err)
		}
		job.Description = editMessage
		if editMessage == "" {
			updates = append(updates, "description cleared")
		} else {
			updates = append(updates, fmt.Sprintf("description: %s", editMessage))
		}
	}

	if cmd.Flags().Changed("directory") {
		if err := db.UpdateJobWorkingDir(database, jobID, editDirectory); err != nil {
			return fmt.Errorf("update directory: %w", err)
		}
		job.WorkingDir = editDirectory
		updates = append(updates, fmt.Sprintf("directory: %s", editDirectory))
	}

	if cmd.Flags().Changed("command") {
		if err := db.UpdateJobCommand(database, jobID, editCommand); err != nil {
			return fmt.Errorf("update command: %w", err)
		}
		job.Command = editCommand
		updates = append(updates, fmt.Sprintf("command: %s", editCommand))
	}

	if envChanged {
		var newEnv []string
		if !editClearEnv {
			newEnv = append([]string(nil), editEnvVars...)
		}
		if err := db.SetJobEnvVars(database, jobID, newEnv); err != nil {
			return fmt.Errorf("update env vars: %w", err)
		}
		job.EnvVars = newEnv
		// Update GPU field to match CUDA_VISIBLE_DEVICES in env vars
		gpu := extractGPUFromEnvVars(newEnv)
		if err := db.SetJobGPU(database, jobID, gpu); err != nil {
			return fmt.Errorf("update gpu from env: %w", err)
		}
		job.GPU = gpu
		if len(newEnv) == 0 {
			updates = append(updates, "env vars cleared")
		} else {
			updates = append(updates, fmt.Sprintf("env vars: %s", strings.Join(newEnv, ", ")))
		}
	}

	if gpuClassChanged {
		if err := db.SetJobGPUClass(database, jobID, editGPUClass); err != nil {
			return fmt.Errorf("update GPU class: %w", err)
		}
		job.GPUClass = editGPUClass
		// Clear GPU device pin when setting a class, so the scheduler picks the best device
		if editGPUClass != "" && !envChanged {
			if err := db.SetJobGPU(database, jobID, ""); err != nil {
				return fmt.Errorf("clear GPU device: %w", err)
			}
			job.GPU = ""
			// Remove CUDA_VISIBLE_DEVICES from env vars since class-based scheduling sets it
			var filteredEnv []string
			for _, e := range job.EnvVars {
				if !strings.HasPrefix(e, "CUDA_VISIBLE_DEVICES=") {
					filteredEnv = append(filteredEnv, e)
				}
			}
			if len(filteredEnv) != len(job.EnvVars) {
				if err := db.SetJobEnvVars(database, jobID, filteredEnv); err != nil {
					return fmt.Errorf("update env vars: %w", err)
				}
				job.EnvVars = filteredEnv
			}
		}
		if editGPUClass == "" {
			updates = append(updates, "GPU class cleared")
		} else {
			updates = append(updates, fmt.Sprintf("GPU class: %s", editGPUClass))
		}
	}

	var depSpec string
	var deps []queueDependency
	switch {
	case queueEditClearDeps:
		depSpec = ""
	case dependsChanged:
		depValues := queueEditDepends
		if !cmd.Flags().Changed("depends-on") {
			depValues = nil
		}
		anyValues := queueEditDependsAny
		if !cmd.Flags().Changed("depends-on-any") {
			anyValues = nil
		}
		deps, err = buildQueueEditDependencies(database, job.Host, job.ID, depValues, anyValues)
		if err != nil {
			return err
		}
		depSpec = encodeQueueDependencies(deps)
	default:
		depSpec = job.DepSpec
		deps = decodeQueueDependencies(job.DepSpec)
	}

	if depSpec != job.DepSpec {
		if err := db.SetJobDepSpec(database, jobID, depSpec); err != nil {
			return fmt.Errorf("update job dependencies: %w", err)
		}
		job.DepSpec = depSpec
		if depSpec == "" {
			updates = append(updates, "dependencies cleared")
		} else {
			updates = append(updates, "dependencies: "+formatQueueDependencies(deps))
		}
	}

	// Push to remote queue
	deferredUpdate := false
	if wasRequeued {
		// Reload job to get all fields (including tags, CPU allotment)
		job, err = db.GetJobByID(database, jobID)
		if err != nil {
			return fmt.Errorf("reload job: %w", err)
		}
		if err := ops.AppendJobToQueue(job, ops.DefaultOptions().Timeout); err != nil {
			// Best effort - job is queued locally, sync will eventually push it
			fmt.Fprintf(os.Stderr, "Job saved locally. %s is offline — changes will be applied automatically when the host is reachable.\n", job.Host)
			deferredUpdate = true
		} else {
			_ = db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusQueued)
			_ = db.SetQueuedAtNow(database, job.ID)
		}
	} else {
		// Job was already queued - update existing entry via sync path
		job.DepSpec = depSpec
		result, err := ops.RequestQueueUpdate(database, job, ops.DefaultOptions())
		if err != nil {
			return err
		}
		if result.Deferred {
			fmt.Printf("Job %d saved locally. %s is offline — changes will be applied automatically when the host is reachable.\n", job.ID, job.Host)
			deferredUpdate = true
		}
	}

	if deferredUpdate {
		fmt.Printf("Updated job %d locally (will apply to %s when reachable)\n", jobID, job.Host)
	} else {
		fmt.Printf("Updated job %d in queue on %s\n", jobID, job.Host)
	}
	for _, update := range updates {
		fmt.Printf("  %s\n", update)
	}
	if len(updates) == 0 {
		fmt.Println("  (no metadata fields changed)")
	}
	if syncErr := syncHostAfterQueueChange(database, job.Host); syncErr != nil && !deferredUpdate {
		reportQueueChangeSyncFailure(job.Host, syncErr)
	}
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// parseEffectiveCommand extracts the command from "cd dir && command" patterns.
// Returns the command after "&&" if pattern matches, or the original command.
func parseEffectiveCommand(command string) string {
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "cd ") {
		return command
	}
	andIdx := strings.Index(cmd, " && ")
	if andIdx == -1 {
		return command
	}
	return strings.TrimSpace(cmd[andIdx+4:])
}

func buildQueueEditDependencies(database *sql.DB, host string, targetJobID int64, successVals, anyVals []string) ([]queueDependency, error) {
	var deps []queueDependency
	seen := map[int64]bool{}

	add := func(values []string, defaultAllowFailure bool) error {
		for _, raw := range values {
			for _, part := range splitDependencyValues(raw) {
				allowFailure := defaultAllowFailure
				value := part

				if strings.HasSuffix(value, "+") {
					allowFailure = true
					value = strings.TrimSuffix(value, "+")
				} else if idx := strings.Index(value, ":"); idx != -1 {
					mode := strings.ToLower(strings.TrimSpace(value[idx+1:]))
					value = strings.TrimSpace(value[:idx])
					switch mode {
					case "any", "complete", "completion":
						allowFailure = true
					case "success", "":
						allowFailure = false
					default:
						return fmt.Errorf("unknown dependency mode %q in %q", mode, part)
					}
				}

				if value == "" {
					continue
				}
				depID, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return fmt.Errorf("invalid dependency job ID: %q", part)
				}
				if depID == targetJobID {
					return fmt.Errorf("job %d cannot depend on itself", targetJobID)
				}
				if seen[depID] {
					continue
				}
				if err := ensureSameHostDependency(database, depID, host); err != nil {
					return err
				}
				seen[depID] = true
				deps = append(deps, queueDependency{JobID: depID, AllowFailure: allowFailure})
			}
		}
		return nil
	}

	if err := add(successVals, false); err != nil {
		return nil, err
	}
	if err := add(anyVals, true); err != nil {
		return nil, err
	}
	return deps, nil
}

func splitDependencyValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func formatQueueDependencies(deps []queueDependency) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, dep := range deps {
		mode := "success"
		if dep.AllowFailure {
			mode = "completion"
		}
		parts = append(parts, fmt.Sprintf("%d (%s)", dep.JobID, mode))
	}
	return strings.Join(parts, ", ")
}

func decodeQueueDependencies(spec string) []queueDependency {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	chunks := strings.Split(spec, ",")
	deps := make([]queueDependency, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		allowFailure := false
		if strings.HasSuffix(chunk, ":any") {
			allowFailure = true
			chunk = strings.TrimSuffix(chunk, ":any")
		}
		id, err := strconv.ParseInt(chunk, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		deps = append(deps, queueDependency{JobID: id, AllowFailure: allowFailure})
	}
	return deps
}

func addEditFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&editMessage, "message", "m", "", "Set job description")
	cmd.Flags().StringVarP(&editDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	cmd.Flags().StringVar(&editCommand, "command", "", "Set command (queued jobs only)")
	cmd.Flags().StringSliceVarP(&editEnvVars, "env", "e", nil, "Replace environment variables (VAR=value)")
	cmd.Flags().BoolVar(&editClearEnv, "clear-env", false, "Remove all environment variables")
	cmd.Flags().StringSliceVar(&queueEditDepends, "depends-on", nil, "Wait for these job IDs to succeed before running (comma-separated or repeated)")
	cmd.Flags().StringSliceVar(&queueEditDependsAny, "depends-on-any", nil, "Wait for these job IDs to finish (success or failure)")
	cmd.Flags().BoolVar(&queueEditClearDeps, "clear-depends", false, "Remove all dependencies from the job")
	cmd.Flags().StringVar(&editStatus, "status", "", "Change job status (only 'queued' is allowed, from killed/dead/failed/canceled)")
	cmd.Flags().BoolVar(&editRetry, "retry", false, "Requeue the job (shorthand for --status=queued)")
	cmd.Flags().StringVar(&editGPUClass, "gpu-class", "", "GPU class or generation (e.g., a100, ampere, ampere+); '+' means that generation or newer")
}
