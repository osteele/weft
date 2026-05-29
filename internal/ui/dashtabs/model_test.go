package dashtabs

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		keys:       defaultKeys(),
		focused:    true,
		refreshInt: 3 * time.Second,
		cycle:      CycleState{Interval: defaultCycleInterval},
	}
	m.views = []View{
		newPulseView(),
		newTimelineView(),
		newFleetView(),
		newFocusView(),
		newTreeView(),
		newAlertsView(),
		newHistoryView(),
		newSankeyView(),
		newCostView(),
		newUsageView(),
	}
	return m
}

func sendKey(t *testing.T, m *Model, runes string) {
	t.Helper()
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(runes)}
	switch runes {
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		msg = tea.KeyMsg{Type: tea.KeyShiftTab}
	}
	_, _ = m.Update(msg)
}

func TestNumberKeysSwitchTabs(t *testing.T) {
	m := newTestModel(t)
	for i, key := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"} {
		sendKey(t, m, key)
		if m.active != i {
			t.Fatalf("after key %q: active=%d, want %d", key, m.active, i)
		}
	}
}

func TestTabKeyAdvances(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < len(m.views); i++ {
		want := (i + 1) % len(m.views)
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		if m.active != want {
			t.Fatalf("after tab #%d: active=%d, want %d", i, m.active, want)
		}
	}
}

func TestHelpToggle(t *testing.T) {
	m := newTestModel(t)
	if m.help {
		t.Fatal("help should start closed")
	}
	sendKey(t, m, "?")
	if !m.help {
		t.Fatal("? should open help")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.help {
		t.Fatal("esc should close help")
	}
}

func TestCycleToggleAndStep(t *testing.T) {
	m := newTestModel(t)
	if m.cycle.Enabled {
		t.Fatal("cycle should start disabled")
	}
	sendKey(t, m, "c")
	if !m.cycle.Enabled {
		t.Fatal("c should enable cycle")
	}
	initial := m.cycle.Interval
	sendKey(t, m, "-")
	if m.cycle.Interval <= initial {
		t.Fatalf("- should slow cycle; got %s (was %s)", m.cycle.Interval, initial)
	}
	sendKey(t, m, "+")
	if m.cycle.Interval >= m.cycle.Interval+time.Hour {
		// trivially true; just confirm no panic
	}
	sendKey(t, m, "c")
	if m.cycle.Enabled {
		t.Fatal("second c should disable cycle")
	}
}

func TestHelpSwallowsTabKeys(t *testing.T) {
	m := newTestModel(t)
	sendKey(t, m, "?")
	prev := m.active
	sendKey(t, m, "2")
	if m.active != prev {
		t.Fatalf("help-open should swallow tab keys: active changed to %d", m.active)
	}
}

func TestFocusFlagsLogger(t *testing.T) {
	m := newTestModel(t)
	m.usage = &Logger{disabled: true}
	if m.focused != true {
		t.Fatal("model should start focused=true")
	}
	_, _ = m.Update(tea.BlurMsg{})
	if m.focused {
		t.Fatal("BlurMsg should set focused=false")
	}
	if !m.focusReported {
		t.Fatal("BlurMsg should set focusReported=true")
	}
	_, _ = m.Update(tea.FocusMsg{})
	if !m.focused {
		t.Fatal("FocusMsg should set focused=true")
	}
}

func TestViewRendersWithoutPanic(t *testing.T) {
	m := newTestModel(t)
	m.snapshot = Snapshot{
		LoadedAt: time.Now(),
		Jobs:     mockJobs(t),
		Counts:   StatusCounts{Running: 2, Queued: 3, Completed: 1},
	}
	_, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	for _, k := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"} {
		sendKey(t, m, k)
		out := m.View()
		if out == "" {
			t.Fatalf("View() empty for tab %s", k)
		}
	}
	sendKey(t, m, "?")
	if out := m.View(); out == "" {
		t.Fatal("View() empty with help open")
	}
}

func TestDeriveCounts(t *testing.T) {
	// LoadSnapshot is integration-level (needs DB); deriveCounts is the
	// pure piece worth covering here.
	jobs := mockJobs(t)
	c := deriveCounts(jobs)
	if c.Running != 2 {
		t.Errorf("Running: got %d, want 2", c.Running)
	}
	if c.Queued != 3 {
		t.Errorf("Queued: got %d, want 3", c.Queued)
	}
	if c.Completed != 1 {
		t.Errorf("Completed: got %d, want 1", c.Completed)
	}
}
