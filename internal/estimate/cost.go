package estimate

import (
	"database/sql"
	"math"
	"time"

	"github.com/osteele/weft/internal/db"
)

type RentalCostInfo struct {
	Cost        float64
	Basis       string
	Provisional bool
}

type RentalHistoryCostInfo struct {
	Cost        float64
	Launches    int
	Provisional bool
}

func LaunchCostSoFar(launch *db.Launch, now time.Time) float64 {
	if launch == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if launch.LaunchedAt != nil && launch.CostPerHourCents > 0 {
		start := time.Unix(*launch.LaunchedAt, 0)
		end := now
		if launch.EndedAt != nil && *launch.EndedAt > 0 {
			end = time.Unix(*launch.EndedAt, 0)
		}
		if end.Before(start) {
			end = start
		}
		return end.Sub(start).Hours() * (float64(launch.CostPerHourCents) / 100.0)
	}
	if launch.ActualSpendCents > 0 {
		return float64(launch.ActualSpendCents) / 100.0
	}
	return 0
}

func RentalCostSummary(database *sql.DB, job *db.Job, now time.Time) (RentalCostInfo, bool) {
	if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
		return RentalCostInfo{}, false
	}
	launch, err := db.GetLaunch(database, *job.LaunchID)
	if err != nil || launch == nil {
		return RentalCostInfo{}, false
	}

	currentJobs, err := db.GetLaunchJobs(database, *job.LaunchID)
	if err != nil {
		return RentalCostInfo{}, false
	}
	isOnlyJob := len(currentJobs) == 1 && currentJobs[0] != nil && currentJobs[0].ID == job.ID

	if isOnlyJob {
		return RentalCostInfo{
			Cost:        LaunchCostSoFar(launch, now),
			Basis:       "instance total",
			Provisional: !launch.IsTerminal(),
		}, true
	}

	ratePerHour := float64(launch.CostPerHourCents) / 100.0
	if ratePerHour <= 0 {
		return RentalCostInfo{}, false
	}
	timings, _ := db.GetJobPhaseTimings(database, job.ID)
	setup := JobSetupDuration(job, timings, now)
	run := JobRunElapsedDuration(job, timings, now)
	billable := setup + run
	if billable <= 0 {
		return RentalCostInfo{}, false
	}
	cost := math.Max(0, billable.Hours()*ratePerHour)
	return RentalCostInfo{
		Cost:        cost,
		Basis:       "shared instance: setup + run",
		Provisional: !launch.IsTerminal(),
	}, true
}

// RentalHistoryCostSummary totals launches attributable only to this job.
// Shared launches are excluded because an instance total cannot be assigned to
// one job without a supported allocation record.
func RentalHistoryCostSummary(database *sql.DB, job *db.Job, now time.Time) (RentalHistoryCostInfo, bool) {
	if database == nil || job == nil {
		return RentalHistoryCostInfo{}, false
	}
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil {
		return RentalHistoryCostInfo{}, false
	}
	launchIDs := make(map[int64]struct{})
	for _, attempt := range attempts {
		if attempt.LaunchID != nil && *attempt.LaunchID > 0 {
			launchIDs[*attempt.LaunchID] = struct{}{}
		}
	}
	if len(launchIDs) < 2 {
		return RentalHistoryCostInfo{}, false
	}

	info := RentalHistoryCostInfo{Launches: len(launchIDs)}
	for launchID := range launchIDs {
		launch, err := db.GetLaunch(database, launchID)
		if err != nil || launch == nil {
			return RentalHistoryCostInfo{}, false
		}
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, launchID)
		if err != nil || len(jobs) != 1 || jobs[0] == nil || jobs[0].ID != job.ID {
			return RentalHistoryCostInfo{}, false
		}
		info.Cost += LaunchCostSoFar(launch, now)
		info.Provisional = info.Provisional || !launch.IsTerminal()
	}
	return info, true
}
