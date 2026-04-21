package ids

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const jobIDPrefix = "wj"

// FormatJobID returns the canonical CLI representation for a job ID.
func FormatJobID(id int64) string {
	return fmt.Sprintf("%s%d", jobIDPrefix, id)
}

// ParseJobID parses a single job ID token.
// Accepted forms are numeric IDs ("750") and prefixed IDs ("wj750").
func ParseJobID(raw string) (int64, error) {
	return parseJobIDToken(strings.TrimSpace(raw))
}

// FormatJobIDListCompact returns a sorted, compact job ID list.
// Example: []int64{750, 751, 752, 760} => "wj750:wj752,wj760"
func FormatJobIDListCompact(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	parts := make([]string, 0, len(sorted))
	start := sorted[0]
	prev := sorted[0]
	for i := 1; i < len(sorted); i++ {
		id := sorted[i]
		if id == prev || id == prev+1 {
			if id > prev {
				prev = id
			}
			continue
		}
		parts = append(parts, formatJobIDRange(start, prev))
		start = id
		prev = id
	}
	parts = append(parts, formatJobIDRange(start, prev))
	return strings.Join(parts, ",")
}

func parseJobIDToken(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty job ID")
	}
	if len(s) >= len(jobIDPrefix) && strings.EqualFold(s[:len(jobIDPrefix)], jobIDPrefix) {
		s = s[len(jobIDPrefix):]
		if s == "" {
			return 0, fmt.Errorf("missing numeric suffix")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func formatJobIDRange(start, end int64) string {
	if start == end {
		return FormatJobID(start)
	}
	return fmt.Sprintf("%s:%s", FormatJobID(start), FormatJobID(end))
}
