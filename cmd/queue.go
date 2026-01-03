package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/queuerunner"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/slack"
	"github.com/osteele/remote-jobs/internal/ssh"
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
  remove  Remove a queued job before it starts
  start   Start the queue runner
  stop    Stop the queue runner after current job
  list    List jobs in the queue
  status  Show queue runner status
  upgrade Restart the queue runner if the script is outdated`,
}

var queueAddCmd = &cobra.Command{
	Use:   "add <host> <command>",
	Short: "Add a job to the queue",
	Long: `Add a job to a remote queue for sequential execution.

The job will be executed when the queue runner reaches it. Jobs run
in FIFO order.

Examples:
  remote-jobs queue add cool30 'python train.py --epochs 100'
  remote-jobs queue add -m "Training run 1" cool30 'python train.py'
  remote-jobs queue add -e CUDA_VISIBLE_DEVICES=0 cool30 'python train.py'
  remote-jobs queue add --after 42 cool30 'python eval.py'  # Run after job 42 completes
  remote-jobs queue add --queue gpu cool30 'python train.py'`,
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runQueueAdd,
}

var queueStartCmd = &cobra.Command{
	Use:   "start <host>",
	Short: "Start the queue runner on a remote host",
	Long: `Start the queue runner on a remote host.

The queue runner processes jobs from the queue file sequentially.
It continues running even when you disconnect.

This command is idempotent - safe to call multiple times.

Examples:
  remote-jobs queue start cool30
  remote-jobs queue start --queue gpu cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueStart,
}

var queueStopCmd = &cobra.Command{
	Use:   "stop <host>",
	Short: "Stop the queue runner after current job",
	Long: `Stop the queue runner after the current job completes.

This sends a stop signal that the runner will detect after the current
job finishes. The runner will exit gracefully.

Examples:
  remote-jobs queue stop cool30
  remote-jobs queue stop --queue gpu cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueStop,
}

var queueListCmd = &cobra.Command{
	Use:   "list <host>",
	Short: "List jobs in the queue",
	Long: `Show jobs waiting in the queue and the currently running job.

Examples:
  remote-jobs queue list cool30
  remote-jobs queue list --queue gpu cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueList,
}

var queueStatusCmd = &cobra.Command{
	Use:   "status <host>",
	Short: "Show queue runner status",
	Long: `Show the status of the queue runner on a remote host.

Displays whether the runner is active, current job (if any), and queue depth.

Examples:
  remote-jobs queue status cool30
  remote-jobs queue status --queue gpu cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueStatus,
}

var queueUpgradeCmd = &cobra.Command{
	Use:   "upgrade <host>",
	Short: "Restart queue runner if the remote script is outdated",
	Long: `Restart the queue runner on a host if the remote script is outdated.

This checks the embedded queue runner build number against the remote copy.
If they differ, the runner is stopped, the script is redeployed, and the
runner is restarted.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueUpgrade,
}

