package terminal

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
)

// loadAutoRunRateSoftTargetCentsPerHour returns the configured run-rate
// target in cents/hour. cfg may be nil; only then is the global config
// re-read from disk.
func loadAutoRunRateSoftTargetCentsPerHour(cfg *config.Config) int {
	if cfg == nil {
		loaded, err := config.Load()
		if err != nil || loaded == nil {
			return 0
		}
		cfg = loaded
	}
	return cfg.AutoRunRateSoftTargetCentsPerHour()
}

func saveAutoRunRateSoftTargetCentsPerHour(cents int) error {
	if cents < 0 {
		cents = 0
	}
	return config.SetAutoRunRateSoftTarget(float64(cents) / 100)
}

// loadAutoRunawaySpendDailyCapCents returns the runaway-breaker daily-cap
// threshold in cents. cfg may be nil; only then is the global config re-read
// from disk.
func loadAutoRunawaySpendDailyCapCents(cfg *config.Config) int {
	if cfg == nil {
		loaded, err := config.Load()
		if err != nil || loaded == nil {
			return 0
		}
		cfg = loaded
	}
	return cfg.AutoRunawaySpendNoProgressLimitCents()
}

func saveAutoRunawaySpendDailyCapCents(cents int) error {
	if cents < 0 {
		cents = 0
	}
	return config.SetAutoRunawaySpendNoProgressLimit(float64(cents) / 100)
}

func formatAutoDailyCap(cents int) string {
	if cents <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f/day", float64(cents)/100)
}

func parseAutoRunRateTargetInput(raw string) (int, error) {
	return parseAutoDollarInput(raw, "rate")
}

// parseAutoDollarInput parses a dollar amount in cents. Accepts "2.5",
// "$2.5", or "off"/"none"/"0" to disable. label is interpolated into error
// messages so callers can distinguish which prompt the input came from.
func parseAutoDollarInput(raw string, label string) (int, error) {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return 0, fmt.Errorf("enter a value like 2.5, $2.5, or off")
	}
	if trimmed == "off" || trimmed == "none" || trimmed == "disable" || trimmed == "disabled" || trimmed == "0" || trimmed == "$0" {
		return 0, nil
	}
	trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "$"))
	if trimmed == "" {
		return 0, fmt.Errorf("missing dollar amount")
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", label, raw)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must be non-negative", label)
	}
	return int(value*100 + 0.5), nil
}

func formatAutoRunRateTarget(cents int) string {
	if cents <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f/hr", float64(cents)/100)
}

// autoBudgetPhase is the panel's mode: top-level menu, a single-field
// editor, or a closing notice.
type autoBudgetPhase int

const (
	autoBudgetPhaseMenu autoBudgetPhase = iota
	autoBudgetPhaseEditHourly
	autoBudgetPhaseEditDaily
	autoBudgetPhaseNotice
)

// autoBudgetState is the budget panel's UI state, shared by the list and
// watch TUIs. Active turns the panel on; Phase selects the current view.
// HourlyCents and DailyCents reflect the persisted values, used both to
// pre-fill editors and to render the menu.
type autoBudgetState struct {
	Active      bool
	Phase       autoBudgetPhase
	Value       string
	HourlyCents int
	DailyCents  int
}

// autoBudgetEffect is what handleAutoBudgetKey reports back to the caller.
// Status text (if any) should be displayed; RetriggerPilot signals that a
// save or breaker reset just landed, so callers with an autopilot should
// consider kicking it. BreakerReset is set on a successful breaker reset
// so the caller can clear any persistent block-reason cache it maintains.
type autoBudgetEffect struct {
	StatusText     string
	StatusIsError  bool
	RetriggerPilot bool
	BreakerReset   bool
}

// beginAutoBudget initializes the panel on the menu phase. The hourly and
// daily values are the persisted ones; they are shown in the menu rows
// and used to pre-fill the editors.
func beginAutoBudget(hourlyCents, dailyCents int) autoBudgetState {
	return autoBudgetState{
		Active:      true,
		Phase:       autoBudgetPhaseMenu,
		HourlyCents: hourlyCents,
		DailyCents:  dailyCents,
	}
}

