package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
  remote-jobs run -C /mnt/code/LM2 cool30 'python train.py'
  remote-jobs run --queue cool30 'python train.py'`,
	Args: cobra.MinimumNArgs(2),
	RunE: runRun,
}

var (
	runDir         string
	runDescription string
	runQueue       bool
	runQueueOnFail bool
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path)")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "Description of the job")
	runCmd.Flags().BoolVar(&runQueue, "queue", false, "Queue job for later instead of running now")
	runCmd.Flags().BoolVar(&runQueueOnFail, "queue-on-fail", false, "Queue job if connection fails")
}

func runRun(cmd *cobra.Command, args []string) error {
	host := args[0]
	command := strings.Join(args[1:], " ")

	// Set defaults
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

	// Create job record first to get the ID
	jobID, err := db.RecordJobStarting(database, host, workingDir, command, runDescription)
	if err != nil {
		return fmt.Errorf("create job record: %w", err)
	}

	// Get the job to access start time
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		return fmt.Errorf("get job: %w", err)
	}

	// Generate tmux session name and file paths from job ID
	tmuxSession := session.TmuxSessionName(jobID)
	logFile := session.LogFile(jobID, job.StartTime)
	statusFile := session.StatusFile(jobID, job.StartTime)
	metadataFile := session.MetadataFile(jobID, job.StartTime)
	pidFile := session.PidFile(jobID, job.StartTime)

	// Test connection and check if session already exists
	exists, err := ssh.TmuxSessionExists(host, tmuxSession)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) && runQueueOnFail {
			// Convert to pending job so it can be retried later
			fmt.Println("Connection failed. Queuing job for later...")
			if err := db.UpdateJobPending(database, jobID); err != nil {
				return fmt.Errorf("queue job: %w", err)
			}
			fmt.Printf("Job queued with ID: %d\n\n", jobID)
			fmt.Printf("To retry when connection is available:\n")
			fmt.Printf("  remote-jobs retry %d\n", jobID)
			return nil
		}
		// Mark job as failed
		db.UpdateJobFailed(database, jobID, err.Error())
		return fmt.Errorf("check session: %w", err)
	}

	if exists {
		// This shouldn't happen with unique job IDs, but handle it anyway
		db.UpdateJobFailed(database, jobID, "Session already exists")
		return fmt.Errorf("session '%s' already exists on %s", tmuxSession, host)
	}

	// Create log directory on remote
	logDir := session.LogDir
	mkdirCmd := fmt.Sprintf("mkdir -p %s", logDir)
	if _, stderr, err := ssh.RunWithRetry(host, mkdirCmd); err != nil {
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, jobID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	fmt.Printf("Starting job %d on %s\n", jobID, host)
	fmt.Printf("Working directory: %s\n", workingDir)
	fmt.Printf("Command: %s\n", command)
	if runDescription != "" {
		fmt.Printf("Description: %s\n", runDescription)
	}
	fmt.Printf("Log file: %s\n\n", logFile)

	// Save metadata
	metadata := session.FormatMetadata(jobID, workingDir, command, host, runDescription, job.StartTime)
	// Don't quote path - it contains ~ which needs shell expansion
	metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
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
			if _, stderr, err := ssh.Run(host, fmt.Sprintf("chmod +x '%s'", remoteNotifyScript)); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to chmod notify script: %s\n", stderr)
			}
			// $EXIT_CODE is expanded by bash when the command runs
			// Pass through optional settings
			envVars := fmt.Sprintf("REMOTE_JOBS_SLACK_WEBHOOK='%s'", slackWebhook)
			if v := os.Getenv("REMOTE_JOBS_SLACK_VERBOSE"); v == "1" {
				envVars += " REMOTE_JOBS_SLACK_VERBOSE=1"
			}
			if v := os.Getenv("REMOTE_JOBS_SLACK_NOTIFY"); v != "" {
				envVars += fmt.Sprintf(" REMOTE_JOBS_SLACK_NOTIFY='%s'", v)
			}
			if v := os.Getenv("REMOTE_JOBS_SLACK_MIN_DURATION"); v != "" {
				envVars += fmt.Sprintf(" REMOTE_JOBS_SLACK_MIN_DURATION='%s'", v)
			}
			notifyCmd = fmt.Sprintf("; %s '%s' 'rj-%d' $EXIT_CODE '%s' '%s'",
				envVars, remoteNotifyScript, jobID, host, metadataFile)
			fmt.Println("Slack notifications: enabled")
		}
	}

	// Create the wrapped command using the common builder (tested for tilde expansion)
	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      jobID,
		WorkingDir: workingDir,
		Command:    command,
		LogFile:    logFile,
		StatusFile: statusFile,
		PidFile:    pidFile,
		NotifyCmd:  notifyCmd,
	})

	// Escape single quotes in wrapped command for embedding in single-quoted string
	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

	// Start tmux session - use single quotes to prevent shell expansion
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		errMsg := ssh.FriendlyError(host, stderr, err)
		db.UpdateJobFailed(database, jobID, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	// Mark job as running
	if err := db.UpdateJobRunning(database, jobID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update job status: %v\n", err)
	}

	fmt.Println("✓ Session started successfully")
	fmt.Printf("Job ID: %d\n", jobID)

	fmt.Printf("\nMonitor progress:\n")
	fmt.Printf("  ssh %s -t 'tmux attach -t %s'  # Attach (Ctrl+B D to detach)\n", host, tmuxSession)
	fmt.Printf("  remote-jobs log %d  # View log\n", jobID)
	fmt.Printf("  remote-jobs log %d -f  # Follow log\n", jobID)
	fmt.Printf("\nCheck status:\n")
	fmt.Printf("  remote-jobs status %d\n", jobID)

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
