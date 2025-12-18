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
	Use:     "log <job-id>",
	Aliases: []string{"logs"},
	Short:   "View log output from a remote job",
	Long: `View the log file for a specific remote job.

Examples:
  remote-jobs log 25           # View log for job #25 (last 50 lines)
  remote-jobs log 25 -f        # Follow job #25's log
  remote-jobs log 25 -n 100    # Last 100 lines
  remote-jobs log 25 --from 50           # Lines from 50 onwards
  remote-jobs log 25 --from 50 --to 100  # Lines 50-100
  remote-jobs log 25 --to 100            # First 100 lines
  remote-jobs log 25 --grep error        # Lines containing "error"
  remote-jobs log 25 -f --grep epoch     # Follow, filter for "epoch"`,
	Args: cobra.ExactArgs(1),
	RunE: runLog,
}

var (
	logFollow bool
	logLines  int
	logFrom   int
	logTo     int
	logGrep   string
)

func init() {
	rootCmd.AddCommand(logCmd)

	logCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	logCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show (last N lines)")
	logCmd.Flags().IntVar(&logFrom, "from", 0, "Show lines starting from line N")
	logCmd.Flags().IntVar(&logTo, "to", 0, "Show lines up to line N")
	logCmd.Flags().StringVar(&logGrep, "grep", "", "Filter lines matching pattern")
}

func runLog(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	// Validate flag combinations
	hasLineRange := logFrom > 0 || logTo > 0
	if hasLineRange && cmd.Flags().Changed("lines") {
		return fmt.Errorf("--from/--to cannot be used with -n/--lines")
	}
	if logFollow && logTo > 0 {
		return fmt.Errorf("--follow cannot be used with --to")
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

	// Build the remote command based on flags
	remoteCmd := buildLogCommand(logFile)

	if logFollow {
		// Follow mode - use interactive SSH
		sshCmd := exec.Command("ssh", job.Host, remoteCmd)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return sshCmd.Run()
	}

	// Regular mode
	stdout, stderr, err := ssh.Run(job.Host, remoteCmd)
	if err != nil {
		if stderr != "" {
			return fmt.Errorf("read log: %s", stderr)
		}
		return fmt.Errorf("read log: %w", err)
	}

	fmt.Print(stdout)
	return nil
}

// buildLogCommand constructs the remote command for reading log files
// based on the provided flags (--from, --to, -n, --grep, -f)
func buildLogCommand(logFile string) string {
	var cmd string

	// Determine line selection strategy
	// Priority: --from/--to > -n (default)
	if logFrom > 0 && logTo > 0 {
		// Lines from N to M: tail -n +N | head -n (M-N+1)
		count := logTo - logFrom + 1
		if count < 1 {
			count = 1
		}
		cmd = fmt.Sprintf("tail -n +%d %s | head -n %d", logFrom, logFile, count)
	} else if logFrom > 0 {
		// Lines from N onwards
		if logFollow {
			// For follow mode with --from: get from line N then follow
			cmd = fmt.Sprintf("tail -n +%d -f %s", logFrom, logFile)
		} else {
			cmd = fmt.Sprintf("tail -n +%d %s", logFrom, logFile)
		}
	} else if logTo > 0 {
		// First N lines (up to line N)
		cmd = fmt.Sprintf("head -n %d %s", logTo, logFile)
	} else if logFollow {
		// Follow mode with default or -n lines
		cmd = fmt.Sprintf("tail -n %d -f %s", logLines, logFile)
	} else {
		// Default: last N lines
		cmd = fmt.Sprintf("tail -n %d %s", logLines, logFile)
	}

	// Add grep filter if specified
	if logGrep != "" {
		if logFollow {
			// Use --line-buffered for real-time grep output
			cmd = fmt.Sprintf("%s | grep --line-buffered '%s'", cmd, escapeShellArg(logGrep))
		} else {
			cmd = fmt.Sprintf("%s | grep '%s'", cmd, escapeShellArg(logGrep))
		}
	}

	return cmd
}

// escapeShellArg escapes a string for use in single quotes in shell
func escapeShellArg(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	result := ""
	for _, c := range s {
		if c == '\'' {
			result += "'\\''"
		} else {
			result += string(c)
		}
	}
	return result
}
