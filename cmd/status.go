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

// Exit codes
const (
	ExitSuccess  = 0
	ExitFailed   = 1
	ExitRunning  = 2
	ExitNotFound = 3
)

var statusCmd = &cobra.Command{
	Use:   "status <job-id>",
	Short: "Check the status of a specific job",
	Long: `Check the status of a specific job.

Exit codes:
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Example:
  remote-jobs status 42`,
	Args: cobra.ExactArgs(1),
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get job from database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}

	if job == nil {
		fmt.Printf("Job %d not found\n", jobID)
		os.Exit(ExitNotFound)
		return nil
	}

	// If job is already marked as completed or dead, use cached result
	if job.Status == db.StatusCompleted || job.Status == db.StatusDead {
		return printJobStatus(job)
	}

	// Job is marked as running - verify actual status on remote
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExists(job.Host, tmuxSession)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}

	if !exists {
		// Session doesn't exist - check for status file
		statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
		content, err := ssh.ReadRemoteFile(job.Host, statusFile)
		if err != nil {
			return fmt.Errorf("read status file: %w", err)
		}

		if content != "" {
			// Job completed, update database
			exitCode, _ := strconv.Atoi(strings.TrimSpace(content))
			endTime := time.Now().Unix()
			if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusCompleted
			job.ExitCode = &exitCode
			job.EndTime = &endTime
		} else {
			// No status file - job died unexpectedly
			if err := db.MarkDeadByID(database, job.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update database: %v\n", err)
			}
			job.Status = db.StatusDead
		}
	} else {
		// Session still running - show last few lines of output
		output, _ := ssh.TmuxCapturePaneOutput(job.Host, tmuxSession, 5)
		if output != "" {
			fmt.Println("Last output:")
			fmt.Println(output)
		}
	}

	return printJobStatus(job)
}

func printJobStatus(job *db.Job) error {
	fmt.Printf("Job ID:   %d\n", job.ID)
	fmt.Printf("Host:     %s\n", job.Host)
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
