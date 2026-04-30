package instanceintent

import "testing"

func TestEffectiveState_ExplicitState(t *testing.T) {
	for _, s := range []State{StateOpen, StateDestroying, StateSucceeded, StateFailed} {
		m := &Marker{State: s, TerminalStatus: "failed"}
		if got := m.EffectiveState(); got != s {
			t.Errorf("explicit State=%q -> EffectiveState=%q", s, got)
		}
	}
}

func TestEffectiveState_DerivesFromTimestamps(t *testing.T) {
	cases := []struct {
		name string
		m    Marker
		want State
	}{
		{"empty marker", Marker{}, ""},
		{"open from terminal_status alone", Marker{TerminalStatus: "failed"}, StateOpen},
		{"destroying from start time", Marker{TerminalStatus: "failed", DestroyStartedAtUnix: 1}, StateDestroying},
		{"succeeded from success time", Marker{TerminalStatus: "failed", DestroyStartedAtUnix: 1, DestroySucceededAtUnix: 2}, StateSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.EffectiveState(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsActive(t *testing.T) {
	cases := []struct {
		state State
		want  bool
	}{
		{StateOpen, true},
		{StateDestroying, true},
		{StateSucceeded, false},
		{StateFailed, false},
	}
	for _, tc := range cases {
		m := &Marker{State: tc.state}
		if got := m.IsActive(); got != tc.want {
			t.Errorf("State=%q IsActive=%v, want %v", tc.state, got, tc.want)
		}
	}
	if (*Marker)(nil).IsActive() {
		t.Error("nil marker should not be active")
	}
}
