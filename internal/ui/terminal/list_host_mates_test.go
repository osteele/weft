package terminal

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestHostMateKey(t *testing.T) {
	cases := []struct {
		name string
		job  *db.Job
		want string
	}{
		{"nil", nil, ""},
		{"unplaced (no host, no launch)", &db.Job{}, ""},
		{"inventory cool30", &db.Job{Host: "cool30"}, "host:cool30"},
		{"inventory whitespace trimmed", &db.Job{Host: "  cool30  "}, "host:cool30"},
		{"rental with launch id", &db.Job{LaunchID: ptrInt64(42)}, "rental:wi42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostMateKey(tc.job); got != tc.want {
				t.Errorf("hostMateKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostMatesForFlatView(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "cool30"},
		{ID: 3, Host: "cool100"},
		{ID: 4}, // unplaced
		{ID: 5, Host: "cool30"},
		{ID: 6}, // unplaced
	}

	t.Run("cursor on cool30 row matches other cool30 jobs", func(t *testing.T) {
		mates, ok := hostMatesForFlatView(jobs, 0)
		if !ok {
			t.Fatalf("expected mates active")
		}
		if mates[0] {
			t.Errorf("cursor row should not be marked as a mate")
		}
		if !mates[1] || !mates[4] {
			t.Errorf("expected rows 1 and 4 to be mates, got %v", mates)
		}
		if mates[2] {
			t.Errorf("row 2 (cool100) should not be a mate")
		}
		if mates[3] || mates[5] {
			t.Errorf("unplaced rows should not match a placed cursor")
		}
	})

	t.Run("cursor on cool100 has no mates", func(t *testing.T) {
		_, ok := hostMatesForFlatView(jobs, 2)
		if ok {
			t.Errorf("expected no mates for solitary host")
		}
	})

	t.Run("cursor on unplaced job is inactive", func(t *testing.T) {
		_, ok := hostMatesForFlatView(jobs, 3)
		if ok {
			t.Errorf("unplaced cursor should not activate host-mate highlighting")
		}
	})

	t.Run("out-of-range cursor", func(t *testing.T) {
		_, ok := hostMatesForFlatView(jobs, -1)
		if ok {
			t.Errorf("expected inactive for negative cursor")
		}
		_, ok = hostMatesForFlatView(jobs, len(jobs))
		if ok {
			t.Errorf("expected inactive for out-of-range cursor")
		}
	})
}

func TestHostMatesForFlatViewRental(t *testing.T) {
	jobs := []*db.Job{
		{ID: 10, LaunchID: ptrInt64(7)},
		{ID: 11, LaunchID: ptrInt64(7)},
		{ID: 12, LaunchID: ptrInt64(8)},
		{ID: 13, Host: "cool30"},
	}
	mates, ok := hostMatesForFlatView(jobs, 0)
	if !ok {
		t.Fatalf("expected mates active for rental cursor")
	}
	if !mates[1] {
		t.Errorf("expected rental sibling on launch 7 to match")
	}
	if mates[2] {
		t.Errorf("different rental instance should not match")
	}
	if mates[3] {
		t.Errorf("on-prem job should not match a rental")
	}
}

func TestHostMatesForGroupedView(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "cool30"},
		{ID: 3, Host: "cool100"},
	}
	mates, ok := hostMatesForGroupedView(jobs[0], jobs)
	if !ok {
		t.Fatalf("expected mates active")
	}
	if !mates[2] {
		t.Errorf("expected job 2 in mates, got %v", mates)
	}
	if mates[1] {
		t.Errorf("selected job should not be a mate of itself")
	}
	if mates[3] {
		t.Errorf("cool100 job should not match")
	}

	if _, ok := hostMatesForGroupedView(nil, jobs); ok {
		t.Errorf("nil selected should be inactive")
	}
	if _, ok := hostMatesForGroupedView(&db.Job{ID: 99}, jobs); ok {
		t.Errorf("unplaced selected should be inactive")
	}
}

func TestHostMatesForGroupedRowsDistinguishesMoveAttempts(t *testing.T) {
	sourceLaunchID := int64(4817)
	targetLaunchID := int64(4821)
	sourceAttemptID := int64(101)
	targetAttemptID := int64(102)
	rows := []groupedStatusRow{
		{isHeader: true, text: "Queued (1):"},
		{job: &db.Job{ID: 4108, LaunchID: &sourceLaunchID, DisplayAttemptID: sourceAttemptID, DisplayMoveDim: true}},
		{isHeader: true, text: "Placing (2):"},
		{job: &db.Job{ID: 4108, LaunchID: &targetLaunchID, DisplayAttemptID: targetAttemptID}},
		{job: &db.Job{ID: 4109, LaunchID: &targetLaunchID, DisplayAttemptID: 103}},
	}

	mates, ok := hostMatesForGroupedRows(rows, 3)
	if !ok {
		t.Fatalf("expected target launch mates")
	}
	if mates[1] {
		t.Fatalf("source row for same logical job should not be marked as target mate: %v", mates)
	}
	if mates[3] {
		t.Fatalf("selected display row should not mark itself: %v", mates)
	}
	if !mates[4] {
		t.Fatalf("expected other row on target launch to be marked: %v", mates)
	}
}

func TestApplyHostMateMarkerPreservesWidth(t *testing.T) {
	cases := []string{
		"- ▲ wj1586 - markov-attention",
		"  - ▲ wj1590 - role-encoding-injection",
		"x",
	}
	for _, line := range cases {
		got := applyHostMateMarker(line)
		if visualWidth(got) != visualWidth(line) {
			t.Errorf("applyHostMateMarker(%q) width = %d, want %d", line, visualWidth(got), visualWidth(line))
		}
	}
}

func TestApplyHostMateMarkerDoesNotExposeLeadingANSI(t *testing.T) {
	line := "\x1b[38;5;196m- ⌂ wj3256 — dependency-routing\x1b[0m"

	got := applyHostMateMarker(line)
	plain := stripANSI(got)
	if strings.Contains(plain, "[38;5;196m") {
		t.Fatalf("marker overlay exposed ANSI parameters: %q", plain)
	}
	if plain != "▎ ⌂ wj3256 — dependency-routing" {
		t.Fatalf("applyHostMateMarker() = %q, want marker over first visible cell", plain)
	}
	if visualWidth(got) != visualWidth(line) {
		t.Fatalf("applyHostMateMarker() width = %d, want %d", visualWidth(got), visualWidth(line))
	}
}

func TestApplyHostMateMarkerEmpty(t *testing.T) {
	got := applyHostMateMarker("")
	if visualWidth(got) != 1 {
		t.Errorf("empty input should yield 1-cell marker, got width %d", visualWidth(got))
	}
}

func visualWidth(s string) int {
	return lipgloss.Width(s)
}
