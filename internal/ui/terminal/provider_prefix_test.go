package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestProviderShortPrefix(t *testing.T) {
	tests := map[string]string{
		"vastai": "va",
		"runpod": "rp",
		"":       "",
		"gcp":    "",
		"aws":    "",
	}
	for provider, want := range tests {
		if got := providerShortPrefix(provider); got != want {
			t.Errorf("providerShortPrefix(%q) = %q, want %q", provider, got, want)
		}
	}
}

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

func TestSelectedJobDetail_RentalIncludesProviderPrefix(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(42)
	job := &db.Job{
		ID:        7,
		LaunchID:  &launchID,
		Tags:      []string{"provider:runpod"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-2 * time.Minute).Unix(),
	}
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %v", lines)
	}
	if !strings.Contains(lines[0], "instance rp:wi42") {
		t.Errorf("expected 'instance rp:wi42' in detail line, got: %s", lines[0])
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
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %v", lines)
	}
	if !strings.Contains(lines[0], "instance wi9") {
		t.Errorf("expected 'instance wi9' fallback, got: %s", lines[0])
	}
	if strings.Contains(lines[0], "instance :wi9") || strings.Contains(lines[0], "instance va:") || strings.Contains(lines[0], "instance rp:") {
		t.Errorf("unexpected provider prefix without a provider tag, got: %s", lines[0])
	}
}
