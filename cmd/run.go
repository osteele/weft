package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <host> <command>",
	Short: "Start a persistent tmux session on a remote host",
	Long: `Start a persistent tmux session on a remote host.

The session continues running even when you disconnect. Use SSH + tmux
to create robust, long-running processes on remote machines.

Examples:
  remote-jobs run cool30 'python train.py'
  remote-jobs run -d "Training GPT-2" cool30 'with-gpu python train.py'
  remote-jobs run -C /mnt/code/LM2 cool30 'python train.py'
  remote-jobs run -e CUDA_VISIBLE_DEVICES=0 -e BATCH_SIZE=32 cool30 'python train.py'
  remote-jobs run --after 42 cool30 'python eval.py'  # Run after job 42 completes
  remote-jobs run --queue cool30 'python train.py'
  remote-jobs run -f cool30 'python train.py'   # Start and follow log
  remote-jobs run cool30 --kill 42              # Kill job 42`,
	Args: func(cmd *cobra.Command, args []string) error {
		// --kill mode only needs host
		if runKillJobID > 0 {
			if len(args) < 1 {
				return fmt.Errorf("requires host argument")
			}
			return nil
		}
		// Normal mode needs exactly host + command
		if len(args) != 2 {
			return fmt.Errorf("requires exactly host and command arguments")
		}
		return nil
	},
	RunE: runRun,
}

