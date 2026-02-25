package tui

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/remote-jobs/internal/db"
)

// formatDuration formats a duration in a human-readable form
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatInlineRangeDuration renders a duration compactly for inline Start/End rows.
func formatInlineRangeDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Truncate(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		if s == 0 {
			return fmt.Sprintf("0h%02dm", m)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatCompactDuration renders a duration using up to two time units (e.g. "3m20s")
func formatCompactDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Truncate(time.Second)

	var parts []string
	const day = 24 * time.Hour

	if d >= day {
		days := d / day
		parts = append(parts, fmt.Sprintf("%dd", days))
		d -= days * day
	}
	if len(parts) < 2 && d >= time.Hour {
		hours := d / time.Hour
		parts = append(parts, fmt.Sprintf("%dh", hours))
		d -= hours * time.Hour
	}
	if len(parts) < 2 && d >= time.Minute {
		minutes := d / time.Minute
		parts = append(parts, fmt.Sprintf("%dm", minutes))
		d -= minutes * time.Minute
	}
	if len(parts) < 2 && d >= time.Second {
		seconds := d / time.Second
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, "")
}

func formatDetailTimestamp(t time.Time) string {
	timeStr, suffix := formatDetailTimeParts(t)
	return timeStr + suffix
}

func formatDetailTimeParts(t time.Time) (string, string) {
	now := time.Now()
	loc := now.Location()
	tt := t.In(loc)
	timeStr := tt.Format("15:04:05")

	startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	startYesterday := startToday.Add(-24 * time.Hour)
	ttDay := time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, loc)

	switch {
	case !ttDay.Before(startToday):
		return timeStr, ""
	case !ttDay.Before(startYesterday):
		return timeStr, " (yesterday)"
	default:
		dateStr := tt.Format("01/02")
		if tt.Year() != now.Year() {
			dateStr += tt.Format("/2006")
		}
		return timeStr, " (" + dateStr + ")"
	}
}

func sameLocalDay(a, b time.Time) bool {
	a = a.In(time.Now().Location())
	b = b.In(time.Now().Location())
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// parseMiB extracts a MiB value from various memory string formats
// Handles: "123MiB", "80GiB", "16G", "128Gi", "58.5G", etc.
func parseMiB(mem string) int {
	mem = strings.TrimSpace(mem)

	// Try MiB suffix first
	if strings.HasSuffix(mem, "MiB") {
		numStr := strings.TrimSuffix(mem, "MiB")
		if mib, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
			return mib
		}
	}

	// Try GiB suffix (convert to MiB)
	if strings.HasSuffix(mem, "GiB") {
		numStr := strings.TrimSuffix(mem, "GiB")
		if gib, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gib * 1024)
		}
	}

	// Try Gi suffix (convert to MiB)
	if strings.HasSuffix(mem, "Gi") {
		numStr := strings.TrimSuffix(mem, "Gi")
		if gib, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gib * 1024)
		}
	}

	// Try G suffix (treat as GB, convert to MiB approximately)
	if strings.HasSuffix(mem, "G") {
		numStr := strings.TrimSuffix(mem, "G")
		if gb, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gb * 1024) // Approximate GB as GiB for simplicity
		}
	}

	return 0
}

// renderProgressBar renders a progress bar with the given percentage and width
func tuiFormatMemoryKB(kb int64) string {
	switch {
	case kb >= 1024*1024:
		return fmt.Sprintf("%.1f GB", float64(kb)/(1024*1024))
	case kb >= 1024:
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	default:
		return fmt.Sprintf("%d KB", kb)
	}
}

func renderProgressBar(percent int, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := (percent * width) / 100
	empty := width - filled

	bar := progressBarFilledStyle.Render(strings.Repeat("█", filled))
	bar += progressBarEmptyStyle.Render(strings.Repeat("░", empty))
	return bar
}

// formatGPUMem formats GPU memory, converting large MiB values to GiB
func formatGPUMem(mem string) string {
	mem = strings.TrimSpace(mem)
	// Try to parse as MiB
	if strings.HasSuffix(mem, "MiB") {
		numStr := strings.TrimSuffix(mem, "MiB")
		if mib, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
			if mib >= 1024 {
				gib := float64(mib) / 1024.0
				return fmt.Sprintf("%.1fGiB", gib)
			}
			return fmt.Sprintf("%dMiB", mib)
		}
	}
	return mem
}