func formatCentsForInput(cents int) string {
	if cents <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

// handleAutoBudgetKey advances the panel's state machine in response to a
// key. The database is needed when the breaker reset is invoked from the
// menu.
//
// Esc backs out one level: from an editor it returns to the menu; from
// the menu it closes the panel. Saves persist the new value and close the
// panel. Clear actions show a notice that closes on Enter or Esc.
func handleAutoBudgetKey(s autoBudgetState, msg tea.KeyMsg, database *sql.DB) (autoBudgetState, autoBudgetEffect) {
	switch s.Phase {
	case autoBudgetPhaseMenu:
		return handleAutoBudgetMenuKey(s, msg, database)
	case autoBudgetPhaseEditHourly, autoBudgetPhaseEditDaily:
		return handleAutoBudgetEditKey(s, msg)
	case autoBudgetPhaseNotice:
		return handleAutoBudgetNoticeKey(s, msg)
	}
	return s, autoBudgetEffect{}
}

func handleAutoBudgetMenuKey(s autoBudgetState, msg tea.KeyMsg, database *sql.DB) (autoBudgetState, autoBudgetEffect) {
	switch msg.String() {
	case "esc", "q":
		s.Active = false
		s.Value = ""
		s.Phase = autoBudgetPhaseMenu
		return s, autoBudgetEffect{}
	case "h":
		s.Phase = autoBudgetPhaseEditHourly
		s.Value = formatCentsForInput(s.HourlyCents)
		return s, autoBudgetEffect{}
	case "d":
		s.Phase = autoBudgetPhaseEditDaily
		s.Value = formatCentsForInput(s.DailyCents)
		return s, autoBudgetEffect{}
	case "H":
		if err := saveAutoRunRateSoftTargetCentsPerHour(0); err != nil {
			return s, autoBudgetEffect{StatusText: fmt.Sprintf("Hourly target clear failed: %v", err), StatusIsError: true}
		}
		s.HourlyCents = 0
		s.Phase = autoBudgetPhaseNotice
		s.Value = "Hourly target cleared"
		return s, autoBudgetEffect{StatusText: s.Value, RetriggerPilot: true}
	case "D":
		if err := saveAutoRunawaySpendDailyCapCents(0); err != nil {
			return s, autoBudgetEffect{StatusText: fmt.Sprintf("Daily cap clear failed: %v", err), StatusIsError: true}
		}
		s.DailyCents = 0
		s.Phase = autoBudgetPhaseNotice
		s.Value = "Daily cap cleared"
		return s, autoBudgetEffect{StatusText: s.Value, RetriggerPilot: true}
	case "r":
		if err := campaign.ResetGlobalRunawayBreaker(database, "TUI"); err != nil {
			return s, autoBudgetEffect{
				StatusText:    fmt.Sprintf("Reset breaker failed: %v", err),
				StatusIsError: true,
			}
		}
		s.Active = false
		s.Phase = autoBudgetPhaseMenu
		s.Value = ""
		return s, autoBudgetEffect{
			StatusText:     "Runaway breaker reset",
			RetriggerPilot: true,
			BreakerReset:   true,
		}
	}
	return s, autoBudgetEffect{}
}

// editPhaseOps describes the per-phase wiring an editor needs: the label
// used in status/error messages, the panel header, the parser, and how
// to commit the parsed cents (persist + reflect on the autoBudgetState).
type editPhaseOps struct {
	label       string
	panelHeader string
	parse       func(string) (int, error)
	commit      func(s *autoBudgetState, cents int) (saved string, err error)
}

var editOpsByPhase = map[autoBudgetPhase]editPhaseOps{
	autoBudgetPhaseEditHourly: {
		label:       "Hourly target",
		panelHeader: "── Hourly target ($/hr) ──",
		parse:       parseAutoRunRateTargetInput,
		commit: func(s *autoBudgetState, cents int) (string, error) {
			if err := saveAutoRunRateSoftTargetCentsPerHour(cents); err != nil {
				return "", err
			}
			s.HourlyCents = cents
			return formatAutoRunRateTarget(cents), nil
		},
	},
	autoBudgetPhaseEditDaily: {
		label:       "Daily cap",
		panelHeader: "── Daily cap ($/day) ──",
		parse:       func(raw string) (int, error) { return parseAutoDollarInput(raw, "daily cap") },
		commit: func(s *autoBudgetState, cents int) (string, error) {
			if err := saveAutoRunawaySpendDailyCapCents(cents); err != nil {
				return "", err
			}
			s.DailyCents = cents
			return formatAutoDailyCap(cents), nil
		},
	},
}

func handleAutoBudgetEditKey(s autoBudgetState, msg tea.KeyMsg) (autoBudgetState, autoBudgetEffect) {
	ops := editOpsByPhase[s.Phase]
	switch msg.String() {
	case "esc":
		s.Phase = autoBudgetPhaseMenu
		s.Value = ""
		return s, autoBudgetEffect{}
	case "enter":
		cents, err := ops.parse(s.Value)
		if err != nil {
			return s, autoBudgetEffect{StatusText: ops.label + ": " + err.Error(), StatusIsError: true}
		}
		saved, err := ops.commit(&s, cents)
		if err != nil {
			return s, autoBudgetEffect{StatusText: fmt.Sprintf("%s save failed: %v", ops.label, err), StatusIsError: true}
		}
		s.Active = false
		s.Phase = autoBudgetPhaseMenu
		s.Value = ""
		return s, autoBudgetEffect{
			StatusText:     fmt.Sprintf("%s set to %s", ops.label, saved),
			RetriggerPilot: true,
		}
	case "backspace", "ctrl+h":
		if len(s.Value) > 0 {
			runes := []rune(s.Value)
			s.Value = string(runes[:len(runes)-1])
		}
		return s, autoBudgetEffect{}
	}
	for _, r := range msg.Runes {
		if isAutoBudgetInputRune(r) {
			s.Value += string(r)
		}
	}
	return s, autoBudgetEffect{}
}

func handleAutoBudgetNoticeKey(s autoBudgetState, msg tea.KeyMsg) (autoBudgetState, autoBudgetEffect) {
	switch msg.String() {
	case "enter", "esc", "q":
		s.Active = false
		s.Phase = autoBudgetPhaseMenu
		s.Value = ""
		return s, autoBudgetEffect{}
	}
	return s, autoBudgetEffect{}
}

// isAutoBudgetInputRune accepts characters that can plausibly appear in a
// dollar-amount input: digits, ".", "$", and the lowercase letters that
// spell "off"/"none"/"disable". Anything else (arrow-key escape sequences,
// stray keys) is dropped so the input value reflects the user's intent.
func isAutoBudgetInputRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '$':
		return true
	}
	switch r {
	case 'o', 'f', 'n', 'e', 'd', 'i', 's', 'a', 'b', 'l':
		return true
	}
	return false
}

