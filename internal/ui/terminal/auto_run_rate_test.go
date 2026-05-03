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

func menuState(hourly, daily int) autoBudgetState {
	return autoBudgetState{
		Active:      true,
		Phase:       autoBudgetPhaseMenu,
		HourlyCents: hourly,
		DailyCents:  daily,
	}
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

func TestFormatAutoDailyCap(t *testing.T) {
	if got := formatAutoDailyCap(0); got != "off" {
		t.Errorf("0 cents = %q, want off", got)
	}
	if got := formatAutoDailyCap(500); got != "$5.00/day" {
		t.Errorf("500 cents = %q, want $5.00/day", got)
	}
}

func TestBeginAutoBudgetOpensOnMenu(t *testing.T) {
	s := beginAutoBudget(250, 5000)
	if !s.Active || s.Phase != autoBudgetPhaseMenu {
		t.Errorf("beginAutoBudget should open on menu, got %+v", s)
	}
	if s.HourlyCents != 250 || s.DailyCents != 5000 {
		t.Errorf("beginAutoBudget didn't carry persisted values: %+v", s)
	}
	if s.Value != "" {
		t.Errorf("menu phase should not have an editor value: %q", s.Value)
	}
}

func TestRenderAutoBudgetPanelMenuShowsAllActions(t *testing.T) {
	lines := renderAutoBudgetPanel(menuState(250, 5000))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"h", "Hourly", "$2.50/hr", "H", "Clear hourly", "d", "Daily", "$50.00/day", "D", "Clear daily", "r", "Reset", "Esc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("menu missing %q in:\n%s", want, joined)
		}
	}
}

func TestHandleAutoBudgetKeyMenuRouting(t *testing.T) {
	database := setupBudgetTest(t)

	t.Run("h opens hourly editor", func(t *testing.T) {
		s := menuState(250, 5000)
		next, _ := handleAutoBudgetKey(s, keyMsg("h"), database)
		if next.Phase != autoBudgetPhaseEditHourly {
			t.Fatalf("phase = %v, want EditHourly", next.Phase)
		}
		if next.Value != "2.50" {
			t.Errorf("editor should prefill with hourly value, got %q", next.Value)
		}
	})

	t.Run("d opens daily editor", func(t *testing.T) {
		s := menuState(250, 5000)
		next, _ := handleAutoBudgetKey(s, keyMsg("d"), database)
		if next.Phase != autoBudgetPhaseEditDaily {
			t.Fatalf("phase = %v, want EditDaily", next.Phase)
		}
		if next.Value != "50.00" {
			t.Errorf("editor should prefill with daily value, got %q", next.Value)
		}
	})

	t.Run("r opens reset confirmation", func(t *testing.T) {
		s := menuState(250, 5000)
		next, _ := handleAutoBudgetKey(s, keyMsg("r"), database)
		if next.Phase != autoBudgetPhaseConfirmReset {
			t.Fatalf("phase = %v, want ConfirmReset", next.Phase)
		}
	})

	t.Run("H clears hourly target and shows notice", func(t *testing.T) {
		s := menuState(250, 5000)
		next, eff := handleAutoBudgetKey(s, keyMsg("H"), database)
		if next.Phase != autoBudgetPhaseNotice {
			t.Fatalf("phase = %v, want Notice", next.Phase)
		}
		if next.HourlyCents != 0 || next.DailyCents != 5000 {
			t.Fatalf("state = %+v, want hourly cleared only", next)
		}
		if !eff.RetriggerPilot || eff.StatusText != "Hourly target cleared" {
			t.Fatalf("effect = %+v", eff)
		}
	})

	t.Run("D clears daily cap and shows notice", func(t *testing.T) {
		s := menuState(250, 5000)
		next, eff := handleAutoBudgetKey(s, keyMsg("D"), database)
		if next.Phase != autoBudgetPhaseNotice {
			t.Fatalf("phase = %v, want Notice", next.Phase)
		}
		if next.HourlyCents != 250 || next.DailyCents != 0 {
			t.Fatalf("state = %+v, want daily cleared only", next)
		}
		if !eff.RetriggerPilot || eff.StatusText != "Daily cap cleared" {
			t.Fatalf("effect = %+v", eff)
		}
	})

	t.Run("esc closes panel from menu", func(t *testing.T) {
		s := menuState(250, 5000)
		next, _ := handleAutoBudgetKey(s, keyMsg("esc"), database)
		if next.Active {
			t.Fatal("esc on menu should close the panel")
		}
	})

	t.Run("q closes panel from menu", func(t *testing.T) {
		s := menuState(250, 5000)
		next, _ := handleAutoBudgetKey(s, keyMsg("q"), database)
		if next.Active {
			t.Fatal("q on menu should close the panel")
		}
	})
}

func TestHandleAutoBudgetEditorTypingAndBackspace(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Phase: autoBudgetPhaseEditHourly, Value: "1.2"}

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

