package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
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
// Uses the three-way merge model: sets pending_status to canceled and reconciles
// via the sync path. If the host is unreachable, the pending status remains for
// later reconciliation.
func CancelQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}

	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d (status: %s): %w", job.ID, effectiveStatus, ErrNotQueued)
	}

	oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceling queued job"))

	outcome, err := requestJobStatus(database, job, db.StatusCanceled, opts)
	if err != nil {
		return Result{}, err
	}

	if !outcome.hostAvailable {
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host,
			oplog.WithDetail("cancel deferred: host unreachable"))
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %d cancel pending (host unreachable)", job.ID),
		}, nil
	}

	if !outcome.resolved {
		return Result{
			Success:  true,
			Deferred: true,
			JobID:    job.ID,
			Message:  fmt.Sprintf("Job %d cancel pending (remote state uncertain)", job.ID),
		}, nil
	}

	switch outcome.currentStatus {
	case db.StatusCanceled:
		oplog.LogJob(oplog.OpJobCancel, job.ID, job.Host, oplog.WithDetail("canceled"))
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d canceled", job.ID),
		}, nil
	case db.StatusCompleted:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d already completed", job.ID),
		}, nil
	case db.StatusFailed, db.StatusDead, db.StatusKilled:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d already %s", job.ID, outcome.currentStatus),
		}, nil
	default:
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %d now %s", job.ID, outcome.currentStatus),
		}, nil
	}
}
