package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/osteele/remote-jobs/internal/db"
)

type jobWaitResult struct {
	job *db.Job
	err error
}

func streamCommandUntilJobDone(database *sql.DB, jobID int64, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start log streaming: %w", err)
	}

	tailErrCh := make(chan error, 1)
	go func() {
		tailErrCh <- cmd.Wait()
	}()

	jobCh := make(chan jobWaitResult, 1)
	go func() {
		job, err := waitForJobCompletion(database, jobID, 0, nil)
		jobCh <- jobWaitResult{job: job, err: err}
	}()

	tailClosedEarly := false
	tailStopped := false
	var tailErr error
	var finalJob *db.Job
	var jobErr error

	for tailErrCh != nil || jobCh != nil {
		select {
		case err := <-tailErrCh:
			tailErr = err
			tailErrCh = nil
			if !tailStopped && jobCh != nil {
				tailClosedEarly = true
				fmt.Fprintf(os.Stderr, "\nStopped streaming logs (connection closed). Waiting for job %d to complete...\n", jobID)
			}
		case res := <-jobCh:
			finalJob = res.job
			jobErr = res.err
			jobCh = nil
			if cmd.Process != nil {
				tailStopped = true
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}

	if jobErr != nil {
		return jobErr
	}

	if tailErr != nil && !tailStopped && !tailClosedEarly {
		return fmt.Errorf("log streaming failed: %w", tailErr)
	}

	if finalJob != nil {
		fmt.Printf("\nJob %d finished with status %s", finalJob.ID, finalJob.Status)
		if finalJob.ExitCode != nil {
			fmt.Printf(" (exit code %d)", *finalJob.ExitCode)
		}
		fmt.Println()
	}

	return nil
}
