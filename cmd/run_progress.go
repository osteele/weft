package cmd

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// runPhaseRecorder times phases of `weft run` and emits progress lines + a
// final summary on stderr. The goal is to attribute wall-clock time to
// specific phases so users (and agents) can identify where time was spent
// without guessing.
//
// Output shape:
//
//	weft run: <command>                              <- banner (on construction)
//	weft run: checking predictor… (0.0s)             <- Phase start
//	weft run: syncing host cool30… (0.3s)            <- Phase start
//	weft run: sync skipped (--no-sync) — ... (1.5s)  <- Event (one-shot)
//	weft run: predictor 0.0s, sync 1.1s, total 1.5s  <- PrintSummary
type runPhaseRecorder struct {
	w     io.Writer
	start time.Time

	mu        sync.Mutex
	phases    []phaseRecord
	finalized bool
}

type phaseRecord struct {
	key      string
	duration time.Duration
}

// newRunPhaseRecorder constructs a recorder, prints the banner, and starts
// the wall clock.
func newRunPhaseRecorder(w io.Writer, command string) *runPhaseRecorder {
	r := &runPhaseRecorder{
		w:     w,
		start: time.Now(),
	}
	if trimmed := strings.TrimSpace(command); trimmed != "" {
		fmt.Fprintf(r.w, "weft run: %s\n", trimmed)
	} else {
		fmt.Fprintln(r.w, "weft run")
	}
	return r
}

// Phase prints "weft run: <gerund>… (Xs)" with elapsed-since-banner and
// returns an end-func that records the phase duration in the summary.
// `key` is the short identifier used in the summary line.
func (r *runPhaseRecorder) Phase(key, gerund string) func() {
	if r == nil {
		return func() {}
	}
	started := time.Now()
	fmt.Fprintf(r.w, "weft run: %s… (%s)\n", gerund, formatElapsed(time.Since(r.start)))
	return func() {
		d := time.Since(started)
		r.mu.Lock()
		r.phases = append(r.phases, phaseRecord{key: key, duration: d})
		r.mu.Unlock()
	}
}

// Event prints "weft run: <msg> (Xs)" immediately. Use for one-shot
// status that doesn't represent a measurable phase (e.g.
// "sync skipped (--no-sync) — will sync on next background sync").
func (r *runPhaseRecorder) Event(msg string) {
	if r == nil {
		return
	}
	fmt.Fprintf(r.w, "weft run: %s (%s)\n", msg, formatElapsed(time.Since(r.start)))
}

// PrintSummary emits a one-line per-phase summary. Safe to call multiple
// times; only the first call prints.
func (r *runPhaseRecorder) PrintSummary() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return
	}
	r.finalized = true
	total := time.Since(r.start)
	if len(r.phases) == 0 {
		fmt.Fprintf(r.w, "weft run: total %s\n", formatElapsed(total))
		return
	}
	var b strings.Builder
	b.WriteString("weft run: ")
	for i, p := range r.phases {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", p.key, formatElapsed(p.duration))
	}
	fmt.Fprintf(&b, ", total %s\n", formatElapsed(total))
	r.w.Write([]byte(b.String()))
}

// formatElapsed renders a duration as e.g. "0.0s" or "12.3s".
func formatElapsed(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
