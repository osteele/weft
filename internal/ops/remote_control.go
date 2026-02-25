package ops

import (
	"time"

	"github.com/osteele/weft/internal/db"
)

// CancelRemoteJob attempts to remove a queued job or stop a running job on the remote backend.
func CancelRemoteJob(job *db.Job, timeout time.Duration) error {
	if job == nil {
		return nil
	}
	if job.Status == db.StatusQueued {
		return applyCancelToRemote(job, timeout)
	}
	return applyKillToRemote(job, timeout)
}
