package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestAutopilotStatusLineDisabledBeatsTransientStates(t *testing.T) {
	// A disabled autopilot must report "off" even when a doomed pass is
	// in flight and jobs are blocked — both of which would otherwise
	// render lines that imply the autopilot is working.
	line := autopilotStatusLine(autopilotDisplayInput{
		paused:         true,
		pausedReason:   "manual relaunch",
		inFlight:       true,
		passStartedAt:  time.Now(),
		blockedSummary: "waiting for output from wj2037",
		blockedJobs:    1,
	})
	if !strings.Contains(line, "off") {
		t.Fatalf("status line = %q, want off", line)
	}
	if !strings.Contains(line, "manual relaunch") {
		t.Fatalf("status line = %q, want pause reason", line)
	}
	for _, banned := range []string{"evaluating", "blocked"} {
		if strings.Contains(line, banned) {
			t.Fatalf("status line = %q, want off to override %q", line, banned)
		}
	}
}

func TestAutopilotStatusLineBlockedUsesBlockedWording(t *testing.T) {
	// The blocked state must say "blocked", not "paused" — the watch TUI
	// historically mislabeled it, colliding with the global enabled state.
	line := autopilotStatusLine(autopilotDisplayInput{
		blockedSummary: "no offers from providers",
		blockedJobs:    2,
	})
	if !strings.Contains(line, "Auto-pilot: blocked — no offers from providers (2 jobs)") {
		t.Fatalf("status line = %q, want blocked wording with count", line)
	}
	if strings.Contains(line, "paused") {
		t.Fatalf("status line = %q, blocked state must not say paused", line)
	}
}

func TestAutopilotStatusLineEmptyOnlyWhileBudgetPromptOpen(t *testing.T) {
	if line := autopilotStatusLine(autopilotDisplayInput{inputActive: true}); line != "" {
		t.Fatalf("status line = %q, want empty while the budget prompt is open", line)
	}
	if line := autopilotStatusLine(autopilotDisplayInput{paused: true}); line == "" {
		t.Fatal("a disabled autopilot must still render an off line")
	}
}

func TestAutopilotFooterState(t *testing.T) {
	if got := autopilotFooterState(false); got != "ON" {
		t.Fatalf("autopilotFooterState(false) = %q, want ON", got)
	}
	if got := autopilotFooterState(true); got != "OFF" {
		t.Fatalf("autopilotFooterState(true) = %q, want OFF", got)
	}
}

func TestAutopilotPauseStateReadsDB(t *testing.T) {
	database := db.SetupTestDB(t)

	if paused, reason := autopilotPauseState(database); paused || reason != "" {
		t.Fatalf("fresh DB: paused=%v reason=%q, want false/empty", paused, reason)
	}

	if _, err := db.PauseAutopilot(database, "tester", "verifying cu124 fix"); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	paused, reason := autopilotPauseState(database)
	if !paused {
		t.Fatal("after PauseAutopilot: paused = false, want true")
	}
	if reason != "verifying cu124 fix" {
		t.Fatalf("after PauseAutopilot: reason = %q, want %q", reason, "verifying cu124 fix")
	}

	if _, err := db.ResumeAutopilot(database); err != nil {
		t.Fatalf("ResumeAutopilot: %v", err)
	}
	if paused, _ := autopilotPauseState(database); paused {
		t.Fatal("after ResumeAutopilot: paused = true, want false")
	}
}
