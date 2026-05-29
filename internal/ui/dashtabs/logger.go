package dashtabs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger records tab dwell time as NDJSON for post-hoc analysis. The user
// asked for foreground-time tracking and a record of whether focus reporting
// works at all — no in-UI indicator.
//
// All writes are best-effort; we silently swallow errors rather than disturb
// the TUI.
type Logger struct {
	mu            sync.Mutex
	path          string
	f             *os.File
	enc           *json.Encoder
	currentTab    string
	enterTS       time.Time
	focused       bool
	focusObserved bool
	disabled      bool
}

// NewLogger opens (or creates) ~/.cache/weft/dashboard-usage.log for append.
func NewLogger() *Logger {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Logger{disabled: true}
	}
	dir := filepath.Join(home, ".cache", "weft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Logger{disabled: true}
	}
	path := filepath.Join(dir, "dashboard-usage.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return &Logger{disabled: true}
	}
	l := &Logger{path: path, f: f, enc: json.NewEncoder(f)}
	l.write(map[string]any{
		"event": "session_start",
		"ts":    time.Now().Format(time.RFC3339Nano),
		"pid":   os.Getpid(),
	})
	return l
}

// Close finalizes any in-progress dwell record and closes the underlying file.
func (l *Logger) Close() {
	if l == nil || l.disabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.currentTab != "" && !l.enterTS.IsZero() && l.focused {
		l.writeLocked(map[string]any{
			"event":          "tab_exit",
			"tab":            l.currentTab,
			"ts":             time.Now().Format(time.RFC3339Nano),
			"dwell_ms":       time.Since(l.enterTS).Milliseconds(),
			"focused":        l.focused,
			"focus_observed": l.focusObserved,
			"reason":         "session_end",
		})
	}
	l.writeLocked(map[string]any{
		"event":          "session_end",
		"ts":             time.Now().Format(time.RFC3339Nano),
		"focus_observed": l.focusObserved,
	})
	_ = l.f.Close()
}

// TabEnter records that the user switched into a new tab. If the window
// isn't currently focused, we still record the enter but the dwell doesn't
// start accumulating until focus arrives.
func (l *Logger) TabEnter(tab string) {
	if l == nil || l.disabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Close any in-progress dwell first.
	l.closeDwellLocked("tab_switch")
	l.currentTab = tab
	if l.focused {
		l.enterTS = time.Now()
	} else {
		l.enterTS = time.Time{}
	}
	l.writeLocked(map[string]any{
		"event":          "tab_enter",
		"tab":            tab,
		"ts":             time.Now().Format(time.RFC3339Nano),
		"focused":        l.focused,
		"focus_observed": l.focusObserved,
	})
}

// SetFocused records a focus state change. The first call (with either value)
// flips focus_observed to true, indicating this terminal does report focus.
func (l *Logger) SetFocused(f bool) {
	if l == nil || l.disabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	wasObserved := l.focusObserved
	l.focusObserved = true
	if f == l.focused && wasObserved {
		return
	}
	prev := l.focused
	l.focused = f
	switch {
	case f && !prev:
		// Gained focus — start dwell clock if a tab is active.
		if l.currentTab != "" {
			l.enterTS = time.Now()
		}
	case !f && prev:
		// Lost focus — close in-progress dwell.
		l.closeDwellLocked("blur")
	}
	l.writeLocked(map[string]any{
		"event":          "focus_change",
		"ts":             time.Now().Format(time.RFC3339Nano),
		"focused":        f,
		"focus_observed": true,
	})
}

func (l *Logger) closeDwellLocked(reason string) {
	if l.currentTab == "" || l.enterTS.IsZero() || !l.focused {
		return
	}
	l.writeLocked(map[string]any{
		"event":          "tab_exit",
		"tab":            l.currentTab,
		"ts":             time.Now().Format(time.RFC3339Nano),
		"dwell_ms":       time.Since(l.enterTS).Milliseconds(),
		"focused":        l.focused,
		"focus_observed": l.focusObserved,
		"reason":         reason,
	})
	l.enterTS = time.Time{}
}

func (l *Logger) write(rec map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLocked(rec)
}

func (l *Logger) writeLocked(rec map[string]any) {
	if l.enc == nil {
		return
	}
	_ = l.enc.Encode(rec)
}
