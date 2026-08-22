package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

type attemptDisplayProvenance struct {
	latest           *db.JobAttempt
	latestStarted    *db.JobAttempt
	currentUnstarted bool
}

func loadAttemptDisplayProvenance(database *sql.DB, job *db.Job) (attemptDisplayProvenance, error) {
	var provenance attemptDisplayProvenance
	if database == nil || job == nil {
		return provenance, nil
	}
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil {
		return provenance, err
	}
	if len(attempts) > 0 {
		latest := attempts[0]
		provenance.latest = &latest
	}
	for _, attempt := range attempts {
		if attempt.StartTime != nil {
			started := attempt
			provenance.latestStarted = &started
			break
		}
	}
	switch job.EffectiveStatus() {
	case db.StatusQueued, db.StatusPendingPlacement:
		provenance.currentUnstarted = provenance.latest == nil || provenance.latest.StartTime == nil
	}
	return provenance, nil
}

func printStatusAttemptProvenance(provenance attemptDisplayProvenance) {
	if provenance.latest == nil {
		return
	}
	if !provenance.currentUnstarted {
		fmt.Printf("Attempt:  #%d\n", provenance.latest.AttemptNumber)
		return
	}
	fmt.Println("Attempt:  current retry has not started")
	if provenance.latestStarted != nil {
		attempt := provenance.latestStarted
		fmt.Printf("Evidence: #%d %s on %s (latest started attempt)\n",
			attempt.AttemptNumber, attemptStatus(*attempt), attemptTarget(*attempt))
	}
}

func printAttemptLogHint(jobID int64, attempt *db.JobAttempt) {
	if attempt == nil {
		return
	}
	fmt.Printf("Logs:        weft log %s --attempt %d\n", ids.FormatJobID(jobID), attempt.AttemptNumber)
}

func jobForAttempt(job *db.Job, attempt *db.JobAttempt) *db.Job {
	if job == nil || attempt == nil {
		return job
	}
	selected := *job
	selected.Host = attempt.Host
	selected.LaunchID = attempt.LaunchID
	selected.LatestRunID = &attempt.ID
	selected.Status = attempt.Status
	selected.QueuedAt = 0
	if attempt.QueuedAt != nil {
		selected.QueuedAt = *attempt.QueuedAt
	}
	selected.StartTime = 0
	if attempt.StartTime != nil {
		selected.StartTime = *attempt.StartTime
	}
	selected.EndTime = attempt.EndTime
	selected.ExitCode = attempt.ExitCode
	selected.ErrorMessage = attempt.ErrorMessage
	selected.FailureReason = attempt.FailureReason
	selected.PendingStatus = nil
	selected.PendingAt = nil
	if attempt.Backend != "" {
		selected.Backend = attempt.Backend
	}
	return &selected
}
