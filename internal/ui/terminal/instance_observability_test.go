package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
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

	if activity.Phase != "running job wj88 (observed from DB)" {
		t.Fatalf("phase = %q, want DB-running fallback", activity.Phase)
	}
	if activity.Bootstrap != "" {
		t.Fatalf("bootstrap = %q, want empty", activity.Bootstrap)
	}
}

func TestFormatObservedActivityReconcilesStoredPhaseWithDBJobs(t *testing.T) {
	instanceID := int64(107)
	runningJob := &db.Job{ID: 88, Status: db.StatusRunning, LaunchID: &instanceID}
	tests := []struct {
		name  string
		phase string
		want  string
	}{
		{
			name:  "empty uses DB running job",
			phase: "",
			want:  "running job wj88 (observed from DB)",
		},
		{
			name:  "ready uses DB running job",
			phase: "ready",
			want:  "running job wj88 (observed from DB)",
		},
		{
			name:  "setup for running job promotes to running",
			phase: "setup:88",
			want:  "running job wj88 (observed from DB)",
		},
		{
			name:  "running phase stays job scoped",
			phase: "running:88",
			want:  "running job 88",
		},
		{
			name:  "disk full is not masked by running job",
			phase: campaign.PhaseDiskFull,
			want:  "disk full",
		},
		{
			name:  "grace is not masked by running job",
			phase: campaign.PhaseGrace,
			want:  "grace period",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity := formatObservedActivity(campaign.InstanceUpdate{
				Launch: &db.Launch{
					ID:                 instanceID,
					Status:             db.LaunchStatusRunning,
					Provider:           "vastai",
					ProviderInstanceID: "32712487",
					GPUSpec:            "A100",
				},
				InstancePhase: tt.phase,
				Jobs:          []*db.Job{runningJob},
			}, time.Now())

			if activity.Phase != tt.want {
				t.Fatalf("phase = %q, want %q", activity.Phase, tt.want)
			}
			if activity.Bootstrap != "" {
				t.Fatalf("bootstrap = %q, want empty", activity.Bootstrap)
			}
		})
	}
}

func TestFormatBootstrapWaitingShowsElapsedTime(t *testing.T) {
	launchedAt := time.Now().Add(-45 * time.Second).Unix()
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 109,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "12345",
			GPUSpec:            "A100",
			LaunchedAt:         &launchedAt,
		},
	}, time.Now())

	if !strings.Contains(activity.Bootstrap, "waiting for bootstrap activity") {
		t.Fatalf("bootstrap = %q, want base message", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "elapsed") {
		t.Fatalf("bootstrap = %q, want elapsed time", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "terminate in") {
		t.Fatalf("bootstrap = %q, want termination countdown", activity.Bootstrap)
	}
}

func TestFormatObservedActivityShowsRunpodSSHReadinessWait(t *testing.T) {
	now := time.Now()
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 112,
			Status:             db.LaunchStatusLaunching,
			Provider:           "runpod",
			ProviderInstanceID: "pod-123",
			GPUSpec:            "RTX A6000",
		},
		InstancePhase:  "waiting for RunPod SSH readiness",
		PhaseChangedAt: &now,
	}, now.Add(2*time.Minute))

	if !strings.Contains(activity.Phase, "waiting for RunPod SSH readiness") {
		t.Fatalf("phase = %q, want RunPod SSH readiness message", activity.Phase)
	}
	if !strings.Contains(activity.Phase, "for 2m0s") {
		t.Fatalf("phase = %q, want duration", activity.Phase)
	}
	if activity.Bootstrap != "" {
		t.Fatalf("bootstrap = %q, want empty", activity.Bootstrap)
	}
}

