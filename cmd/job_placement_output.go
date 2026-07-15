package cmd

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/queueblock"
)

type jobPlacementLine struct {
	Label string
	Value string
}

func queuedPlacementLines(database *sql.DB, job *db.Job) []jobPlacementLine {
	if job == nil {
		return nil
	}
	status := job.EffectiveStatus()
	if status != db.StatusQueued && status != db.StatusPendingPlacement {
		return nil
	}

	lines := []jobPlacementLine{{Label: "Placement", Value: placementSummary(job)}}
	if job.StartTime == 0 {
		if reason := queueReasonSummary(database, job); reason != "" {
			lines = append(lines, jobPlacementLine{Label: "Queue reason", Value: reason})
		}
	}
	lines = append(lines, queuedExpectationLines(database, job)...)
	return lines
}

func placementSummary(job *db.Job) string {
	switch job.TargetKind() {
	case db.JobTargetExternal:
		return "external executor"
	case db.JobTargetUnplaced:
		return "unplaced, awaiting assignment"
	case db.JobTargetRentalInstance:
		return "assigned to " + job.TargetDisplay()
	case db.JobTargetInventoryHost:
		return "assigned to " + job.TargetDisplay()
	default:
		return job.TargetDisplay()
	}
}

func queueReasonSummary(database *sql.DB, job *db.Job) string {
	if database != nil {
		if running := runningJobAhead(database, job); running != nil {
			return "waiting behind " + ids.FormatJobID(running.ID)
		}
	}
	switch job.TargetKind() {
	case db.JobTargetExternal:
		return "waiting for external executor status"
	case db.JobTargetUnplaced:
		if result := blockreason.Resolve(job, blockreason.Options{Compact: true}); result.Blocked {
			reason := blockreason.DisplayReasonForKind(result.Kind, result.Reason)
			return fmt.Sprintf("%s: %s - see weft diagnose job %s", result.Kind, reason, ids.FormatJobID(job.ID))
		}
		return "awaiting assignment"
	case db.JobTargetInventoryHost:
		if reason := queueBlockedReasonSummary(job); reason != "" {
			return reason
		}
		if job.LastSyncedStatus != db.StatusQueued {
			status, err := daemoncontrol.CurrentStatus(daemoncontrol.DefaultPaths())
			if err != nil {
				return "waiting for daemon dispatch; daemon status unavailable"
			}
			switch {
			case status.Live:
				return "waiting for daemon dispatch"
			case status.Stale:
				return "waiting for daemon dispatch; daemon stale"
			default:
				return "waiting for daemon dispatch; daemon stopped"
			}
		}
		return "waiting for assigned target to start the job"
	case db.JobTargetRentalInstance:
		return "waiting for assigned target to start the job"
	default:
		return ""
	}
}

func queueBlockedReasonSummary(job *db.Job) string {
	if job == nil {
		return ""
	}
	display := queueblock.Display(job, nil)
	if display.Kind != "" && display.Reason != "" {
		return display.Kind + ": " + display.Reason
	}
	return ""
}

func queuedExpectationLines(database *sql.DB, job *db.Job) []jobPlacementLine {
	if job == nil {
		return nil
	}
	lines := []jobPlacementLine{}
	if waiting := queuedWaitingDuration(job, time.Now()); waiting != "" {
		lines = append(lines, jobPlacementLine{Label: "Waiting", Value: waiting})
	}
	lines = append(lines, jobPlacementLine{Label: "Normal range", Value: normalQueueRange(job)})
	lines = append(lines, jobPlacementLine{Label: "Action", Value: queuedAction(database, job)})
	return lines
}

func queuedWaitingDuration(job *db.Job, now time.Time) string {
	if job == nil {
		return ""
	}
	since := job.QueuedAt
	if since <= 0 {
		since = job.CreatedAt
	}
	if since <= 0 {
		return ""
	}
	elapsed := now.Unix() - since
	if elapsed <= 0 {
		return ""
	}
	return db.FormatDuration(elapsed)
}

func normalQueueRange(job *db.Job) string {
	if job == nil {
		return "placement and dispatch can take minutes; status will update when the target changes"
	}
	switch job.TargetKind() {
	case db.JobTargetRentalInstance:
		return "new rental startup commonly takes 5-40m after assignment; Weft may replace failed launches"
	case db.JobTargetInventoryHost:
		return "inventory dispatch usually starts within one daemon sync after the target is free"
	case db.JobTargetExternal:
		return "external executors report asynchronously; watch job status rather than local processes"
	case db.JobTargetUnplaced:
		if job.UsesRentalPlacement() || job.ProviderName() != "" {
			return "rental placement commonly takes 5-40m and may retry 1-6 provider offers or launches"
		}
		return "autopilot placement can take minutes; rental fallback may retry offers or launches"
	default:
		return "placement and dispatch can take minutes; status will update when the target changes"
	}
}

func queuedAction(database *sql.DB, job *db.Job) string {
	if job == nil {
		return "wait; keep monitoring at the job level"
	}
	if job.TargetKind() == db.JobTargetUnplaced {
		if result := blockreason.Resolve(job, blockreason.Options{Compact: true}); result.Blocked {
			return "inspect blocker with weft diagnose job " + ids.FormatJobID(job.ID)
		}
		return "wait; autopilot owns placement, monitor with weft status " + ids.FormatJobID(job.ID) + " --wait"
	}
	if database != nil {
		if running := runningJobAhead(database, job); running != nil {
			return "wait; queued behind " + ids.FormatJobID(running.ID) + ", monitor with weft status " + ids.FormatJobID(job.ID) + " --wait"
		}
	}
	return "wait; no manual retry or kill indicated, monitor with weft status " + ids.FormatJobID(job.ID) + " --wait"
}

func runningJobAhead(database *sql.DB, job *db.Job) *db.Job {
	var jobs []*db.Job
	var err error
	switch job.TargetKind() {
	case db.JobTargetRentalInstance:
		if job.LaunchID == nil || *job.LaunchID <= 0 {
			return nil
		}
		jobs, err = db.GetLaunchJobs(database, *job.LaunchID)
	case db.JobTargetInventoryHost:
		jobs, err = db.ListActiveJobs(database, job.Host)
	default:
		return nil
	}
	if err != nil {
		return nil
	}
	for _, other := range jobs {
		if other == nil || other.ID == job.ID {
			continue
		}
		switch other.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			return other
		}
	}
	return nil
}

func printPlacementLines(lines []jobPlacementLine, labelWidth int) {
	for _, line := range lines {
		fmt.Printf("%-*s %s\n", labelWidth, line.Label+":", line.Value)
	}
}
