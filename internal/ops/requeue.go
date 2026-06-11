package ops

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
)

// RequeueJob changes a job's status to queued.
// Queue-runner jobs are always deferred so host sync can refresh sources before
// appending to the remote queue.
func RequeueJob(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	_ = opts
	if err := RefreshProjectDerivedMetadata(database, job); err != nil {
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

	return Result{
		Success:  true,
		Deferred: true,
		JobID:    job.ID,
		Message:  fmt.Sprintf("Job %s requeued locally; source sync + dispatch will run on next host sync", ids.FormatJobID(job.ID)),
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