func formatCPUPercent(value float64) string {
	rounded := math.Round(value)
	if math.Abs(value-rounded) < 0.05 {
		return fmt.Sprintf("%.0f%%", rounded)
	}
	return fmt.Sprintf("%.1f%%", value)
}

func defaultAllotmentPercent(host *Host) (int, bool) {
	if host == nil || host.CPUs == 0 {
		return 0, false
	}
	percent := int(math.Round(float64(defaultAllotmentCores) * 100.0 / float64(host.CPUs)))
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}
	return percent, true
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	return ansi.Truncate(s, max, "…")
}

func shortenCommandPath(cmd string) string {
	if cmd == "" {
		return ""
	}
	prefixes := []string{"/home/", "/usr/home/", "/Users/"}
	result := cmd
	for _, prefix := range prefixes {
		result = replaceHomePrefix(result, prefix)
	}
	return result
}

func replaceHomePrefix(s, prefix string) string {
	idx := strings.Index(s, prefix)
	for idx != -1 {
		start := idx + len(prefix)
		end := start
		for end < len(s) && isUsernameChar(s[end]) {
			end++
		}
		if end == start {
			break
		}
		username := s[start:end]
		replacement := "~" + username
		if localUserName != "" && strings.EqualFold(username, localUserName) {
			replacement = "~"
		}
		s = s[:idx] + replacement + s[end:]
		idx = strings.Index(s, prefix)
	}
	return s
}

func isUsernameChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '.'
}

func processCarriageReturns(content string) string {
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		// If line contains \r, take only the part after the last \r
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

func parseCPUAllotmentInput(input string) (*int, error) {
	trimmed := strings.TrimSpace(strings.ToLower(input))
	if trimmed == "" || trimmed == "default" {
		return nil, nil
	}
	trimmed = strings.TrimSuffix(trimmed, "%")
	value, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("CPU allotment must be 1-100 or default")
	}
	if value < 1 || value > 100 {
		return nil, fmt.Errorf("CPU allotment must be 1-100 or default")
	}
	return &value, nil
}

func formatCPUAllotmentInput(allotment *int) string {
	if allotment == nil {
		return ""
	}
	return strconv.Itoa(*allotment)
}

func equalCPUAllotment(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func formatPresetList(presets []int) string {
	parts := make([]string, 0, len(presets))
	for _, preset := range presets {
		parts = append(parts, fmt.Sprintf("%d", preset))
	}
	return strings.Join(parts, "/")
}

func parseEnvInput(input string) []string {
	var envVars []string
	if input == "" {
		return envVars
	}
	for _, ev := range strings.Split(input, ",") {
		ev = strings.TrimSpace(ev)
		if ev != "" {
			envVars = append(envVars, ev)
		}
	}
	return envVars
}

func formatEnvInput(envVars []string) string {
	if len(envVars) == 0 {
		return ""
	}
	return strings.Join(envVars, ", ")
}

func splitGPUEnvVars(envVars []string) (string, []string) {
	var (
		gpu   string
		rest  []string
		found bool
	)
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") && !found {
			gpu = strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
			found = true
			continue
		}
		rest = append(rest, ev)
	}
	return gpu, rest
}

func mergeGPUEnvVars(envVars []string, gpu string) []string {
	var filtered []string
	for _, ev := range envVars {
		if !strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			filtered = append(filtered, ev)
		}
	}
	if gpu != "" {
		filtered = append(filtered, "CUDA_VISIBLE_DEVICES="+gpu)
	}
	return filtered
}

func equalEnvVars(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// naturalSortStrings sorts strings using macOS Finder-style natural ordering
// where numeric segments are compared as numbers (e.g., "cool30" < "cool100")
func naturalSortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool {
		return naturalLess(s[i], s[j])
	})
}

// naturalLess compares two strings using natural ordering
func naturalLess(a, b string) bool {
	aParts := splitIntoSegments(a)
	bParts := splitIntoSegments(b)

	// Compare segment by segment
	minLen := len(aParts)
	if len(bParts) < minLen {
		minLen = len(bParts)
	}

	for i := 0; i < minLen; i++ {
		aSeg := aParts[i]
		bSeg := bParts[i]

		// If both are numeric, compare as numbers
		aNum, aIsNum := parseNumber(aSeg)
		bNum, bIsNum := parseNumber(bSeg)

		if aIsNum && bIsNum {
			if aNum != bNum {
				return aNum < bNum
			}
			// Numbers are equal, continue to next segment
		} else {
			// Compare as strings (case-insensitive)
			aLower := strings.ToLower(aSeg)
			bLower := strings.ToLower(bSeg)
			if aLower != bLower {
				return aLower < bLower
			}
			// Case-insensitive equal, continue to next segment
		}
	}

	// If all compared segments are equal, shorter one comes first
	// If same length, use case-sensitive comparison as final tiebreaker
	if len(aParts) != len(bParts) {
		return len(aParts) < len(bParts)
	}
	return a < b
}

