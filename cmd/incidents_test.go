package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
)

// TestCollectIncidentsCoalescesByFingerprint verifies that jobs sharing a
// Structured.Fingerprint coalesce into one IncidentSummary, with Count and
// JobIDs reflecting the cohort. This is the data-plumbing parity check
// against the TUI rollup — both surfaces must agree.
func TestCollectIncidentsCoalescesByFingerprint(t *testing.T) {
	database := db.SetupTestDB(t)

	fingerprint := "vastai/search-offers/400/bad-field:driver_vers"
	mkUnplaced := func(t *testing.T, blocked *blockreason.Structured) int64 {
		t.Helper()
		id, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "job")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobPlacementBlocked(database, id, blocked.Marshal()); err != nil {
			t.Fatalf("SetJobPlacementBlocked: %v", err)
		}
		return id
	}
	id1 := mkUnplaced(t, &blockreason.Structured{
		Summary:     "planner: search offers gpu_ram>=10 …",
		Launch:      "planner: search offers gpu_ram>=10 …",
		Fingerprint: fingerprint,
	})
	id2 := mkUnplaced(t, &blockreason.Structured{
		Summary:     "planner: search offers gpu_ram>=82 …",
		Launch:      "planner: search offers gpu_ram>=82 …",
		Fingerprint: fingerprint,
	})
	id3 := mkUnplaced(t, &blockreason.Structured{
		Summary:     "planner: no offers from providers …",
		Launch:      "planner: no offers from providers …",
		Fingerprint: "vastai/search-offers/empty-result:vram",
	})

	incs, err := collectIncidents(database)
	if err != nil {
		t.Fatalf("collectIncidents: %v", err)
	}
	// The vastai/search-offers/empty-result:vram fingerprint appears on only
	// one job — should NOT surface as an incident.
	for _, inc := range incs {
		if inc.Fingerprint == "vastai/search-offers/empty-result:vram" {
			t.Fatalf("single-job fingerprint surfaced as incident: %+v", inc)
		}
	}
	if len(incs) != 1 {
		t.Fatalf("expected 1 incident (the driver_vers cohort), got %d: %+v", len(incs), incs)
	}
	inc := incs[0]
	if inc.Fingerprint != fingerprint {
		t.Fatalf("Fingerprint = %q, want %q", inc.Fingerprint, fingerprint)
	}
	if inc.Count != 2 {
		t.Fatalf("Count = %d, want 2", inc.Count)
	}
	if len(inc.JobIDs) != 2 {
		t.Fatalf("JobIDs len = %d, want 2", len(inc.JobIDs))
	}
	wantJobs := map[int64]bool{id1: true, id2: true}
	for _, jid := range inc.JobIDs {
		if !wantJobs[jid] {
			t.Fatalf("JobIDs contains %d not in cohort {%d, %d}", jid, id1, id2)
		}
	}
	if _, in := wantJobs[id3]; in == false { /* id3 is supposed to be excluded — ok */
	}
}

// TestCollectIncidentsEmptyWhenNothingClassified verifies the no-fingerprint
// fallback: jobs blocked by unclassified errors don't produce phantom
// incidents.
func TestCollectIncidentsEmptyWhenNothingClassified(t *testing.T) {
	database := db.SetupTestDB(t)
	id, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	// Persist a placement_blocked with no Fingerprint.
	s := &blockreason.Structured{Summary: "planner: something opaque"}
	if err := db.SetJobPlacementBlocked(database, id, s.Marshal()); err != nil {
		t.Fatalf("SetJobPlacementBlocked: %v", err)
	}

	incs, err := collectIncidents(database)
	if err != nil {
		t.Fatalf("collectIncidents: %v", err)
	}
	if len(incs) != 0 {
		t.Fatalf("expected 0 incidents, got %d: %+v", len(incs), incs)
	}
}

// TestTruncateForTable is a tiny guard on the table-formatting truncation so
// the row width stays bounded when an upstream message is unusually long.
func TestTruncateForTable(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 10, ""},
		{"short", 10, "short"},
		{"abcdefghijk", 10, "abcdefghi…"},
		{strings.Repeat("a", 200), 50, strings.Repeat("a", 49) + "…"},
	}
	for _, tc := range cases {
		if got := truncateForTable(tc.in, tc.max); got != tc.want {
			t.Fatalf("truncateForTable(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}
