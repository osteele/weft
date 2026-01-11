package cmd

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ParseJobIDs parses command-line arguments into a deduplicated, sorted list of job IDs.
// Supports individual IDs (123) and ranges (123:127 expands to 123,124,125,126,127).
// Prints a warning to stderr if duplicates are found.
//
// Syntax:
//   - Single ID: 123
//   - Range: 123:127 (inclusive, expands to 123, 124, 125, 126, 127)
//   - Mixed: 123 125:127 130 (expands to 123, 125, 126, 127, 130)
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
				uniqueDups = append(uniqueDups, strconv.FormatInt(id, 10))
			}
		}
		fmt.Fprintf(os.Stderr, "Warning: ignoring duplicate job ID(s): %s\n", strings.Join(uniqueDups, ", "))
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// parseJobIDArg parses a single argument which may be an ID or a range.
func parseJobIDArg(arg string) ([]int64, error) {
	// Check for range syntax (start:end)
	if idx := strings.Index(arg, ":"); idx >= 0 {
		startStr := arg[:idx]
		endStr := arg[idx+1:]

		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid job ID range start %q: %w", startStr, err)
		}
		end, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid job ID range end %q: %w", endStr, err)
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
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid job ID %q: %w", arg, err)
	}
	return []int64{id}, nil
}
