package db

import (
	"database/sql"
	"time"
)

const (
	PlacementBucketRunning        = "running"
	PlacementBucketPaused         = "paused"
	PlacementBucketPlacing        = "placing"
	PlacementBucketLaunching      = "launching"
	PlacementBucketQueued         = "queued"
	PlacementBucketUnplaced       = "unplaced"
	PlacementBucketCompletions    = "completions"
	PlacementBucketFailures       = "failures"
	PlacementBucketKilledCanceled = "killed_canceled"
)

// PlacementStatus is the canonical DB/read-model view of a job's current
// placement bucket and display timestamp. UI code can render from this instead
// of independently combining launch rows, intent rows, and attempt history.
type PlacementStatus struct {
	JobID         int64
	Bucket        string
	DisplayAt     int64
	HasOpenIntent bool
	LaunchID      *int64
}

// PlacementStatusForJobs returns placement display state for the supplied jobs.
// It is a read-only model: no stale-row repair or provider checks are run here.
func PlacementStatusForJobs(database *sql.DB, jobs []*Job, now time.Time) (map[int64]PlacementStatus, error) {
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
		if job.EffectiveStatus() == StatusQueued && job.TargetKind() == JobTargetRentalInstance {
			rentalQueuedIDs = append(rentalQueuedIDs, job.ID)
		}
		if job.LaunchID != nil && *job.LaunchID > 0 {
			if _, ok := seenLaunches[*job.LaunchID]; !ok {
				seenLaunches[*job.LaunchID] = struct{}{}
				launchIDs = append(launchIDs, *job.LaunchID)
			}
		}
	}

	openIntentCreatedAt, err := OpenMoveOrPlacementIntentCreatedAt(database, jobIDs)
	if err != nil {
		return nil, err
	}
	displayQueuedAt, err := PlacementDisplayQueuedAt(database, rentalQueuedIDs)
	if err != nil {
		return nil, err
	}
	launchStatusByID, err := GetLaunchStatuses(database, launchIDs)
	if err != nil {
		return nil, err
	}
	launchLiveByID, err := GetLaunchLiveStates(database, launchIDs)
	if err != nil {
		return nil, err
	}
	launchesWithActiveJob := placementLaunchesWithActiveJob(jobs, launchLiveByID)

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
		ps.Bucket = placementBucketForJob(job, launchStatusByID, launchesWithActiveJob, ps.HasOpenIntent)
		out[job.ID] = ps
	}
	return out, nil
}

func placementLaunchesWithActiveJob(jobs []*Job, launchLiveByID map[int64]*LaunchLiveState) map[int64]bool {
	out := make(map[int64]bool)
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		status := job.EffectiveStatus()
		if status == StatusRunning || status == StatusStarting {
			out[*job.LaunchID] = true
			continue
		}
		if live := launchLiveByID[*job.LaunchID]; live != nil && live.JobProgressID > 0 {
			out[*job.LaunchID] = true
		}
	}
	return out
}

func placementBucketForJob(job *Job, launchStatusByID map[int64]string, launchesWithActiveJob map[int64]bool, hasOpenIntent bool) string {
	if job == nil {
		return ""
	}
	status := job.EffectiveStatus()
	if placementLaunchStatusForJob(job, launchStatusByID) == LaunchStatusPaused {
		switch status {
		case StatusRunning, StatusStarting, StatusQueued, StatusPendingPlacement, StatusPaused:
			return PlacementBucketPaused
		}
	}
	if hasOpenIntent {
		switch status {
		case StatusQueued, StatusPendingPlacement:
			return PlacementBucketPlacing
		}
	}
	switch status {
	case StatusRunning, StatusStarting:
		return PlacementBucketRunning
	case StatusPaused:
		return PlacementBucketPaused
	case StatusPendingPlacement:
		if job.TargetKind() == JobTargetUnplaced {
			return PlacementBucketUnplaced
		}
		switch placementLaunchStatusForJob(job, launchStatusByID) {
		case LaunchStatusRunning, LaunchStatusGrace, LaunchStatusCompleted:
			return PlacementBucketQueued
		case LaunchStatusFailed, LaunchStatusCancelled:
			return PlacementBucketUnplaced
		}
		return PlacementBucketLaunching
	case StatusQueued:
		if job.TargetKind() == JobTargetUnplaced {
			return PlacementBucketUnplaced
		}
		switch placementLaunchStatusForJob(job, launchStatusByID) {
		case LaunchStatusPlanned, LaunchStatusLaunching:
			return PlacementBucketLaunching
		case LaunchStatusFailed, LaunchStatusCancelled:
			return PlacementBucketUnplaced
		case LaunchStatusRunning:
			if job.LaunchID != nil && launchesWithActiveJob[*job.LaunchID] {
				return PlacementBucketQueued
			}
			return PlacementBucketLaunching
		}
		return PlacementBucketQueued
	case StatusKilled, StatusCanceled:
		return PlacementBucketKilledCanceled
	case StatusFailed, StatusDead:
		return PlacementBucketFailures
	case StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return PlacementBucketFailures
		}
		return PlacementBucketCompletions
	default:
		return ""
	}
}

func placementLaunchStatusForJob(job *Job, launchStatusByID map[int64]string) string {
	if job == nil || job.LaunchID == nil || launchStatusByID == nil {
		return ""
	}
	return launchStatusByID[*job.LaunchID]
}
