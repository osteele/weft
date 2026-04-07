package cmd

import (
	"fmt"
	"os"
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

// ParseJobIDs parses command-line arguments into a deduplicated, sorted list of job IDs.
// Supports individual IDs (123 / wj123), ranges (123:127 / wj123:127 / wj123:wj127
// / 123::127 / 123...127), and comma-separated lists (123,124,125).
// Prints a warning to stderr if duplicates are found.
//
// Syntax:
//   - Single ID: 123 or wj123
//   - Range: 123:127, wj123:127, wj123:wj127, 123::127, or 123...127 (inclusive)
//   - List: 123,124,125
//   - Mixed: 123 wj125:127 130,131 (expands to 123, 125, 126, 127, 130, 131)
func ParseJobIDs(args []string) ([]int64, error) {
	seen := make(map[int64]bool)
	var ids []int64
	var duplicates []int64

	for _, arg := range args {
		parsed, err := parseJobIDArg(arg)
		if err != nil {
			return nil, err
		}

		for _, id := range parsed {
			if seen[id] {
				duplicates = append(duplicates, id)
			} else {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	if len(duplicates) > 0 {
		// Deduplicate the duplicates list for cleaner warning
		dupSeen := make(map[int64]bool)
		var uniqueDups []string
		for _, id := range duplicates {
			if !dupSeen[id] {
				dupSeen[id] = true
				uniqueDups = append(uniqueDups, FormatJobID(id))
			}
		}
		fmt.Fprintf(os.Stderr, "Warning: ignoring duplicate job ID(s): %s\n", strings.Join(uniqueDups, ", "))
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// parseJobIDArg parses a single argument which may be an ID or a range.
func parseJobIDArg(arg string) ([]int64, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("invalid empty job ID")
	}

	// Support comma-separated IDs/ranges within a single arg (e.g. 12,13,14 or 12:14,20).
	if strings.Contains(arg, ",") {
		parts := strings.Split(arg, ",")
		ids := make([]int64, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				return nil, fmt.Errorf("invalid empty job ID in %q", arg)
			}
			partIDs, err := parseJobIDArg(part)
			if err != nil {
				return nil, err
			}
			ids = append(ids, partIDs...)
		}
		return ids, nil
	}

	// Normalize ellipsis range syntax (12...14) to colon syntax (12:14).
	if strings.Contains(arg, "...") {
		if strings.Count(arg, "...") != 1 || strings.Contains(arg, ":") {
			return nil, fmt.Errorf("invalid job ID range %q", arg)
		}
		arg = strings.Replace(arg, "...", ":", 1)
	}

	if strings.Count(arg, ":") > 1 {
		if strings.Contains(arg, "::") && strings.Count(arg, ":") == 2 {
			arg = strings.Replace(arg, "::", ":", 1)
		} else {
			return nil, fmt.Errorf("invalid job ID range %q", arg)
		}
	}

	// Check for range syntax (start:end)
	if idx := strings.Index(arg, ":"); idx >= 0 {
		startStr := arg[:idx]
		endStr := arg[idx+1:]

		start, err := parseJobIDToken(startStr)
		if err != nil {
			return nil, fmt.Errorf("invalid job ID range start %q: expected a numeric or wj-prefixed ID", startStr)
		}
		end, err := parseJobIDToken(endStr)
		if err != nil {
			return nil, fmt.Errorf("invalid job ID range end %q: expected a numeric or wj-prefixed ID", endStr)
		}

		if start > end {
			return nil, fmt.Errorf("invalid job ID range %s: start (%d) must be <= end (%d)", arg, start, end)
		}

		// Limit range size to prevent accidental huge expansions
		const maxRangeSize = 1000
		rangeSize := end - start + 1
		if rangeSize > maxRangeSize {
			return nil, fmt.Errorf("job ID range %s too large (%d jobs, max %d)", arg, rangeSize, maxRangeSize)
		}

		ids := make([]int64, 0, rangeSize)
		for id := start; id <= end; id++ {
			ids = append(ids, id)
		}
		return ids, nil
	}

	// Single ID
	id, err := parseJobIDToken(arg)
	if err != nil {
		return nil, fmt.Errorf("invalid job ID %q: expected a numeric or wj-prefixed ID", arg)
	}
	return []int64{id}, nil
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
