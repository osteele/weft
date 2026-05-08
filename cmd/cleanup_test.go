package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestCleanupTerminalLaunchMatches(t *testing.T) {
	cases := []struct {
		name     string
		launch   *db.Launch
		provider string
		want     bool
	}{
		{
			name:     "terminal matching provider",
			launch:   &db.Launch{Status: db.LaunchStatusFailed, Provider: string(cloud.ProviderRunpod), ProviderInstanceID: "pod-123"},
			provider: string(cloud.ProviderRunpod),
			want:     true,
		},
		{
			name:     "terminal other provider",
			launch:   &db.Launch{Status: db.LaunchStatusFailed, Provider: string(cloud.ProviderVastai), ProviderInstanceID: "12345"},
			provider: string(cloud.ProviderRunpod),
			want:     false,
		},
		{
			name:     "running skipped",
			launch:   &db.Launch{Status: db.LaunchStatusRunning, Provider: string(cloud.ProviderRunpod), ProviderInstanceID: "pod-123"},
			provider: string(cloud.ProviderRunpod),
			want:     false,
		},
		{
			name:     "missing provider id skipped",
			launch:   &db.Launch{Status: db.LaunchStatusFailed, Provider: string(cloud.ProviderRunpod)},
			provider: string(cloud.ProviderRunpod),
			want:     false,
		},
		{
			name:     "unknown provider skipped",
			launch:   &db.Launch{Status: db.LaunchStatusFailed, Provider: "mystery", ProviderInstanceID: "123"},
			provider: "",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanupTerminalLaunchMatches(tc.launch, tc.provider)
			if got != tc.want {
				t.Fatalf("cleanupTerminalLaunchMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCleanupProviderInstanceNeedsDelete(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{cloud.ProviderStatusRunning, true},
		{cloud.ProviderStatusStopped, true},
		{cloud.ProviderStatusExited, true},
		{cloud.ProviderStatusDestroyed, false},
		{cloud.ProviderStatusDead, false},
	}
	for _, tc := range cases {
		got := cleanupProviderInstanceNeedsDelete(&cloud.Instance{Status: tc.status})
		if got != tc.want {
			t.Fatalf("cleanupProviderInstanceNeedsDelete(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
