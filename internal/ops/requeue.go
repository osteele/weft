package ops

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
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

	// Invalidate local log cache so stale logs aren't served
	_ = logcache.Delete(job.ID)

	// Reload job to get refreshed record with status=queued
	job, err := db.GetJobByID(database, job.ID)
	if err != nil {
		return Result{}, fmt.Errorf("reload job: %w", err)
	}

	timeout := queueOpTimeout(opts)

	// Remove old completion files so the runner doesn't skip the requeued job
	if err := RemoveRemoteCompletionFiles(job.Host, job.ID, timeout); err != nil {
		log.Printf("requeue: failed to remove remote completion files for job %d: %v", job.ID, err)
	}

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

// RemoveRemoteCompletionFiles removes stale status, pid, and pgid files from
// the remote host so the runner doesn't skip a requeued job.
func RemoveRemoteCompletionFiles(host string, jobID int64, timeout time.Duration) error {
	cmd := fmt.Sprintf("rm -f %s/%d.status %s/%d.pid %s/%d.pgid",
		session.LogDir, jobID, session.LogDir, jobID, session.LogDir, jobID)
	_, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	return err
}
