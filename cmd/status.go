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

// Exit codes
const (
	ExitSuccess  = 0
	ExitFailed   = 1
	ExitRunning  = 2
	ExitNotFound = 3
)

var statusCmd = &cobra.Command{
	Use:   "status <host> <session>",
	Short: "Check the status of a specific job",
	Long: `Check the status of a specific job with database quick-path.

Exit codes:
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Example:
  remote-jobs status cool30 train-gpt2`,
	Args: cobra.ExactArgs(2),
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	host := args[0]
	sessionName := args[1]

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Check database first (fast path)
	job, err := db.GetJob(database, host, sessionName)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}

	if job == nil {
		fmt.Printf("Job '%s' not found in database for host %s\n", sessionName, host)
		os.Exit(ExitNotFound)
		return nil
	}

	// If job is already marked as completed or dead, use cached result
	if job.Status == db.StatusCompleted || job.Status == db.StatusDead {
		return printJobStatus(job)
	}

	// Job is marked as running - verify actual status on remote
	exists, err := ssh.TmuxSessionExists(host, sessionName)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}

	if !exists {
		// Session doesn't exist - check for status file
		statusFile := session.StatusFile(sessionName)
		content, err := ssh.ReadRemoteFile(host, statusFile)
		if err != nil {
			return fmt.Errorf("read status file: %w", err)
		}

		if content != "" {
			// Job completed, update database
			exitCode, _ := strconv.Atoi(content)
			endTime := time.Now().Unix()
			if err := db.RecordCompletion(database, host, sessionName, exitCode, endTime); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusCompleted
			job.ExitCode = &exitCode
			job.EndTime = &endTime
		} else {
			// No status file - job died unexpectedly
			if err := db.MarkDead(database, host, sessionName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusDead
		}
	} else {
		// Session still running - show last few lines of output
		output, _ := ssh.TmuxCapturePaneOutput(host, sessionName, 5)
		if output != "" {
			fmt.Println("Last output:")
			fmt.Println(output)
		}
	}

	return printJobStatus(job)
}

func printJobStatus(job *db.Job) error {
	fmt.Printf("Host:     %s\n", job.Host)
	fmt.Printf("Session:  %s\n", job.SessionName)
	fmt.Printf("Status:   %s\n", job.Status)

	if job.Description != "" {
		fmt.Printf("Desc:     %s\n", job.Description)
	}

	startTime := time.Unix(job.StartTime, 0)
	fmt.Printf("Started:  %s\n", startTime.Format("2006-01-02 15:04:05"))

	if job.EndTime != nil {
		endTime := time.Unix(*job.EndTime, 0)
		duration := *job.EndTime - job.StartTime
		fmt.Printf("Ended:    %s\n", endTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("Duration: %s\n", db.FormatDuration(duration))
	} else if job.Status == db.StatusRunning {
		duration := time.Now().Unix() - job.StartTime
		fmt.Printf("Running:  %s\n", db.FormatDuration(duration))
	}

	if job.ExitCode != nil {
		fmt.Printf("Exit:     %d\n", *job.ExitCode)
	}

	// Set exit code based on status
	switch job.Status {
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode == 0 {
			os.Exit(ExitSuccess)
		} else {
			os.Exit(ExitFailed)
		}
	case db.StatusDead:
		os.Exit(ExitFailed)
	case db.StatusRunning:
		os.Exit(ExitRunning)
	default:
		os.Exit(ExitNotFound)
	}

	return nil
}
