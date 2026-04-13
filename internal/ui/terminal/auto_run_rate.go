package terminal

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/config"
)

func loadAutoRunRateSoftTargetCentsPerHour() int {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return 0
	}
	return cfg.AutoRunRateSoftTargetCentsPerHour()
}

func saveAutoRunRateSoftTargetCentsPerHour(cents int) error {
	if cents < 0 {
		cents = 0
	}
	return config.SetAutoRunRateSoftTarget(float64(cents) / 100)
}

func parseAutoRunRateTargetInput(raw string) (int, error) {
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
		return 0, fmt.Errorf("invalid rate %q", raw)
	}
	if value < 0 {
		return 0, fmt.Errorf("rate must be non-negative")
	}
	return int(value*100 + 0.5), nil
}

func formatAutoRunRateTarget(cents int) string {
	if cents <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f/hr", float64(cents)/100)
}
