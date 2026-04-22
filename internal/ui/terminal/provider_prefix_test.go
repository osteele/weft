package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestFormatJobListHost_RentalShowsProviderPrefix(t *testing.T) {
	launchID := int64(1210)
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"vastai", []string{"provider:vastai"}, "va:wi1210"},
		{"runpod", []string{"provider:runpod"}, "rp:wi1210"},
		{"unknown provider falls back", nil, "wi1210"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &db.Job{LaunchID: &launchID, Tags: tc.tags}
			if got := formatJobListHost(job); got != tc.want {
				t.Errorf("formatJobListHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatJobListHost_InventoryUnchanged(t *testing.T) {
	job := &db.Job{Host: "cool30"}
	// Tag "provider:vastai" is meaningless for an inventory job and must not
	// leak into the HOST column.
	job.Tags = []string{"provider:vastai"}
	if got := formatJobListHost(job); got != "cool30" {
		t.Errorf("formatJobListHost() = %q, want %q", got, "cool30")
	}
}

func TestSelectedJobDetail_RentalHostLineIncludesProvider(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(42)
	job := &db.Job{
		ID:        7,
		LaunchID:  &launchID,
		Tags:      []string{"provider:runpod"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-2 * time.Minute).Unix(),
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 detail lines (Job + Host), got %v", lines)
	}
	hostLine := lines[1]
	for _, want := range []string{"Host: wi42", "@ RunPod"} {
		if !strings.Contains(hostLine, want) {
			t.Errorf("expected %q in Host line, got: %s", want, hostLine)
		}
	}
}

func TestSelectedJobDetail_RentalWithoutProviderFallsBack(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(9)
	job := &db.Job{
		ID:        3,
		LaunchID:  &launchID,
		Status:    db.StatusRunning,
		StartTime: now.Add(-1 * time.Minute).Unix(),
	}
	lines := renderSelectedJobDetail(job, selectedJobContext{}, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 detail lines, got %v", lines)
	}
	hostLine := lines[1]
	if !strings.Contains(hostLine, "wi9") {
		t.Errorf("expected 'wi9' in Host line, got: %s", hostLine)
	}
	if strings.Contains(hostLine, "provider:") {
		t.Errorf("unexpected provider suffix without a provider tag, got: %s", hostLine)
	}
}