// splitIntoSegments splits a string into alternating alphabetic and numeric segments
func splitIntoSegments(s string) []string {
	var segments []string
	var current strings.Builder

	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')

		if current.Len() == 0 {
			current.WriteRune(r)
		} else {
			lastRune := []rune(current.String())[current.Len()-1]
			lastIsDigit := lastRune >= '0' && lastRune <= '9'
			lastIsAlpha := (lastRune >= 'a' && lastRune <= 'z') || (lastRune >= 'A' && lastRune <= 'Z')

			// Check if we're in the same segment type
			sameType := (isDigit && lastIsDigit) || (isAlpha && lastIsAlpha) || (!isDigit && !isAlpha && !lastIsDigit && !lastIsAlpha)

			if sameType {
				current.WriteRune(r)
			} else {
				segments = append(segments, current.String())
				current.Reset()
				current.WriteRune(r)
			}
		}
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}

// parseNumber attempts to parse a string as an integer
func parseNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// parseGPUIndices parses a CUDA_VISIBLE_DEVICES value into GPU indices
func parseGPUIndices(gpuStr string) []int {
	if gpuStr == "" {
		return nil
	}
	parts := strings.Split(gpuStr, ",")
	var indices []int
	for _, p := range parts {
		if idx, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			indices = append(indices, idx)
		}
	}
	return indices
}

// formatStartTime formats a start time as relative ("2h ago") for recent jobs
// or as absolute ("01/02 15:04") for older jobs
func formatStartTime(startTime int64) string {
	// Handle queued jobs that haven't started yet
	if startTime == 0 {
		return "—"
	}

	t := time.Unix(startTime, 0)
	elapsed := time.Since(t)

	if elapsed < 12*time.Hour {
		if elapsed < time.Minute {
			return "just now"
		} else if elapsed < time.Hour {
			return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
		}
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	}
	return t.Format("01/02 15:04")
}

// formatJobTime formats the time column for a job, showing either start time or queue time
func formatJobTime(job *db.Job) string {
	// Show end time for any job that has completed/terminated
	if job.EndTime != nil && job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused {
		return formatStartTime(*job.EndTime)
	}
	// If job has started, show start time
	if job.StartTime != 0 {
		return formatStartTime(job.StartTime)
	}
	// For unstarted jobs (pending/queued), show when they were queued
	if job.CreatedAt != 0 {
		return "Q:" + formatQueueTime(job.CreatedAt)
	}
	return "—"
}

// formatQueueTime formats a queue time in a compact form
func formatQueueTime(createdAt int64) string {
	t := time.Unix(createdAt, 0)
	elapsed := time.Since(t)

	if elapsed < time.Minute {
		return "<1m"
	} else if elapsed < time.Hour {
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	} else if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
	days := int(elapsed.Hours() / 24)
	return fmt.Sprintf("%dd", days)
}

// wrapTextWithIndent wraps text to the given width, indenting continuation lines.
// It handles both explicit newlines in the text and wrapping at word boundaries.
// Long tokens that exceed the width are broken at the width boundary.
func wrapTextWithIndent(text string, width, indent int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	indentStr := strings.Repeat(" ", indent)

	// Split by explicit newlines first
	lines := strings.Split(text, "\n")
	for li, line := range lines {
		if li > 0 {
			result.WriteString("\n")
			result.WriteString(indentStr)
		}

		// Wrap this line
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}

		lineLen := 0
		for i, word := range words {
			wordLen := len(word)

			if i == 0 && li == 0 {
				// First word of first line (no indent needed)
				if wordLen > width {
					// Break long word
					for len(word) > 0 {
						take := width - lineLen
						if take <= 0 {
							result.WriteString("\n")
							result.WriteString(indentStr)
							lineLen = 0
							take = width
						}
						if take > len(word) {
							take = len(word)
						}
						result.WriteString(word[:take])
						word = word[take:]
						lineLen += take
					}
				} else {
					result.WriteString(word)
					lineLen = wordLen
				}
			} else if lineLen+1+wordLen <= width {
				// Word fits on current line
				if lineLen > 0 {
					result.WriteString(" ")
					lineLen++
				}
				result.WriteString(word)
				lineLen += wordLen
			} else if wordLen > width {
				// Word is longer than available width - break it
				if lineLen > 0 {
					result.WriteString("\n")
					result.WriteString(indentStr)
					lineLen = 0
				}
				for len(word) > 0 {
					take := width
					if take > len(word) {
						take = len(word)
					}
					result.WriteString(word[:take])
					word = word[take:]
					lineLen = take
					if len(word) > 0 {
						result.WriteString("\n")
						result.WriteString(indentStr)
						lineLen = 0
					}
				}
			} else {
				// Need to wrap - word fits on next line
				result.WriteString("\n")
				result.WriteString(indentStr)
				result.WriteString(word)
				lineLen = wordLen
			}
		}
	}

	return result.String()
}

