package terminal

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestAutoPilotPhaseLabel(t *testing.T) {
	cases := []struct {
		name string
		ev   *db.LifecycleEvent
		want string
	}{
		{"nil event", nil, ""},
		{"eligible", &db.LifecycleEvent{EventKind: db.EventRelaunchEligible}, "scanning candidates"},
		{"disk bump", &db.LifecycleEvent{EventKind: db.EventRelaunchDiskBump}, "raising disk allowance"},
		{"launch success", &db.LifecycleEvent{EventKind: db.EventRelaunchLaunchSuccess}, "pod launched, bootstrapping"},
		{"launch failed without error text", &db.LifecycleEvent{EventKind: db.EventRelaunchLaunchFailed}, "launch failed"},
		{"launch failed with error text uses first line", &db.LifecycleEvent{EventKind: db.EventRelaunchLaunchFailed, ErrorText: "pod not ready\nsecond line"}, "launch failed: pod not ready"},
		{"pass_summary maps to empty", &db.LifecycleEvent{EventKind: db.EventRelaunchPassSummary}, ""},
		{"unknown kind", &db.LifecycleEvent{EventKind: "something.else"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoPilotPhaseLabel(tc.ev); got != tc.want {
				t.Errorf("autoPilotPhaseLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActivePassPhaseIgnoresEventsBeforePassStart(t *testing.T) {
	// Events from a prior autopilot pass must not leak into the current
	// pass's status line — otherwise the user sees stale phase text that
	// has no relationship to what's happening right now.
	start := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	stale := start.Add(-5 * time.Minute)
	fresh := start.Add(2 * time.Second)

	if got := activePassPhase("scanning candidates", stale, start); got != "" {
		t.Errorf("stale event should produce empty suffix, got %q", got)
	}
	if got := activePassPhase("scanning candidates", fresh, start); got != " — scanning candidates" {
		t.Errorf("fresh event suffix = %q, want %q", got, " — scanning candidates")
	}
	if got := activePassPhase("", fresh, start); got != "" {
		t.Errorf("empty label should produce empty suffix, got %q", got)
	}
}
