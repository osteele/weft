package terminal

import (
	"time"

	"github.com/osteele/weft/internal/db"
)

// autoPilotPhaseLabel returns a short human-readable label for the current
// autopilot pass phase, derived from the most recent relaunch.* lifecycle
// event since the pass started. Returns "" when there's nothing useful to
// display (no event, terminal pass_summary, or event too stale to still be
// representative). Keeps the AP status line informative without requiring
// the orchestration layer to push phase updates over a channel.
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
			return "launch failed: " + truncatePhase(msg, 60)
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
	case db.EventRelaunchPassSummary:
		return ""
	}
	return ""
}

// autoPilotPhaseQueryWindow is the lookback when probing for the current
// pass's latest event. Passes normally complete well within a minute but
// wedged RunPod SSH bootstraps have been seen running 20+ minutes.
const autoPilotPhaseQueryWindow = 30 * time.Minute

// activePassPhase formats the phase suffix for the in-progress AP status
// line: " — scanning candidates" when a recent relaunch event maps to a
// label, empty string otherwise. Only events occurring after the pass
// started are shown, so the label reflects THIS pass, not a prior one.
func activePassPhase(label string, eventAt, passStartedAt time.Time) string {
	if label == "" {
		return ""
	}
	if !passStartedAt.IsZero() && eventAt.Before(passStartedAt) {
		return ""
	}
	return " — " + label
}

func truncatePhase(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}