// wrapText wraps text to the given width without indentation
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}

	lineLen := 0
	for i, word := range words {
		wordLen := len(word)
		if i == 0 {
			result.WriteString(word)
			lineLen = wordLen
		} else if lineLen+1+wordLen <= width {
			result.WriteString(" ")
			result.WriteString(word)
			lineLen += 1 + wordLen
		} else {
			result.WriteString("\n")
			result.WriteString(word)
			lineLen = wordLen
		}
	}

	return result.String()
}

// abbreviateProject shortens a hyphenated project name to fit within maxWidth.
// It iteratively shortens the longest segment first, preserving shorter segments,
// then falls back to initials, then truncates.
func abbreviateProject(name string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if len(name) <= maxWidth {
		return name
	}

	parts := strings.Split(name, "-")
	if len(parts) == 1 {
		// Non-hyphenated: just truncate with ellipsis
		return truncate(name, maxWidth)
	}

	// Shorten the longest segment first, one char at a time
	lens := make([]int, len(parts))
	for i, p := range parts {
		lens[i] = len(p)
	}
	hyphens := len(parts) - 1
	for sumInts(lens)+hyphens > maxWidth {
		// Find last segment with max length (last wins ties,
		// so earlier segments are preserved first)
		maxIdx := 0
		for i := 1; i < len(lens); i++ {
			if lens[i] >= lens[maxIdx] {
				maxIdx = i
			}
		}
		if lens[maxIdx] <= 1 {
			break
		}
		lens[maxIdx]--
	}

	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(p[:lens[i]])
	}
	candidate := b.String()
	if len(candidate) <= maxWidth {
		return candidate
	}

	// Single-char segments with hyphens still too long; drop hyphens (initials)
	initials := initialString(parts)
	if len(initials) <= maxWidth {
		return initials
	}

	// Even initials too long: truncate with ellipsis
	return truncate(initials, maxWidth)
}

func sumInts(a []int) int {
	s := 0
	for _, v := range a {
		s += v
	}
	return s
}

func initialString(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteByte(p[0])
		}
	}
	return b.String()
}

// Chroma syntax highlighting for shell commands
var (
	shellLexer     chroma.Lexer
	shellFormatter chroma.Formatter
	shellStyle     *chroma.Style
)

func init() {
	// Use bash lexer for shell command highlighting
	shellLexer = lexers.Get("bash")
	if shellLexer == nil {
		shellLexer = lexers.Fallback
	}
	shellLexer = chroma.Coalesce(shellLexer)

	// Use terminal256 formatter for ANSI output
	shellFormatter = formatters.Get("terminal256")
	if shellFormatter == nil {
		shellFormatter = formatters.Fallback
	}

	// Use pygments style (works well with light backgrounds)
	shellStyle = styles.Get("pygments")
	if shellStyle == nil {
		shellStyle = styles.Fallback
	}
}

// highlightCommand applies syntax coloring to a shell command using chroma.
func highlightCommand(cmd string) string {
	if cmd == "" {
		return cmd
	}

	iterator, err := shellLexer.Tokenise(nil, cmd)
	if err != nil {
		return cmd // Fall back to plain text on error
	}

	var buf bytes.Buffer
	err = shellFormatter.Format(&buf, shellStyle, iterator)
	if err != nil {
		return cmd // Fall back to plain text on error
	}

	// Remove trailing newline that chroma adds
	result := buf.String()
	result = strings.TrimSuffix(result, "\n")
	return result
}
