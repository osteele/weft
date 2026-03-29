package campaign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
)

func TestFormatPlainUpdate_Initial(t *testing.T) {
	prev := InstanceUpdate{}
	curr := InstanceUpdate{
		Launch: &db.Launch{
			ID:               5,
			Status:           db.LaunchStatusLaunching,
			VastaiInstanceID: "12345678",
		},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "instance 5") {
		t.Errorf("should contain instance ID, got %q", output)
	}
	if !strings.Contains(output, "status=launching") {
		t.Errorf("should contain status, got %q", output)
	}
	if !strings.Contains(output, "provider_id=12345678") {
		t.Errorf("should contain provider instance ID, got %q", output)
	}
}

func TestFormatPlainUpdate_WithSSH(t *testing.T) {
	prev := InstanceUpdate{}
	curr := InstanceUpdate{
		Launch: &db.Launch{
			ID:               5,
			Status:           db.LaunchStatusRunning,
			VastaiInstanceID: "12345678",
		},
		Instance: &cloud.Instance{
			ProviderID: "12345678",
			SSHHost:    "ssh6.vast.ai",
			SSHPort:    34567,
		},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "ssh=") {
		t.Errorf("should contain SSH command, got %q", output)
	}
}

func TestFormatPlainUpdate_JobStatusChange(t *testing.T) {
	prev := InstanceUpdate{
		Launch: &db.Launch{ID: 5, Status: db.LaunchStatusRunning},
		Jobs:   []*db.Job{{ID: 88, Status: db.StatusQueued}},
	}
	curr := InstanceUpdate{
		Launch: &db.Launch{ID: 5, Status: db.LaunchStatusRunning},
		Jobs:   []*db.Job{{ID: 88, Status: db.StatusRunning, WorkingDir: "/work/alpha"}},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "job 88 status=running") {
		t.Errorf("should contain job status change, got %q", output)
	}
	if !strings.Contains(output, "dir=alpha") {
		t.Errorf("should contain job directory tail, got %q", output)
	}
}

func TestFormatPlainUpdate_NoChange(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusRunning}
	jobs := []*db.Job{{ID: 88, Status: db.StatusRunning}}

	prev := InstanceUpdate{Launch: ci, Jobs: jobs}
	curr := InstanceUpdate{Launch: ci, Jobs: jobs}

	output := FormatPlainUpdate(prev, curr)
	if output != "" {
		t.Errorf("no change should produce empty output, got %q", output)
	}
}

