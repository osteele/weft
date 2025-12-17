package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

const (
	defaultQueueName = "default"
	queueDir         = "~/.cache/remote-jobs/queue"
	queueRunnerPath  = "~/.cache/remote-jobs/scripts/queue-runner.sh"
)

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage job queues for sequential execution on remote hosts",
	Long: `Manage job queues that run sequentially on remote hosts.

Jobs added to a queue run one after another without requiring the local
machine to stay connected. The queue runner runs in a tmux session on
the remote host.

Subcommands:
  add     Add a job to the queue
  start   Start the queue runner
  stop    Stop the queue runner after current job
  list    List jobs in the queue
  status  Show queue runner status`,
}

var queueAddCmd = &cobra.Command{
	Use:   "add <host> <command...>",
	Short: "Add a job to the queue",
	Long: `Add a job to a remote queue for sequential execution.

The job will be executed when the queue runner reaches it. Jobs run
in FIFO order.

Examples:
  remote-jobs queue add cool30 'python train.py --epochs 100'
  remote-jobs queue add -d "Training run 1" cool30 'python train.py'
  remote-jobs queue add --queue gpu cool30 'python train.py'`,
	Args: cobra.MinimumNArgs(2),
	RunE: runQueueAdd,
}

var queueStartCmd = &cobra.Command{
	Use:   "start <host>",
	Short: "Start the queue runner on a remote host",
	Long: `Start the queue runner on a remote host.

The queue runner processes jobs from the queue file sequentially.
It runs in a tmux session and continues running even when you disconnect.

This command is idempotent - safe to call multiple times.

Examples:
  remote-jobs queue start cool30
  remote-jobs queue start --queue gpu cool30`,
	Args: cobra.ExactArgs(1),
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
	Args: cobra.ExactArgs(1),
	RunE: runQueueStop,
}

var queueListCmd = &cobra.Command{
	Use:   "list <host>",
	Short: "List jobs in the queue",
	Long: `Show jobs waiting in the queue and the currently running job.

Examples:
  remote-jobs queue list cool30
  remote-jobs queue list --queue gpu cool30`,
	Args: cobra.ExactArgs(1),
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
	Args: cobra.ExactArgs(1),
	RunE: runQueueStatus,
}

var (
	queueName        string
	queueDir_        string
	queueDescription string
)

func init() {
	rootCmd.AddCommand(queueCmd)
	queueCmd.AddCommand(queueAddCmd)
	queueCmd.AddCommand(queueStartCmd)
	queueCmd.AddCommand(queueStopCmd)
	queueCmd.AddCommand(queueListCmd)
	queueCmd.AddCommand(queueStatusCmd)

	// Add flags to all subcommands
	for _, cmd := range []*cobra.Command{queueAddCmd, queueStartCmd, queueStopCmd, queueListCmd, queueStatusCmd} {
		cmd.Flags().StringVar(&queueName, "queue", defaultQueueName, "Queue name")
	}

	queueAddCmd.Flags().StringVarP(&queueDir_, "directory", "C", "", "Working directory (default: current directory path)")
	queueAddCmd.Flags().StringVarP(&queueDescription, "description", "d", "", "Description of the job")
}

