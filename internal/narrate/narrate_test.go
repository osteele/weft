package narrate

import (
	"strings"
	"testing"
	"time"
)

func TestDiffSnapshots_FirstTickIsAllAdds(t *testing.T) {
	next := &Snapshot{
		Time: time.Now(),
		Jobs: map[int64]JobView{
			1: {ID: 1, Status: "running"},
			2: {ID: 2, Status: "queued"},
		},
		Instances: map[int64]InstanceView{
			10: {ID: 10, Status: "running"},
		},
	}
	d := DiffSnapshots(nil, next)
	if len(d.JobAdded) != 2 {
		t.Fatalf("want 2 jobs added, got %d", len(d.JobAdded))
	}
	if len(d.InstAdded) != 1 {
		t.Fatalf("want 1 inst added, got %d", len(d.InstAdded))
	}
	if d.Empty() {
		t.Fatal("delta should not be empty when items added")
	}
}

func TestDiffSnapshots_StatusTransition(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "queued"}},
		Instances: map[int64]InstanceView{},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
	}
	d := DiffSnapshots(prev, next)
	if len(d.JobChanged) != 1 {
		t.Fatalf("want 1 changed, got %d", len(d.JobChanged))
	}
	if d.JobChanged[0].Before.Status != "queued" || d.JobChanged[0].After.Status != "running" {
		t.Fatalf("unexpected change: %+v", d.JobChanged[0])
	}
}

func TestDiffSnapshots_NoChangeIsEmpty(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{1: {ID: 1, Status: "running"}},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	if !DiffSnapshots(prev, next).Empty() {
		t.Fatal("identical snapshots should produce empty delta")
	}
}

func TestDiffSnapshots_InstanceGraceTransition(t *testing.T) {
	deadline := int64(1_000_000_000)
	prev := &Snapshot{
		Instances: map[int64]InstanceView{10: {ID: 10, Status: "running"}},
		Jobs:      map[int64]JobView{},
	}
	next := &Snapshot{
		Instances: map[int64]InstanceView{10: {ID: 10, Status: "grace", GraceDeadline: &deadline}},
		Jobs:      map[int64]JobView{},
	}
	d := DiffSnapshots(prev, next)
	if len(d.InstChanged) != 1 {
		t.Fatalf("want 1 inst changed, got %d", len(d.InstChanged))
	}
	if d.InstChanged[0].Before.Status != "running" || d.InstChanged[0].After.Status != "grace" {
		t.Fatalf("unexpected change: %+v", d.InstChanged[0])
	}
}

func TestDiffSnapshots_AutopilotStateChange(t *testing.T) {
	prev := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "idle"},
	}
	next := &Snapshot{
		Jobs:      map[int64]JobView{},
		Instances: map[int64]InstanceView{},
		Autopilot: AutopilotView{State: "running"},
	}
	d := DiffSnapshots(prev, next)
	if d.Empty() {
		t.Fatal("autopilot state change should produce non-empty delta")
	}
	if d.AutopilotOld.State != "idle" || d.AutopilotNew.State != "running" {
		t.Fatalf("unexpected: %s -> %s", d.AutopilotOld.State, d.AutopilotNew.State)
	}
}

func TestSession_AppendRecap_TriggersCompactionAtThreshold(t *testing.T) {
	s, err := NewSession(SessionOptions{CompactionThreshold: 50})
	if err != nil {
		t.Fatal(err)
	}
	// First small recap: under threshold
	if shouldCompact := s.AppendRecap("short recap"); shouldCompact {
		t.Fatal("should not compact yet")
	}
	// Big recap pushes us over threshold (50 token approximation: ~200 chars)
	big := strings.Repeat("x", 250)
	if shouldCompact := s.AppendRecap(big); !shouldCompact {
		t.Fatal("should have triggered compaction")
	}
}

func TestSession_ReplaceWithCompacted_ResetsTokens(t *testing.T) {
	s, err := NewSession(SessionOptions{CompactionThreshold: 50})
	if err != nil {
		t.Fatal(err)
	}
	s.AppendRecap(strings.Repeat("x", 400))
	if len(s.Recaps()) != 1 {
		t.Fatalf("want 1 recap, got %d", len(s.Recaps()))
	}
	s.ReplaceWithCompacted("compact summary")
	if len(s.Recaps()) != 1 {
		t.Fatalf("compacted should leave one entry, got %d", len(s.Recaps()))
	}
	if s.Recaps()[0].Recap != "compact summary" {
		t.Fatalf("unexpected entry: %q", s.Recaps()[0].Recap)
	}
}

func TestFormatPriorRecap_EmptyAndNonEmpty(t *testing.T) {
	if FormatPriorRecap(nil) != "" {
		t.Fatal("nil entries should produce empty string")
	}
	out := FormatPriorRecap([]recapEntry{
		{At: time.Unix(1700000000, 0).UTC(), Recap: "alpha"},
		{At: time.Unix(1700000060, 0).UTC(), Recap: "beta"},
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("expected both recaps in output: %q", out)
	}
	if !strings.Contains(out, "---") {
		t.Fatalf("expected separator between entries: %q", out)
	}
}

func TestFormatSnapshot_StableJSON(t *testing.T) {
	snap := &Snapshot{
		Time: time.Unix(1700000000, 0).UTC(),
		Jobs: map[int64]JobView{
			2: {ID: 2, Status: "running", Host: "cool30"},
			1: {ID: 1, Status: "queued"},
		},
		Autopilot: AutopilotView{State: "idle"},
	}
	out := FormatSnapshot(snap)
	idxOne := strings.Index(out, `"id":1`)
	idxTwo := strings.Index(out, `"id":2`)
	if idxOne < 0 || idxTwo < 0 || idxOne > idxTwo {
		t.Fatalf("jobs not sorted by id ascending:\n%s", out)
	}
}
