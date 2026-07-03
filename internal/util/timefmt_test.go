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
