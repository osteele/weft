package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestFormatUploadSummary_IgnoresLegacyWorkspaceUploadBytes(t *testing.T) {
	timings := &db.JobPhaseTimings{
		UploadWorkspaceBytes: watchTestInt64Ptr(4096),
		UploadResultsBytes:   watchTestInt64Ptr(8192),
	}

	got := formatUploadSummary(timings)
	if strings.Contains(got, "outputs") {
		t.Fatalf("formatUploadSummary() = %q, should not report legacy workspace bytes as outputs", got)
	}
	if !strings.Contains(got, "logs 8.0 KiB") {
		t.Fatalf("formatUploadSummary() = %q, want logs summary", got)
	}
}

func TestFormatWatchInstanceBlock_HidesStaleUploadSummaryForQueuedRetry(t *testing.T) {
	update := campaign.InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:                 106,
			Status:             db.CloudInstanceStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "32712486",
			GPUSpec:            "A100",
		},
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusQueued, WorkingDir: "/workspace/project-alpha", Description: "train model"},
		},
		JobPhaseTimings: map[int64]*db.JobPhaseTimings{
			88: {
				UploadWorkspaceBytes:  watchTestInt64Ptr(4096),
				OutputUploadFiles:     watchTestIntPtr(2),
				OutputUploadDuration:  watchTestInt64Ptr(250),
				UploadResultsBytes:    watchTestInt64Ptr(8192),
				ResultsUploadFiles:    watchTestIntPtr(4),
				ResultsUploadDuration: watchTestInt64Ptr(600),
			},
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	if strings.Contains(out, "uploads:") {
		t.Fatalf("formatWatchInstanceBlock() should hide stale upload summaries for queued jobs, got:\n%s", out)
	}
}
