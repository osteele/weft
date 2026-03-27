package slack

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestFormatCampaignMessage_AllSucceeded(t *testing.T) {
	now := int64(1710000000)
	ended := now + 720 // 12 minutes
	launched := now + 60

	campaign := db.Campaign{
		ID:        42,
		Status:    db.CampaignStatusCompleted,
		CreatedAt: now,
		EndedAt:   &ended,
	}

	instances := []*db.Launch{
		{
			ID:              1,
			Status:          db.LaunchStatusCompleted,
			ResolvedGPUName: "RTX 4090",
			GPUSpec:         "nvidia>=24GB",
			CreatedAt:       now,
			LaunchedAt:      &launched,
			EndedAt:         &ended,
		},
		{
			ID:              2,
			Status:          db.LaunchStatusCompleted,
			ResolvedGPUName: "A100 80GB",
			GPUSpec:         "nvidia>=80GB",
			CreatedAt:       now,
			LaunchedAt:      &launched,
			EndedAt:         &ended,
		},
	}

	msg := FormatCampaignMessage(campaign, instances)

	if !strings.Contains(msg, "Campaign 42 completed") {
		t.Errorf("expected campaign header, got: %s", msg)
	}
	if !strings.Contains(msg, "2 succeeded") {
		t.Errorf("expected '2 succeeded', got: %s", msg)
	}
	if !strings.Contains(msg, "12m elapsed") {
		t.Errorf("expected '12m elapsed', got: %s", msg)
	}
	if !strings.Contains(msg, "✓ instance 1 on RTX 4090") {
		t.Errorf("expected instance 1 line, got: %s", msg)
	}
	if !strings.Contains(msg, "✓ instance 2 on A100 80GB") {
		t.Errorf("expected instance 2 line, got: %s", msg)
	}
}

func TestFormatCampaignMessage_Mixed(t *testing.T) {
	now := int64(1710000000)
	ended := now + 180

	campaign := db.Campaign{
		ID:        7,
		Status:    db.CampaignStatusCompleted,
		CreatedAt: now,
		EndedAt:   &ended,
	}

	failEnded := now + 120
	instances := []*db.Launch{
		{
			ID:              10,
			Status:          db.LaunchStatusCompleted,
			ResolvedGPUName: "RTX 4090",
			CreatedAt:       now,
			EndedAt:         &ended,
		},
		{
			ID:              11,
			Status:          db.LaunchStatusFailed,
			ResolvedGPUName: "RTX 3090",
			CreatedAt:       now,
			EndedAt:         &failEnded,
		},
	}

	msg := FormatCampaignMessage(campaign, instances)

	if !strings.Contains(msg, "1 succeeded") {
		t.Errorf("expected '1 succeeded', got: %s", msg)
	}
	if !strings.Contains(msg, "1 failed") {
		t.Errorf("expected '1 failed', got: %s", msg)
	}
	if !strings.Contains(msg, "✗ instance 11") {
		t.Errorf("expected failed icon for instance 11, got: %s", msg)
	}
}

func TestFormatCampaignMessage_AllFailed(t *testing.T) {
	now := int64(1710000000)
	ended := now + 60

	campaign := db.Campaign{
		ID:        3,
		Status:    db.CampaignStatusFailed,
		CreatedAt: now,
		EndedAt:   &ended,
	}

	instances := []*db.Launch{
		{
			ID:        20,
			Status:    db.LaunchStatusFailed,
			GPUClass:  "A100",
			CreatedAt: now,
			EndedAt:   &ended,
		},
	}

	msg := FormatCampaignMessage(campaign, instances)

	if !strings.Contains(msg, "Campaign 3 failed") {
		t.Errorf("expected 'Campaign 3 failed', got: %s", msg)
	}
	if !strings.Contains(msg, "1 failed") {
		t.Errorf("expected '1 failed', got: %s", msg)
	}
}

func TestTruncateTail(t *testing.T) {
	short := "hello world"
	if got := truncateTail(short, 100); got != short {
		t.Errorf("short string should be unchanged, got: %q", got)
	}

	long := "line1\nline2\nline3\nline4\nline5"
	got := truncateTail(long, 15)
	if !strings.HasPrefix(got, "...\n") {
		t.Errorf("truncated string should start with '...\\n', got: %q", got)
	}
	if len(got) > 20 {
		t.Errorf("truncated string too long: %d bytes", len(got))
	}
}

func TestSummarizeJobLogs_NoKey(t *testing.T) {
	// With no API key configured, summarizeJobLogs should return empty string
	t.Setenv("ANTHROPIC_API_KEY", "")
	logs := map[int64]string{1: "some output"}
	if got := summarizeJobLogs(logs); got != "" {
		t.Errorf("expected empty summary without API key, got: %q", got)
	}
}

func TestSummarizeJobLogs_EmptyLogs(t *testing.T) {
	if got := summarizeJobLogs(nil); got != "" {
		t.Errorf("expected empty summary for nil logs, got: %q", got)
	}
	if got := summarizeJobLogs(map[int64]string{}); got != "" {
		t.Errorf("expected empty summary for empty logs, got: %q", got)
	}
}
