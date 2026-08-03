package campaign

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/cloud"
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

// PersistResolvedCloudAfterPins records the attempt pins shipped to the agent
// for this consumer's current attempt.
func PersistResolvedCloudAfterPins(database *sql.DB, job *db.Job, refs []cloud.CloudAfterRef) error {
	if database == nil || job == nil || job.ID <= 0 {
		return nil
	}
	meta := job.Metadata
	if meta == nil {
		if len(refs) == 0 {
			return nil
		}
		meta = &db.JobMetadata{}
	}
	deps := meta.Dependencies
	if deps == nil {
		if len(refs) == 0 {
			return nil
		}
		deps = &db.JobDependencyMetadata{}
		meta.Dependencies = deps
	}
	deps.ResolvedCloudAfter = deps.ResolvedCloudAfter[:0]
	for _, ref := range refs {
		if ref.JobID <= 0 || ref.RunID <= 0 {
			continue
		}
		deps.ResolvedCloudAfter = append(deps.ResolvedCloudAfter, db.JobDependencyResolvedRef{
			JobID: ref.JobID,
			RunID: ref.RunID,
		})
	}
	if dependencyMetadataEmpty(deps) {
		meta.Dependencies = nil
	}
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		return fmt.Errorf("persist resolved cloud-after pins for %s: %w", ids.FormatJobID(job.ID), err)
	}
	job.Metadata = meta
	return nil
}

func dependencyMetadataEmpty(deps *db.JobDependencyMetadata) bool {
	return deps == nil ||
		(len(deps.CloudAfter) == 0 &&
			len(deps.CloudNeeds) == 0 &&
			len(deps.ResolvedCloudAfter) == 0)
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

// ReconcileStaleCloudAfterPins resets queued same-instance consumers whose
// persisted launch-time pin no longer names the producer's current attempt or
// instance.
func ReconcileStaleCloudAfterPins(database *sql.DB) (StrandedCloudAfterReset, error) {
	var result StrandedCloudAfterReset
	if database == nil {
		return result, nil
	}
	jobs, err := db.ListJobs(database, db.StatusQueued, "", 0, nil, "unprocessed")
	if err != nil {
		return result, fmt.Errorf("list queued jobs for cloud-after pin reconcile: %w", err)
	}
	seenJobs := map[int64]struct{}{}
	for _, job := range jobs {
		if !isCloudAfterPinReconcileCandidate(job) {
			continue
		}
		for _, pin := range job.Metadata.Dependencies.ResolvedCloudAfter {
			state, detail := evaluateResolvedCloudAfterPin(database, job, pin)
			if state != resolvedCloudAfterPinStale {
				continue
			}
			if _, seen := seenJobs[job.ID]; seen {
				break
			}
			result.JobIDs = append(result.JobIDs, job.ID)
			result.AttemptIDs = append(result.AttemptIDs, *job.LatestRunID)
			seenJobs[job.ID] = struct{}{}
			if err := resetCloudAfterConsumerForStalePin(database, job, pin, detail); err != nil {
				return result, err
			}
			break
		}
	}
	return result, nil
}

func isCloudAfterPinReconcileCandidate(job *db.Job) bool {
	if job == nil || job.LatestRunID == nil || *job.LatestRunID <= 0 || job.LaunchID == nil || *job.LaunchID <= 0 {
		return false
	}
	if job.EffectiveStatus() != db.StatusQueued || job.StartTime > 0 || job.EndTime != nil {
		return false
	}
	return job.Metadata != nil &&
		job.Metadata.Dependencies != nil &&
		len(job.Metadata.Dependencies.ResolvedCloudAfter) > 0
}

type resolvedCloudAfterPinState int

const (
	resolvedCloudAfterPinUnknown resolvedCloudAfterPinState = iota
	resolvedCloudAfterPinCurrent
	resolvedCloudAfterPinStale
)

func evaluateResolvedCloudAfterPin(database *sql.DB, consumer *db.Job, pin db.JobDependencyResolvedRef) (resolvedCloudAfterPinState, string) {
	if database == nil || consumer == nil || consumer.LaunchID == nil || *consumer.LaunchID <= 0 || pin.JobID <= 0 || pin.RunID <= 0 {
		return resolvedCloudAfterPinUnknown, ""
	}
	producer, err := db.GetJobByID(database, pin.JobID)
	if err != nil || producer == nil {
		return resolvedCloudAfterPinUnknown, ""
	}
	if producer.LaunchID == nil || *producer.LaunchID != *consumer.LaunchID {
		current := "unplaced"
		if producer.LaunchID != nil && *producer.LaunchID > 0 {
			current = ids.FormatInstanceID(*producer.LaunchID)
		}
		return resolvedCloudAfterPinStale, fmt.Sprintf("producer %s is now on %s, not %s",
			ids.FormatJobID(pin.JobID), current, ids.FormatInstanceID(*consumer.LaunchID))
	}
	if producer.LatestRunID == nil || *producer.LatestRunID <= 0 {
		return resolvedCloudAfterPinUnknown, ""
	}
	if *producer.LatestRunID != pin.RunID {
		return resolvedCloudAfterPinStale, fmt.Sprintf("producer %s run changed from %d to %d on %s",
			ids.FormatJobID(pin.JobID), pin.RunID, *producer.LatestRunID, ids.FormatInstanceID(*consumer.LaunchID))
	}
	return resolvedCloudAfterPinCurrent, ""
}

func resetCloudAfterConsumerForStalePin(database *sql.DB, job *db.Job, pin db.JobDependencyResolvedRef, detail string) error {
	if err := db.ResetJobToUnplaced(database, job.ID); err != nil {
		return fmt.Errorf("reset stale cloud-after consumer %s: %w", ids.FormatJobID(job.ID), err)
	}
	reason := fmt.Sprintf("cloud-after pin for producer %s run %d is stale; job returned to unplaced queue",
		ids.FormatJobID(pin.JobID), pin.RunID)
	if detail != "" {
		reason = fmt.Sprintf("%s (%s)", reason, detail)
	}
	if err := db.SetJobPlacementReasons(database, job.ID, []string{reason}); err != nil {
		return fmt.Errorf("set placement reason for stale cloud-after consumer %s: %w", ids.FormatJobID(job.ID), err)
	}
	return nil
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
