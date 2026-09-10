package util

import "time"

const (
	EmptyCellCLI = "-"
	EmptyCellTUI = "—"
)

// cliTimeLocation is configured before command goroutines start.
var cliTimeLocation = time.UTC

// SetCLITimeLocation selects the location for human-readable CLI timestamps.
// Call before starting command goroutines, with a non-nil resolved location.
func SetCLITimeLocation(location *time.Location) {
	cliTimeLocation = location
}

// CLITimeLocation returns the CLI presentation location, UTC by default.
func CLITimeLocation() *time.Location {
	return cliTimeLocation
}

// FormatCLITime formats t in the configured CLI location using baseLayout,
// which must be marker-free (no zone tokens). A zone suffix is appended so
// the output is unambiguous: " UTC" when the CLI location is UTC, otherwise
// the location's abbreviation plus its numeric offset (" MST -07:00").
func FormatCLITime(t time.Time, baseLayout string) string {
	location := CLITimeLocation()
	suffix := " MST"
	if location != time.UTC {
		suffix += " -07:00"
	}
	return t.In(location).Format(baseLayout + suffix)
}

// FormatCLIUnixTimeOr formats a Unix timestamp via FormatCLITime, returning
// empty when the timestamp is null/zero (ts <= 0).
func FormatCLIUnixTimeOr(ts int64, baseLayout, empty string) string {
	if ts <= 0 {
		return empty
	}
	return FormatCLITime(time.Unix(ts, 0), baseLayout)
}

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
