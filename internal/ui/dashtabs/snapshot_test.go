package dashtabs

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func mockJobs(t *testing.T) []*db.Job {
	t.Helper()
	now := time.Now().Unix()
	end := now - 600
	return []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "host-alpha", StartTime: now - 100},
		{ID: 2, Status: db.StatusRunning, Host: "host-beta", StartTime: now - 60},
		{ID: 3, Status: db.StatusQueued, QueuedAt: now - 10},
		{ID: 4, Status: db.StatusQueued, QueuedAt: now - 5},
		{ID: 5, Status: db.StatusQueued, QueuedAt: now},
		{ID: 6, Status: db.StatusCompleted, Host: "host-alpha", EndTime: &end},
	}
}

func TestPushHistoryRollsForward(t *testing.T) {
	var h RecentHistory
	now := time.Now()
	for i := 0; i < HistoryLen+5; i++ {
		snap := Snapshot{Counts: StatusCounts{Queued: i, Running: 1}, SpendUSDPerHour: float64(i)}
		h = pushHistory(h, now.Add(time.Duration(i)*time.Second), snap, 0)
	}
	if len(h.QueueDepth) != HistoryLen {
		t.Errorf("QueueDepth length: got %d, want %d", len(h.QueueDepth), HistoryLen)
	}
	if h.QueueDepth[0] != HistoryLen+4 {
		t.Errorf("most recent QueueDepth: got %d, want %d", h.QueueDepth[0], HistoryLen+4)
	}
}

func TestExtractExpKey(t *testing.T) {
	cases := []struct {
		j    *db.Job
		want string
	}{
		{&db.Job{Description: "EXP-079 multiseed: dpo_kl_anchored"}, "EXP-079"},
		{&db.Job{Description: "no exp here", Project: "head-type-ontology"}, "(no EXP)"},
		{&db.Job{Command: "run EXP-180 something", Description: ""}, "EXP-180"},
		{&db.Job{}, "(no EXP)"},
	}
	for _, tc := range cases {
		got := extractExpKey(tc.j)
		if got != tc.want {
			t.Errorf("extractExpKey(%+v) = %q, want %q", tc.j, got, tc.want)
		}
	}
}

func TestGroupByProjectThenExp(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Project: "head-type-ontology", Description: "EXP-181: LLaMA-3.1-8B fingerprints"},
		{ID: 2, Project: "head-type-ontology", Description: "EXP-180: Pythia-410m fingerprints"},
		{ID: 3, Project: "head-type-ontology", Description: "EXP-181 followup"},
		{ID: 4, Project: "continuous-thought", Description: "EXP-055: training dynamics"},
		{ID: 5, Project: "continuous-thought", Description: "ad-hoc test, no EXP"},
	}
	out := groupByProjectThenExp(jobs)
	if len(out) != 2 {
		t.Fatalf("projects: got %d, want 2", len(out))
	}
	hto := out["head-type-ontology"]
	if got := len(hto["EXP-181"]); got != 2 {
		t.Errorf("EXP-181 jobs in head-type-ontology: got %d, want 2", got)
	}
	if got := len(hto["EXP-180"]); got != 1 {
		t.Errorf("EXP-180 jobs in head-type-ontology: got %d, want 1", got)
	}
	ct := out["continuous-thought"]
	if got := len(ct["EXP-055"]); got != 1 {
		t.Errorf("EXP-055 jobs in continuous-thought: got %d, want 1", got)
	}
	if got := len(ct[noExpKey]); got != 1 {
		t.Errorf("noExp jobs in continuous-thought: got %d, want 1", got)
	}
}
