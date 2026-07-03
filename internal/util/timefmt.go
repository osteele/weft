package util

import "time"

const (
	EmptyCellCLI = "-"
	EmptyCellTUI = "—"
)

// FormatUnixTimeOr formats a Unix timestamp in local time using layout,
// returning empty when the timestamp is null/zero (ts <= 0). Job start and
// end times use 0/NULL as "not set"; without this guard a zero value renders
// as the Unix epoch. Callers pass their surface's empty sentinel.
func FormatUnixTimeOr(ts int64, layout, empty string) string {
	if ts <= 0 {
		return empty
	}
	return time.Unix(ts, 0).Format(layout)
}
