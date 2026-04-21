package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/ids"
)

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
	var parsed []int64
	var duplicates []int64

	for _, arg := range args {
		argIDs, err := parseJobIDArg(arg)
		if err != nil {
			return nil, err
		}

		for _, id := range argIDs {
			if seen[id] {
				duplicates = append(duplicates, id)
			} else {
				seen[id] = true
				parsed = append(parsed, id)
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
				uniqueDups = append(uniqueDups, ids.FormatJobID(id))
			}
		}
		fmt.Fprintf(os.Stderr, "Warning: ignoring duplicate job ID(s): %s\n", strings.Join(uniqueDups, ", "))
	}

	sort.Slice(parsed, func(i, j int) bool { return parsed[i] < parsed[j] })
	return parsed, nil
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
		out := make([]int64, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				return nil, fmt.Errorf("invalid empty job ID in %q", arg)
			}
			partIDs, err := parseJobIDArg(part)
			if err != nil {
				return nil, err
			}
			out = append(out, partIDs...)
		}
		return out, nil
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

		start, err := ids.ParseJobID(startStr)
		if err != nil {
			return nil, fmt.Errorf("invalid job ID range start %q: expected a numeric or wj-prefixed ID", startStr)
		}
		end, err := ids.ParseJobID(endStr)
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

		out := make([]int64, 0, rangeSize)
		for id := start; id <= end; id++ {
			out = append(out, id)
		}
		return out, nil
	}

	// Single ID
	id, err := ids.ParseJobID(arg)
	if err != nil {
		return nil, fmt.Errorf("invalid job ID %q: expected a numeric or wj-prefixed ID", arg)
	}
	return []int64{id}, nil
}

// ParseJobIDsWithExplicitPrefix parses job IDs and requires at least one explicit
// "wj" prefix across the provided args. Instance-prefixed IDs ("wi") are rejected.
func ParseJobIDsWithExplicitPrefix(args []string) ([]int64, error) {
	kind, err := resolveIDTargetKind(args)
	if err != nil {
		return nil, err
	}
	if kind != idTargetJob {
		return nil, usageErrorf("ambiguous ID(s): use wj... for jobs")
	}
	return ParseJobIDs(args)
}
