package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// waitForQueuedJobCompletion waits for a queued job to complete.
// It handles host offline scenarios by polling until connected.
func waitForQueuedJobCompletion(database *sql.DB, jobID int64, deferred bool) error {
	tracker := newHostConnectionTracker()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	if deferred {
		fmt.Printf("Job saved locally. Waiting for %s to come online...\n", job.Host)
	}

	// Use existing waitForJobCompletion with connection tracking
	finalJob, err := waitForJobCompletion(database, jobID, 0, tracker)
	if err != nil {
		return err
	}

	// Report final status
	printJobStatusLine(finalJob)

	// Exit with appropriate code based on job result
	if finalJob.ExitCode != nil && *finalJob.ExitCode == 0 {
		os.Exit(ExitSuccess)
	}
	if finalJob.Status == db.StatusCompleted {
		os.Exit(ExitSuccess)
	}
	os.Exit(ExitFailed)
	return nil // unreachable
}

// followQueuedJob follows a queued job's logs, waiting for the job to start first.
// It handles host offline scenarios by polling until connected.
func followQueuedJob(database *sql.DB, jobID int64, host string, deferred bool) error {
	tracker := newHostConnectionTracker()

	if deferred {
		fmt.Printf("Job saved locally. Waiting for %s to come online...\n", host)
	}

	// Wait until job is running (or terminal)
	job, err := waitUntilJobRunning(database, jobID, tracker)
	if err != nil {
		return err
	}

	// Check if already terminal
	if isWaitTerminalStatus(job.Status) {
		fmt.Printf("Job #%d already completed with status: %s\n", jobID, job.Status)
		if job.ExitCode != nil {
			fmt.Printf("Exit code: %d\n", *job.ExitCode)
		}
		return nil
	}

	// Get log file path
	logFile := session.SimpleLogFile(job.ID)

	// Stream logs
	fmt.Printf("\nFollowing log output until job completes (Ctrl+C to stop)...\n\n")
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 1; done; tail -n 50 -F %s",
		shellQuote(logFile), shellQuote(logFile))
	remoteCmd := fmt.Sprintf("sh -c %s", shellQuote(script))
	sshCmd := exec.Command("ssh", host, remoteCmd)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	return streamCommandUntilJobDone(database, jobID, sshCmd)
}

// waitUntilJobRunning polls until the job reaches running or terminal state.
func waitUntilJobRunning(database *sql.DB, jobID int64, tracker *hostConnectionTracker) (*db.Job, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastStatus := ""

	for {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, fmt.Errorf("job %d not found", jobID)
		}

		// Report status changes
		if job.Status != lastStatus {
			printJobStatusLine(job)
			lastStatus = job.Status
		}

		// Terminal states - done
		if isWaitTerminalStatus(job.Status) {
			return job, nil
		}

		// Running - ready for streaming
		if job.Status == db.StatusRunning {
			return job, nil
		}

		// Try to sync to refresh status
		if shouldAttemptSync(job.Status) {
			if _, syncErr := ops.SyncJob(database, job, ops.DefaultSyncOptions()); syncErr != nil {
				if ssh.IsConnectionError(syncErr.Error()) {
					if tracker != nil {
						tracker.MarkDown(job.Host)
					}
				} else {
					return nil, syncErr
				}
			} else {
				if tracker != nil {
					tracker.MarkUp(job.Host, true)
				}
				// Re-fetch after sync
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return nil, err
				}
				if job != nil {
					if isWaitTerminalStatus(job.Status) || job.Status == db.StatusRunning {
						return job, nil
					}
				}
			}
		}

		<-ticker.C
	}
}
