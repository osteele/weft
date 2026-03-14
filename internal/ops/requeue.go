package ops

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/osteele/weft/internal/db"
)

// RequeueJob changes a job's status to queued and appends it to the remote queue.
// If the host is unreachable, the job is queued locally and will be synced later.
func RequeueJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if err := RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
		return Result{}, err
	}
	if err := db.RequeueByID(database, job.ID); err != nil {
		return Result{}, fmt.Errorf("update status to queued: %w", err)
	}

	// Reload job to get refreshed record with status=queued
	job, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return Result{}, fmt.Errorf("reload job: %w", err)
	}

	timeout := queueOpTimeout(opts)
	if err := AppendJobToQueue(job, timeout); err != nil {
		if isQueueConnectionError(err) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %d queued locally (will append to queue when host is online)", job.ID),
			}, nil
		}
		return Result{}, fmt.Errorf("append to remote queue: %w", err)
	}

	// Successfully appended — mark as synced
	if err := db.UpdateLastSyncedStatus(database, job.ID, db.StatusQueued); err != nil {
		return Result{}, fmt.Errorf("update synced status: %w", err)
	}
	if err := db.SetQueuedAtNow(database, job.ID); err != nil {
		log.Printf("requeue: failed to update queued_at for job %d: %v", job.ID, err)
	}

	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %d requeued on %s", job.ID, job.Host),
	}, nil
}
