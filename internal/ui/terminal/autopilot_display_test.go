package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestAutopilotStatusLinePausedBeatsTransientStates(t *testing.T) {
	// A paused autopilot must report "paused" even when a doomed pass is
	// in flight and jobs are blocked — both of which would otherwise
	// render lines that imply the autopilot is working.
	line := autopilotStatusLine(autopilotDisplayInput{
		autoMode:       true,
		paused:         true,
		pausedReason:   "manual relaunch",
		inFlight:       true,
		passStartedAt:  time.Now(),
		blockedSummary: "waiting for output from wj2037",
		blockedJobs:    1,
	})
	if !strings.Contains(line, "paused") {
		t.Fatalf("status line = %q, want paused", line)
	}
	if !strings.Contains(line, "manual relaunch") {
		t.Fatalf("status line = %q, want pause reason", line)
	}
	for _, banned := range []string{"evaluating", "blocked"} {
		if strings.Contains(line, banned) {
			t.Fatalf("status line = %q, want paused to override %q", line, banned)
		}
	}
}

func TestAutopilotStatusLineBlockedUsesBlockedWording(t *testing.T) {
	// The blocked state must say "blocked", not "paused" — the watch TUI
	// historically mislabeled it, colliding with the singleton pause.
	line := autopilotStatusLine(autopilotDisplayInput{
		autoMode:       true,
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

func TestAutopilotStatusLineEmptyWhenAutoModeOff(t *testing.T) {
	if line := autopilotStatusLine(autopilotDisplayInput{autoMode: false, paused: true}); line != "" {
		t.Fatalf("status line = %q, want empty when auto mode off", line)
	}
}

func TestAutopilotFooterState(t *testing.T) {
	cases := []struct {
		autoMode bool
		paused   bool
		want     string
	}{
		{false, false, "OFF"},
		{false, true, "OFF"},
		{true, false, "ON"},
		{true, true, "PAUSED"},
	}
	for _, tc := range cases {
		if got := autopilotFooterState(tc.autoMode, tc.paused); got != tc.want {
			t.Fatalf("autopilotFooterState(%v, %v) = %q, want %q", tc.autoMode, tc.paused, got, tc.want)
		}
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
