package terminal

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

// autoPilotPhaseHint is a short description of the autopilot's current
// phase, sourced from the most recent relaunch.* lifecycle event. Label
// is empty when nothing useful is available. At is the event timestamp
// and is used by activePassPhase to reject phases predating the current
// pass.
type autoPilotPhaseHint struct {
	Label string
	At    time.Time
}

// autoPilotPhaseQueryWindow is the lookback when probing for the latest
// relaunch event. Passes normally complete within a minute, but wedged
// RunPod SSH bootstraps have been seen running 20+ minutes.
const autoPilotPhaseQueryWindow = 30 * time.Minute

// autoPilotPhaseLabel maps a lifecycle event to a short status-line label.
// Returns "" when the event doesn't correspond to an actionable phase.
func autoPilotPhaseLabel(ev *db.LifecycleEvent) string {
	if ev == nil {
		return ""
	}
	switch ev.EventKind {
	case db.EventRelaunchEligible:
		return "scanning candidates"
	case db.EventRelaunchDiskBump:
		return "raising disk allowance"
	case db.EventRelaunchLaunchSuccess:
		return "pod launched, bootstrapping"
	case db.EventRelaunchLaunchFailed:
		if msg := firstLine(ev.ErrorText); msg != "" {
			return "launch failed: " + truncate(msg, 60)
		}
		return "launch failed"
	case db.EventRelaunchRunawayTripped, db.EventRelaunchRunawayBlocked:
		return "runaway breaker tripped"
	case db.EventRelaunchSkippedNoOffers:
		return "no offers match"
	case db.EventRelaunchSkippedOfferError:
		return "offer fetch error"
	case db.EventRelaunchSkippedMaxAttempts:
		return "max attempts reached"
	case db.EventRelaunchSkippedNoClient:
		return "no provider client"
	}
	return ""
}

// activePassPhase returns " — <label>" when the hint belongs to the
// current pass (event at-or-after passStartedAt), or "" otherwise.
// Stale hints are suppressed so a prior pass's phase doesn't leak into
// the next pass's status line.
func activePassPhase(hint autoPilotPhaseHint, passStartedAt time.Time) string {
	if hint.Label == "" {
		return ""
	}
	if !passStartedAt.IsZero() && hint.At.Before(passStartedAt) {
		return ""
	}
	return " — " + hint.Label
}

// loadLatestAutoPilotPhase fetches the most recent relaunch.* event within
// the query window and maps it to a phase hint. Returns a zero hint when
// no mappable event is present (including when the latest event is
// pass_summary, which means the pass completed).
func loadLatestAutoPilotPhase(database *sql.DB) autoPilotPhaseHint {
	if database == nil {
		return autoPilotPhaseHint{}
	}
	ev, err := db.LatestLifecycleEvent(database, db.LifecycleEventFilter{
		KindPrefix: "relaunch.",
		Since:      time.Now().Add(-autoPilotPhaseQueryWindow),
	})
	if err != nil || ev == nil {
		return autoPilotPhaseHint{}
	}
	label := autoPilotPhaseLabel(ev)
	if label == "" {
		return autoPilotPhaseHint{}
	}
	return autoPilotPhaseHint{Label: label, At: time.Unix(ev.OccurredAt, 0)}
}
