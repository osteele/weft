package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
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
	return lines
}

func placementSummary(job *db.Job) string {
	switch job.TargetKind() {
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
	case db.JobTargetUnplaced:
		return "awaiting assignment"
	case db.JobTargetRentalInstance, db.JobTargetInventoryHost:
		return "waiting for assigned target to start the job"
	default:
		return ""
	}
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
