package cmd

import (
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/db"
)

// printJobLocalDiagnostics is the shared diagnostic surface for `weft
// job info` and `weft job status`. See specs/job-lifecycle.allium —
// new local-only signals SHOULD extend this so both verbs grow
// together.
func printJobLocalDiagnostics(database *sql.DB, job *db.Job) {
	if !hasFailureSignal(job) {
		return
	}
	printDiagnosisSummary(job)
	printLaunchTerminationDetail(database, job)
	printJobPhasesAndPeaks(database, job)
}

// hasFailureSignal gates the local-diagnostics block so an in-progress
// or clean-completed job doesn't trigger a log-cache read + regex scan
// on every `weft job info` / `weft job status` invocation.
func hasFailureSignal(job *db.Job) bool {
	if job == nil {
		return false
	}
	if job.ExitCode != nil && *job.ExitCode != 0 {
		return true
	}
	if job.FailureReason != "" || job.ErrorMessage != "" || job.ErrorDiagnosis != "" {
		return true
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead, db.StatusKilled:
		return true
	}
	return false
}

func printLaunchTerminationDetail(database *sql.DB, job *db.Job) {
	if job.LaunchID == nil || *job.LaunchID == 0 {
		return
	}
	launch, err := db.GetLaunch(database, *job.LaunchID)
	if err != nil || launch == nil || launch.TerminationDetail == "" {
		return
	}
	if launch.Status == db.LaunchStatusFailed || launch.Status == db.LaunchStatusGrace {
		fmt.Printf("Termination: %s\n", launch.TerminationDetail)
	}
}

func printJobPhasesAndPeaks(database *sql.DB, job *db.Job) {
	t, err := db.GetJobPhaseTimings(database, job.ID)
	if err != nil || t == nil {
		return
	}
	if phases := formatPhaseDurations(t); phases != "" {
		fmt.Printf("Phases:    %s\n", phases)
	}
	if gpu := formatGPUMetrics(t); gpu != "" {
		fmt.Printf("GPU stats: %s\n", gpu)
	}
}
