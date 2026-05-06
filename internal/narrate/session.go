package narrate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Session is the in-memory state of a single `weft narrate` invocation.
// It owns the accumulated recap chain and the previous snapshot used for diffing.
type Session struct {
	StartedAt time.Time

	prev       *Snapshot
	lastStatus *StatusLine
	recaps     []recapEntry
	tokens     int
	limit      int
	tickIdx    int

	debug     bool
	debugSink io.Writer
}

type recapEntry struct {
	At    time.Time
	Recap string
}

// SessionOptions configures a new session.
type SessionOptions struct {
	CompactionThreshold int
	Debug               bool
	DebugSink           io.Writer // where --debug payloads go (defaults to stderr if Debug is true)
}

// NewSession constructs a session.
func NewSession(opts SessionOptions) (*Session, error) {
	s := &Session{
		StartedAt: time.Now().UTC(),
		limit:     opts.CompactionThreshold,
		debug:     opts.Debug,
		debugSink: opts.DebugSink,
	}
	if s.limit <= 0 {
		s.limit = 15000
	}
	return s, nil
}

// Prev returns the previous snapshot used for diffing, or nil for the first tick.
func (s *Session) Prev() *Snapshot { return s.prev }

// SetPrev records the latest snapshot for next-tick diffing.
func (s *Session) SetPrev(snap *Snapshot) { s.prev = snap }

// UpdateStatus records sl and returns whether it differs from the previous
// recorded status.
func (s *Session) UpdateStatus(sl StatusLine) bool {
	changed := s.lastStatus == nil || !s.lastStatus.Equal(sl)
	c := sl
	s.lastStatus = &c
	return changed
}

// AppendRecap adds a recap entry and returns whether compaction should run
// next (caller is responsible for invoking Compact).
func (s *Session) AppendRecap(recap string) bool {
	recap = strings.TrimSpace(recap)
	if recap == "" {
		return false
	}
	s.recaps = append(s.recaps, recapEntry{At: time.Now().UTC(), Recap: recap})
	s.tokens += approxTokens(recap)
	return s.tokens >= s.limit
}

// Recaps returns the accumulated recap entries. Callers must treat the
// result as read-only.
func (s *Session) Recaps() []recapEntry { return s.recaps }

// ReplaceWithCompacted swaps the recap chain for a single compacted block.
func (s *Session) ReplaceWithCompacted(recap string) {
	recap = strings.TrimSpace(recap)
	s.recaps = []recapEntry{{At: time.Now().UTC(), Recap: recap}}
	s.tokens = approxTokens(recap)
}

// EmitEntry prints a unified entry: a multi-line header (timestamp +
// status counters + project mix), followed by the narration body word-
// wrapped under a 4-space indent. A trailing blank line separates
// entries visually so the eye can chunk the log.
//
// width is the visible terminal width (used for project abbreviation and
// narration wrap). 0 disables width-aware truncation/wrap (use a sane
// default of 100).
//
// If statusChanged is false AND narration is empty, nothing is printed.
func (s *Session) EmitEntry(out io.Writer, sl StatusLine, narration string, statusChanged bool, width int) {
	s.tickIdx++
	narration = strings.TrimSpace(narration)
	if !statusChanged && narration == "" {
		return
	}
	if out == nil {
		return
	}
	if width <= 0 {
		width = 100
	}
	for _, line := range sl.HeaderLines(width) {
		fmt.Fprintln(out, line)
	}
	if narration != "" {
		fmt.Fprintln(out, indentWrap(narration, "    ", width-4))
	}
	fmt.Fprintln(out)
}

// indentWrap wraps text to width and prefixes every line with indent.
// width is the visible column width of body text (excluding the indent).
func indentWrap(text, indent string, width int) string {
	if width <= 0 {
		width = 80
	}
	var out strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			out.WriteString("\n")
		}
		out.WriteString(wrapOne(line, indent, width))
	}
	return out.String()
}

func wrapOne(line, indent string, width int) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return indent
	}
	var sb strings.Builder
	sb.WriteString(indent)
	col := 0
	for i, w := range words {
		if i > 0 {
			if col+1+len(w) > width {
				sb.WriteString("\n")
				sb.WriteString(indent)
				col = 0
			} else {
				sb.WriteString(" ")
				col++
			}
		}
		sb.WriteString(w)
		col += len(w)
	}
	return sb.String()
}

// EmitDebug writes a debug payload to the configured debug sink (stderr by
// default when --debug is on).
func (s *Session) EmitDebug(label string, payload any) {
	if !s.debug || s.debugSink == nil {
		return
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(s.debugSink, "# DEBUG %s: marshal failed: %v\n", label, err)
		return
	}
	fmt.Fprintf(s.debugSink, "# DEBUG %s\n%s\n", label, b)
}

// approxTokens is a rough heuristic — about 4 chars per token for English
// prose. Used to decide when to compact, not for billing.
func approxTokens(s string) int {
	return (len(s) + 3) / 4
}
