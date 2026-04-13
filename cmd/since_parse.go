package cmd

import (
	"fmt"
	"strings"
	"time"
)

// parseSinceCutoff parses --since as one of:
// - YYYY-MM-DD (local midnight)
// - RFC3339 timestamp
// - Go duration (plus d suffix support via parseDuration), optionally suffixed with "ago"
func parseSinceCutoff(raw string, now time.Time) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, fmt.Errorf("--since is required")
	}

	if t, err := time.ParseInLocation("2006-01-02", value, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}

	lower := strings.ToLower(value)
	if strings.HasSuffix(lower, "ago") {
		value = strings.TrimSpace(value[:len(value)-len("ago")])
	}

	d, err := parseDuration(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q (use YYYY-MM-DD, RFC3339, or duration like \"24h ago\"/\"7d\")", raw)
	}
	if d < 0 {
		return time.Time{}, fmt.Errorf("invalid --since %q (duration must be positive)", raw)
	}
	return now.Add(-d), nil
}