// renderAutoBudgetPanel returns the multi-line panel for the current phase.
// width is the screen width; the panel is left-aligned and wider lines are
// truncated by the caller. Each line is left as plain text — the caller
// applies the appropriate style (e.g. an inverse-video panel style).
func renderAutoBudgetPanel(s autoBudgetState) []string {
	switch s.Phase {
	case autoBudgetPhaseMenu:
		return []string{
			"── Auto-pilot budget ──",
			fmt.Sprintf("  h   Hourly target  %s", formatAutoRunRateTarget(s.HourlyCents)),
			"  H   Clear hourly target",
			fmt.Sprintf("  d   Daily cap      %s", formatAutoDailyCap(s.DailyCents)),
			"  D   Clear daily cap",
			"  r   Reset runaway breaker",
			"  Esc close",
		}
	case autoBudgetPhaseEditHourly, autoBudgetPhaseEditDaily:
		return []string{
			editOpsByPhase[s.Phase].panelHeader,
			"  " + s.Value + "▏",
			"  Enter: save   Esc: back",
		}
	case autoBudgetPhaseNotice:
		msg := strings.TrimSpace(s.Value)
		if msg == "" {
			msg = "Updated"
		}
		return []string{
			"── Auto-pilot budget ──",
			"  " + msg + ".",
			"  Enter / Esc: return",
		}
	}
	return nil
}