func TestInstancePhaseLabel(t *testing.T) {
	tests := []struct {
		phase string
		want  string
	}{
		{"setup:42", "setup (job 42)"},
		{"running:123", "running job 123"},
		{"finalizing:7", "finalizing job 7"},
		{"uploading:7", "uploading outputs (job 7)"},
		{"uploading-results:7", "uploading logs/results (job 7)"},
		{"disk-full:7", "disk full (job 7)"},
		{"grace", "grace period"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		if got := InstancePhaseLabel(tt.phase); got != tt.want {
			t.Errorf("InstancePhaseLabel(%q) = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestJobAttemptProgressKey_UsesLatestRunID(t *testing.T) {
	runID := int64(77)
	jobs := []*db.Job{
		{ID: 42, LatestRunID: &runID},
	}

	got := jobAttemptProgressKey(42, jobs)
	want := "jobs/42/runs/77/progress"
	if got != want {
		t.Fatalf("jobAttemptProgressKey() = %q, want %q", got, want)
	}
}

func TestJobAttemptProgressKey_FallsBackToJobScopedOnlyWithoutRun(t *testing.T) {
	got := jobAttemptProgressKey(42, []*db.Job{{ID: 42}})
	want := "jobs/42/progress"
	if got != want {
		t.Fatalf("jobAttemptProgressKey() = %q, want %q", got, want)
	}
}

func TestInferInitialPhaseChangedAt_RunningUsesJobStartTime(t *testing.T) {
	startTime := time.Now().Add(-45 * time.Second).Unix()
	got := inferInitialPhaseChangedAt(
		"running:42",
		nil,
		[]*db.Job{{ID: 42, StartTime: startTime}},
		nil,
	)
	if got == nil {
		t.Fatal("expected non-nil phase start")
	}
	if got.Unix() != startTime {
		t.Fatalf("phase start = %d, want %d", got.Unix(), startTime)
	}
}

func TestInferInitialPhaseChangedAt_UnknownUploadPhaseReturnsNil(t *testing.T) {
	got := inferInitialPhaseChangedAt(
		"uploading-results:42",
		nil,
		[]*db.Job{{ID: 42, StartTime: time.Now().Unix()}},
		nil,
	)
	if got != nil {
		t.Fatalf("phase start = %v, want nil", got)
	}
}

func TestInferInitialPhaseChangedAt_ClampsToLaunchedAt(t *testing.T) {
	launchedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	staleSetupStart := launchedAt.Add(-108 * time.Hour).Unix()
	ci := &db.Launch{LaunchedAt: phaseTestInt64Ptr(launchedAt.Unix())}

	got := inferInitialPhaseChangedAt(
		"setup:42",
		ci,
		[]*db.Job{{ID: 42}},
		map[int64]*db.JobPhaseTimings{
			42: {JobID: 42, SetupStart: phaseTestInt64Ptr(staleSetupStart)},
		},
	)
	if got == nil {
		t.Fatal("expected non-nil phase start")
	}
	if !got.Equal(launchedAt) {
		t.Fatalf("phase start = %v, want launched_at %v", got, launchedAt)
	}
}

func TestInferInitialPhaseChangedAt_PreservesCurrentAttemptSetupStart(t *testing.T) {
	launchedAt := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	setupStart := launchedAt.Add(90 * time.Second)
	ci := &db.Launch{LaunchedAt: phaseTestInt64Ptr(launchedAt.Unix())}

	got := inferInitialPhaseChangedAt(
		"setup:42",
		ci,
		[]*db.Job{{ID: 42}},
		map[int64]*db.JobPhaseTimings{
			42: {JobID: 42, SetupStart: phaseTestInt64Ptr(setupStart.Unix())},
		},
	)
	if got == nil {
		t.Fatal("expected non-nil phase start")
	}
	if !got.Equal(setupStart) {
		t.Fatalf("phase start = %v, want setup_start %v", got, setupStart)
	}
}

func TestInferInitialPhaseChangedAt_FallsBackToReadyAtThenCreatedAt(t *testing.T) {
	readyAt := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	staleSetupStart := readyAt.Add(-2 * time.Hour).Unix()
	gotReady := inferInitialPhaseChangedAt(
		"setup:42",
		&db.Launch{ReadyAt: phaseTestInt64Ptr(readyAt.Unix())},
		[]*db.Job{{ID: 42}},
		map[int64]*db.JobPhaseTimings{
			42: {JobID: 42, SetupStart: phaseTestInt64Ptr(staleSetupStart)},
		},
	)
	if gotReady == nil || !gotReady.Equal(readyAt) {
		t.Fatalf("phase start with ready_at = %v, want %v", gotReady, readyAt)
	}

	createdAt := time.Now().Add(-4 * time.Minute).Truncate(time.Second)
	gotCreated := inferInitialPhaseChangedAt(
		"setup:42",
		&db.Launch{CreatedAt: createdAt.Unix()},
		[]*db.Job{{ID: 42}},
		map[int64]*db.JobPhaseTimings{
			42: {JobID: 42, SetupStart: phaseTestInt64Ptr(staleSetupStart)},
		},
	)
	if gotCreated == nil || !gotCreated.Equal(createdAt) {
		t.Fatalf("phase start with created_at = %v, want %v", gotCreated, createdAt)
	}
}

func phaseTestInt64Ptr(v int64) *int64 {
	return &v
}

func TestFailureTerminationReasonFromPhase(t *testing.T) {
	if got := failureTerminationReasonFromPhase("disk-full:42", db.TerminationReasonProviderFailure); got != db.TerminationReasonDiskFull {
		t.Fatalf("failureTerminationReasonFromPhase(disk-full:42) = %q, want %q", got, db.TerminationReasonDiskFull)
	}
	if got := failureTerminationReasonFromPhase("running:42", db.TerminationReasonProviderFailure); got != db.TerminationReasonProviderFailure {
		t.Fatalf("failureTerminationReasonFromPhase(running:42) = %q, want %q", got, db.TerminationReasonProviderFailure)
	}
}

func TestFormatPlainUpdate_PhaseChange(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusRunning}
	prev := InstanceUpdate{Launch: ci, InstancePhase: "setup:42"}
	changedAt := time.Now().Add(-5 * time.Second)
	curr := InstanceUpdate{Launch: ci, InstancePhase: "running:42", PhaseChangedAt: &changedAt}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "phase: running job 42 (for") {
		t.Errorf("should contain phase change, got %q", output)
	}
}

func TestFormatPlainUpdate_TerminationDetail(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusFailed}
	prev := InstanceUpdate{Launch: ci}
	curr := InstanceUpdate{
		Launch: ci,
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:  db.LaunchStatusFailed,
			RequestedAtUnix: time.Now().Add(-3 * time.Second).Unix(),
			DestroyAttempts: 2,
			LastError:       "exit status 22",
		},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "cleanup detail:") {
		t.Fatalf("expected cleanup detail, got %q", output)
	}
	if !strings.Contains(output, "attempts=2") {
		t.Fatalf("expected attempt count, got %q", output)
	}
}