var (
	runDir         string
	runDescription string
	runQueue       bool
	runQueueOnFail bool
	runFollow      bool
	runAllow       bool
	runKillJobID   int64
	runFrom        int64
	runTimeout     string
	runEnvVars     []string
	runAfter       int64
	runAfterAny    int64
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path)")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "Description of the job")
	runCmd.Flags().BoolVar(&runQueue, "queue", false, "Queue job for later instead of running now")
	runCmd.Flags().BoolVar(&runQueueOnFail, "queue-on-fail", false, "Queue job if connection fails")
	runCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	runCmd.Flags().BoolVar(&runAllow, "allow", false, "Stream the job log live and stay attached until interrupted")
	runCmd.Flags().Int64Var(&runKillJobID, "kill", 0, "Kill a job by ID (synonym for 'remote-jobs kill')")
	runCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID (replaces retry)")
	runCmd.Flags().StringVar(&runTimeout, "timeout", "", "Kill job after duration (e.g., \"2h\", \"30m\", \"1h30m\")")
	runCmd.Flags().StringSliceVarP(&runEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	runCmd.Flags().Int64Var(&runAfter, "after", 0, "Start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfterAny, "after-any", 0, "Start job after another job completes, success or failure (implies --queue)")
}

func runRun(cmd *cobra.Command, args []string) error {
	// Handle --kill mode
	if runKillJobID > 0 {
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		return killJob(database, runKillJobID)
	}

	// Open database early for --from support
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var host, command string

	// Handle --from mode: copy settings from existing job
	if runFrom > 0 {
		fromJob, err := db.GetJobByID(database, runFrom)
		if err != nil {
			return fmt.Errorf("get job %d: %w", runFrom, err)
		}
		if fromJob == nil {
			return fmt.Errorf("job %d not found", runFrom)
		}

		// Copy settings from existing job
		host = fromJob.Host
		command = fromJob.Command
		if runDir == "" {
			runDir = fromJob.WorkingDir
		}
		if runDescription == "" {
			runDescription = fromJob.Description
		}

		// Allow overriding host from command line
		if len(args) > 0 {
			host = args[0]
		}
		// Allow overriding command from command line
		if len(args) > 1 {
			command = args[1]
		}
	} else {
		// Normal mode: require host and command
		if len(args) < 2 {
			return fmt.Errorf("usage: remote-jobs run <host> <command>")
		}
		host = args[0]
		command = args[1]
	}

	// Validate flag combinations
	if runFollow && runQueue {
		return fmt.Errorf("--follow cannot be used with --queue")
	}
	if runAllow && runQueue {
		return fmt.Errorf("--allow cannot be used with --queue")
	}
	if runFollow && runAfter > 0 {
		return fmt.Errorf("--follow cannot be used with --after")
	}
	if runAllow && runAfter > 0 {
		return fmt.Errorf("--allow cannot be used with --after")
	}
	if runFollow && runAfterAny > 0 {
		return fmt.Errorf("--follow cannot be used with --after-any")
	}
	if runAllow && runAfterAny > 0 {
		return fmt.Errorf("--allow cannot be used with --after-any")
	}
	if runAfter > 0 && runAfterAny > 0 {
		return fmt.Errorf("cannot use both --after and --after-any")
	}
	if runAllow && runFollow {
		return fmt.Errorf("--allow cannot be used with --follow")
	}

	// --after and --after-any imply queue mode (job added to remote queue for dependency handling)
	if runAfter > 0 || runAfterAny > 0 {
		runQueue = true
	}

	// Parse "cd /path && command" pattern to extract working directory
	// Only if -C/--directory wasn't explicitly provided
	parsedDir, parsedCmd := parseCdPrefix(command)
	if parsedDir != "" && runDir == "" {
		command = parsedCmd
		runDir = parsedDir
	}

	// Set defaults
	workingDir := runDir
	if workingDir == "" {
		var err error
		workingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return fmt.Errorf("get working dir: %w", err)
		}
	}

	// Queue-only mode (including when --after is used)
	if runQueue {
		// When --after or --after-any is specified, use the remote queue system for dependency handling
		if runAfter > 0 || runAfterAny > 0 {
			afterID := runAfter
			afterAny := false
			if runAfterAny > 0 {
				afterID = runAfterAny
				afterAny = true
			}
			jobID, err := queueJob(database, queueJobOptions{
				Host:        host,
				WorkingDir:  workingDir,
				Command:     command,
				Description: runDescription,
				EnvVars:     runEnvVars,
				QueueName:   defaultQueueName,
				AfterJobID:  afterID,
				AfterAny:    afterAny,
			})
			if err != nil {
				return fmt.Errorf("queue job: %w", err)
			}

			waitType := "succeeds"
			if afterAny {
				waitType = "completes"
			}
			fmt.Printf("Job %d added to queue on %s, will run after job %d %s\n\n", jobID, host, afterID, waitType)
			fmt.Printf("  Working dir: %s\n", workingDir)
			fmt.Printf("  Command: %s\n", command)
			if runDescription != "" {
				fmt.Printf("  Description: %s\n", runDescription)
			}
			if len(runEnvVars) > 0 {
				fmt.Printf("  Env vars: %s\n", strings.Join(runEnvVars, ", "))
			}
			fmt.Printf("  After job: %d (%s)\n", afterID, waitType)
			fmt.Printf("\nTo start the queue runner (if not already running):\n")
			fmt.Printf("  remote-jobs queue start %s\n", host)
			return nil
		}

		// Standard local pending mode (no dependency)
		jobID, err := db.RecordPending(database, host, workingDir, command, runDescription)
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}

		fmt.Printf("Job queued with ID: %d\n\n", jobID)
		fmt.Printf("  Host: %s\n", host)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		fmt.Printf("\nTo start this job:\n")
		fmt.Printf("  remote-jobs retry %d\n", jobID)
		return nil
	}

	result, err := startJob(database, startJobOptions{
		Host:        host,
		WorkingDir:  workingDir,
		Command:     command,
		Description: runDescription,
		EnvVars:     runEnvVars,
		Timeout:     runTimeout,
		QueueOnFail: runQueueOnFail,
		OnPrepared: func(info StartJobPreparedInfo) {
			fmt.Printf("Starting job %d on %s\n", info.JobID, info.Host)
			fmt.Printf("Working directory: %s\n", info.WorkingDir)
			fmt.Printf("Command: %s\n", info.Command)
			if info.Description != "" {
				fmt.Printf("Description: %s\n", info.Description)
			}
			fmt.Printf("Log file: %s\n\n", info.LogFile)
		},
	})
	if err != nil {
		return err
	}

	if result.QueuedOnConnectionFailure {
		fmt.Println("Connection failed. Queuing job for later...")
		fmt.Printf("Job queued with ID: %d\n\n", result.Info.JobID)
		fmt.Printf("To retry when connection is available:\n")
		fmt.Printf("  remote-jobs retry %d\n", result.Info.JobID)
		return nil
	}

	if result.SlackEnabled {
		fmt.Println("Slack notifications: enabled")
	}

	fmt.Println("✓ Session started successfully")
	fmt.Printf("Job ID: %d\n", result.Info.JobID)

	if runAllow {
		return streamJobLogAllow(host, result.Info.LogFile, result.Info.JobID)
	}

	if runFollow {
		fmt.Printf("\nFollowing log output (Ctrl+C to stop)...\n\n")
		tailCmd := fmt.Sprintf("tail -n 50 -f %s", result.Info.LogFile)
		sshCmd := exec.Command("ssh", host, tailCmd)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return sshCmd.Run()
	}

	fmt.Printf("\nMonitor progress:\n")
	fmt.Printf("  remote-jobs log %d      # View log\n", result.Info.JobID)
	fmt.Printf("  remote-jobs log %d -f   # Follow log\n", result.Info.JobID)
	fmt.Printf("\nCheck status:\n")
	fmt.Printf("  remote-jobs status %d\n", result.Info.JobID)

	return nil
}

