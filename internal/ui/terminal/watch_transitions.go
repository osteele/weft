package terminal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/status"
)

var stdoutDefault io.Writer = os.Stdout

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

// Event type discriminators emitted on the `type` field of stream output.
const (
	EventTypeTransition = "transition"
	EventTypeSnapshot   = "snapshot"
)

// TransitionEvent describes a single status change observed across two
// consecutive watch iterations. Stable v1 fields are documented in
// specs/job-lifecycle.allium § WatchEmitsTransitionEvents.
type TransitionEvent struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp"`
	JobID      string `json:"job_id"`
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	PrevStatus string `json:"prev_status"`
	Host       string `json:"host"`
	Project    string `json:"project"`
	InstanceID *int64 `json:"instance_id"`
	ExitCode   *int   `json:"exit_code"`
}

// IsTerminal reports whether this event lands the job in a terminal state.
func (e TransitionEvent) IsTerminal() bool {
	return status.IsTerminal(e.Status)
}

// SnapshotJob is the per-job record emitted in JSONL snapshot mode.
type SnapshotJob struct {
	JobID      string `json:"job_id"`
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Host       string `json:"host"`
	Project    string `json:"project"`
	InstanceID *int64 `json:"instance_id"`
	ExitCode   *int   `json:"exit_code"`
}

// SnapshotEvent is one JSONL line per iteration in `--jsonl` snapshot mode.
type SnapshotEvent struct {
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Jobs      []SnapshotJob `json:"jobs"`
}

// TransitionTracker carries job effective-status across watch iterations
// and reports the diffs as TransitionEvents.
type TransitionTracker struct {
	prev   map[int64]string
	seeded bool
}

// NewTransitionTracker returns an empty tracker. The first Diff call seeds
// it (returns nil) and subsequent calls report changes.
func NewTransitionTracker() *TransitionTracker {
	return &TransitionTracker{prev: make(map[int64]string)}
}

// Diff updates the tracker with the current job slice and returns the list
// of transitions since the last Diff call. The first call always returns
// nil (baseline). Jobs appearing for the first time after the baseline are
// reported with PrevStatus = "" (never seen before).
//
// Jobs missing from a subsequent slice are not reported as transitions —
// they are simply forgotten, so a later reappearance is treated as a new
// arrival.
func (t *TransitionTracker) Diff(jobs []*db.Job, now time.Time) []TransitionEvent {
	cur := make(map[int64]*db.Job, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
		// dedupe by ID; later occurrences win (caller is expected to
		// pre-sort attempts so the latest wins, but we do not depend
		// on that — we only compare effective_status).
		cur[j.ID] = j
	}

	if !t.seeded {
		for id, j := range cur {
			t.prev[id] = j.EffectiveStatus()
		}
		t.seeded = true
		return nil
	}

	var events []TransitionEvent
	for id, j := range cur {
		s := j.EffectiveStatus()
		prev, hadPrev := t.prev[id]
		if hadPrev && prev == s {
			continue
		}
		events = append(events, transitionEventFor(j, prev, s, now))
		t.prev[id] = s
	}
	// Forget jobs that have dropped out of the window so a future
	// reappearance is reported as a new arrival.
	for id := range t.prev {
		if _, ok := cur[id]; !ok {
			delete(t.prev, id)
		}
	}
	return events
}

func transitionEventFor(j *db.Job, prev, cur string, now time.Time) TransitionEvent {
	return TransitionEvent{
		Type:       EventTypeTransition,
		Timestamp:  now.UTC().Format(time.RFC3339),
		JobID:      ids.FormatJobID(j.ID),
		ID:         j.ID,
		Status:     cur,
		PrevStatus: prev,
		Host:       j.Host,
		Project:    j.Project,
		InstanceID: j.LaunchID,
		ExitCode:   j.ExitCode,
	}
}

// BuildSnapshotEvent constructs the SnapshotEvent for `--jsonl` snapshot mode.
func BuildSnapshotEvent(jobs []*db.Job, now time.Time) SnapshotEvent {
	out := make([]SnapshotJob, 0, len(jobs))
	seen := make(map[int64]struct{}, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if _, ok := seen[j.ID]; ok {
			continue
		}
		seen[j.ID] = struct{}{}
		out = append(out, SnapshotJob{
			JobID:      ids.FormatJobID(j.ID),
			ID:         j.ID,
			Status:     j.EffectiveStatus(),
			Host:       j.Host,
			Project:    j.Project,
			InstanceID: j.LaunchID,
			ExitCode:   j.ExitCode,
		})
	}
	return SnapshotEvent{
		Type:      EventTypeSnapshot,
		Timestamp: now.UTC().Format(time.RFC3339),
		Jobs:      out,
	}
}

// RenderTransitionPlain returns one human-readable line for an event, e.g.
//
//	wj1531  running → completed  cool30  exit=0
//
// Fields after the status arrow are omitted when empty/nil.
func RenderTransitionPlain(ev TransitionEvent) string {
	var b strings.Builder
	b.WriteString(ev.JobID)
	b.WriteString("  ")
	if ev.PrevStatus == "" {
		b.WriteString("(new)")
	} else {
		b.WriteString(ev.PrevStatus)
	}
	b.WriteString(" → ")
	b.WriteString(ev.Status)
	if ev.Host != "" {
		b.WriteString("  ")
		b.WriteString(ev.Host)
	}
	if ev.InstanceID != nil {
		fmt.Fprintf(&b, "  %s", ids.FormatInstanceID(*ev.InstanceID))
	}
	if ev.ExitCode != nil {
		fmt.Fprintf(&b, "  exit=%d", *ev.ExitCode)
	}
	if ev.Project != "" {
		b.WriteString("  project=")
		b.WriteString(ev.Project)
	}
	return b.String()
}

// EncodeJSONLine writes v as a single-line JSON object followed by '\n'.
func EncodeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