func runQueueAdd(cmd *cobra.Command, args []string) error {
	host := args[0]
	command := strings.Join(args[1:], " ")

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

	// Record job in database
	jobID, err := db.RecordQueued(database, host, workingDir, command, queueDescription, queueName)
	if err != nil {
		return fmt.Errorf("record job: %w", err)
	}

	// Create queue directory on remote
	remoteQueueDir := queueDir
	mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteQueueDir)
	if _, stderr, err := ssh.Run(host, mkdirCmd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create queue directory: %s\n", stderr)
	}

	// Append job to queue file
	// Format: job_id\tworking_dir\tcommand\tdescription
	queueFile := fmt.Sprintf("%s/%s.queue", remoteQueueDir, queueName)
	jobLine := fmt.Sprintf("%d\t%s\t%s\t%s", jobID, workingDir, command, queueDescription)
	appendCmd := fmt.Sprintf("echo '%s' >> %s", ssh.EscapeForSingleQuotes(jobLine), queueFile)

	if _, stderr, err := ssh.Run(host, appendCmd); err != nil {
		return fmt.Errorf("append to queue: %s", stderr)
	}

	fmt.Printf("Job %d added to queue '%s' on %s\n\n", jobID, queueName, host)
	fmt.Printf("  Working dir: %s\n", workingDir)
	fmt.Printf("  Command: %s\n", command)
	if queueDescription != "" {
		fmt.Printf("  Description: %s\n", queueDescription)
	}
	fmt.Printf("\nTo start the queue runner (if not already running):\n")
	fmt.Printf("  remote-jobs queue start %s", host)
	if queueName != defaultQueueName {
		fmt.Printf(" --queue %s", queueName)
	}
	fmt.Println()

	return nil
}

func runQueueStart(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Check if queue runner is already running
	runnerSession := fmt.Sprintf("rj-queue-%s", queueName)
	exists, err := ssh.TmuxSessionExists(host, runnerSession)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}

	if exists {
		fmt.Printf("Queue runner '%s' is already running on %s\n", queueName, host)
		fmt.Printf("\nTo check status:\n")
		fmt.Printf("  remote-jobs queue status %s", host)
		if queueName != defaultQueueName {
			fmt.Printf(" --queue %s", queueName)
		}
		fmt.Println()
		return nil
	}

	// Create directories on remote
	scriptsDir := "~/.cache/remote-jobs/scripts"
	mkdirCmd := fmt.Sprintf("mkdir -p %s %s", queueDir, scriptsDir)
	if _, stderr, err := ssh.Run(host, mkdirCmd); err != nil {
		return fmt.Errorf("create directories: %s", stderr)
	}

	// Deploy queue runner script
	writeCmd := fmt.Sprintf("cat > %s << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", queueRunnerPath, string(queueRunnerScript))
	if _, stderr, err := ssh.Run(host, writeCmd); err != nil {
		return fmt.Errorf("write queue runner script: %s", stderr)
	}

	// Make script executable
	chmodCmd := fmt.Sprintf("chmod +x %s", queueRunnerPath)
	if _, stderr, err := ssh.Run(host, chmodCmd); err != nil {
		return fmt.Errorf("chmod script: %s", stderr)
	}

	// Deploy notify script if Slack is configured
	slackWebhook := getSlackWebhook()
	if slackWebhook != "" {
		notifyScript := "/tmp/remote-jobs-notify-slack.sh"
		writeNotifyCmd := fmt.Sprintf("cat > '%s' << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", notifyScript, string(notifySlackScript))
		if _, _, err := ssh.Run(host, writeNotifyCmd); err == nil {
			ssh.Run(host, fmt.Sprintf("chmod +x '%s'", notifyScript))
		}
	}

	// Build environment variables for the runner
	envVars := ""
	if slackWebhook != "" {
		envVars = fmt.Sprintf("REMOTE_JOBS_SLACK_WEBHOOK='%s' ", slackWebhook)
		if v := os.Getenv("REMOTE_JOBS_SLACK_VERBOSE"); v == "1" {
			envVars += "REMOTE_JOBS_SLACK_VERBOSE=1 "
		}
		if v := os.Getenv("REMOTE_JOBS_SLACK_NOTIFY"); v != "" {
			envVars += fmt.Sprintf("REMOTE_JOBS_SLACK_NOTIFY='%s' ", v)
		}
		if v := os.Getenv("REMOTE_JOBS_SLACK_MIN_DURATION"); v != "" {
			envVars += fmt.Sprintf("REMOTE_JOBS_SLACK_MIN_DURATION='%s' ", v)
		}
	}

	// Start queue runner in tmux
	// Need to expand tilde for the script path
	runnerCmd := fmt.Sprintf("%s$HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", envVars, queueName)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", runnerSession, ssh.EscapeForSingleQuotes(runnerCmd))

	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return fmt.Errorf("start queue runner: %s", stderr)
	}

	fmt.Printf("Queue runner '%s' started on %s\n", queueName, host)
	fmt.Printf("Session: %s\n\n", runnerSession)
	fmt.Printf("Monitor:\n")
	fmt.Printf("  ssh %s -t 'tmux attach -t %s'  # Attach (Ctrl+B D to detach)\n", host, runnerSession)
	fmt.Printf("  remote-jobs queue status %s", host)
	if queueName != defaultQueueName {
		fmt.Printf(" --queue %s", queueName)
	}
	fmt.Println()

	return nil
}