func TestFormatBootstrapWaitingShowsConditionalEstimate(t *testing.T) {
	launchedAt := time.Now().Add(-45 * time.Second).Unix()
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 110,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "12345",
			GPUSpec:            "A100",
			LaunchedAt:         &launchedAt,
		},
		BootstrapDurations: db.BootstrapDurations{
			30 * time.Second, 60 * time.Second, 90 * time.Second,
			120 * time.Second, 150 * time.Second,
		},
	}, time.Now())

	if !strings.Contains(activity.Bootstrap, "elapsed") {
		t.Fatalf("bootstrap = %q, want elapsed time", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "remaining") {
		t.Fatalf("bootstrap = %q, want remaining estimate", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "terminate in") {
		t.Fatalf("bootstrap = %q, want termination countdown", activity.Bootstrap)
	}
}

func TestFormatBootstrapWaitingNoEstimateWhenPastAllDurations(t *testing.T) {
	launchedAt := time.Now().Add(-200 * time.Second).Unix()
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 111,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "12345",
			GPUSpec:            "A100",
			LaunchedAt:         &launchedAt,
		},
		BootstrapDurations: db.BootstrapDurations{
			30 * time.Second, 60 * time.Second, 90 * time.Second,
		},
	}, time.Now())

	if !strings.Contains(activity.Bootstrap, "elapsed") {
		t.Fatalf("bootstrap = %q, want elapsed time", activity.Bootstrap)
	}
	if strings.Contains(activity.Bootstrap, "remaining") {
		t.Fatalf("bootstrap = %q, should not show remaining when past all durations", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "terminate in") {
		t.Fatalf("bootstrap = %q, want termination countdown", activity.Bootstrap)
	}
}

func TestFormatBootstrapWaitingUsesBootstrapOrigin(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	providerRunningAt := time.Now().Add(-2 * time.Minute).Unix()
	activity := formatObservedActivity(campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 211,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "12345",
			GPUSpec:            "A100",
			LaunchedAt:         &launchedAt,
			ProviderRunningAt:  &providerRunningAt,
		},
	}, time.Now())

	if strings.Contains(activity.Bootstrap, "30m") {
		t.Fatalf("bootstrap = %q, should use provider running origin instead of launched_at", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "2m") {
		t.Fatalf("bootstrap = %q, want elapsed near 2m from bootstrap origin", activity.Bootstrap)
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

func TestFormatWatchInstanceBlock_UploadsVisibility(t *testing.T) {
	tests := []struct {
		name          string
		launchStatus  string
		instancePhase string
		wantUploads   bool
	}{
		{"shown while uploading", db.LaunchStatusRunning, "uploading-results:88", true},
		{"hidden after completion", db.LaunchStatusCompleted, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			update := campaign.InstanceUpdate{
				Launch: &db.Launch{
					ID:       110,
					Status:   tt.launchStatus,
					Provider: "vastai",
					GPUSpec:  "A100",
				},
				InstancePhase: tt.instancePhase,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusCompleted, WorkingDir: "/workspace/project", Description: "train model"},
				},
				JobAttemptOutcomes: map[int64]string{88: db.StatusCompleted},
				JobPhaseTimings: map[int64]*db.JobPhaseTimings{
					88: {
						UploadResultsBytes:    watchTestInt64Ptr(8192),
						ResultsUploadFiles:    watchTestIntPtr(4),
						ResultsUploadDuration: watchTestInt64Ptr(600),
					},
				},
			}

			out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
			hasUploads := strings.Contains(out, "uploads:")
			if hasUploads != tt.wantUploads {
				t.Fatalf("uploads visible=%v, want %v; output:\n%s", hasUploads, tt.wantUploads, out)
			}
		})
	}
}

func TestFormatObservedActivity_ProviderExited_StopsTimer(t *testing.T) {
	launchedAt := time.Now().Add(-3 * time.Minute).Unix()
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "12345",
			LaunchedAt:         &launchedAt,
		},
		Instance: &cloud.Instance{Status: cloud.ProviderStatusExited},
	}
	activity := formatObservedActivity(update, time.Now())
	if activity.Bootstrap == "" {
		t.Fatal("expected bootstrap message, got empty")
	}
	if strings.Contains(activity.Bootstrap, "elapsed") {
		t.Errorf("expected no ticking timer when provider exited, got: %s", activity.Bootstrap)
	}
	if !strings.Contains(activity.Bootstrap, "provider exited") {
		t.Errorf("expected provider status in message, got: %s", activity.Bootstrap)
	}
}

func TestFormatObservedActivity_HidesPhaseForTerminalLaunch(t *testing.T) {
	now := time.Unix(5_000, 0)
	phaseChangedAt := now.Add(-2 * time.Minute)
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:     220,
			Status: db.LaunchStatusFailed,
		},
		InstancePhase:  "setup:733",
		PhaseChangedAt: &phaseChangedAt,
	}

	activity := formatObservedActivity(update, now)
	if activity.Phase != "" {
		t.Fatalf("phase = %q, want empty for terminal launch", activity.Phase)
	}
}
