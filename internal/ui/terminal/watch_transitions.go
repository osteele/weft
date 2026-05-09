package terminal

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/watchevents"
)

var stdoutDefault io.Writer = os.Stdout

type TransitionEvent = watchevents.TransitionEvent
type SnapshotJob = watchevents.SnapshotJob
type SnapshotEvent = watchevents.SnapshotEvent
type TransitionTracker = watchevents.TransitionTracker

const (
	EventTypeTransition = watchevents.EventTypeTransition
	EventTypeSnapshot   = watchevents.EventTypeSnapshot
)

func DedupeJobsByID(slices ...[]*db.Job) []*db.Job { return watchevents.DedupeJobsByID(slices...) }
func NewTransitionTracker() *TransitionTracker     { return watchevents.NewTransitionTracker() }
func BuildSnapshotEvent(jobs []*db.Job, now time.Time) SnapshotEvent {
	return watchevents.BuildSnapshotEvent(jobs, now)
}
func RenderTransitionPlain(ev TransitionEvent) string { return watchevents.RenderTransitionPlain(ev) }
func EncodeJSONLine(w io.Writer, v any) error         { return watchevents.EncodeJSONLine(w, v) }

// WatchPlainOptions configures the plain-mode watch loops shared by
// `weft watch`, `weft job watch`, and `weft project watch`.
type WatchPlainOptions struct {
	Follow           bool
	TransitionsOnly  bool
	JSONLines        bool
	UntilAnyTerminal bool
	// Stdout / Stderr default to os.Stdout / os.Stderr when nil.
	Stdout io.Writer
	Stderr io.Writer
}

// EmitTransitions emits transition events according to the options. Returns
// whether any emitted event was terminal (relevant to --until-any-terminal).
func (o WatchPlainOptions) EmitTransitions(events []TransitionEvent) (anyTerminal bool, err error) {
	w := o.Stdout
	if w == nil {
		w = stdoutDefault
	}
	for _, ev := range events {
		if ev.IsTerminal() {
			anyTerminal = true
		}
		if o.JSONLines {
			if err := EncodeJSONLine(w, ev); err != nil {
				return anyTerminal, err
			}
		} else if o.TransitionsOnly {
			if _, err := fmt.Fprintln(w, RenderTransitionPlain(ev)); err != nil {
				return anyTerminal, err
			}
		}
	}
	return anyTerminal, nil
}

// EmitSnapshotJSON writes one JSON-snapshot line for the iteration.
func (o WatchPlainOptions) EmitSnapshotJSON(jobs []*db.Job, now time.Time) error {
	w := o.Stdout
	if w == nil {
		w = stdoutDefault
	}
	return EncodeJSONLine(w, BuildSnapshotEvent(jobs, now))
}

// EmitsEvents reports whether the loop should run a TransitionTracker.
// Snapshot-only modes (no JSONL, no transitions filter) skip tracking.
func (o WatchPlainOptions) EmitsEvents() bool {
	return o.TransitionsOnly || o.JSONLines || o.UntilAnyTerminal
}

// SuppressTextSnapshot reports whether the human-readable snapshot block
// should be omitted (because output is going through transition events
// or JSON encoding instead).
func (o WatchPlainOptions) SuppressTextSnapshot() bool {
	return o.TransitionsOnly || o.JSONLines
}

// SnapshotMode describes which snapshot output to produce per iteration.
type SnapshotMode int

const (
	// SnapshotText emits the full human-readable snapshot block.
	SnapshotText SnapshotMode = iota
	// SnapshotJSON emits one JSON-snapshot object per iteration.
	SnapshotJSON
	// SnapshotNone emits no snapshot; per-event output is used instead.
	SnapshotNone
)

// SnapshotMode chooses the per-iteration output style based on the flag mix.
func (o WatchPlainOptions) SnapshotMode() SnapshotMode {
	switch {
	case o.TransitionsOnly:
		return SnapshotNone
	case o.JSONLines:
		return SnapshotJSON
	default:
		return SnapshotText
	}
}
