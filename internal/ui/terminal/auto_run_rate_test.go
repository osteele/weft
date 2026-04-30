package terminal

import (
	"strings"
	"testing"
)

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
