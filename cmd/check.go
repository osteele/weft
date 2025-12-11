package cmd

import (
	"fmt"
	"os"
	"strconv"
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
- List of all active tmux sessions
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
				if err := db.MarkDead(database, host, job.SessionName); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to mark job %d as dead: %v\n", job.ID, err)
				}
			}
		}
		return nil
	}

	fmt.Printf("Found %d session(s) on %s:\n\n", len(sessions), host)

	for _, sessionName := range sessions {
		fmt.Printf("=== %s ===\n", sessionName)

		// Check if job is still running by looking for child processes
		panePID, _ := ssh.GetTmuxPanePID(host, sessionName)
		hasChildren, _ := ssh.HasChildProcesses(host, panePID)

		statusFile := session.StatusFile(sessionName)
		statusContent, _ := ssh.ReadRemoteFile(host, statusFile)

		if statusContent != "" {
			// Job has finished
			exitCode, _ := strconv.Atoi(statusContent)
			if exitCode == 0 {
				fmt.Printf("Status: FINISHED ✓\n")
			} else {
				fmt.Printf("Status: FINISHED ✗ (exit code: %d)\n", exitCode)
			}

			// Update database
			endTime := time.Now().Unix()
			if err := db.RecordCompletion(database, host, sessionName, exitCode, endTime); err != nil {
				// Might fail if job not in DB, that's ok
			}
		} else if hasChildren {
			fmt.Printf("Status: RUNNING\n")
		} else {
			fmt.Printf("Status: FINISHED (no status file - may have died)\n")

			// Mark as dead in database
			if err := db.MarkDead(database, host, sessionName); err != nil {
				// Might fail if job not in DB, that's ok
			}
		}

		// Get job info from database if available
		job, _ := db.GetJob(database, host, sessionName)
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
