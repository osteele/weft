package cmd

import (
	"strings"
	"testing"
	"time"

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
		Launch: &db.Launch{
			ID:                 106,
			Status:             db.LaunchStatusRunning,
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

func TestFormatObservedActivityFallsBackToDBRunningJob(t *testing.T) {
	instanceID := int64(107)
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 instanceID,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "32712487",
			GPUSpec:            "A100",
		},
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusRunning, LaunchID: &instanceID},
		},
	}, time.Now())

	if activity.Phase != "running job 88 (observed from DB)" {
		t.Fatalf("phase = %q, want DB-running fallback", activity.Phase)
	}
	if activity.Bootstrap != "" {
		t.Fatalf("bootstrap = %q, want empty", activity.Bootstrap)
	}
}

func TestFormatObservedActivityShowsProvisioningFallback(t *testing.T) {
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:       108,
			Status:   db.LaunchStatusLaunching,
			Provider: "vastai",
			GPUSpec:  "A100",
		},
	}, time.Now())

	if activity.Bootstrap != "provisioning instance" {
		t.Fatalf("bootstrap = %q, want provisioning fallback", activity.Bootstrap)
	}
}
