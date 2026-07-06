package terminal

import (
	"database/sql"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// autopilotDisplayInput is the normalized per-surface autopilot state the
// shared status formatter needs. Both the list and watch TUIs build this
// from their own model and call autopilotStatusLine / autopilotFooterState,
// so the precedence and the "off"/"blocked" wording cannot drift between
// the two surfaces. The autopilot is a single global concept; there is no
// per-surface toggle.
type autopilotDisplayInput struct {
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

	targetCents    int
	suppressTarget bool
}

// autopilotStatusLine renders the "Auto-pilot: ..." status line, or "" only
// while the budget prompt is open. The disabled state is checked before every
// transient state so a disabled autopilot never renders as "evaluating" or
// "blocked" — both of which imply it is working.
func autopilotStatusLine(in autopilotDisplayInput) string {
	if in.inputActive {
		return ""
	}
	if in.paused {
		line := "Auto-pilot: off — press A to enable"
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
			return fmt.Sprintf("Auto-pilot: syncing %s...%s", strings.Join(in.syncHosts, ", "), autopilotTargetParen(target, in.suppressTarget))
		}
		return "Auto-pilot: syncing cloud state..." + autopilotTargetParen(target, in.suppressTarget)
	}
	if in.inFlight {
		elapsed := ""
		if !in.passStartedAt.IsZero() {
			elapsed = fmt.Sprintf(" (%s)", time.Since(in.passStartedAt).Round(time.Second))
		}
		phase := activePassPhase(in.passPhase, in.passStartedAt)
		return fmt.Sprintf("Auto-pilot: evaluating %s%s%s...%s",
			pluralize(in.unplaced, "unplaced job", "unplaced jobs"), phase, elapsed, autopilotTargetParen(target, in.suppressTarget))
	}
	if in.launching {
		return "Auto-pilot: launching instance..." + autopilotTargetParen(target, in.suppressTarget)
	}
	if err := strings.TrimSpace(in.persistentError); err != "" {
		return "Auto-pilot: failed — " + err
	}
	if summary := strings.TrimSpace(in.blockedSummary); summary != "" {
		if in.blockedJobs > 0 && (in.unplaced <= 0 || in.blockedJobs >= in.unplaced) {
			return fmt.Sprintf("Auto-pilot: blocked — %s (%d jobs)", summary, in.blockedJobs)
		}
		if in.blockedJobs <= 0 {
			return "Auto-pilot: blocked — " + summary
		}
	}
	if !in.nextPassAt.IsZero() && time.Now().Before(in.nextPassAt) {
		return formatAutoPilotNextPass(in.nextPassAt, in.unplaced)
	}
	if !in.nextSyncAt.IsZero() && time.Now().Before(in.nextSyncAt) {
		return fmt.Sprintf("Auto-pilot: idle — next sync in %s · watching DB (%d unplaced, %d running%s)",
			waitUntil(in.nextSyncAt), in.unplaced, in.running, autopilotTargetClause(target, in.suppressTarget))
	}
	return fmt.Sprintf("Auto-pilot: monitoring (%d unplaced, %d running%s)", in.unplaced, in.running, autopilotTargetClause(target, in.suppressTarget))
}

func autopilotTargetParen(target string, suppress bool) string {
	if suppress {
		return ""
	}
	return " (target " + target + ")"
}

func autopilotTargetClause(target string, suppress bool) string {
	if suppress {
		return ""
	}
	return ", target " + target
}

// autopilotFooterState returns the A-key footer token for the single global
// autopilot enabled/disabled state.
func autopilotFooterState(paused bool) string {
	if paused {
		return "OFF"
	}
	return "ON"
}

// autopilotActor identifies who toggled the autopilot, recorded as paused_by.
// Empty is acceptable — db.PauseAutopilot tolerates it.
func autopilotActor() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
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
