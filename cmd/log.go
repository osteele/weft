package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:   "log <job-id>",
	Short: "View log output from a remote job",
	Long: `View the log file for a specific remote job.

Examples:
  remote-jobs log 25           # View log for job #25
  remote-jobs log 25 -f        # Follow job #25's log
  remote-jobs log 25 -n 100    # Last 100 lines`,
	Args: cobra.ExactArgs(1),
	RunE: runLog,
}

var (
	logFollow bool
	logLines  int
)

func init() {
	rootCmd.AddCommand(logCmd)

	logCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	logCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show")
}

func runLog(cmd *cobra.Command, args []string) error {
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

	// Determine log file path based on whether this is an old or new job
	var logFile string
	if job.SessionName != "" {
		// Old job with session name - use legacy path
		logFile = session.LegacyLogFile(job.SessionName)
	} else {
		// New job - use ID-based path
		logFile = session.LogFile(jobID, job.StartTime)
	}

	// Check if log file exists
	exists, err := ssh.RemoteFileExists(job.Host, logFile)
	if err != nil {
		return fmt.Errorf("check log file: %w", err)
	}
	if !exists {
		return fmt.Errorf("log file not found: %s:%s", job.Host, logFile)
	}

	if logFollow {
		// Follow mode - use interactive SSH
		// Don't quote path - it contains ~ which needs shell expansion
		sshCmd := exec.Command("ssh", job.Host, fmt.Sprintf("tail -f %s", logFile))
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return sshCmd.Run()
	}

	// Regular mode - fetch last N lines
	// Don't quote path - it contains ~ which needs shell expansion
	tailCmd := fmt.Sprintf("tail -%d %s", logLines, logFile)
	stdout, stderr, err := ssh.Run(job.Host, tailCmd)
	if err != nil {
		if stderr != "" {
			return fmt.Errorf("read log: %s", stderr)
		}
		return fmt.Errorf("read log: %w", err)
	}

	fmt.Print(stdout)
	return nil
}
