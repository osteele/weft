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

var (
	statusSync   bool
	statusNoSync bool
)

var statusCmd = &cobra.Command{
	Use:   "status <job-id>...",
	Short: "Check the status of one or more jobs",
	Long: `Check the status of one or more jobs.

Exit codes (single job only):
  0: Job completed successfully
  1: Job failed or error
  2: Job is still running
  3: Job not found

Examples:
  remote-jobs status 42
  remote-jobs status 42 43 44`,
	Args: cobra.MinimumNArgs(1),
	RunE: runStatus,
}

func init() {
	// Removed: Status command is now only available as `job status`
	// rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&statusSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	statusCmd.Flags().BoolVar(&statusNoSync, "no-sync", false, "Skip syncing job statuses before checking")
}

func runStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Sync logic: fast sync by default, full sync with --sync, skip with --no-sync
	if !statusNoSync {
		if statusSync {
			// Full sync requested
			// Reuse the list sync logic
			hosts, err := db.ListUniqueActiveHosts(database)
			if err == nil && len(hosts) > 0 {
				for _, host := range hosts {
					syncHost(database, host)
				}
			}
		} else {
			// Fast sync by default
			completed := performFastSync(database, false)
			if !completed {
				fmt.Fprintf(os.Stderr, "Note: Some hosts timed out. Run with --sync for full sync.\n")
			}
		}
	}

	singleJob := len(args) == 1

	for i, arg := range args {
		if i > 0 {
			fmt.Println("---")
		}

		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid job ID: %s\n", arg)
			if singleJob {
				os.Exit(ExitNotFound)
			}
			continue
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: %v\n", jobID, err)
			if singleJob {
				os.Exit(ExitNotFound)
			}
			continue
		}

		if job == nil {
			fmt.Printf("Job %d not found\n", jobID)
			if singleJob {
				os.Exit(ExitNotFound)
			}
			continue
		}

		// If job is already marked as completed or dead, use cached result
		if job.Status == db.StatusCompleted || job.Status == db.StatusDead {
			printJobStatus(job, singleJob)
			continue
		}

		// Job is marked as running - verify actual status on remote
		tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		exists, err := ssh.TmuxSessionExists(job.Host, tmuxSession)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: check session: %v\n", jobID, err)
			continue
		}

		if !exists {
			// Session doesn't exist - check for status file
			statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
			content, err := ssh.ReadRemoteFile(job.Host, statusFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Job %d: read status file: %v\n", jobID, err)
				continue
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
		} else if singleJob {
			// Session still running - show last few lines of output (only for single job)
			output, _ := ssh.TmuxCapturePaneOutput(job.Host, tmuxSession, 5)
			if output != "" {
				fmt.Println("Last output:")
				fmt.Println(output)
			}
		}

		printJobStatus(job, singleJob)
	}

	return nil
}

func printJobStatus(job *db.Job, exitOnComplete bool) {
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

	// Set exit code based on status (only for single job)
	if exitOnComplete {
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
	}
}
