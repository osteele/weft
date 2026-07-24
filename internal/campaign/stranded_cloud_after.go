package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/runner"
)

// StrandedCloudAfterReset summarizes same-instance consumers returned to the
// unplaced queue after their producer moved off their source instance.
type StrandedCloudAfterReset struct {
	JobIDs     []int64
	AttemptIDs []int64
}

// ResetStrandedCloudAfterConsumers resets not-yet-started consumers that were
// submitted to sourceLaunchID only because producerID was expected to run
// there first. Once the producer move is accepted elsewhere, those consumers
// must be reclassified instead of waiting on a stale CloudAfter attempt.
func ResetStrandedCloudAfterConsumers(database *sql.DB, producerID, sourceLaunchID int64) (StrandedCloudAfterReset, error) {
	var result StrandedCloudAfterReset
	if database == nil || producerID <= 0 || sourceLaunchID <= 0 {
		return result, nil
	}
	jobs, err := db.GetLaunchJobs(database, sourceLaunchID)
	if err != nil {
		return result, fmt.Errorf("list source launch jobs: %w", err)
	}
	for _, job := range jobs {
		if !isResettableCloudAfterConsumer(job, producerID) {
			continue
		}
		result.JobIDs = append(result.JobIDs, job.ID)
		result.AttemptIDs = append(result.AttemptIDs, *job.LatestRunID)
	}
	reason := fmt.Sprintf("producer %s moved off instance %s; job returned to unplaced queue",
		ids.FormatJobID(producerID), ids.FormatInstanceID(sourceLaunchID))
	for _, jobID := range result.JobIDs {
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return result, fmt.Errorf("reset stranded consumer %s: %w", ids.FormatJobID(jobID), err)
		}
		if err := db.SetJobPlacementReasons(database, jobID, []string{reason}); err != nil {
			return result, fmt.Errorf("set placement reason for stranded consumer %s: %w", ids.FormatJobID(jobID), err)
		}
	}
	return result, nil
}

func isResettableCloudAfterConsumer(job *db.Job, producerID int64) bool {
	if job == nil || job.ID == producerID || job.LatestRunID == nil || *job.LatestRunID <= 0 {
		return false
	}
	if job.EffectiveStatus() != db.StatusQueued || job.StartTime > 0 || job.EndTime != nil {
		return false
	}
	return needsProducer(job.Needs, producerID)
}

func needsProducer(needs []string, producerID int64) bool {
	for _, raw := range needs {
		parsed, err := runner.ParseNeedsSpec(raw)
		if err != nil || parsed.IsAsset() {
			continue
		}
		if parsed.Version == producerID {
			return true
		}
	}
	return false
}
