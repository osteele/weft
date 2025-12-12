package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:   "log <job-id> | <host> <session>",
	Short: "View log output from a remote job",
	Long: `View the log file for a specific remote job.

Examples:
  remote-jobs log 25                          # View log for job #25
  remote-jobs log 25 -f                       # Follow job #25's log
  remote-jobs log cool30 train-gpt2           # Last 50 lines
  remote-jobs log cool30 train-gpt2 -f        # Follow (like tail -f)
  remote-jobs log cool30 train-gpt2 -n 100    # Last 100 lines`,
	Args: cobra.RangeArgs(1, 2),
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
	var host, sessionName string

	if len(args) == 1 {
		// Single arg: treat as job ID
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

		host = job.Host
		sessionName = job.SessionName
	} else {
		// Two args: host and session
		host = args[0]
		sessionName = args[1]
	}

	logFile := session.LogFile(sessionName)

	// Check if log file exists
	exists, err := ssh.RemoteFileExists(host, logFile)
	if err != nil {
		return fmt.Errorf("check log file: %w", err)
	}
	if !exists {
		return fmt.Errorf("log file not found: %s:%s", host, logFile)
	}

	// Update database with current status
	database, err := db.Open()
	if err == nil {
		defer database.Close()
		updateJobStatusFromRemote(database, host, sessionName)
	}

	if logFollow {
		// Follow mode - use interactive SSH
		sshCmd := exec.Command("ssh", host, fmt.Sprintf("tail -f '%s'", logFile))
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return sshCmd.Run()
	}

	// Regular mode - fetch last N lines
	tailCmd := fmt.Sprintf("tail -%d '%s'", logLines, logFile)
	stdout, stderr, err := ssh.Run(host, tailCmd)
	if err != nil {
		if stderr != "" {
			return fmt.Errorf("read log: %s", stderr)
		}
		return fmt.Errorf("read log: %w", err)
	}

	fmt.Print(stdout)
	return nil
}

// updateJobStatusFromRemote checks and updates job status
func updateJobStatusFromRemote(database *sql.DB, host, sessionName string) {
	job, err := db.GetJob(database, host, sessionName)
	if err != nil || job == nil {
		return
	}

	if job.Status != db.StatusRunning {
		return
	}

	// Check if session still exists
	exists, err := ssh.TmuxSessionExists(host, sessionName)
	if err != nil {
		return
	}

	if !exists {
		statusFile := session.StatusFile(sessionName)
		content, _ := ssh.ReadRemoteFile(host, statusFile)

		if content != "" {
			exitCode, _ := strconv.Atoi(content)
			endTime := time.Now().Unix()
			db.RecordCompletion(database, host, sessionName, exitCode, endTime)
		} else {
			db.MarkDead(database, host, sessionName)
		}
	}
}
