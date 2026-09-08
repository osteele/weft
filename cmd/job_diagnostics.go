package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// printJobLocalDiagnostics is the shared diagnostic surface for `weft
// job info` and `weft job status`. See specs/job-lifecycle.allium —
// new local-only signals SHOULD extend this so both verbs grow
// together.
func printJobLocalDiagnostics(w io.Writer, database *sql.DB, job *db.Job) {
	printJobKillAttribution(w, database, job)
	for _, line := range jobPublicationDiagnosticLines(database, job) {
		fmt.Fprintln(w, line)
	}
	if !hasFailureSignal(job) {
		return
	}
	printDiagnosisSummary(w, job)
	printLaunchTerminationDetail(w, database, job)
	printJobPhasesAndPeaks(w, database, job)
}

func printJobKillAttribution(w io.Writer, database *sql.DB, job *db.Job) {
	if database == nil || job == nil {
		return
	}
	attribution := db.JobKillAttribution{
		Actor:    job.KillActor,
		Reason:   job.KillReason,
		KilledAt: job.KilledAt,
	}
	if attribution.KilledAt == nil {
		var err error
		attribution, err = db.JobKillAttributionForJob(database, job.ID)
		if err != nil {
			return
		}
	}
	if attribution.KilledAt == nil {
		return
	}
	fmt.Fprintf(w, "Killed At:    %s\n", time.Unix(*attribution.KilledAt, 0).Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "Kill Actor:   %s\n", attribution.Actor)
	if attribution.Reason != "" {
		fmt.Fprintf(w, "Kill Reason:  %s\n", attribution.Reason)
	}
}

func jobPublicationDiagnosticLines(database *sql.DB, job *db.Job) []string {
	if database == nil || job == nil {
		return nil
	}
	publication, err := db.GetLatestAttemptPublicationState(database, job.ID)
	if err != nil || publication == nil {
		return nil
	}
	lines := []string{
		formatPublicationFacet("Execution", publication.ExecutionState, publication.ExecutionCompletedAt),
		formatPublicationFacet("Artifacts", publication.RequiredArtifactsState, publication.RequiredArtifactsReadyAt),
	}
	drain := formatPublicationFacet("Drain", publication.DrainState, publication.DrainCompletedAt)
	var remaining []string
	if publication.QueuedItems != nil || publication.InflightItems != nil {
		items := 0
		if publication.QueuedItems != nil {
			items += *publication.QueuedItems
		}
		if publication.InflightItems != nil {
			items += *publication.InflightItems
		}
		remaining = append(remaining, fmt.Sprintf("%d item(s) remaining", items))
	}
	if publication.QueuedBytes != nil || publication.InflightBytes != nil {
		bytes := int64(0)
		if publication.QueuedBytes != nil {
			bytes += *publication.QueuedBytes
		}
		if publication.InflightBytes != nil {
			bytes += *publication.InflightBytes
		}
		remaining = append(remaining, humanizeBytes(bytes)+" remaining")
	}
	if len(remaining) > 0 {
		drain += "; " + strings.Join(remaining, ", ")
	}
	lines = append(lines, drain)
	if publication.LastProgressAt != nil {
		lines = append(lines, "Publication progress: "+formatUnixTime(*publication.LastProgressAt))
	}
	if publication.UnknownReason != "" {
		lines = append(lines, "Publication unknown: "+publication.UnknownReason)
	}
	if publication.Detail != "" {
		lines = append(lines, "Publication detail: "+publication.Detail)
	}
	return lines
}

func formatPublicationFacet(label, state string, completedAt *int64) string {
	line := label + ": " + state
	if completedAt != nil {
		line += " at " + time.Unix(*completedAt, 0).Format("2006-01-02 15:04:05")
	}
	return line
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

func printLaunchTerminationDetail(w io.Writer, database *sql.DB, job *db.Job) {
	if database == nil || job == nil || job.LaunchID == nil || *job.LaunchID == 0 {
		return
	}
	launch, err := db.GetLaunch(database, *job.LaunchID)
	if err != nil || launch == nil || launch.TerminationDetail == "" {
		return
	}
	if launch.Status == db.LaunchStatusFailed || launch.Status == db.LaunchStatusGrace {
		fmt.Fprintf(w, "Termination: %s\n", launch.TerminationDetail)
	}
}

func printJobPhasesAndPeaks(w io.Writer, database *sql.DB, job *db.Job) {
	if database == nil || job == nil {
		return
	}
	t, err := db.GetJobPhaseTimings(database, job.ID)
	if err != nil || t == nil {
		return
	}
	if phases := formatPhaseDurations(t); phases != "" {
		fmt.Fprintf(w, "Phases:    %s\n", phases)
	}
	if gpu := formatGPUMetrics(t); gpu != "" {
		fmt.Fprintf(w, "GPU stats: %s\n", gpu)
	}
}