func TestTerminationIntentDetail_HidesCompletedCleanup(t *testing.T) {
	marker := &instanceintent.Marker{
		TerminalStatus:         db.LaunchStatusCompleted,
		RequestedAtUnix:        time.Now().Add(-5 * time.Second).Unix(),
		DestroyStartedAtUnix:   time.Now().Add(-4 * time.Second).Unix(),
		DestroySucceededAtUnix: time.Now().Add(-3 * time.Second).Unix(),
	}
	if got := TerminationIntentLabel(marker); got != "" {
		t.Fatalf("TerminationIntentLabel() = %q, want empty", got)
	}
	if got := TerminationIntentDetail(marker); got != "" {
		t.Fatalf("TerminationIntentDetail() = %q, want empty", got)
	}
}

func TestFormatPlainUpdate_BootstrapStallWarning(t *testing.T) {
	ci := &db.Launch{ID: 7, Status: db.LaunchStatusRunning}
	prev := InstanceUpdate{Launch: ci}
	curr := InstanceUpdate{
		Launch:       ci,
		StallMessage: "bootstrap stalled — no activity after 15m0s",
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "WARNING") {
		t.Errorf("should contain WARNING, got %q", output)
	}
	if !strings.Contains(output, "bootstrap stalled") {
		t.Errorf("should contain stall message, got %q", output)
	}
}

func TestFormatPlainUpdate_BootstrapStallTerminate(t *testing.T) {
	ci := &db.Launch{ID: 7, Status: db.LaunchStatusFailed}
	prev := InstanceUpdate{Launch: &db.Launch{ID: 7, Status: db.LaunchStatusRunning}}
	curr := InstanceUpdate{
		Launch:       ci,
		StallMessage: "bootstrap timeout after 20m0s — terminating instance, jobs reset to queued",
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "terminating instance") {
		t.Errorf("should contain termination message, got %q", output)
	}
	if !strings.Contains(output, "status=failed") {
		t.Errorf("should contain failed status, got %q", output)
	}
}

func TestFormatPlainUpdate_BootstrapStallNoRepeat(t *testing.T) {
	ci := &db.Launch{ID: 7, Status: db.LaunchStatusRunning}
	msg := "bootstrap stalled — no activity after 15m0s"
	prev := InstanceUpdate{Launch: ci, StallMessage: msg}
	curr := InstanceUpdate{Launch: ci, StallMessage: msg}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "WARNING") {
		t.Errorf("should not repeat same stall warning, got %q", output)
	}
}

func TestWatchInstance_BootstrapTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a cloud instance, then set launched_at and provider_instance_id
	// (CreateLaunch doesn't persist these fields)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	// Set launched_at to 25 minutes ago (beyond terminate threshold) and provider_instance_id
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	_, err = database.Exec(`UPDATE launches SET launched_at = ?, provider_instance_id = ? WHERE id = ?`,
		launchedAt, "test-123", instanceID)
	if err != nil {
		t.Fatalf("update launched_at: %v", err)
	}

	// Create a queued job assigned to this instance
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	var destroyed bool
	mockClient := &cloud.MockClient{
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyed = true
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := WatchInstance(ctx, mockClient, database, instanceID, 100*time.Millisecond, 100*time.Millisecond)

	var gotStallMsg bool
	var gotTerminateMsg bool
	for update := range ch {
		if update.StallMessage != "" {
			gotStallMsg = true
		}
		if strings.Contains(update.StallMessage, "terminating instance") {
			gotTerminateMsg = true
		}
	}

	if !gotStallMsg {
		t.Error("expected StallMessage to be set")
	}
	if !gotTerminateMsg {
		t.Error("expected termination message")
	}
	if !destroyed {
		t.Error("expected DestroyInstance to be called")
	}

	// Verify instance was marked as failed in DB
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
}

