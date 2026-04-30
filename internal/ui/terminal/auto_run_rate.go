package terminal

import (
	"fmt"
	"strconv"
	"strings"

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
