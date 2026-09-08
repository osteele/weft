package ops

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

const queueOutcomeUnknownAfter = 15 * time.Minute

const queueOutcomeUnresolvedReason = "status file absent and worker process absent; queue-runner state did not resolve the outcome"

func effectiveQueueUnknownAfter(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return queueOutcomeUnknownAfter
}

func syncNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func observeQueueOutcomeUnresolved(database *sql.DB, job *db.Job, now time.Time, bound time.Duration) (bool, error) {
	if job == nil || job.LatestRunID == nil {
		return false, nil
	}
	return db.ObserveQueueWorkerAbsent(database, job.ID, *job.LatestRunID, now, bound, queueOutcomeUnresolvedReason)
}

func clearQueueOutcomeUnknown(database *sql.DB, job *db.Job) error {
	if job == nil || job.LatestRunID == nil {
		return nil
	}
	return db.ClearQueueWorkerAbsentObservation(database, job.ID, *job.LatestRunID)
}
