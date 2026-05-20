package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// autopilotDisplayInput is the normalized per-surface autopilot state the
// shared status formatter needs. Both the list and watch TUIs build this
// from their own model and call autopilotStatusLine / autopilotFooterState,
// so the pause-first precedence and the "blocked" vs "paused" wording cannot
// drift between the two surfaces.
type autopilotDisplayInput struct {
	// autoMode is the per-TUI A-key toggle. When false there is no
	// autopilot line and the footer reads OFF.
	autoMode bool
	// inputActive suppresses the line while the run-rate budget prompt is
	// open (the prompt is rendered on the controls line instead).
	inputActive bool
	// paused and pausedReason come from the singleton autopilot_state row.
	paused       bool
	pausedReason string

	syncing   bool
	syncHosts []string

	inFlight      bool
	passPhase     autoPilotPhaseHint
	passStartedAt time.Time

	launching bool

	persistentError string

	// blockedSummary describes jobs the autopilot cannot place; blockedJobs
	// is the count, or 0 to omit the "(N jobs)" suffix.
	blockedSummary string
	blockedJobs    int

	nextPassAt time.Time
	nextSyncAt time.Time

	unplaced int
	running  int

	targetCents int
}

// autopilotStatusLine renders the "Auto-pilot: ..." status line, or "" when
// the surface should show no line. The singleton pause is checked before
// every transient state so a paused autopilot never renders as "evaluating"
// or "blocked" — both of which imply it is working.
func autopilotStatusLine(in autopilotDisplayInput) string {
	if !in.autoMode || in.inputActive {
		return ""
	}
	if in.paused {
		line := "Auto-pilot: paused — resume with `weft autopilot resume`"
		if reason := strings.TrimSpace(in.pausedReason); reason != "" {
			line += " (" + reason + ")"
		}
		return line
	}
	if line := formatAgentBuildStatus(); line != "" {
		return line
	}
	target := formatAutoRunRateTarget(in.targetCents)
	if in.syncing {
		if len(in.syncHosts) > 0 {
			return fmt.Sprintf("Auto-pilot: syncing %s... (target %s)", strings.Join(in.syncHosts, ", "), target)
		}
		return "Auto-pilot: syncing cloud state... (target " + target + ")"
	}
	if in.inFlight {
		elapsed := ""
		if !in.passStartedAt.IsZero() {
			elapsed = fmt.Sprintf(" (%s)", time.Since(in.passStartedAt).Round(time.Second))
		}
		phase := activePassPhase(in.passPhase, in.passStartedAt)
		return fmt.Sprintf("Auto-pilot: evaluating %s%s%s... (target %s)",
			pluralize(in.unplaced, "unplaced job", "unplaced jobs"), phase, elapsed, target)
	}
	if in.launching {
		return "Auto-pilot: launching instance... (target " + target + ")"
	}
	if err := strings.TrimSpace(in.persistentError); err != "" {
		return "Auto-pilot: failed — " + err
	}
	if summary := strings.TrimSpace(in.blockedSummary); summary != "" {
		if in.blockedJobs > 0 {
			return fmt.Sprintf("Auto-pilot: blocked — %s (%d jobs)", summary, in.blockedJobs)
		}
		return "Auto-pilot: blocked — " + summary
	}
	if !in.nextPassAt.IsZero() && time.Now().Before(in.nextPassAt) {
		return formatAutoPilotNextPass(in.nextPassAt, in.unplaced)
	}
	if !in.nextSyncAt.IsZero() && time.Now().Before(in.nextSyncAt) {
		return fmt.Sprintf("Auto-pilot: idle — next sync in %s · watching DB (%d unplaced, %d running, target %s)",
			waitUntil(in.nextSyncAt), in.unplaced, in.running, target)
	}
	return fmt.Sprintf("Auto-pilot: monitoring (%d unplaced, %d running, target %s)", in.unplaced, in.running, target)
}

// autopilotFooterState returns the A-key footer token. PAUSED is shown when
// the local toggle is on but the singleton autopilot is paused, so the
// footer never claims this TUI is placing jobs while it cannot.
func autopilotFooterState(autoMode, paused bool) string {
	switch {
	case !autoMode:
		return "OFF"
	case paused:
		return "PAUSED"
	default:
		return "ON"
	}
}

// autopilotPauseState reads the singleton autopilot_state row and reports
// whether autopilot is paused, with the operator-supplied reason. A read
// error is treated as "not paused" — the display must not block on it.
func autopilotPauseState(database *sql.DB) (paused bool, reason string) {
	if database == nil {
		return false, ""
	}
	state, err := db.LoadAutopilotState(database)
	if err != nil || state == nil {
		return false, ""
	}
	return state.Paused, strings.TrimSpace(state.PausedReason)
}
