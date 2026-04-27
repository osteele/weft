package terminal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func mkJob(id int64, status string) *db.Job {
	host := "cool30"
	return &db.Job{ID: id, Status: status, Host: host, Project: "demo"}
}

func TestTransitionTrackerSeedEmitsNothing(t *testing.T) {
	tr := NewTransitionTracker()
	jobs := []*db.Job{mkJob(1, "running"), mkJob(2, "queued")}
	if events := tr.Diff(jobs, time.Now()); events != nil {
		t.Fatalf("seed Diff returned events: %+v", events)
	}
	if !tr.seeded {
		t.Fatal("tracker not marked seeded")
	}
}

func TestTransitionTrackerDetectsStatusChange(t *testing.T) {
	tr := NewTransitionTracker()
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	tr.Diff([]*db.Job{mkJob(1, "running")}, now)

	events := tr.Diff([]*db.Job{mkJob(1, "completed")}, now)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d (%+v)", len(events), events)
	}
	ev := events[0]
	if ev.PrevStatus != "running" || ev.Status != "completed" {
		t.Errorf("got prev=%q cur=%q", ev.PrevStatus, ev.Status)
	}
	if ev.JobID != "wj1" || ev.ID != 1 {
		t.Errorf("bad ids: %+v", ev)
	}
	if ev.Type != "transition" {
		t.Errorf("type=%q want transition", ev.Type)
	}
	if !ev.IsTerminal() {
		t.Error("completed should be terminal")
	}
	if ev.Timestamp != "2026-04-27T10:30:00Z" {
		t.Errorf("timestamp=%q", ev.Timestamp)
	}
}

func TestTransitionTrackerIgnoresUnchanged(t *testing.T) {
	tr := NewTransitionTracker()
	now := time.Now()
	tr.Diff([]*db.Job{mkJob(1, "running")}, now)
	if events := tr.Diff([]*db.Job{mkJob(1, "running")}, now); len(events) != 0 {
		t.Fatalf("unchanged status produced events: %+v", events)
	}
}

func TestTransitionTrackerNewJobAfterSeed(t *testing.T) {
	tr := NewTransitionTracker()
	now := time.Now()
	tr.Diff([]*db.Job{mkJob(1, "running")}, now)
	events := tr.Diff([]*db.Job{mkJob(1, "running"), mkJob(2, "queued")}, now)
	if len(events) != 1 {
		t.Fatalf("want 1 event for new job, got %d", len(events))
	}
	if events[0].ID != 2 || events[0].PrevStatus != "" || events[0].Status != "queued" {
		t.Errorf("bad new-job event: %+v", events[0])
	}
}

func TestTransitionTrackerForgetsDroppedJob(t *testing.T) {
	tr := NewTransitionTracker()
	now := time.Now()
	tr.Diff([]*db.Job{mkJob(1, "running")}, now)
	if events := tr.Diff([]*db.Job{}, now); len(events) != 0 {
		t.Fatalf("dropped job should not emit: %+v", events)
	}
	// Reappearance is treated as new arrival.
	events := tr.Diff([]*db.Job{mkJob(1, "completed")}, now)
	if len(events) != 1 || events[0].PrevStatus != "" {
		t.Fatalf("reappearance: %+v", events)
	}
}

func TestRenderTransitionPlain(t *testing.T) {
	exit := 0
	inst := int64(42)
	ev := TransitionEvent{
		JobID:      "wj7",
		PrevStatus: "running",
		Status:     "completed",
		Host:       "cool30",
		Project:    "demo",
		InstanceID: &inst,
		ExitCode:   &exit,
	}
	got := RenderTransitionPlain(ev)
	for _, want := range []string{"wj7", "running", "completed", "cool30", "exit=0", "project=demo"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestRenderTransitionPlainNewArrival(t *testing.T) {
	got := RenderTransitionPlain(TransitionEvent{JobID: "wj7", Status: "queued"})
	if !strings.Contains(got, "(new)") {
		t.Errorf("want (new) marker for empty prev_status, got %q", got)
	}
}

func TestEncodeTransitionJSONStableFields(t *testing.T) {
	exit := 1
	inst := int64(99)
	ev := TransitionEvent{
		Type:       "transition",
		Timestamp:  "2026-04-27T10:30:00Z",
		JobID:      "wj7",
		ID:         7,
		Status:     "failed",
		PrevStatus: "running",
		Host:       "cool30",
		Project:    "demo",
		InstanceID: &inst,
		ExitCode:   &exit,
	}
	var buf bytes.Buffer
	if err := EncodeJSONLine(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Error("EncodeJSONLine should end with newline")
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("want exactly one newline, got %d", strings.Count(out, "\n"))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "timestamp", "job_id", "id", "status", "prev_status", "host", "project", "instance_id", "exit_code"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing stable field %q in JSON output: %s", key, out)
		}
	}
}

func TestBuildSnapshotEventDedupesByID(t *testing.T) {
	now := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	jobs := []*db.Job{mkJob(1, "running"), mkJob(1, "running"), mkJob(2, "queued")}
	snap := BuildSnapshotEvent(jobs, now)
	if snap.Type != "snapshot" {
		t.Errorf("type=%q want snapshot", snap.Type)
	}
	if len(snap.Jobs) != 2 {
		t.Errorf("want 2 deduped jobs, got %d", len(snap.Jobs))
	}
	if snap.Timestamp != "2026-04-27T00:00:00Z" {
		t.Errorf("timestamp=%q", snap.Timestamp)
	}
}

func TestWatchPlainOptionsModes(t *testing.T) {
	cases := []struct {
		name        string
		opts        WatchPlainOptions
		emitsEvents bool
		mode        SnapshotMode
	}{
		{"default", WatchPlainOptions{}, false, SnapshotText},
		{"transitions", WatchPlainOptions{TransitionsOnly: true}, true, SnapshotNone},
		{"jsonl", WatchPlainOptions{JSONLines: true}, true, SnapshotJSON},
		{"jsonl+transitions", WatchPlainOptions{JSONLines: true, TransitionsOnly: true}, true, SnapshotNone},
		{"until-any-only", WatchPlainOptions{UntilAnyTerminal: true}, true, SnapshotText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.EmitsEvents(); got != tc.emitsEvents {
				t.Errorf("EmitsEvents=%v want %v", got, tc.emitsEvents)
			}
			if got := tc.opts.SnapshotMode(); got != tc.mode {
				t.Errorf("SnapshotMode=%v want %v", got, tc.mode)
			}
		})
	}
}
