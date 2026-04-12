package estimate

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

func JobElapsedDuration(job *db.Job, now time.Time) time.Duration {
	if job == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if job.StartTime <= 0 {
		return 0
	}
	start := time.Unix(job.StartTime, 0)
	end := now
	if job.EndTime != nil && *job.EndTime > 0 {
		end = time.Unix(*job.EndTime, 0)
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func JobSetupDuration(job *db.Job, timings *db.JobPhaseTimings, now time.Time) time.Duration {
	if job == nil || timings == nil || timings.SetupStart == nil || *timings.SetupStart <= 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	start := time.Unix(*timings.SetupStart, 0)
	switch {
	case timings.SetupEnd != nil && *timings.SetupEnd > 0:
		end := time.Unix(*timings.SetupEnd, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	case timings.RunStart != nil && *timings.RunStart > 0:
		end := time.Unix(*timings.RunStart, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	}
	if job.EndTime != nil && *job.EndTime > 0 {
		end := time.Unix(*job.EndTime, 0)
		if end.After(start) {
			return end.Sub(start)
		}
	}
	if now.After(start) {
		return now.Sub(start)
	}
	return 0
}

func JobRunElapsedDuration(job *db.Job, timings *db.JobPhaseTimings, now time.Time) time.Duration {
	if job == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	if timings != nil && timings.RunStart != nil && *timings.RunStart > 0 {
		start := time.Unix(*timings.RunStart, 0)
		end := now
		if timings.RunEnd != nil && *timings.RunEnd > 0 {
			end = time.Unix(*timings.RunEnd, 0)
		} else if job.EndTime != nil && *job.EndTime > 0 {
			end = time.Unix(*job.EndTime, 0)
		}
		if end.After(start) {
			return end.Sub(start)
		}
		return 0
	}
	return JobElapsedDuration(job, now)
}

func predictionToEstimate(predictedSeconds *float64) (Estimate, bool) {
	if predictedSeconds == nil || *predictedSeconds <= 0 {
		return Estimate{}, false
	}
	meanSeconds := *predictedSeconds
	return FromSeconds(meanSeconds, meanSeconds*0.5, meanSeconds*2.0), true
}

func JobTimeEstimate(job *db.Job, database *sql.DB, now time.Time) (Estimate, bool) {
	if job == nil {
		return Estimate{}, false
	}
	timeEstimate, ok := predictionToEstimate(nil)
	if job.PlacementMeta != nil {
		timeEstimate, ok = predictionToEstimate(job.PlacementMeta.PredictedDurationS)
	}
	if !ok {
		timeEstimate = DefaultJobDuration
		ok = true
	}
	if job.LaunchID != nil && *job.LaunchID > 0 {
		if timings, err := db.GetJobPhaseTimings(database, job.ID); err == nil && timings != nil {
			setup := JobSetupDuration(job, timings, now)
			if setup > 0 {
				timeEstimate = Estimate{
					Mean:  timeEstimate.Mean + setup,
					Lower: timeEstimate.Lower + setup,
					Upper: timeEstimate.Upper + setup,
				}
			}
		}
	}
	return timeEstimate, ok
}

func Remaining(total Estimate, elapsed time.Duration) Estimate {
	remaining := Estimate{
		Mean:  max(0, total.Mean-elapsed),
		Lower: max(0, total.Lower-elapsed),
		Upper: max(0, total.Upper-elapsed),
	}
	if remaining.Upper < remaining.Lower {
		remaining.Upper = remaining.Lower
	}
	if remaining.Mean < remaining.Lower {
		remaining.Mean = remaining.Lower
	}
	if remaining.Mean > remaining.Upper {
		remaining.Mean = remaining.Upper
	}
	return remaining
}
