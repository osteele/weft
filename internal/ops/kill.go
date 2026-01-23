package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// KillJob sets the pending status to killed and attempts to reconcile immediately.
// Uses the three-way merge model: sets pending_status as user intent, then
// tries to apply to remote. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func KillJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	oplog.LogJob(oplog.OpJobKill, job.ID, job.Host, oplog.WithDetail("killing job"))

	outcome, err := requestJobStatus(database, job, db.StatusKilled, opts)
	if err != nil {
		return Result{}, err
	}

	if !outcome.hostAvailable {
		oplog.LogJob(oplog.OpJobKill, job.ID, job.Host,
			oplog.WithDetail("kill deferred: host unreachable"))
		return Result{
			Success:  true,
			Deferred: !outcome.hostAvailable,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %d kill pending (host unreachable)", job.ID),
		}, nil
	}

	if !outcome.resolved {
		return Result{}, fmt.Errorf("unable to reconcile kill for job %d (status: %s)", job.ID, outcome.currentStatus)
	}

	switch outcome.currentStatus {
	case db.StatusKilled:
		oplog.LogJob(oplog.OpJobKilled, job.ID, job.Host,
			oplog.WithDetail("killed via reconciliation"))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d killed", job.ID),
		}, nil
	case db.StatusCompleted:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d already completed", job.ID),
		}, nil
	case db.StatusFailed, db.StatusDead:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d already failed", job.ID),
		}, nil
	default:
		return Result{}, fmt.Errorf("job %d remains %s after kill request", job.ID, outcome.currentStatus)
	}

}

// CancelQueuedJob cancels a queued job so it won't run when the queue drains to it.
// Uses the three-way merge model: sets pending_status to canceled, then tries to
// remove from remote queue. If successful, status is updated; if not, the
// pending_status remains for later reconciliation during sync.
func CancelQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d is not queued (status: %s)", job.ID, effectiveStatus)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceling queued job"))

	// Set pending status to record user intent
	if err := db.SetPendingStatus(database, job.ID, db.StatusCanceled); err != nil {
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithError(err), oplog.WithDetail("set pending status failed"))
		return Result{}, fmt.Errorf("set pending status: %w", err)
	}

	// Try to remove from remote queue file immediately
	queueName := DefaultQueueName
	err := removeFromQueueFile(job.Host, queueName, job.ID, opts.Timeout)

	if err != nil {

		// Connection error - leave pending status for later reconciliation

		if ssh.IsConnectionError(err.Error()) {

			oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host,

				oplog.WithDetail("cancel deferred: connection error"))

			return Result{

				Success: true,

				JobID: job.ID,

				Message: fmt.Sprintf("Job %d cancel pending (host unreachable)", job.ID),
			}, nil

		}

		return Result{}, err

	}

	// Success - update status and clear pending
	if err := db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusCanceled); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceled"))

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d canceled", job.ID),
	}, nil
}
