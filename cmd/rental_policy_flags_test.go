package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseRentalPolicyFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var maxRate, maxSpend, maxTime, grace string
	var minSurvival float64
	cmd.Flags().StringVar(&maxRate, "max-hourly-rate", "", "")
	cmd.Flags().StringVar(&maxSpend, "max-spend", "", "")
	cmd.Flags().StringVar(&maxTime, "max-time", "", "")
	cmd.Flags().StringVar(&grace, "grace-period", "", "")
	cmd.Flags().Float64Var(&minSurvival, "min-survival", 0.4, "")
	for name, value := range map[string]string{
		"max-hourly-rate": "$33.20",
		"max-spend":       "83",
		"max-time":        "2.5h",
		"grace-period":    "0",
		"min-survival":    "0.9",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	parsed, err := parseRentalPolicyFlags(cmd, maxRate, maxSpend, maxTime, grace, minSurvival)
	if err != nil {
		t.Fatalf("parseRentalPolicyFlags: %v", err)
	}
	if parsed.maxHourlyRateCents == nil || *parsed.maxHourlyRateCents != 3320 {
		t.Fatalf("max hourly rate = %+v, want 3320", parsed.maxHourlyRateCents)
	}
	if parsed.maxSpendCents == nil || *parsed.maxSpendCents != 8300 {
		t.Fatalf("max spend = %+v, want 8300", parsed.maxSpendCents)
	}
	if parsed.maxTimeSeconds == nil || *parsed.maxTimeSeconds != 9000 {
		t.Fatalf("max time = %+v, want 9000", parsed.maxTimeSeconds)
	}
	if parsed.gracePeriodSeconds == nil || *parsed.gracePeriodSeconds != 0 {
		t.Fatalf("grace period = %+v, want explicit 0", parsed.gracePeriodSeconds)
	}
	if parsed.minSurvival == nil || *parsed.minSurvival != 0.9 {
		t.Fatalf("min survival = %+v, want 0.9", parsed.minSurvival)
	}
}

func TestParseRentalPolicyFlagsRejectsFractionalCents(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var maxRate string
	cmd.Flags().StringVar(&maxRate, "max-hourly-rate", "", "")
	cmd.Flags().String("max-spend", "", "")
	cmd.Flags().String("max-time", "", "")
	cmd.Flags().String("grace-period", "", "")
	cmd.Flags().Float64("min-survival", 0.4, "")
	if err := cmd.Flags().Set("max-hourly-rate", "1.001"); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	_, err := parseRentalPolicyFlags(cmd, maxRate, "", "", "", 0.4)
	if err == nil || !strings.Contains(err.Error(), "two decimal places") {
		t.Fatalf("error = %v, want fractional-cent rejection", err)
	}
}