func TestHandleAutoBudgetEditorEscReturnsToMenu(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Phase: autoBudgetPhaseEditHourly, Value: "9.99"}
	next, eff := handleAutoBudgetKey(s, keyMsg("esc"), database)
	if next.Phase != autoBudgetPhaseMenu {
		t.Errorf("esc should return to menu, got phase %v", next.Phase)
	}
	if !next.Active {
		t.Error("esc from editor should not close the panel")
	}
	if next.Value != "" {
		t.Errorf("editor value should be cleared on cancel, got %q", next.Value)
	}
	if eff.StatusText != "" {
		t.Errorf("cancel should produce no status text, got %q", eff.StatusText)
	}
}

func TestHandleAutoBudgetEditorEnterSavesAndClosesPanel(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{
		Active:      true,
		Phase:       autoBudgetPhaseEditHourly,
		Value:       "3.50",
		HourlyCents: 250,
		DailyCents:  5000,
	}
	next, eff := handleAutoBudgetKey(s, keyMsg("enter"), database)
	if next.Phase != autoBudgetPhaseMenu {
		t.Fatalf("after save expected menu phase, got %v", next.Phase)
	}
	if next.Active {
		t.Fatal("save should close the panel")
	}
	if next.HourlyCents != 350 {
		t.Errorf("hourly cents = %d, want 350", next.HourlyCents)
	}
	if next.DailyCents != 5000 {
		t.Errorf("daily should be unchanged: %d", next.DailyCents)
	}
	if !eff.RetriggerPilot {
		t.Error("save should retrigger pilot")
	}
	if !strings.Contains(eff.StatusText, "Hourly target set to $3.50/hr") {
		t.Errorf("status = %q", eff.StatusText)
	}
}

func TestHandleAutoBudgetEditorEnterParseError(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Phase: autoBudgetPhaseEditHourly, Value: "abc"}
	next, eff := handleAutoBudgetKey(s, keyMsg("enter"), database)
	if next.Phase != autoBudgetPhaseEditHourly {
		t.Errorf("invalid input should keep editor open, got phase %v", next.Phase)
	}
	if !eff.StatusIsError || !strings.Contains(eff.StatusText, "Hourly target") {
		t.Errorf("expected error effect, got %+v", eff)
	}
}

func TestHandleAutoBudgetConfirmReset(t *testing.T) {
	database := setupBudgetTest(t)

	t.Run("y resets and shows notice", func(t *testing.T) {
		s := autoBudgetState{Active: true, Phase: autoBudgetPhaseConfirmReset, HourlyCents: 250, DailyCents: 5000}
		next, eff := handleAutoBudgetKey(s, keyMsg("y"), database)
		if next.Phase != autoBudgetPhaseNotice {
			t.Errorf("y should show notice, got %v", next.Phase)
		}
		if !next.Active {
			t.Error("notice should keep the panel open")
		}
		if !eff.BreakerReset || !eff.RetriggerPilot {
			t.Errorf("expected BreakerReset+RetriggerPilot, got %+v", eff)
		}
		if eff.StatusText != "Runaway breaker reset" {
			t.Errorf("status = %q", eff.StatusText)
		}
	})

	t.Run("n returns to menu without resetting", func(t *testing.T) {
		s := autoBudgetState{Active: true, Phase: autoBudgetPhaseConfirmReset, HourlyCents: 250}
		next, eff := handleAutoBudgetKey(s, keyMsg("n"), database)
		if next.Phase != autoBudgetPhaseMenu {
			t.Errorf("n should return to menu, got %v", next.Phase)
		}
		if eff.BreakerReset {
			t.Error("n should not reset breaker")
		}
	})

	t.Run("esc returns to menu", func(t *testing.T) {
		s := autoBudgetState{Active: true, Phase: autoBudgetPhaseConfirmReset}
		next, _ := handleAutoBudgetKey(s, keyMsg("esc"), database)
		if next.Phase != autoBudgetPhaseMenu {
			t.Errorf("esc should return to menu, got %v", next.Phase)
		}
	})
}

func TestHandleAutoBudgetNoticeClosesOnEnterOrEsc(t *testing.T) {
	database := setupBudgetTest(t)
	for _, key := range []string{"enter", "esc"} {
		t.Run(key, func(t *testing.T) {
			s := autoBudgetState{Active: true, Phase: autoBudgetPhaseNotice, Value: "Daily cap cleared"}
			next, _ := handleAutoBudgetKey(s, keyMsg(key), database)
			if next.Active {
				t.Fatalf("%s should close notice", key)
			}
			if next.Phase != autoBudgetPhaseMenu || next.Value != "" {
				t.Fatalf("state after %s = %+v", key, next)
			}
		})
	}
}

func TestHandleAutoBudgetEditorFiltersStrayCharacters(t *testing.T) {
	database := setupBudgetTest(t)
	s := autoBudgetState{Active: true, Phase: autoBudgetPhaseEditHourly, Value: "2.50"}

	// 'q' is dropped (it's a likely "quit" reflex, not a numeric input).
	next, _ := handleAutoBudgetKey(s, keyMsg("q"), database)
	if next.Value != "2.50" {
		t.Errorf("'q' should be dropped, got value %q", next.Value)
	}

	// 'o' is kept because it's part of "off".
	next, _ = handleAutoBudgetKey(autoBudgetState{Active: true, Phase: autoBudgetPhaseEditHourly}, keyMsg("o"), database)
	if next.Value != "o" {
		t.Errorf("'o' should be accepted, got %q", next.Value)
	}
}
