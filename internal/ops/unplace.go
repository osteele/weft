package ops

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
)

// UnplaceQueuedJob removes a queued on-prem job's host requirement. The job is
// removed from the old host's queue immediately when possible, or via deferred
// cleanup when that host is unreachable, then returned to the unplaced pool.
func UnplaceQueuedJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}
	if job.EffectiveStatus() != db.StatusQueued {
		return Result{}, fmt.Errorf("job %d is %s, not queued", job.ID, job.EffectiveStatus())
	}
	if !job.HasInventoryHost() {
		return Result{}, fmt.Errorf("job %d is not an inventory queued job", job.ID)
	}
	if job.UsesSlurm() {
		return Result{}, fmt.Errorf("job %d is not managed by an on-prem local queue", job.ID)
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
		return Result{}, fmt.Errorf("move job %d to unplaced: %w", job.ID, err)
	}

	timeout := queueOpTimeout(opts)
	if err := applyCancelToRemote(job, timeout); err != nil {
		if isQueueConnectionError(err) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %d moved to unplaced jobs; removal from %s is pending", job.ID, oldHost),
			}, nil
		}
		return Result{}, err
	}

	if err := db.DeletePendingOperation(database, job.ID, db.OpRemoveQueued); err != nil {
		return Result{}, fmt.Errorf("clear deferred cleanup for job %d: %w", job.ID, err)
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d moved from %s back to unplaced jobs", job.ID, oldHost),
	}, nil
}
