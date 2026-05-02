package terminal

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// setupBudgetTest redirects config writes to a temp dir and returns a fresh
// test DB so handleAutoBudgetKey's side effects don't leak.
func setupBudgetTest(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	restore := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	t.Cleanup(restore)
	return db.SetupTestDB(t)
}

func TestParseAutoDollarInput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		label     string
		wantCents int
		wantErr   string
	}{
		{name: "plain number", input: "2.5", label: "rate", wantCents: 250},
		{name: "dollar prefix", input: "$1.25", label: "rate", wantCents: 125},
		{name: "off", input: "off", label: "rate", wantCents: 0},
		{name: "none", input: "none", label: "daily cap", wantCents: 0},
		{name: "zero", input: "0", label: "rate", wantCents: 0},
		{name: "$0", input: "$0", label: "rate", wantCents: 0},
		{name: "two decimals", input: "1.51", label: "rate", wantCents: 151},
		{name: "empty", input: "", label: "rate", wantErr: "enter a value"},
		{name: "negative", input: "-1", label: "daily cap", wantErr: "daily cap must be non-negative"},
		{name: "garbage", input: "abc", label: "rate", wantErr: "invalid rate"},
		{name: "lone dollar", input: "$", label: "rate", wantErr: "missing dollar amount"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cents, err := parseAutoDollarInput(tc.input, tc.label)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cents != tc.wantCents {
				t.Fatalf("cents = %d, want %d", cents, tc.wantCents)
			}
		})
	}
}

func TestAutoBudgetPromptStatus(t *testing.T) {
	hourly := autoBudgetPromptStatus(autoBudgetStepHourly, "2.50")
	if !strings.Contains(hourly, "$/hr") || !strings.Contains(hourly, "Enter=next") {
		t.Errorf("hourly prompt missing expected fragments: %q", hourly)
	}
	daily := autoBudgetPromptStatus(autoBudgetStepDaily, "5.00")
	if !strings.Contains(daily, "$/day") || !strings.Contains(daily, "Ctrl-R=reset breaker") {
		t.Errorf("daily prompt missing expected fragments: %q", daily)
	}
}

func TestFormatAutoDailyCap(t *testing.T) {
	if got := formatAutoDailyCap(0); got != "off" {
		t.Errorf("0 cents = %q, want off", got)
	}
	if got := formatAutoDailyCap(500); got != "$5.00/day" {
		t.Errorf("500 cents = %q, want $5.00/day", got)
	}
}

func TestBeginAutoBudget(t *testing.T) {
	empty := beginAutoBudget(0)
	if !empty.Active || empty.Step != autoBudgetStepHourly || empty.Value != "" {
		t.Errorf("beginAutoBudget(0) = %+v, want active hourly with empty value", empty)
	}
	prefilled := beginAutoBudget(250)
	if prefilled.Value != "2.50" {
		t.Errorf("beginAutoBudget(250).Value = %q, want \"2.50\"", prefilled.Value)
	}
}

func TestHandleAutoBudgetKey_Esc(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "1.23"}
	next, eff := handleAutoBudgetKey(s, keyMsg("esc"), database)
	if next.Active || next.Value != "" {
		t.Errorf("esc on hourly: state = %+v, want closed/empty", next)
	}
	if eff.StatusText != "Run-rate target unchanged" {
		t.Errorf("esc on hourly: status = %q", eff.StatusText)
	}

	s = autoBudgetState{Active: true, Step: autoBudgetStepDaily, Value: "5"}
	_, eff = handleAutoBudgetKey(s, keyMsg("esc"), database)
	if eff.StatusText != "Daily cap unchanged" {
		t.Errorf("esc on daily: status = %q", eff.StatusText)
	}
}

func TestHandleAutoBudgetKey_TypingAndBackspace(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "1.2"}

	next, eff := handleAutoBudgetKey(s, keyMsg("5"), database)
	if next.Value != "1.25" {
		t.Errorf("after typing 5: value = %q", next.Value)
	}
	if eff.StatusText != "" || eff.RetriggerPilot {
		t.Errorf("typing should produce no effect, got %+v", eff)
	}

	next, _ = handleAutoBudgetKey(next, keyMsg("backspace"), database)
	if next.Value != "1.2" {
		t.Errorf("after backspace: value = %q", next.Value)
	}
}

