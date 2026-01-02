package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// DraftJob marks a job as draft and ensures any remote execution is cleaned up.
func DraftJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job not found")
	}

	if job.Status == db.StatusDraft {
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d already draft", job.ID),
		}, nil
	}

	oplog.LogJob(oplog.OpJobSync, job.ID, job.Host, oplog.WithDetail("marking job draft"))

	// Remove any pending operations that would run the job in the future.
	db.DeletePendingOperationsForJob(database, job.ID,
		db.OpQueueJob, db.OpRunJob, db.OpRestartJob, db.OpStartQueuedJob)

	// Terminal jobs can be marked draft immediately.
	if db.IsTerminalStatus(job.Status) {
		if err := db.MarkJobDraftPending(database, job.ID); err != nil {
			return Result{}, fmt.Errorf("set draft status: %w", err)
		}
		if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDraft); err != nil {
			return Result{}, fmt.Errorf("update draft status: %w", err)
		}
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d marked as draft", job.ID),
		}, nil
	}

	if err := db.MarkJobDraftPending(database, job.ID); err != nil {
		return Result{}, fmt.Errorf("set draft pending: %w", err)
	}

	refreshed, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return Result{}, fmt.Errorf("reload job: %w", err)
	}
	if refreshed == nil {
		return Result{}, fmt.Errorf("job %d not found after marking draft", job.ID)
	}

	cleaned, err := SyncDraftJob(database, refreshed, SyncOptions{Timeout: opts.Timeout})
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %d draft pending (host unreachable)", job.ID),
			}, nil
		}
		return Result{}, err
	}

	if !cleaned {
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %d draft pending (remote state uncertain)", job.ID),
		}, nil
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d marked as draft", job.ID),
	}, nil
}
