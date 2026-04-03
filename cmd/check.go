package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

var checkCmd = &cobra.Command{
	Use:   "check <host>",
	Short: "Check status of all jobs on a remote host",
	Long: `Check the status of all running sessions on a remote host.

Shows:
- List of all active sessions (rj-* pattern)
- Status of each job (RUNNING or FINISHED)
- Exit code for finished jobs
- Last 10 lines of output from each session

Example:
  weft check cool30`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runCheck,
}

func init() {
	// Removed: Check command is deprecated, use `job list --host <host>` or TUI instead
	// rootCmd.AddCommand(checkCmd)
}

func runCheck(cmd *cobra.Command, args []string) error {
	host := args[0]

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get list of tmux sessions on remote host
	sessions, err := ssh.TmuxListSessions(host)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Printf("No sessions found on %s\n", host)

		// Check if any jobs in DB are marked as running for this host
		runningJobs, err := db.ListRunning(database, host)
		if err != nil {
			return fmt.Errorf("list running: %w", err)
		}

		if len(runningJobs) > 0 {
			fmt.Printf("\nWarning: %d jobs in database are marked as running but no sessions found.\n", len(runningJobs))
			fmt.Println("These jobs may have died unexpectedly. Marking as dead...")
			for _, job := range runningJobs {
				if err := db.MarkDeadByID(database, job.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to mark job %d as dead: %v\n", job.ID, err)
				}
			}
		}
		return nil
	}

	fmt.Printf("Found %d session(s) on %s:\n\n", len(sessions), host)

	for _, sessionName := range sessions {
		fmt.Printf("=== %s ===\n", sessionName)

		// Try to parse job ID from session name (rj-{id} pattern)
		var job *db.Job
		if strings.HasPrefix(sessionName, "rj-") {
			if jobID, err := strconv.ParseInt(sessionName[3:], 10, 64); err == nil {
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return fmt.Errorf("get job %d: %w", jobID, err)
				}
			}
		} else {
			// Legacy session - try to look up by session name
			var err error
			job, err = db.GetJob(database, host, sessionName)
			if err != nil {
				return fmt.Errorf("get job %s: %w", sessionName, err)
			}
		}

		// Check if job is still running by looking for child processes
		panePID, err := ssh.GetTmuxPanePID(host, sessionName)
		if err != nil {
			return fmt.Errorf("get tmux pane pid %s: %w", sessionName, err)
		}
		hasChildren, err := ssh.HasChildProcesses(host, panePID)
		if err != nil {
			return fmt.Errorf("check child processes %s: %w", sessionName, err)
		}

		// Determine status file path
		var statusFile string
		if job != nil {
			statusFile = session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
		} else if strings.HasPrefix(sessionName, "rj-") {
			// New-style session but no job in DB - can't determine status file
			statusFile = ""
		} else {
			// Legacy session without job record
			statusFile = session.LegacyStatusFile(sessionName)
		}

		var result *ops.StatusFileResult
		if statusFile != "" {
			result, err = ops.ReadStatusFile(host, statusFile, 10*time.Second)
			if err != nil {
				return fmt.Errorf("read status file %s:%s: %w", host, statusFile, err)
			}
		}

		if result != nil {
			// Job has finished
			exitCode, err := strconv.Atoi(strings.TrimSpace(result.Content))
			if err != nil {
				return fmt.Errorf("parse exit code for %s: %w", sessionName, err)
			}
			if exitCode == 0 {
				fmt.Printf("Status: FINISHED ✓\n")
			} else {
				fmt.Printf("Status: FINISHED ✗ (exit code: %d)\n", exitCode)
			}

			// Update database
			if job != nil {
				if err := ops.RecordJobCompletion(database, job.ID, exitCode, result.Mtime); err != nil {
					return fmt.Errorf("record completion for job %d: %w", job.ID, err)
				}
			}
		} else if hasChildren {
			fmt.Printf("Status: RUNNING\n")
		} else {
			fmt.Printf("Status: FINISHED (no status file - may have died)\n")

			// Mark as dead in database
			if job != nil {
				if err := db.MarkDeadByID(database, job.ID); err != nil {
					return fmt.Errorf("mark job %d dead: %w", job.ID, err)
				}
			}
		}

		// Show job info if available
		if job != nil {
			if job.Description != "" {
				fmt.Printf("Description: %s\n", job.Description)
			}
			if job.StartTime > 0 {
				startTime := time.Unix(job.StartTime, 0)
				fmt.Printf("Started: %s\n", startTime.Format("2006-01-02 15:04:05"))
			}

			if job.Status == db.StatusRunning && job.StartTime > 0 {
				duration := time.Now().Unix() - job.StartTime
				fmt.Printf("Running for: %s\n", db.FormatDuration(duration))
			}
		}

		// Show last 10 lines of output
		output, err := ssh.TmuxCapturePaneOutput(host, sessionName, 10)
		if err == nil && output != "" {
			fmt.Printf("\nLast output:\n%s\n", output)
		}

		fmt.Println()
	}

	return nil
}
