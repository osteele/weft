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

func TestParseWatchdogTimeoutOverride(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    *int
		wantErr bool
	}{
		{"minutes", "45m", intPtr(2700), false},
		{"sub-minute rounds down to seconds", "90s", intPtr(90), false},
		{"hours", "1h30m", intPtr(5400), false},
		{"off disables", "off", intPtr(0), false},
		{"zero duration disables", "0s", intPtr(0), false},
		{"default clears", "default", nil, false},
		{"empty clears", "", nil, false},
		{"auto clears", "auto", nil, false},
		{"negative rejected", "-5m", nil, true},
		{"sub-second rejected", "500ms", nil, true},
		{"garbage rejected", "whenever", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWatchdogTimeoutOverride("gpu-idle-timeout", tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse(%q) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) errored: %v", tc.raw, err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("parse(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("parse(%q) = %d, want %d", tc.raw, *got, *tc.want)
			}
		})
	}
}

func TestParseWatchdogTimeoutFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "run"}
	cmd.Flags().String("gpu-idle-timeout", "", "")
	if got, err := parseWatchdogTimeoutFlag(cmd, "gpu-idle-timeout", ""); err != nil || got != nil {
		t.Fatalf("unset flag = (%v, %v), want (nil, nil)", got, err)
	}
	if err := cmd.Flags().Set("gpu-idle-timeout", "45m"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	raw, err := cmd.Flags().GetString("gpu-idle-timeout")
	if err != nil {
		t.Fatalf("get flag: %v", err)
	}
	got, err := parseWatchdogTimeoutFlag(cmd, "gpu-idle-timeout", raw)
	if err != nil || got == nil || *got != 2700 {
		t.Fatalf("set flag = (%v, %v), want (2700, nil)", got, err)
	}
}
