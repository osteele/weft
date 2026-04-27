package campaign

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
)

// ClassifyNeedsForLaunch inspects each --needs spec attached to a consumer job
// at placement/launch time and decides, per-spec, whether the producer's
// output is reachable via same-instance co-location (CloudAfter) or whether
// the output needs to be staged from R2 (CloudNeeds).
//
// Inputs are re-read from the database so that producer status changes between
// submission and launch (retries, instance churn, grace expiration) are
// reflected.
//
// Rules:
//   - Producer is on-prem: skip. The consumer was host-pinned at submission
//     time (see resolveArtifactNeedsPlacement); local queue-runner handles it.
//   - Producer is a rental job pinned to the same LaunchID as the consumer
//     and that launch is not terminal or self-destructing: emit a CloudAfter
//     ref. This covers both same-batch co-location (instance still
//     `planned`/`launching`) and reuse of an already-`running`/`grace`
//     instance. The consumer reads outputs from the shared workdir; the
//     agent will skip the consumer if the producer failed on this instance.
//   - Producer is a rental job on a different live instance: fall through to
//     R2 staging (cross-instance co-location is a future enhancement).
//   - Producer is a rental job whose instance is terminal (completed,
//     failed, canceled) or missing: resolve the spec to an R2 key via
//     cloudneeds.ResolveSpecs and emit a CloudNeed.
func ClassifyNeedsForLaunch(
	ctx context.Context,
	database *sql.DB,
	client *r2.Client,
	job *db.Job,
	targetInstanceID int64,
) ([]cloud.CloudNeed, []cloud.CloudAfterRef, []string, error) {
	if job == nil {
		return nil, nil, nil, fmt.Errorf("job is nil")
	}
	if len(job.Needs) == 0 {
		return nil, nil, nil, nil
	}

	var (
		r2Specs    []string
		cloudAfter []cloud.CloudAfterRef
		onPrem     []string
		seenAfter  = make(map[int64]bool)
	)

	for _, spec := range job.Needs {
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse --needs %q: %w", spec, err)
		}
		producer, err := db.GetJobByID(database, parsed.Version)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("lookup producer job %s for %q: %w", ids.FormatJobID(parsed.Version), spec, err)
		}
		if producer == nil {
			return nil, nil, nil, fmt.Errorf("producer job %s for %q not found", ids.FormatJobID(parsed.Version), spec)
		}

		// On-prem producer: handled by submit-time host pinning.
		if producer.HasInventoryHost() {
			onPrem = append(onPrem, spec)
			continue
		}

		// Rental or unplaced producer: decide between same-instance co-location
		// and cross-instance R2 staging based on the producer's current launch.
		if isSameLiveInstance(database, producer, targetInstanceID) {
			if !seenAfter[producer.ID] {
				runID := int64(0)
				if producer.LatestRunID != nil {
					runID = *producer.LatestRunID
				}
				cloudAfter = append(cloudAfter, cloud.CloudAfterRef{
					JobID: producer.ID,
					RunID: runID,
				})
				seenAfter[producer.ID] = true
			}
			continue
		}

		r2Specs = append(r2Specs, spec)
	}

	if len(r2Specs) == 0 {
		return nil, cloudAfter, onPrem, nil
	}

	cloudNeeds, err := resolveCloudNeedsFunc(ctx, database, client, r2Specs)
	if err != nil {
		return nil, nil, nil, err
	}
	return cloudNeeds, cloudAfter, onPrem, nil
}

// isSameLiveInstance reports whether the producer job is bound to the same
// rental instance the consumer is being launched on, and that instance has
// not yet reached a terminal state. It covers both mid-launch batches (where
// the instance is still "planned" or "launching") and reuse of an existing
// running/grace instance; co-location is guaranteed by shared LaunchID in
// either case.
//
// A self-destructing instance (HasActiveTerminationIntent) is rejected so we
// do not co-locate a consumer onto an instance about to disappear.
func isSameLiveInstance(database *sql.DB, producer *db.Job, targetInstanceID int64) bool {
	if producer == nil || producer.LaunchID == nil || targetInstanceID == 0 {
		return false
	}
	if *producer.LaunchID != targetInstanceID {
		return false
	}
	launch, err := db.GetLaunch(database, targetInstanceID)
	if err != nil || launch == nil {
		return false
	}
	switch launch.Status {
	case db.LaunchStatusCompleted, db.LaunchStatusFailed, db.LaunchStatusCancelled:
		return false
	}
	return !launch.HasActiveTerminationIntent()
}
