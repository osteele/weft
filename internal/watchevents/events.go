package watchevents

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

// DedupeJobsByID flattens any number of job slices and returns one job per
// unique ID. Within a single ID, the last seen non-nil entry wins.
func DedupeJobsByID(slices ...[]*db.Job) []*db.Job {
	by := make(map[int64]*db.Job)
	order := make([]int64, 0)
	for _, s := range slices {
		for _, j := range s {
			if j == nil {
				continue
			}
			if _, seen := by[j.ID]; !seen {
				order = append(order, j.ID)
			}
			by[j.ID] = j
		}
	}
	out := make([]*db.Job, 0, len(order))
	for _, id := range order {
		out = append(out, by[id])
	}
	return out
}

const (
	EventTypeTransition = "transition"
	EventTypeSnapshot   = "snapshot"
)

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

func (e TransitionEvent) IsTerminal() bool {
	return status.IsTerminal(e.Status)
}

type SnapshotJob struct {
	JobID      string `json:"job_id"`
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Host       string `json:"host"`
	Project    string `json:"project"`
	InstanceID *int64 `json:"instance_id"`
	ExitCode   *int   `json:"exit_code"`
}

type SnapshotEvent struct {
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Jobs      []SnapshotJob `json:"jobs"`
}

type TransitionTracker struct {
	prev   map[int64]string
	seeded bool
}

func NewTransitionTracker() *TransitionTracker {
	return &TransitionTracker{prev: make(map[int64]string)}
}

func (t *TransitionTracker) Seeded() bool {
	return t.seeded
}

func (t *TransitionTracker) Diff(jobs []*db.Job, now time.Time) []TransitionEvent {
	cur := make(map[int64]*db.Job, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
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

func BuildSnapshotEvent(jobs []*db.Job, now time.Time) SnapshotEvent {
	out := make([]SnapshotJob, 0, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
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

func RenderTransitionPlain(ev TransitionEvent) string {
	var b strings.Builder
	b.WriteString(ev.JobID)
	b.WriteString("  ")
	if ev.PrevStatus == "" {
		b.WriteString("(new)")
	} else {
		b.WriteString(ev.PrevStatus)
	}
	b.WriteString(" -> ")
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

func EncodeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
