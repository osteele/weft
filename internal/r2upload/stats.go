package r2upload

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// statsLineRE matches the bytes-transferred section of rclone's "--stats-one-line"
// summary, e.g. "Transferred: 1.234 MiB / 100.000 MiB, 1%, ...". Capture group 1
// is the count, group 2 is the unit (possibly empty for bare bytes).
var statsLineRE = regexp.MustCompile(`Transferred:\s+([\d.]+)\s*([KMGT]i?B|B)?\s*/`)

// jsonMsg is the subset of an rclone --use-json-log line we care about.
type jsonMsg struct {
	Msg string `json:"msg"`
}

// extractBytes parses a single rclone stderr line and returns the cumulative
// bytes-transferred count when the line is a stats summary. ok == false for
// any other line (per-file messages, errors, etc.).
//
// Handles both the JSON-log form (--use-json-log) and the raw text form, so
// older rclone versions still drive the watchdog correctly.
func extractBytes(line string) (n int64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, false
	}
	// JSON form: {"level":"notice","msg":"Transferred: 1.234 MiB / ...", ...}
	if len(line) > 0 && line[0] == '{' {
		var m jsonMsg
		if err := json.Unmarshal([]byte(line), &m); err == nil && m.Msg != "" {
			line = m.Msg
		}
	}
	match := statsLineRE.FindStringSubmatch(line)
	if len(match) < 2 {
		return 0, false
	}
	val, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	unit := ""
	if len(match) >= 3 {
		unit = match[2]
	}
	multiplier, ok := unitMultiplier(unit)
	if !ok {
		return 0, false
	}
	return int64(val * multiplier), true
}

// unitMultiplier returns the bytes-multiplier for an rclone size suffix.
// rclone uses IEC binary units (KiB, MiB, GiB, TiB); the "B" / "" form means
// bare bytes. We tolerate both "KB" and "KiB" since older rclone versions
// occasionally elide the "i".
func unitMultiplier(unit string) (float64, bool) {
	switch strings.ToUpper(unit) {
	case "", "B":
		return 1, true
	case "KIB", "KB":
		return 1 << 10, true
	case "MIB", "MB":
		return 1 << 20, true
	case "GIB", "GB":
		return 1 << 30, true
	case "TIB", "TB":
		return float64(int64(1) << 40), true
	}
	return 0, false
}
