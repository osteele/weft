package jobview

import (
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

type Bucket string

const (
	BucketRunning        Bucket = "running"
	BucketPaused         Bucket = "paused"
	BucketPlacing        Bucket = "placing"
	BucketLaunching      Bucket = "launching"
	BucketQueued         Bucket = "queued"
	BucketUnplaced       Bucket = "unplaced"
	BucketCompletions    Bucket = "completions"
	BucketFailures       Bucket = "failures"
	BucketKilledCanceled Bucket = "killed_canceled"
)

type ClassifyInput struct {
	LaunchStatusByID   map[int64]string
	LaunchesWithActive map[int64]bool
	// LaunchEverReady marks launches whose agent has signaled readiness at
	// least once (db.Launch.AgentReadyAtUnix is set). Once an agent has been
	// ready, "Launching" no longer applies — queued jobs on such a launch
	// belong in BucketQueued even if no job is currently active (e.g. the
	// agent is between jobs uploading outputs or polling for new work).
	LaunchEverReady      map[int64]bool
	HasOpenPlacingIntent bool
}

func LaunchesWithActiveJob(jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState) map[int64]bool {
	out := make(map[int64]bool)
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil {
			continue
		}
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			out[*job.LaunchID] = true
		}
	}
	for launchID, live := range launchLiveByID {
		if live == nil {
			continue
		}
		verb, phaseJobID, ok := campaign.ParsePhaseJobID(live.InstancePhase)
		if ok && verb == campaign.PhaseRunning && phaseJobID > 0 {
			out[launchID] = true
		}
	}
	return out
}

// LaunchesEverReady returns the set of launch IDs whose agent has signaled
// readiness at least once. "Ready" is determined from db.Launch.IsAgentReady,
// which is set by the live-state sync on the first transition.
func LaunchesEverReady(launchByID map[int64]*db.Launch) map[int64]bool {
	out := make(map[int64]bool, len(launchByID))
	for id, launch := range launchByID {
		if launch.IsAgentReady() {
			out[id] = true
		}
	}
	return out
}

// DisplayBucket returns the section bucket for a possibly move-expanded display
// row. It is the single place that decides how a row lands in a grouped list,
// so display code never re-derives buckets of its own:
//   - a dim source move row keeps its own authoritative status;
//   - a move target row is "placing" while the move stays open;
//   - any other row trusts the bucket precomputed in PlacementStatus, falling
//     back to a fresh classification only when none was computed.
//
// The caller sets input.HasOpenPlacingIntent for the fallback case; the move
// rows override it because their role, not a placing intent, fixes the bucket.
func DisplayBucket(job *db.Job, input ClassifyInput, placementStatusByJob map[int64]PlacementStatus) Bucket {
	if job == nil {
		return ""
	}
	if job.DisplayMoveDim {
		input.HasOpenPlacingIntent = false
		return ClassifyBucket(job, input)
	}
	if strings.TrimSpace(job.DisplayMoveSource) != "" && strings.TrimSpace(job.DisplayMoveTarget) != "" {
		input.HasOpenPlacingIntent = true
		return ClassifyBucket(job, input)
	}
	if ps, ok := placementStatusByJob[job.ID]; ok && ps.Bucket != "" {
		return ps.Bucket
	}
	return ClassifyBucket(job, input)
}

func ClassifyBucket(job *db.Job, input ClassifyInput) Bucket {
	if job == nil {
		return ""
	}
	status := job.EffectiveStatus()
	if launchStatusForJob(job, input.LaunchStatusByID) == db.LaunchStatusPaused {
		switch status {
		case db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusPendingPlacement, db.StatusPaused:
			return BucketPaused
		}
	}
	if input.HasOpenPlacingIntent {
		switch status {
		case db.StatusQueued, db.StatusPendingPlacement:
			return BucketPlacing
		}
	}
	switch status {
	case db.StatusRunning, db.StatusStarting:
		return BucketRunning
	case db.StatusPaused:
		return BucketPaused
	case db.StatusPendingPlacement:
		if job.TargetKind() == db.JobTargetUnplaced {
			return BucketUnplaced
		}
		switch launchStatusForJob(job, input.LaunchStatusByID) {
		case db.LaunchStatusRunning, db.LaunchStatusGrace, db.LaunchStatusCompleted:
			return BucketQueued
		case db.LaunchStatusFailed, db.LaunchStatusCancelled:
			return BucketUnplaced
		}
		return BucketLaunching
	case db.StatusQueued:
		if job.TargetKind() == db.JobTargetUnplaced {
			return BucketUnplaced
		}
		switch launchStatusForJob(job, input.LaunchStatusByID) {
		case db.LaunchStatusPlanned, db.LaunchStatusLaunching:
			return BucketLaunching
		case db.LaunchStatusFailed, db.LaunchStatusCancelled:
			return BucketUnplaced
		case db.LaunchStatusRunning:
			if job.LaunchID != nil {
				// Once the agent has been ready, the launch is past
				// bootstrap and "Launching" no longer applies — even if
				// the agent is between jobs (uploading outputs, draining
				// background work, polling for new work).
				if input.LaunchEverReady[*job.LaunchID] {
					return BucketQueued
				}
				if input.LaunchesWithActive[*job.LaunchID] {
					return BucketQueued
				}
			}
			return BucketLaunching
		}
		return BucketQueued
	case db.StatusKilled, db.StatusCanceled:
		return BucketKilledCanceled
	case db.StatusFailed, db.StatusDead:
		return BucketFailures
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return BucketFailures
		}
		return BucketCompletions
	default:
		return ""
	}
}

func launchStatusForJob(job *db.Job, launchStatusByID map[int64]string) string {
	if job == nil || job.LaunchID == nil || launchStatusByID == nil {
		return ""
	}
	return launchStatusByID[*job.LaunchID]
}
