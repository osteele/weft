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

func autoBudgetPromptStatus(step autoBudgetInputStep, value string) string {
	label, hints := autoBudgetPromptLabelAndHints(step, "=")
	return fmt.Sprintf("%s: %s (%s)", label, value, hints)
}

func autoBudgetPromptControls(step autoBudgetInputStep, value string) string {
	label, hints := autoBudgetPromptLabelAndHints(step, ":")
	return label + ": " + value + "  " + hints
}

func autoBudgetPromptLabelAndHints(step autoBudgetInputStep, sep string) (label, hints string) {
	switch step {
	case autoBudgetStepDaily:
		return "Daily cap ($/day)",
			"Enter" + sep + "save  Esc" + sep + "cancel  Ctrl-R" + sep + "reset breaker"
	default:
		return "Run-rate target ($/hr)",
			"Enter" + sep + "next  Esc" + sep + "cancel"
	}
}

// autoBudgetState is the input-prompt state machine shared by the list and
// watch TUIs. The two TUIs translate effects into their own status surface
// (statusMessage vs flash) and decide whether to retrigger the autopilot.
type autoBudgetState struct {
	Active      bool
	Step        autoBudgetInputStep
	Value       string
	HourlyCents int
	DailyCents  int
}

// autoBudgetEffect is what handleAutoBudgetKey reports back to the caller.
// Status text (if any) should be displayed; RetriggerPilot signals that a
// save or breaker reset just landed, so callers with an autopilot should
// consider kicking it. BreakerReset is set on a successful Ctrl-R so the
// caller can clear any persistent block-reason cache it maintains.
type autoBudgetEffect struct {
	StatusText     string
	StatusIsError  bool
	RetriggerPilot bool
	BreakerReset   bool
}

// beginAutoBudget initializes the prompt at the hourly step, prefilled
// with the current hourly target.
func beginAutoBudget(hourlyCents int) autoBudgetState {
	return autoBudgetState{
		Active: true,
		Step:   autoBudgetStepHourly,
		Value:  formatCentsForInput(hourlyCents),
	}
}

func formatCentsForInput(cents int) string {
	if cents <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

// handleAutoBudgetKey advances the prompt's state machine in response to a
// key. database is needed only when Ctrl-R fires on the daily step. On
// success the helper has already persisted the new value (or inserted the
// breaker-resume event); the caller only has to render the status text and
// decide what to do with RetriggerPilot.
func handleAutoBudgetKey(s autoBudgetState, msg tea.KeyMsg, database *sql.DB) (autoBudgetState, autoBudgetEffect) {
	switch msg.String() {
	case "esc":
		text := "Run-rate target unchanged"
		if s.Step == autoBudgetStepDaily {
			text = "Daily cap unchanged"
		}
		s.Active = false
		s.Value = ""
		s.Step = autoBudgetStepHourly
		return s, autoBudgetEffect{StatusText: text}

	case "ctrl+r":
		if s.Step != autoBudgetStepDaily {
			return s, autoBudgetEffect{}
		}
		if err := campaign.ResetGlobalRunawayBreaker(database, "TUI"); err != nil {
			return s, autoBudgetEffect{
				StatusText:    fmt.Sprintf("Reset breaker failed: %v", err),
				StatusIsError: true,
			}
		}
		s.Active = false
		s.Value = ""
		s.Step = autoBudgetStepHourly
		return s, autoBudgetEffect{
			StatusText:     "Daily-budget breaker reset",
			RetriggerPilot: true,
			BreakerReset:   true,
		}

	case "enter":
		switch s.Step {
		case autoBudgetStepHourly:
			cents, err := parseAutoRunRateTargetInput(s.Value)
			if err != nil {
				return s, autoBudgetEffect{StatusText: "Run-rate target: " + err.Error(), StatusIsError: true}
			}
			if err := saveAutoRunRateSoftTargetCentsPerHour(cents); err != nil {
				return s, autoBudgetEffect{StatusText: fmt.Sprintf("Run-rate target save failed: %v", err), StatusIsError: true}
			}
			s.HourlyCents = cents
			s.Step = autoBudgetStepDaily
			s.Value = formatCentsForInput(s.DailyCents)
			return s, autoBudgetEffect{
				StatusText: fmt.Sprintf("Run-rate set to %s — now enter daily cap", formatAutoRunRateTarget(cents)),
			}
		case autoBudgetStepDaily:
			cents, err := parseAutoDollarInput(s.Value, "daily cap")
			if err != nil {
				return s, autoBudgetEffect{StatusText: "Daily cap: " + err.Error(), StatusIsError: true}
			}
			if err := saveAutoRunawaySpendDailyCapCents(cents); err != nil {
				return s, autoBudgetEffect{StatusText: fmt.Sprintf("Daily cap save failed: %v", err), StatusIsError: true}
			}
			s.DailyCents = cents
			s.Active = false
			s.Value = ""
			s.Step = autoBudgetStepHourly
			return s, autoBudgetEffect{
				StatusText:     "Daily cap set to " + formatAutoDailyCap(cents),
				RetriggerPilot: true,
			}
		}
		return s, autoBudgetEffect{}

	case "backspace", "ctrl+h":
		if len(s.Value) > 0 {
			runes := []rune(s.Value)
			s.Value = string(runes[:len(runes)-1])
		}
		return s, autoBudgetEffect{}
	}

	if len(msg.Runes) > 0 {
		s.Value += string(msg.Runes)
	}
	return s, autoBudgetEffect{}
}