func runQueueStop(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Create stop signal file
	stopFile := fmt.Sprintf("%s/%s.stop", queueDir, queueName)
	touchCmd := fmt.Sprintf("touch %s", stopFile)

	if _, stderr, err := ssh.Run(host, touchCmd); err != nil {
		return fmt.Errorf("create stop signal: %s", stderr)
	}

	fmt.Printf("Stop signal sent to queue '%s' on %s\n", queueName, host)
	fmt.Println("The queue runner will exit after the current job completes.")

	return nil
}

func runQueueList(cmd *cobra.Command, args []string) error {
	host := args[0]

	// Get currently running job
	currentFile := fmt.Sprintf("%s/%s.current", queueDir, queueName)
	currentID, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", currentFile))
	currentID = strings.TrimSpace(currentID)

	// Get queue contents
	queueFile := fmt.Sprintf("%s/%s.queue", queueDir, queueName)
	queueContents, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", queueFile))

	// Parse and display queue
	fmt.Printf("Queue '%s' on %s:\n\n", queueName, host)

	if currentID != "" {
		fmt.Printf("Currently running: Job %s\n\n", currentID)
	} else {
		fmt.Println("Currently running: (none)")
		fmt.Println()
	}

	lines := strings.Split(strings.TrimSpace(queueContents), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		fmt.Println("Queue is empty")
	} else {
		fmt.Printf("Waiting (%d jobs):\n", len(lines))
		for i, line := range lines {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 4)
			if len(parts) >= 3 {
				jobID := parts[0]
				command := parseEffectiveCommand(parts[2])
				description := ""
				if len(parts) >= 4 {
					description = parts[3]
				}
				if description != "" {
					fmt.Printf("  %d. [%s] %s - %s\n", i+1, jobID, description, truncate(command, 40))
				} else {
					fmt.Printf("  %d. [%s] %s\n", i+1, jobID, truncate(command, 60))
				}
			}
		}
	}

	return nil
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
	currentFile := fmt.Sprintf("%s/%s.current", queueDir, queueName)
	currentID, _, _ := ssh.Run(host, fmt.Sprintf("cat %s 2>/dev/null || true", currentFile))
	currentID = strings.TrimSpace(currentID)

	if currentID != "" {
		fmt.Printf("Current job: %s\n", currentID)
	} else {
		fmt.Println("Current job: (none)")
	}

	// Get queue depth
	queueFile := fmt.Sprintf("%s/%s.queue", queueDir, queueName)
	countOutput, _, _ := ssh.Run(host, fmt.Sprintf("wc -l < %s 2>/dev/null || echo 0", queueFile))
	countOutput = strings.TrimSpace(countOutput)
	fmt.Printf("Jobs waiting: %s\n", countOutput)

	// Check for stop signal
	stopFile := fmt.Sprintf("%s/%s.stop", queueDir, queueName)
	stopExists, _, _ := ssh.Run(host, fmt.Sprintf("test -f %s && echo yes || echo no", stopFile))
	if strings.TrimSpace(stopExists) == "yes" {
		fmt.Println("\nSTOP signal pending - runner will exit after current job")
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
