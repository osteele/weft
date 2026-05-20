package jobview

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

type PlacementStatus struct {
	JobID         int64
	Bucket        Bucket
	DisplayAt     int64
	HasOpenIntent bool
	LaunchID      *int64
}

// PlacementStatusForJobs returns placement display state for the supplied jobs.
// It is a read-only model: no stale-row repair or provider checks are run here.
func PlacementStatusForJobs(database *sql.DB, jobs []*db.Job, now time.Time) (map[int64]PlacementStatus, error) {
	if database == nil || len(jobs) == 0 {
		return map[int64]PlacementStatus{}, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	jobIDs := make([]int64, 0, len(jobs))
	rentalQueuedIDs := make([]int64, 0, len(jobs))
	launchIDs := make([]int64, 0, len(jobs))
	seenLaunches := make(map[int64]struct{})
	for _, job := range jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		jobIDs = append(jobIDs, job.ID)
		if job.EffectiveStatus() == db.StatusQueued && job.TargetKind() == db.JobTargetRentalInstance {
			rentalQueuedIDs = append(rentalQueuedIDs, job.ID)
		}
		if job.LaunchID != nil && *job.LaunchID > 0 {
			if _, ok := seenLaunches[*job.LaunchID]; !ok {
				seenLaunches[*job.LaunchID] = struct{}{}
				launchIDs = append(launchIDs, *job.LaunchID)
			}
		}
	}

	openIntentCreatedAt, err := db.OpenMoveOrPlacementIntentCreatedAt(database, jobIDs)
	if err != nil {
		return nil, err
	}
	displayQueuedAt, err := db.PlacementDisplayQueuedAt(database, rentalQueuedIDs)
	if err != nil {
		return nil, err
	}
	launchStatusByID, err := db.GetLaunchStatuses(database, launchIDs)
	if err != nil {
		return nil, err
	}
	launchLiveByID, err := db.GetLaunchLiveStates(database, launchIDs)
	if err != nil {
		return nil, err
	}
	launchesWithActiveJob := LaunchesWithActiveJob(jobs, launchLiveByID)

	out := make(map[int64]PlacementStatus, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		ps := PlacementStatus{
			JobID:    job.ID,
			LaunchID: job.LaunchID,
		}
		if createdAt := openIntentCreatedAt[job.ID]; createdAt > 0 {
			ps.HasOpenIntent = true
			ps.DisplayAt = createdAt
		} else if queuedAt := displayQueuedAt[job.ID]; queuedAt > 0 {
			ps.DisplayAt = queuedAt
		}
		ps.Bucket = ClassifyBucket(job, ClassifyInput{
			LaunchStatusByID:     launchStatusByID,
			LaunchesWithActive:   launchesWithActiveJob,
			HasOpenPlacingIntent: ps.HasOpenIntent,
		})
		out[job.ID] = ps
	}
	return out, nil
}
