package jobview

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

const recentMoveIntentWindow = 15 * time.Minute

type MoveDisplay struct {
	IntentID        int64
	State           db.MoveIntentState
	SourceAttemptID *int64
	TargetAttemptID *int64
	SourceLabel     string
	TargetLabel     string
	Phase           string
	CreatedAt       int64
	ResolvedAt      *int64
	Resolution      string
	AttemptsByID    map[int64]db.JobAttempt
}

func moveDisplayForJobs(database *sql.DB, jobs []*db.Job, now time.Time) (map[int64]*MoveDisplay, error) {
	if database == nil || len(jobs) == 0 {
		return map[int64]*MoveDisplay{}, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	since := now.Add(-recentMoveIntentWindow).Unix()
	out := make(map[int64]*MoveDisplay, len(jobs))
	for _, job := range jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		intent, err := db.GetRecentMoveIntent(database, job.ID, since)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return nil, err
		}
		if intent == nil {
			continue
		}
		attempts, err := db.ListAttempts(database, job.ID)
		if err != nil {
			return nil, err
		}
		byID := make(map[int64]db.JobAttempt, len(attempts))
		for _, attempt := range attempts {
			byID[attempt.ID] = attempt
		}
		display := &MoveDisplay{
			IntentID:        intent.ID,
			State:           intent.State,
			SourceAttemptID: intent.SourceAttemptID,
			TargetAttemptID: intent.TargetAttemptID,
			SourceLabel:     moveEndpointLabel(intent.SourceAttemptID, intent.SourceLaunchID, "", byID),
			TargetLabel:     moveTargetLabel(intent, byID),
			Phase:           moveIntentPhase(intent),
			CreatedAt:       intent.CreatedAt,
			ResolvedAt:      intent.ResolvedAt,
			Resolution:      strings.TrimSpace(intent.Resolution),
			AttemptsByID:    byID,
		}
		out[job.ID] = display
	}
	return out, nil
}

func ExpandJobsForOpenMoves(jobs []*db.Job, placementStatusByJob map[int64]PlacementStatus) []*db.Job {
	if len(jobs) == 0 || len(placementStatusByJob) == 0 {
		return jobs
	}
	out := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		ps := placementStatusByJob[job.ID]
		move := ps.Move
		if move == nil || move.State != db.MoveIntentStateOpen {
			out = append(out, job)
			continue
		}
		var source *db.Job
		if move.SourceAttemptID != nil || job.LaunchID == nil {
			source = cloneJobForMoveDisplay(job, move, move.SourceAttemptID, true)
		}
		if source != nil && source.DisplayMoveDim {
			out = append(out, source)
		}
		target := cloneJobForMoveDisplay(job, move, move.TargetAttemptID, false)
		if target != nil {
			out = append(out, target)
		}
	}
	return out
}

func cloneJobForMoveDisplay(job *db.Job, move *MoveDisplay, attemptID *int64, dim bool) *db.Job {
	if job == nil || move == nil {
		return job
	}
	clone := *job
	clone.DisplayMoveSource = move.SourceLabel
	clone.DisplayMoveTarget = move.TargetLabel
	clone.DisplayMovePhase = move.Phase
	clone.DisplayMoveDim = dim
	if attemptID == nil || *attemptID <= 0 {
		return &clone
	}
	attempt, ok := move.AttemptsByID[*attemptID]
	if !ok {
		return &clone
	}
	if dim && isTerminalAttempt(attempt) {
		return nil
	}
	clone.DisplayAttemptID = attempt.ID
	clone.DisplayAttemptNumber = attempt.AttemptNumber
	clone.Host = attempt.Host
	clone.LaunchID = attempt.LaunchID
	if attempt.Status != "" {
		clone.Status = attempt.Status
	}
	if attempt.QueuedAt != nil {
		clone.QueuedAt = *attempt.QueuedAt
	}
	if attempt.StartTime != nil {
		clone.StartTime = *attempt.StartTime
	} else {
		clone.StartTime = 0
	}
	clone.EndTime = attempt.EndTime
	clone.ExitCode = attempt.ExitCode
	clone.ErrorMessage = attempt.ErrorMessage
	clone.FailureReason = attempt.FailureReason
	if attempt.Backend != "" {
		clone.Backend = attempt.Backend
	}
	runID := attempt.ID
	clone.LatestRunID = &runID
	return &clone
}

func isTerminalAttempt(attempt db.JobAttempt) bool {
	if attempt.EndTime != nil {
		return true
	}
	return db.IsTerminalStatus(attempt.Status)
}

func moveEndpointLabel(attemptID, launchID *int64, fallbackHost string, attempts map[int64]db.JobAttempt) string {
	if attemptID != nil {
		if attempt, ok := attempts[*attemptID]; ok {
			if label := attemptTargetLabel(attempt); label != "" {
				return label
			}
		}
	}
	if launchID != nil && *launchID > 0 {
		return ids.FormatInstanceID(*launchID)
	}
	return strings.TrimSpace(fallbackHost)
}

func moveTargetLabel(intent *db.MoveIntent, attempts map[int64]db.JobAttempt) string {
	if intent == nil {
		return ""
	}
	if label := moveEndpointLabel(intent.TargetAttemptID, intent.TargetLaunchID, intent.TargetHost, attempts); label != "" {
		return label
	}
	if strings.TrimSpace(intent.TargetHost) != "" {
		return strings.TrimSpace(intent.TargetHost)
	}
	if intent.TargetKind == db.MoveTargetNew {
		if gpu := strings.TrimSpace(intent.TargetGPUName); gpu != "" {
			return "new " + gpu
		}
		return "new instance"
	}
	return ""
}

func attemptTargetLabel(attempt db.JobAttempt) string {
	if attempt.LaunchID != nil && *attempt.LaunchID > 0 {
		return ids.FormatInstanceID(*attempt.LaunchID)
	}
	return strings.TrimSpace(attempt.Host)
}

func moveIntentPhase(intent *db.MoveIntent) string {
	if intent == nil {
		return ""
	}
	switch intent.State {
	case db.MoveIntentStateOpen:
		switch {
		case intent.TargetAttemptID != nil:
			return "waiting for destination acceptance"
		case intent.TargetRequestID != "":
			return "waiting for destination launch"
		case intent.TargetKind == db.MoveTargetNew:
			return "requesting destination instance"
		default:
			return "waiting for destination acceptance"
		}
	case db.MoveIntentStateConfirmed:
		return "confirmed"
	case db.MoveIntentStateCanceled:
		return "canceled"
	case db.MoveIntentStateObsoleted:
		return "obsoleted"
	default:
		return fmt.Sprintf("%s", intent.State)
	}
}
