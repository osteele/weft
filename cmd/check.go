package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var checkCmd = &cobra.Command{
	Use:   "check <host>",
	Short: "Check status of all jobs on a remote host",
	Long: `Check the status of all running tmux sessions on a remote host.

Shows:
- List of all active tmux sessions (rj-* pattern)
- Status of each job (RUNNING or FINISHED)
- Exit code for finished jobs
- Last 10 lines of output from each session

Example:
  remote-jobs check cool30`,
	Args: cobra.ExactArgs(1),
	RunE: runCheck,
}

func init() {
	rootCmd.AddCommand(checkCmd)
}

func runCheck(cmd *cobra.Command, args []string) error {
	host := args[0]

	database, err := db.Open()
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
		fmt.Printf("No tmux sessions found on %s\n", host)

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
				job, _ = db.GetJobByID(database, jobID)
			}
		} else {
			// Legacy session - try to look up by session name
			job, _ = db.GetJob(database, host, sessionName)
		}

		// Check if job is still running by looking for child processes
		panePID, _ := ssh.GetTmuxPanePID(host, sessionName)
		hasChildren, _ := ssh.HasChildProcesses(host, panePID)

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

		var statusContent string
		if statusFile != "" {
			statusContent, _ = ssh.ReadRemoteFile(host, statusFile)
		}

		if statusContent != "" {
			// Job has finished
			exitCode, _ := strconv.Atoi(strings.TrimSpace(statusContent))
			if exitCode == 0 {
				fmt.Printf("Status: FINISHED ✓\n")
			} else {
				fmt.Printf("Status: FINISHED ✗ (exit code: %d)\n", exitCode)
			}

			// Update database
			endTime := time.Now().Unix()
			if job != nil {
				db.RecordCompletionByID(database, job.ID, exitCode, endTime)
			}
		} else if hasChildren {
			fmt.Printf("Status: RUNNING\n")
		} else {
			fmt.Printf("Status: FINISHED (no status file - may have died)\n")

			// Mark as dead in database
			if job != nil {
				db.MarkDeadByID(database, job.ID)
			}
		}

		// Show job info if available
		if job != nil {
			if job.Description != "" {
				fmt.Printf("Description: %s\n", job.Description)
			}
			startTime := time.Unix(job.StartTime, 0)
			fmt.Printf("Started: %s\n", startTime.Format("2006-01-02 15:04:05"))

			if job.Status == db.StatusRunning {
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