// killJob kills a job by ID (used by --kill flag)

// parseCdPrefix extracts "cd /path && " or "cd /path; " prefix from a command.
// Returns (directory, remaining_command) if found, or ("", original_command) if not.
func parseCdPrefix(command string) (dir string, remaining string) {
	// Match: cd <path> && <rest> or cd <path>; <rest>
	// Path can be quoted or unquoted, may contain ~
	trimmed := strings.TrimSpace(command)

	if !strings.HasPrefix(trimmed, "cd ") {
		return "", command
	}

	// Skip "cd "
	rest := trimmed[3:]

	// Find the path - handle quoted and unquoted paths
	var path string
	var afterPath string

	if strings.HasPrefix(rest, "'") {
		// Single-quoted path
		endQuote := strings.Index(rest[1:], "'")
		if endQuote == -1 {
			return "", command
		}
		path = rest[1 : endQuote+1]
		afterPath = rest[endQuote+2:]
	} else if strings.HasPrefix(rest, "\"") {
		// Double-quoted path
		endQuote := strings.Index(rest[1:], "\"")
		if endQuote == -1 {
			return "", command
		}
		path = rest[1 : endQuote+1]
		afterPath = rest[endQuote+2:]
	} else {
		// Unquoted path - ends at space, &&, or ;
		for i, c := range rest {
			if c == ' ' || c == '&' || c == ';' {
				path = rest[:i]
				afterPath = rest[i:]
				break
			}
		}
		if path == "" {
			// No separator found - just "cd path" with no command after
			return "", command
		}
	}

	// Now look for && or ; separator
	afterPath = strings.TrimSpace(afterPath)
	if strings.HasPrefix(afterPath, "&&") {
		remaining = strings.TrimSpace(afterPath[2:])
		return path, remaining
	} else if strings.HasPrefix(afterPath, ";") {
		remaining = strings.TrimSpace(afterPath[1:])
		return path, remaining
	}

	// No valid separator found
	return "", command
}

func getSlackWebhook() string {
	// Check environment variable first
	if webhook := os.Getenv("REMOTE_JOBS_SLACK_WEBHOOK"); webhook != "" {
		return webhook
	}

	// Check config file
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	configFile := filepath.Join(home, ".config", "remote-jobs", "config")
	content, err := os.ReadFile(configFile)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "SLACK_WEBHOOK=") {
			return strings.TrimPrefix(line, "SLACK_WEBHOOK=")
		}
	}

	return ""
}

func streamJobLogAllow(host, logFile string, jobID int64) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("\nFollowing live output (Ctrl+C to stop streaming; job keeps running)...\n\n")
	waitAndTail := fmt.Sprintf("sh -c 'while [ ! -f %s ]; do sleep 1; done; tail -n +1 -F %s'", logFile, logFile)
	sshCmd := exec.CommandContext(ctx, "ssh", host, waitAndTail)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	sshCmd.Stdin = nil

	err := sshCmd.Run()
	if ctx.Err() != nil {
		fmt.Printf("\nDetached from log stream.\n")
		printDetachedInstructions(jobID)
		return nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nLog streaming stopped with error: %v\n", err)
		printDetachedInstructions(jobID)
		return err
	}

	fmt.Printf("\nLog streaming finished.\n")
	printDetachedInstructions(jobID)
	return nil
}

func printDetachedInstructions(jobID int64) {
	fmt.Printf("Job %d continues running.\n", jobID)
	fmt.Printf("View logs later: remote-jobs log %d -f\n", jobID)
	fmt.Printf("Check status:   remote-jobs job status %d\n", jobID)
}
