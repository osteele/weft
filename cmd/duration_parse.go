package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// parseDuration parses a duration string, supporting "d" suffix for days.
func parseDuration(s string) (time.Duration, error) {
	// Handle "d" suffix for days (Go's time.ParseDuration doesn't support days).
	re := regexp.MustCompile(`^(\d+)d$`)
	if matches := re.FindStringSubmatch(s); matches != nil {
		days, err := strconv.Atoi(matches[1])
		if err != nil {
			return 0, fmt.Errorf("parse days: %w", err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	// Fall back to standard Go duration parsing (handles h, m, s).
	return time.ParseDuration(s)
}
