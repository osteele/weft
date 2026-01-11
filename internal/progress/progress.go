package progress

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Progress represents parsed progress information from a job
type Progress struct {
	Percent int    // 0-100, -1 if using Current/Total instead
	Current int    // For "N/M" format (0 if using Percent)
	Total   int    // For "N/M" format (0 if using Percent)
	RawLine string // The original progress line
}

// DisplayPercent returns the progress as a percentage (0-100).
// For Current/Total format, it calculates the percentage.
// Returns -1 if progress cannot be determined.
func (p *Progress) DisplayPercent() int {
	if p.Percent >= 0 {
		return p.Percent
	}
	if p.Total > 0 {
		return (p.Current * 100) / p.Total
	}
	return -1
}

// Tracker tracks file sizes to enable incremental reading
type Tracker struct {
	mu    sync.Mutex
	sizes map[string]int64 // logPath -> last known file size
}

// NewTracker creates a new file size tracker
func NewTracker() *Tracker {
	return &Tracker{
		sizes: make(map[string]int64),
	}
}

// GetLastSize returns the last known file size for a path, or 0 if unknown
func (t *Tracker) GetLastSize(path string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sizes[path]
}

// SetSize updates the tracked file size for a path
func (t *Tracker) SetSize(path string, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sizes[path] = size
}

// Clear removes a path from tracking (e.g., when job completes)
func (t *Tracker) Clear(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sizes, path)
}

// Progress line patterns:
// - Progress: 75%
// - Progress: 9/14
// - Progress: 9 of 14
// - Progress: 9 out of 14
var (
	// Matches "Progress: 75%" or "Progress: 75 %"
	percentPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s*%`)

	// Matches "Progress: 9/14"
	slashPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s*/\s*(\d+)`)

	// Matches "Progress: 9 of 14" or "Progress: 9 out of 14"
	ofPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s+(?:of|out of)\s+(\d+)`)
)

// ParseProgress parses a single line for progress information.
// Returns nil if no progress pattern found.
func ParseProgress(line string) *Progress {
	line = strings.TrimSpace(line)
	if idx := strings.LastIndex(line, "\r"); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}
	if idx := strings.LastIndex(strings.ToLower(line), "progress:"); idx >= 0 {
		line = strings.TrimSpace(line[idx:])
	}

	// Try percent pattern first
	if m := percentPattern.FindStringSubmatch(line); m != nil {
		percent, _ := strconv.Atoi(m[1])
		return &Progress{
			Percent: percent,
			RawLine: line,
		}
	}

	// Try slash pattern (N/M)
	if m := slashPattern.FindStringSubmatch(line); m != nil {
		current, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		return &Progress{
			Percent: -1,
			Current: current,
			Total:   total,
			RawLine: line,
		}
	}

	// Try "of" pattern (N of M, N out of M)
	if m := ofPattern.FindStringSubmatch(line); m != nil {
		current, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		return &Progress{
			Percent: -1,
			Current: current,
			Total:   total,
			RawLine: line,
		}
	}

	return nil
}

// FindLastProgress searches content for the last progress line.
// Scans from end since we want the most recent progress.
func FindLastProgress(content string) *Progress {
	lines := strings.Split(content, "\n")

	// Scan from end to find most recent progress line
	for i := len(lines) - 1; i >= 0; i-- {
		if prog := ParseProgress(lines[i]); prog != nil {
			return prog
		}
	}

	return nil
}
