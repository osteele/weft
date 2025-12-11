package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <host> <command...>",
	Short: "Start a persistent tmux session on a remote host",
	Long: `Start a persistent tmux session on a remote host.

The session continues running even when you disconnect. Use SSH + tmux
to create robust, long-running processes on remote machines.

Examples:
  remote-jobs run cool30 'python train.py'
  remote-jobs run -d "Training GPT-2" cool30 'with-gpu python train.py'
  remote-jobs run -n train-gpt2 -C /mnt/code/LM2 cool30 'python train.py'
  remote-jobs run --queue cool30 'python train.py'`,
	Args: cobra.MinimumNArgs(2),
	RunE: runRun,
}

var (
	runName        string
	runDir         string
	runDescription string
	runQueue       bool
	runQueueOnFail bool
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&runName, "name", "n", "", "Session name (default: auto-generated from command)")
	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path)")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "Description of the job")
	runCmd.Flags().BoolVar(&runQueue, "queue", false, "Queue job for later instead of running now")
	runCmd.Flags().BoolVar(&runQueueOnFail, "queue-on-fail", false, "Queue job if connection fails")
}

func runRun(cmd *cobra.Command, args []string) error {
	host := args[0]
	command := strings.Join(args[1:], " ")

	// Set defaults
	sessionName := runName
	if sessionName == "" {
		sessionName = session.GenerateName(command)
	}

	workingDir := runDir
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

	// Queue-only mode
	if runQueue {
		jobID, err := db.RecordPending(database, host, sessionName, workingDir, command, runDescription)
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}

		fmt.Printf("Job queued with ID: %d\n\n", jobID)
		fmt.Printf("  Host: %s\n", host)
		fmt.Printf("  Session: %s\n", sessionName)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		fmt.Printf("\nTo start this job:\n")
		fmt.Printf("  remote-jobs retry %d\n", jobID)
		return nil
	}

	// Check if session already exists
	exists, err := ssh.TmuxSessionExists(host, sessionName)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) && runQueueOnFail {
			fmt.Println("Connection failed. Queuing job for later...")
			jobID, err := db.RecordPending(database, host, sessionName, workingDir, command, runDescription)
			if err != nil {
				return fmt.Errorf("queue job: %w", err)
			}
			fmt.Printf("Job queued with ID: %d\n\n", jobID)
			fmt.Printf("To retry when connection is available:\n")
			fmt.Printf("  remote-jobs retry %d\n", jobID)
			return nil
		}
		return fmt.Errorf("check session: %w", err)
	}

	if exists {
		fmt.Fprintf(os.Stderr, "ERROR: Session '%s' already exists on %s\n", sessionName, host)
		fmt.Fprintf(os.Stderr, "Attach with: ssh %s -t 'tmux attach -t %s'\n", host, sessionName)
		fmt.Fprintf(os.Stderr, "Or kill with: remote-jobs kill %s %s\n", host, sessionName)
		os.Exit(1)
	}

	// Create file paths
	logFile := session.LogFile(sessionName)
	statusFile := session.StatusFile(sessionName)
	metadataFile := session.MetadataFile(sessionName)

	// Archive any existing log file
	archiveCmd := fmt.Sprintf("if [ -f '%s' ]; then mv '%s' '%s.$(date +%%Y%%m%%d-%%H%%M%%S).log'; fi",
		logFile, logFile, strings.TrimSuffix(logFile, ".log"))
	ssh.RunWithRetry(host, archiveCmd)

	// Remove old status file
	ssh.RunWithRetry(host, fmt.Sprintf("rm -f '%s'", statusFile))

	fmt.Printf("Starting persistent session '%s' on %s\n", sessionName, host)
	fmt.Printf("Working directory: %s\n", workingDir)
	fmt.Printf("Command: %s\n", command)
	if runDescription != "" {
		fmt.Printf("Description: %s\n", runDescription)
	}
	fmt.Printf("Log file: %s\n\n", logFile)

	// Save metadata
	startTime := time.Now().Unix()
	metadata := session.FormatMetadata(workingDir, command, host, runDescription, startTime)

	metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
	if _, _, err := ssh.RunWithRetry(host, metadataCmd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save metadata: %v\n", err)
	}

	// Set up Slack notification if configured
	notifyCmd := ""
	slackWebhook := getSlackWebhook()
	if slackWebhook != "" {
		// Copy notify script to remote
		scriptDir := getScriptDir()
		notifyScript := filepath.Join(scriptDir, "notify-slack.sh")
		remoteNotifyScript := "/tmp/remote-jobs-notify-slack.sh"

		if err := ssh.CopyTo(notifyScript, host, remoteNotifyScript); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to copy notify script: %v\n", err)
		} else {
			ssh.Run(host, fmt.Sprintf("chmod +x '%s'", remoteNotifyScript))
			// Note: \$EXIT_CODE escapes the $ for the outer shell layer
			notifyCmd = fmt.Sprintf("; REMOTE_JOBS_SLACK_WEBHOOK='%s' '%s' '%s' \\$EXIT_CODE '%s'",
				slackWebhook, remoteNotifyScript, sessionName, host)
			fmt.Println("Slack notifications: enabled")
		}
	}

	// Create the wrapped command
	// Redirect output to log, capture exit code (PIPESTATUS[0] gets the command's exit, not tee's)
	// Note: $ must be escaped as \$ since command runs inside double quotes via ssh
	// User command is wrapped in () to ensure all output goes through tee
	wrappedCommand := fmt.Sprintf(
		"cd '%s' && (%s) 2>&1 | tee '%s'; EXIT_CODE=\\${PIPESTATUS[0]}; echo \\$EXIT_CODE > '%s'%s",
		workingDir, command, logFile, statusFile, notifyCmd)

	// Start tmux session
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", sessionName, wrappedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return fmt.Errorf("start session: %s", stderr)
	}

	fmt.Println("✓ Session started successfully")

	// Record in database
	jobID, err := db.RecordStart(database, host, sessionName, workingDir, command, startTime, runDescription)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record job in database: %v\n", err)
	} else {
		fmt.Printf("Job ID: %d\n", jobID)
	}

	fmt.Printf("\nMonitor progress:\n")
	fmt.Printf("  ssh %s -t 'tmux attach -t %s'  # Attach (Ctrl+B D to detach)\n", host, sessionName)
	fmt.Printf("  ssh %s 'tmux capture-pane -t %s -p | tail -20'  # View last 20 lines\n", host, sessionName)
	fmt.Printf("  ssh %s 'tail -f %s'  # Follow log file\n", host, logFile)
	fmt.Printf("\nCheck status:\n")
	fmt.Printf("  remote-jobs status %s %s\n", host, sessionName)

	return nil
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

func getScriptDir() string {
	// Get the directory where the executable is located
	exe, err := os.Executable()
	if err != nil {
		// Fallback to known location
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "code", "utils", "remote-jobs")
	}

	// Resolve symlinks
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "code", "utils", "remote-jobs")
	}

	return filepath.Dir(exe)
}
