package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/explain"
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
	unknown := unpublishedQueueReason(database, job, time.Now())
	// A recorded blocker outranks queue position for placed targets: both
	// can be true at once, but naming the ahead job as the cause sends the
	// operator to act on a job that is not the problem. Unplaced jobs keep
	// the diagnose-pointing handling in the switch below.
	//
	// It outranks an unobserved queue for the same reason, and the case is
	// not hypothetical: a host that stops publishing often stops because the
	// very thing the blocker names has failed, so suppressing it would hide
	// the most actionable line exactly when it is most likely to be true. A
	// blocker is recorded on this job's own row, not derived from other jobs'
	// rows, so an unread queue does not make it less true.
	if database != nil {
		switch job.TargetKind() {
		case db.JobTargetInventoryHost, db.JobTargetRentalInstance:
			if blocker := recordedBlocker(database, job); blocker != "" {
				if unknown != "" {
					return fmt.Sprintf("%s (%s)", blocker, unknown)
				}
				if running := runningJobAhead(database, job); running != nil {
					return fmt.Sprintf("%s (also waiting behind %s)", blocker, ids.FormatJobID(running.ID))
				}
				return blocker
			}
		}
		if unknown != "" {
			return unknown
		}
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

// unpublishedQueueReason reports that an inventory host's queue is unobserved,
// and is empty whenever weft has a fresh publication to reason from.
//
// Queue position is derived from other jobs' rows, and those rows are only as
// current as the host's last published runner state. While that publication is
// missing or stale, weft knows nothing about the order on that host — including
// whether the jobs it would name as ahead are still running. Naming one asserts
// an order nobody observed, and the assertion outlives the execution it
// describes: reported as wb181, where status printed "waiting behind wj8967"
// and a growing wait for a job that had already failed on the host, while
// wj8967 itself had completed.
//
// Absence of a publication is not absence of progress, so this says unknown
// rather than naming a cause or a position. It speaks only where weft has a
// positive observation that has since gone stale; a host weft has never
// observed at all is left to the dispatch-side reasons below, which already
// distinguish a stopped daemon from a waiting one.
//
// It also stays quiet until the job is actually on the host's queue. A stopped
// daemon both halts dispatch and ages the publication past the freshness
// bound, so for a job still waiting to be dispatched the two conditions arrive
// together — and of the two, "daemon stopped" is the one the operator can act
// on. Reporting an unobserved queue there would replace an actionable cause
// with a true but useless observation about a queue the job has not reached.
func unpublishedQueueReason(database *sql.DB, job *db.Job, now time.Time) string {
	if database == nil || job == nil || job.TargetKind() != db.JobTargetInventoryHost || job.Host == "" {
		return ""
	}
	if job.LastSyncedStatus != db.StatusQueued {
		return ""
	}
	states, err := db.ListHostAgentStates(database)
	if err != nil {
		return ""
	}
	for _, state := range states {
		if state.Host != job.Host {
			continue
		}
		if state.RunningObservedAt <= 0 {
			break
		}
		age := now.Sub(time.Unix(state.RunningObservedAt, 0))
		// A future-dated observation is clock skew between whoever recorded it
		// and this process, so it establishes nothing about the host's queue —
		// classifyHostAgentStatus reaches the same conclusion and reports it as
		// a stale observation. Accepting it as fresh would assert an order from
		// a row weft's own agent-status calls untrustworthy.
		if age < 0 {
			return fmt.Sprintf(
				"%s last published runner state at a future timestamp; queue position and progress on it are unknown",
				job.Host)
		}
		if age <= hostAgentRuntimeFreshness {
			return ""
		}
		return fmt.Sprintf(
			"%s last published runner state %s ago; queue position and progress on it are unknown",
			job.Host, formatAgentObservationAge(age))
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

// recordedBlocker returns the blocking reason recorded on the job itself —
// the host-reported queue block, or a high-confidence explanation — when one
// is more proximate than queue position. Empty means no such blocker is
// recorded and head-of-line waiting, if any, is the whole story.
func recordedBlocker(database *sql.DB, job *db.Job) string {
	if display := queueblock.Display(job, nil); display.Kind != "" && display.Reason != "" {
		return display.Kind + ": " + display.Reason
	}
	if x := explain.ForJob(database, job, time.Now()); explanationHasHighConfidenceBlocker(x) {
		return explanationBlockerSummary(x)
	}
	return ""
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
	monitor := "monitor with weft status " + ids.FormatJobID(job.ID) + " --wait"
	unknown := unpublishedQueueReason(database, job, time.Now())
	behind := ""
	if database != nil && unknown == "" {
		if running := runningJobAhead(database, job); running != nil {
			behind = "queued behind " + ids.FormatJobID(running.ID)
		}
	}
	// The operative blocker comes first. Head-of-line position survives
	// only as parenthetical context; pointing the reader at the ahead job
	// as the cause is how a blocked job's diagnosis goes wrong. An unobserved
	// queue is reported the same way: it qualifies the blocker rather than
	// replacing it, because the blocker is recorded on this job's own row.
	if blocker := recordedBlocker(database, job); blocker != "" {
		switch {
		case unknown != "":
			return "wait; " + blocker + " (" + unknown + "), " + monitor
		case behind != "":
			return "wait; " + blocker + " (also " + behind + "), " + monitor
		}
		return "wait; " + blocker + ", " + monitor
	}
	if unknown != "" {
		return "wait; " + unknown + ", " + monitor
	}
	if behind != "" {
		return "wait; " + behind + ", " + monitor
	}
	return "wait; no manual retry or kill indicated, " + monitor
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

func printPlacementLines(w io.Writer, lines []jobPlacementLine, labelWidth int) {
	for _, line := range lines {
		fmt.Fprintf(w, "%-*s %s\n", labelWidth, line.Label+":", line.Value)
	}
}
