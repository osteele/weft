package cmd

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <host>",
	Short: "Clean up finished sessions and old log files",
	Long: `Clean up finished sessions and old log files on a remote host.

Examples:
  remote-jobs cleanup cool30                    # Clean both
  remote-jobs cleanup cool30 --sessions         # Only finished sessions
  remote-jobs cleanup cool30 --logs --older-than 3  # Logs > 3 days old
  remote-jobs cleanup cool30 --dry-run          # Preview only`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runCleanup,
}

var (
	cleanupSessions  bool
	cleanupLogs      bool
	cleanupOlderThan int
	cleanupDryRun    bool
)

func init() {
	rootCmd.AddCommand(cleanupCmd)

	cleanupCmd.Flags().BoolVar(&cleanupSessions, "sessions", false, "Clean finished sessions only")
	cleanupCmd.Flags().BoolVar(&cleanupLogs, "logs", false, "Clean log files only")
	cleanupCmd.Flags().IntVar(&cleanupOlderThan, "older-than", 7, "Only clean items older than N days")
	cleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Preview without actually deleting")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	host := args[0]

	// If neither specified, do both
	if !cleanupSessions && !cleanupLogs {
		cleanupSessions = true
		cleanupLogs = true
	}

	if cleanupDryRun {
		fmt.Println("DRY RUN - no changes will be made")
		fmt.Println()
	}

	var totalCleaned int

	if cleanupSessions {
		cleaned, err := cleanupFinishedSessions(host)
		if err != nil {
			return fmt.Errorf("cleanup sessions: %w", err)
		}
		totalCleaned += cleaned
	}

	if cleanupLogs {
		cleaned, err := cleanupOldLogs(host)
		if err != nil {
			return fmt.Errorf("cleanup logs: %w", err)
		}
		totalCleaned += cleaned
	}

	if totalCleaned == 0 {
		fmt.Println("Nothing to clean up")
	}

	return nil
}

func cleanupFinishedSessions(host string) (int, error) {
	fmt.Printf("Checking for finished sessions on %s...\n", host)

	database, err := db.Open()
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get list of sessions
	sessions, err := ssh.TmuxListSessions(host)
	if err != nil {
		return 0, err
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found")
		return 0, nil
	}

	var cleaned int
	for _, sessionName := range sessions {
		// Try to get job info from database
		var job *db.Job
		if strings.HasPrefix(sessionName, "rj-") {
			if jobID, err := strconv.ParseInt(sessionName[3:], 10, 64); err == nil {
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return 0, fmt.Errorf("get job %d: %w", jobID, err)
				}
			}
		} else {
			var err error
			job, err = db.GetJob(database, host, sessionName)
			if err != nil {
				return 0, fmt.Errorf("get job %s: %w", sessionName, err)
			}
		}

		// Determine status file path
		var statusFile string
		if job != nil {
			statusFile = session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
		} else if !strings.HasPrefix(sessionName, "rj-") {
			// Legacy session without job record
			statusFile = session.LegacyStatusFile(sessionName)
		}

		var statusContent string
		if statusFile != "" {
			var err error
			statusContent, err = ssh.ReadRemoteFile(host, statusFile)
			if err != nil {
				return 0, fmt.Errorf("read status file %s:%s: %w", host, statusFile, err)
			}
		}

		panePID, err := ssh.GetTmuxPanePID(host, sessionName)
		if err != nil {
			return 0, fmt.Errorf("get tmux pane pid %s: %w", sessionName, err)
		}
		hasChildren, err := ssh.HasChildProcesses(host, panePID)
		if err != nil {
			return 0, fmt.Errorf("check child processes %s: %w", sessionName, err)
		}

		if statusContent != "" || !hasChildren {
			// Session has finished
			if cleanupDryRun {
				fmt.Printf("  Would kill session: %s\n", sessionName)
			} else {
				fmt.Printf("  Killing session: %s\n", sessionName)
				if err := ssh.TmuxKillSession(host, sessionName); err != nil {
					return 0, fmt.Errorf("kill session %s: %w", sessionName, err)
				}
			}
			cleaned++
		}
	}

	if cleaned == 0 {
		fmt.Println("No finished sessions to clean")
	} else {
		fmt.Printf("Cleaned %d session(s)\n", cleaned)
	}

	return cleaned, nil
}

func cleanupOldLogs(host string) (int, error) {
	fmt.Printf("Checking for old log files on %s (older than %d days)...\n", host, cleanupOlderThan)

	var allFiles []string

	// Find legacy archived log files in /tmp (format: /tmp/tmux-*.YYYYMMDD-HHMMSS.log)
	legacyFindCmd := fmt.Sprintf("find /tmp -maxdepth 1 -name 'tmux-*.*.log' -mtime +%d 2>/dev/null", cleanupOlderThan)
	stdout, _, err := ssh.Run(host, legacyFindCmd)
	if err != nil {
		return 0, fmt.Errorf("list legacy logs on %s: %w", host, err)
	}
	for _, file := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if file != "" {
			allFiles = append(allFiles, file)
		}
	}

	// Find new log files in ~/.cache/remote-jobs/logs
	// Note: path not quoted to allow tilde expansion
	newFindCmd := fmt.Sprintf("find ~/.cache/remote-jobs/logs -maxdepth 1 -type f -mtime +%d 2>/dev/null", cleanupOlderThan)
	stdout, _, err = ssh.Run(host, newFindCmd)
	if err != nil {
		return 0, fmt.Errorf("list logs on %s: %w", host, err)
	}
	for _, file := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if file != "" {
			allFiles = append(allFiles, file)
		}
	}

	var cleaned int
	for _, file := range allFiles {
		filename := path.Base(file)
		if cleanupDryRun {
			fmt.Printf("  Would delete: %s\n", filename)
		} else {
			fmt.Printf("  Deleting: %s\n", filename)
			// Note: path not quoted to allow tilde expansion
			if _, stderr, err := ssh.Run(host, fmt.Sprintf("rm -f %s", file)); err != nil {
				return 0, fmt.Errorf("delete log %s: %s", filename, strings.TrimSpace(stderr))
			}
		}
		cleaned++
	}

	if cleaned == 0 {
		fmt.Println("No old log files to clean")
	} else {
		fmt.Printf("Cleaned %d log file(s)\n", cleaned)
	}

	return cleaned, nil
}
