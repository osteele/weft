package util

import (
	"testing"
	"time"
)

func TestFormatUnixTimeOr_NullAndZero(t *testing.T) {
	for _, ts := range []int64{0, -1} {
		if got := FormatUnixTimeOr(ts, "01/02 15:04", EmptyCellTUI); got != EmptyCellTUI {
			t.Errorf("FormatUnixTimeOr(%d) = %q, want em-dash sentinel", ts, got)
		}
		if got := FormatUnixTimeOr(ts, "2006-01-02 15:04:05", EmptyCellCLI); got != EmptyCellCLI {
			t.Errorf("FormatUnixTimeOr(%d) = %q, want hyphen sentinel", ts, got)
		}
	}
}

func TestFormatUnixTimeOr_Set(t *testing.T) {
	ts := time.Date(2026, 6, 11, 13, 48, 59, 0, time.Local).Unix()
	if got := FormatUnixTimeOr(ts, "2006-01-02 15:04:05", EmptyCellCLI); got != "2026-06-11 13:48:59" {
		t.Errorf("FormatUnixTimeOr = %q, want 2026-06-11 13:48:59", got)
	}
}

func TestFormatCLITime(t *testing.T) {
	previous := CLITimeLocation()
	t.Cleanup(func() { SetCLITimeLocation(previous) })
	instant := time.Date(2026, 6, 12, 1, 59, 59, 0, time.FixedZone("SOURCE", 2*3600))
	cases := []struct {
		name     string
		location *time.Location
		layout   string
		want     string
	}{
		{"utc", time.UTC, "2006-01-02 15:04:05", "2026-06-11 23:59:59 UTC"},
		{"fixed", time.FixedZone("TEST", 2*3600), "2006-01-02 15:04:05", "2026-06-12 01:59:59 TEST +02:00"},
		{"minutes", time.UTC, "01/02 15:04", "06/11 23:59 UTC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetCLITimeLocation(tc.location)
			if got := FormatCLITime(instant, tc.layout); got != tc.want {
				t.Errorf("FormatCLITime = %q, want %q", got, tc.want)
			}
			if got := FormatCLIUnixTimeOr(instant.Unix(), tc.layout, "unset"); got != tc.want {
				t.Errorf("FormatCLIUnixTimeOr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatCLIUnixTimeOr_Unset(t *testing.T) {
	for _, ts := range []int64{0, -1} {
		if got := FormatCLIUnixTimeOr(ts, "2006-01-02 15:04:05", "unset"); got != "unset" {
			t.Errorf("FormatCLIUnixTimeOr(%d) = %q, want unset", ts, got)
		}
	}
}