func TestWatchInstance_GraceExpiration(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a grace-period instance with an expired deadline
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusGrace,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "grace-test-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Set grace deadline in the past
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	if err := db.SetLaunchGraceStarted(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set grace started: %v", err)
	}

	// Create a queued job assigned to this instance
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	var destroyed bool
	mockClient := &cloud.MockClient{
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyed = true
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := WatchInstance(ctx, mockClient, database, instanceID, 100*time.Millisecond, 100*time.Millisecond)

	var gotGraceExpiredMsg bool
	for update := range ch {
		if strings.Contains(update.StallMessage, "grace period expired") {
			gotGraceExpiredMsg = true
		}
	}

	if !gotGraceExpiredMsg {
		t.Error("expected grace period expired stall message")
	}
	if !destroyed {
		t.Error("expected DestroyInstance to be called for expired grace instance")
	}

	// Verify instance was marked as failed in DB
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
}

func TestWatchInstance_ShowInstanceErrorDoesNotMarkFailed(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	_, err = database.Exec(`UPDATE launches SET provider_instance_id = ? WHERE id = ?`,
		"test-err", instanceID)
	if err != nil {
		t.Fatalf("update provider_instance_id: %v", err)
	}

	var showCalls int
	mockClient := &cloud.MockClient{
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			showCalls++
			return nil, errors.New("transient API failure")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	ch := WatchInstance(ctx, mockClient, database, instanceID, 20*time.Millisecond, 20*time.Millisecond)
	for range ch {
	}

	if showCalls == 0 {
		t.Fatal("expected ShowInstance to be called at least once")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
}

func TestWatchInstance_UsesLivePhaseBeforeJobLeavesQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	prevFetchIntent := fetchReconcileTerminationIntent
	prevFetchPhase := fetchWatchInstancePhase
	prevFetchBootstrap := fetchWatchBootstrapStage
	prevFetchHeartbeat := fetchWatchHeartbeat
	prevFetchProgress := fetchWatchJobProgress
	t.Cleanup(func() {
		fetchReconcileTerminationIntent = prevFetchIntent
		fetchWatchInstancePhase = prevFetchPhase
		fetchWatchBootstrapStage = prevFetchBootstrap
		fetchWatchHeartbeat = prevFetchHeartbeat
		fetchWatchJobProgress = prevFetchProgress
	})

	fetchReconcileTerminationIntent = func(context.Context, *r2.Client, int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	phaseCalls := 0
	bootstrapCalls := 0
	fetchWatchInstancePhase = func(context.Context, *r2.Client, int64) string {
		phaseCalls++
		return "setup:1"
	}
	fetchWatchBootstrapStage = func(context.Context, *r2.Client, int64) string {
		bootstrapCalls++
		return "starting_jobs"
	}
	fetchWatchHeartbeat = func(context.Context, *r2.Client, int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	fetchWatchJobProgress = func(context.Context, *r2.Client, string, []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchInstance(ctx, &cloud.MockClient{}, database, instanceID, 10*time.Millisecond, time.Hour, &r2.Client{})
	update := <-ch

	if update.InstancePhase != "setup:1" {
		t.Fatalf("InstancePhase = %q, want %q", update.InstancePhase, "setup:1")
	}
	if update.BootstrapStage != "" {
		t.Fatalf("BootstrapStage = %q, want empty string", update.BootstrapStage)
	}
	if phaseCalls == 0 {
		t.Fatal("expected instance phase fetch to be called")
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap stage fetch count = %d, want 0", bootstrapCalls)
	}
}

func TestFormatPlainUpdate_ProgressChange(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusRunning}
	prev := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: -1}
	curr := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "job 42 progress: 50%") {
		t.Errorf("should contain progress update, got %q", output)
	}
}

func TestFormatPlainUpdate_ProgressNoChange(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusRunning}
	prev := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}
	curr := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "progress") {
		t.Errorf("should not report unchanged progress, got %q", output)
	}
}

func TestFormatPlainUpdate_ProgressNoReport(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusRunning}
	prev := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: -1}
	curr := InstanceUpdate{Launch: ci, InstancePhase: "running:42", JobProgress: -1}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "progress") {
		t.Errorf("should not report when no progress, got %q", output)
	}
}

func TestAttemptDisplayStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		outcomes map[int64]string
		want     string
	}{
		{
			name:     "no outcomes, returns job status",
			status:   db.StatusRunning,
			outcomes: nil,
			want:     db.StatusRunning,
		},
		{
			name:     "queued with orphaned outcome shows outcome",
			status:   db.StatusQueued,
			outcomes: map[int64]string{1: db.AttemptOutcomeOrphaned},
			want:     db.AttemptOutcomeOrphaned,
		},
		{
			name:     "queued with failed outcome shows outcome",
			status:   db.StatusQueued,
			outcomes: map[int64]string{1: db.AttemptOutcomeFailed},
			want:     db.AttemptOutcomeFailed,
		},
		{
			name:     "running with outcome shows outcome",
			status:   db.StatusRunning,
			outcomes: map[int64]string{1: db.AttemptOutcomeFailed},
			want:     db.AttemptOutcomeFailed,
		},
		{
			name:     "queued with no matching outcome shows queued",
			status:   db.StatusQueued,
			outcomes: map[int64]string{99: db.AttemptOutcomeFailed},
			want:     db.StatusQueued,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := &db.Job{ID: 1, Status: tt.status}
			got := AttemptDisplayStatus(j, tt.outcomes)
			if got != tt.want {
				t.Errorf("AttemptDisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatPlainUpdate_AttemptDisplayStatusUsed(t *testing.T) {
	ci := &db.Launch{ID: 5, Status: db.LaunchStatusFailed}
	prev := InstanceUpdate{
		Launch: &db.Launch{ID: 5, Status: db.LaunchStatusRunning},
		Jobs:   []*db.Job{{ID: 88, Status: db.StatusRunning}},
	}
	curr := InstanceUpdate{
		Launch:             ci,
		Jobs:               []*db.Job{{ID: 88, Status: db.StatusQueued}},
		JobAttemptOutcomes: map[int64]string{88: db.AttemptOutcomeOrphaned},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "job 88 status=orphaned") {
		t.Errorf("should show attempt outcome instead of queued, got %q", output)
	}
}

func TestIsJobTerminal(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{db.StatusCompleted, true},
		{db.StatusFailed, true},
		{db.AttemptOutcomeOrphaned, true},
		{db.AttemptOutcomeCancelled, true},
		{db.StatusQueued, false},
		{db.StatusRunning, false},
	}
	for _, tt := range tests {
		if got := IsJobTerminal(tt.status); got != tt.want {
			t.Errorf("IsJobTerminal(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestIsInstanceTerminal(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{db.LaunchStatusCompleted, true},
		{db.LaunchStatusFailed, true},
		{db.LaunchStatusCancelled, true},
		{db.LaunchStatusRunning, false},
		{db.LaunchStatusLaunching, false},
		{db.LaunchStatusPlanned, false},
	}
	for _, tt := range tests {
		if got := IsInstanceTerminal(tt.status); got != tt.want {
			t.Errorf("IsInstanceTerminal(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestWatchInstance_TransitionsQueuedJobToRunningFromR2Phase(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Verify job starts as queued
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("initial status = %q, want %q", job.Status, db.StatusQueued)
	}

	prevFetchIntent := fetchReconcileTerminationIntent
	prevFetchPhase := fetchWatchInstancePhase
	prevFetchBootstrap := fetchWatchBootstrapStage
	prevFetchHeartbeat := fetchWatchHeartbeat
	prevFetchProgress := fetchWatchJobProgress
	t.Cleanup(func() {
		fetchReconcileTerminationIntent = prevFetchIntent
		fetchWatchInstancePhase = prevFetchPhase
		fetchWatchBootstrapStage = prevFetchBootstrap
		fetchWatchHeartbeat = prevFetchHeartbeat
		fetchWatchJobProgress = prevFetchProgress
	})

	fetchReconcileTerminationIntent = func(context.Context, *r2.Client, int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	fetchWatchInstancePhase = func(context.Context, *r2.Client, int64) string {
		return fmt.Sprintf("running:%d", jobID)
	}
	fetchWatchBootstrapStage = func(context.Context, *r2.Client, int64) string {
		return ""
	}
	fetchWatchHeartbeat = func(context.Context, *r2.Client, int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	fetchWatchJobProgress = func(context.Context, *r2.Client, string, []*db.Job) (int64, int, int) {
		return jobID, 50, 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchInstance(ctx, &cloud.MockClient{}, database, instanceID, 10*time.Millisecond, time.Hour, &r2.Client{})
	<-ch

	// Verify job was transitioned to running in the DB
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after watch: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("status after R2 phase = %q, want %q", job.Status, db.StatusRunning)
	}
}
