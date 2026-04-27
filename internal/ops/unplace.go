package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// UnplaceQueuedJob removes a queued on-prem job's host requirement. The job is
// removed from the old host's queue immediately when possible, or via deferred
// cleanup when that host is unreachable, then returned to the unplaced pool.
func UnplaceQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}
	if job.EffectiveStatus() != db.StatusQueued {
		return Result{}, fmt.Errorf("job %s (status: %s): %w", ids.FormatJobID(job.ID), job.EffectiveStatus(), ErrNotQueued)
	}

	// Cloud jobs: close the attempt and create a fresh unplaced one.
	if job.IsRentalJob() {
		if err := db.ResetJobToUnplaced(database, job.ID); err != nil {
			return Result{}, fmt.Errorf("unplace cloud job %s: %w", ids.FormatJobID(job.ID), err)
		}
		return Result{
			Success: true,
			JobID:   job.ID,
			Message: fmt.Sprintf("Job %s moved from cloud instance back to unplaced jobs", ids.FormatJobID(job.ID)),
		}, nil
	}

	if !job.HasInventoryHost() {
		return Result{}, fmt.Errorf("job %s is not a placed queued job", ids.FormatJobID(job.ID))
	}
	if job.UsesSlurm() {
		return Result{}, fmt.Errorf("job %s is not managed by an on-prem local queue", ids.FormatJobID(job.ID))
	}

	oldHost := job.Host
	if err := ensureDeferredQueueOp(database, job, db.OpRemoveQueued); err != nil {
		return Result{}, err
	}
	if _, err := db.DeletePendingOperationsForJob(database, job.ID,
		db.OpUpdateQueuedJob,
		db.OpMoveToFront,
		db.OpStartQueuedJob,
		db.OpQueueJob,
	); err != nil {
		return Result{}, err
	}
	if err := db.MoveQueuedJobToUnplaced(database, job.ID); err != nil {
		_ = db.DeletePendingOperation(database, job.ID, db.OpRemoveQueued)
		return Result{}, fmt.Errorf("move job %s to unplaced: %w", ids.FormatJobID(job.ID), err)
	}

	timeout := queueOpTimeout(opts)
	if err := applyCancelToRemote(job, timeout); err != nil {
		if isQueueConnectionError(err) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %s moved to unplaced jobs; removal from %s is pending", ids.FormatJobID(job.ID), oldHost),
			}, nil
		}
		return Result{}, err
	}

	if err := db.DeletePendingOperation(database, job.ID, db.OpRemoveQueued); err != nil {
		return Result{}, fmt.Errorf("clear deferred cleanup for job %s: %w", ids.FormatJobID(job.ID), err)
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %s moved from %s back to unplaced jobs", ids.FormatJobID(job.ID), oldHost),
	}, nil
}