func TestHandleAutoBudgetKey_HourlyEnterAdvances(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "2.50", DailyCents: 700}

	next, eff := handleAutoBudgetKey(s, keyMsg("enter"), database)
	if !next.Active || next.Step != autoBudgetStepDaily {
		t.Fatalf("expected step to advance to daily, got %+v", next)
	}
	if next.HourlyCents != 250 {
		t.Errorf("HourlyCents = %d, want 250", next.HourlyCents)
	}
	if next.Value != "7.00" {
		t.Errorf("Value should prefill from DailyCents, got %q", next.Value)
	}
	if !strings.Contains(eff.StatusText, "Run-rate set to $2.50/hr") {
		t.Errorf("status = %q", eff.StatusText)
	}
	if eff.RetriggerPilot {
		t.Error("hourly advance must not retrigger pilot")
	}
}

func TestHandleAutoBudgetKey_HourlyEnterParseError(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "abc"}

	next, eff := handleAutoBudgetKey(s, keyMsg("enter"), database)
	if next.Step != autoBudgetStepHourly || !next.Active {
		t.Errorf("invalid hourly: state should be unchanged, got %+v", next)
	}
	if !eff.StatusIsError || !strings.Contains(eff.StatusText, "Run-rate target") {
		t.Errorf("expected error effect, got %+v", eff)
	}
}

func TestHandleAutoBudgetKey_DailyEnterClosesAndRetriggers(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepDaily, Value: "8.00", HourlyCents: 250}

	next, eff := handleAutoBudgetKey(s, keyMsg("enter"), database)
	if next.Active || next.Step != autoBudgetStepHourly {
		t.Errorf("daily save: expected closed, got %+v", next)
	}
	if next.DailyCents != 800 {
		t.Errorf("DailyCents = %d, want 800", next.DailyCents)
	}
	if !eff.RetriggerPilot {
		t.Error("daily save should retrigger pilot")
	}
	if !strings.Contains(eff.StatusText, "Daily cap set to $8.00/day") {
		t.Errorf("status = %q", eff.StatusText)
	}
}

func TestHandleAutoBudgetKey_FiltersStrayCharacters(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "2.50"}

	// Pressing 'q' (a likely "quit" reflex) should be dropped, not appended.
	next, _ := handleAutoBudgetKey(s, keyMsg("q"), database)
	if next.Value != "2.50" {
		t.Errorf("'q' should be dropped, got value %q", next.Value)
	}

	// Letters that participate in "off"/"none"/"disable" are still accepted.
	next, _ = handleAutoBudgetKey(autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: ""}, keyMsg("o"), database)
	if next.Value != "o" {
		t.Errorf("'o' should be accepted as part of 'off', got %q", next.Value)
	}
}

func TestHandleAutoBudgetKey_CtrlR(t *testing.T) {
	database := setupBudgetTest(t)

	// Ctrl-R on hourly is a no-op.
	s := autoBudgetState{Active: true, Step: autoBudgetStepHourly, Value: "1.00"}
	next, eff := handleAutoBudgetKey(s, keyMsg("ctrl+r"), database)
	if next != s || eff != (autoBudgetEffect{}) {
		t.Errorf("Ctrl-R on hourly should be a no-op, got next=%+v eff=%+v", next, eff)
	}

	// On the daily step it closes the prompt, sets BreakerReset, and retriggers.
	s = autoBudgetState{Active: true, Step: autoBudgetStepDaily, Value: "5.00"}
	next, eff = handleAutoBudgetKey(s, keyMsg("ctrl+r"), database)
	if next.Active {
		t.Error("Ctrl-R on daily should close the prompt")
	}
	if !eff.BreakerReset || !eff.RetriggerPilot {
		t.Errorf("expected BreakerReset+RetriggerPilot, got %+v", eff)
	}
	if eff.StatusText != "Daily-budget breaker reset" {
		t.Errorf("status = %q", eff.StatusText)
	}
}