var queueRemoveCmd = &cobra.Command{
	Use:   "remove <job-id>...",
	Short: "Remove one or more queued jobs",
	Long: `Remove jobs from the queue before they start.

This removes jobs from both the remote queue file and the local database.
Only works for jobs that haven't started yet (status: queued).

Examples:
  remote-jobs queue remove 123
  remote-jobs queue remove 123 124 125
  remote-jobs queue remove --queue gpu 456`,
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
  remote-jobs queue front 123
  remote-jobs queue front --queue gpu 456`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runQueueFront,
}

var (
	queueName        string
	queueDir_        string
	queueDescription string
	queueEnvVars     []string
	queueAfter       int64
	queueAfterAny    int64
	queueNoStart     bool
	queueDraft       bool
)

func init() {
	rootCmd.AddCommand(queueCmd)
	queueCmd.AddCommand(queueAddCmd)
	queueCmd.AddCommand(queueStartCmd)
	queueCmd.AddCommand(queueStopCmd)
	queueCmd.AddCommand(queueListCmd)
	queueCmd.AddCommand(queueStatusCmd)
	queueCmd.AddCommand(queueUpgradeCmd)
	queueCmd.AddCommand(queueRemoveCmd)
	queueCmd.AddCommand(queueFrontCmd)

	// Add flags to all subcommands
	for _, cmd := range []*cobra.Command{queueAddCmd, queueStartCmd, queueStopCmd, queueListCmd, queueStatusCmd, queueUpgradeCmd, queueRemoveCmd, queueFrontCmd} {
		cmd.Flags().StringVar(&queueName, "queue", defaultQueueName, "Queue name")
	}

	queueAddCmd.Flags().StringVarP(&queueDir_, "directory", "C", "", "Working directory (default: current directory path)")
	queueAddCmd.Flags().StringVarP(&queueDescription, "message", "m", "", "Description of the job")
	queueAddCmd.Flags().StringVarP(&queueDescription, "description", "d", "", "[deprecated: use -m] Description of the job")
	queueAddCmd.Flags().MarkHidden("description")
	queueAddCmd.Flags().StringSliceVarP(&queueEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	queueAddCmd.Flags().Int64Var(&queueAfter, "after", 0, "Start job after another job succeeds (job ID)")
	queueAddCmd.Flags().Int64Var(&queueAfterAny, "after-any", 0, "Start job after another job completes, success or failure (job ID)")
	queueAddCmd.Flags().BoolVar(&queueNoStart, "no-start", false, "Don't auto-start the queue runner")
	queueAddCmd.Flags().BoolVar(&queueDraft, "draft", false, "Create the job in draft status without syncing to the remote queue")
}

func runQueueAdd(cmd *cobra.Command, args []string) error {
	host := args[0]
	command := args[1]

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
		return fmt.Errorf("cannot use both --after and --after-any")
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
		jobID, err := db.RecordDraftJob(database, host, workingDir, command, queueDescription, queueName, gpu, depSpec)
		if err != nil {
			return fmt.Errorf("record draft job: %w", err)
		}
		fmt.Printf("Draft job #%d saved for %s in queue '%s'\n\n", jobID, host, queueName)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if queueDescription != "" {
			fmt.Printf("  Description: %s\n", queueDescription)
		}
		if len(queueEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", strings.Join(queueEnvVars, ", "))
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
		QueueName:    queueName,
		Dependencies: deps,
		AutoStart:    !queueNoStart,
	})
	if err != nil {
		return err
	}
	jobID := result.JobID

	fmt.Printf("Job %d added to queue '%s' on %s\n\n", jobID, queueName, host)
	fmt.Printf("  Working dir: %s\n", workingDir)
	fmt.Printf("  Command: %s\n", command)
	if queueDescription != "" {
		fmt.Printf("  Description: %s\n", queueDescription)
	}
	if len(queueEnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(queueEnvVars, ", "))
	}
	if queueAfter > 0 {
		fmt.Printf("  After job: %d (will wait for success)\n", queueAfter)
	}
	if queueAfterAny > 0 {
		fmt.Printf("  After job: %d (will wait for completion)\n", queueAfterAny)
	}

	// Auto-start queue runner unless --no-start is specified
	if result.Deferred {
		fmt.Printf("\nHost %s is unreachable. This job will be appended to queue '%s' when the host is reachable again.\n", host, queueName)
		fmt.Printf("Run `remote-jobs sync --sync` after %s is online to retry, or wait for the next automatic sync.\n", host)
		return nil
	}

	if !queueNoStart {
		started, err := ensureQueueRunnerStarted(host, queueName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nWarning: failed to start queue runner: %v\n", err)
			fmt.Printf("\nTo start the queue runner manually:\n")
			fmt.Printf("  remote-jobs queue start %s", host)
			if queueName != defaultQueueName {
				fmt.Printf(" --queue %s", queueName)
			}
			fmt.Println()
		} else if started {
			fmt.Printf("\nQueue runner started automatically.\n")
		}
	}

	return nil
}

// ensureQueueRunnerStarted checks if the queue runner is running and starts it if not.
// Returns (true, nil) if the runner was started, (false, nil) if already running,
// or (false, error) if starting failed.
func ensureQueueRunnerStarted(host, queue string) (bool, error) {
	// Deploy notify script if Slack is configured
	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)

	// Build environment variables for the runner
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host, queue)
	return runner.EnsureStarted(envVars)
}

func runQueueStart(cmd *cobra.Command, args []string) error {
	host := args[0]

	started, err := ensureQueueRunnerStarted(host, queueName)
	if err != nil {
		return err
	}

	runnerSession := fmt.Sprintf("rj-queue-%s", queueName)
	if started {
		fmt.Printf("Queue runner '%s' started on %s\n", queueName, host)
		fmt.Printf("Session: %s\n\n", runnerSession)
		if usageHintsEnabled() {
			fmt.Printf("Monitor:\n")
			fmt.Printf("  remote-jobs queue status %s", host)
			if queueName != defaultQueueName {
				fmt.Printf(" --queue %s", queueName)
			}
			fmt.Println()
		}
	} else {
		fmt.Printf("Queue runner '%s' is already running on %s\n", queueName, host)
		if usageHintsEnabled() {
			fmt.Printf("\nTo check status:\n")
			fmt.Printf("  remote-jobs queue status %s", host)
			if queueName != defaultQueueName {
				fmt.Printf(" --queue %s", queueName)
			}
			fmt.Println()
		}
	}

	return nil
}

func runQueueStop(cmd *cobra.Command, args []string) error {
	host := args[0]

	runner := queuerunner.NewRunner(host, queueName)
	if err := runner.SendStopSignal(); err != nil {
		return err
	}
	fmt.Printf("Stop signal sent to queue '%s' on %s\n", queueName, host)
	fmt.Println("The queue runner will exit after the current job completes.")

	return nil
}

func runQueueList(cmd *cobra.Command, args []string) error {
	host := args[0]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get currently running job
	currentFile := fmt.Sprintf("%s/%s.current", queuerunner.QueueDir(), queueName)
	currentID, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", currentFile))
	currentID = strings.TrimSpace(currentID)

	// Get queue contents
	queueFile := fmt.Sprintf("%s/%s.queue", queuerunner.QueueDir(), queueName)
	queueContents, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", queueFile))

	// Collect all job entries (current + waiting)
	type queueEntry struct {
		jobID       string
		command     string
		description string
		status      string
	}
	var entries []queueEntry

	// Add currently running job
	if currentID != "" {
		entry := queueEntry{
			jobID:  currentID,
			status: "running",
		}
		// Look up job details from database
		if jobID, err := strconv.ParseInt(currentID, 10, 64); err == nil {
			if job, err := db.GetJobByID(database, jobID); err == nil && job != nil {
				entry.command = job.EffectiveCommand()
				entry.description = job.Description
			}
		}
		entries = append(entries, entry)
	}

	// Add waiting jobs
	lines := strings.Split(strings.TrimSpace(queueContents), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) >= 3 {
			entry := queueEntry{
				jobID:   parts[0],
				command: parseEffectiveCommand(parts[2]),
				status:  "queued",
			}
			if len(parts) >= 4 {
				entry.description = parts[3]
			}
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		fmt.Printf("No jobs in queue '%s' on %s\n", queueName, host)
		return nil
	}

	// Print in tabular format matching `list` command
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tHOST\tSTATUS\tCOMMAND / DESCRIPTION")

	for _, entry := range entries {
		display := entry.description
		if display == "" {
			display = entry.command
		}
		if len(display) > 40 {
			display = display[:39] + "…"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			entry.jobID, host, entry.status, display)
	}

	return w.Flush()
}

func runQueueStatus(cmd *cobra.Command, args []string) error {
	host := args[0]

	runnerSession := fmt.Sprintf("rj-queue-%s", queueName)

	// Check if runner is active
	exists, err := ssh.TmuxSessionExists(host, runnerSession)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}

	fmt.Printf("Queue '%s' on %s:\n\n", queueName, host)

	if exists {
		fmt.Println("Runner: ACTIVE")
	} else {
		fmt.Println("Runner: STOPPED")
	}

	// Get currently running job
	currentFile := fmt.Sprintf("%s/%s.current", queuerunner.QueueDir(), queueName)
	currentID, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", currentFile))
	currentID = strings.TrimSpace(currentID)

	if currentID != "" {
		fmt.Printf("Current job: %s\n", currentID)
	} else {
		fmt.Println("Current job: (none)")
	}

	// Get queue depth
	queueFile := fmt.Sprintf("%s/%s.queue", queuerunner.QueueDir(), queueName)
	countOutput, _, _ := ssh.Run(host, fmt.Sprintf("wc -l < %s 2>/dev/null || echo 0", queueFile))
	countOutput = strings.TrimSpace(countOutput)
	fmt.Printf("Jobs waiting: %s\n", countOutput)

	// Check for stop signal
	stopFile := fmt.Sprintf("%s/%s.stop", queuerunner.QueueDir(), queueName)
	stopExists, _, _ := ssh.Run(host, fmt.Sprintf("test -f %s && echo yes || echo no", stopFile))
	if strings.TrimSpace(stopExists) == "yes" {
		fmt.Println("\nSTOP signal pending - runner will exit after current job")
	}

	return nil
}

func runQueueUpgrade(cmd *cobra.Command, args []string) error {
	host := args[0]

	runner := queuerunner.NewRunner(host, queueName)
	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)
	envPrefix := slack.BuildRunnerEnvPrefix(slackWebhook)

	result, err := runner.Upgrade(envPrefix, 2*time.Minute)
	if err != nil {
		return err
	}
	if result.RemoteError != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", result.RemoteError)
	}

	switch {
	case result.SkippedNewer:
		fmt.Printf("Queue runner on %s is newer (remote build %d, local build %d); skipping downgrade\n", host, result.RemoteBuild, result.LocalBuild)
	case result.AlreadyUpToDate:
		fmt.Printf("Queue runner on %s already up to date (build %d)\n", host, result.LocalBuild)
	default:
		fmt.Printf("Updating queue runner on %s (remote build %d, local build %d)\n", host, result.RemoteBuild, result.LocalBuild)
		if result.Started {
			fmt.Println("Queue runner restarted with the latest script.")
		} else {
			fmt.Println("Queue runner already running with the latest script.")
		}
	}

	return nil
}

func runQueueRemove(cmd *cobra.Command, args []string) error {
	// Open database
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
		if job.Status != db.StatusQueued {
			errors = append(errors, fmt.Sprintf("job %d has status '%s', can only remove queued jobs", jobID, job.Status))
			continue
		}

		// Determine queue name
		jobQueueName := job.QueueName
		if jobQueueName == "" {
			jobQueueName = queueName // use --queue flag or default
		}

		// Remove from remote queue file
		// The queue file format is: job_id\tworking_dir\tcommand\tdescription\tenv_vars\tdependencies
		// We filter out lines starting with this job ID
		queueFile := fmt.Sprintf("%s/%s.queue", queuerunner.QueueDir(), jobQueueName)
		removeCmd := fmt.Sprintf("grep -v '^%d\\t' %s > %s.tmp 2>/dev/null && mv %s.tmp %s || true",
			jobID, queueFile, queueFile, queueFile, queueFile)

		_, stderr, err := ssh.Run(job.Host, removeCmd)
		if err != nil {
			if ssh.IsConnectionError(stderr) {
				// Host unreachable - mark as dead and let reconciliation handle it
				if err := db.MarkDeadByID(database, jobID); err != nil {
					errors = append(errors, fmt.Sprintf("job %d: failed to mark as dead: %v", jobID, err))
					continue
				}
				if err := db.SetPendingStatus(database, jobID, db.StatusDead); err != nil {
					errors = append(errors, fmt.Sprintf("job %d: failed to set pending status: %v", jobID, err))
					continue
				}
				fmt.Printf("Job %d marked for removal on next sync\n", jobID)
				continue
			}
			// Non-connection error - don't remove from DB
			errors = append(errors, fmt.Sprintf("job %d: failed to remove from remote queue: %s", jobID, strings.TrimSpace(stderr)))
			continue
		}

		// Delete from local database only after successful remote removal
		if err := db.DeleteJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: delete failed: %v", jobID, err))
			continue
		}

		fmt.Printf("Job %d removed from queue '%s' on %s\n", jobID, jobQueueName, job.Host)
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
	if job.Status != db.StatusQueued {
		return fmt.Errorf("job %d has status '%s', can only move queued jobs", jobID, job.Status)
	}

	// Determine queue name
	jobQueueName := job.QueueName
	if jobQueueName == "" {
		jobQueueName = queueName
	}

	moved, err := queuefile.MoveToFront(job.Host, jobQueueName, jobID)
	if err != nil {
		if queuefile.IsConnectionError(err) {
			return fmt.Errorf("host %s unreachable", job.Host)
		}
		return err
	}

	if moved {
		fmt.Printf("Job %d moved to front of queue '%s' on %s\n", jobID, jobQueueName, job.Host)
	} else {
		fmt.Printf("Job %d is already at the front of queue '%s' on %s\n", jobID, jobQueueName, job.Host)
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
