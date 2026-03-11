package campaign

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestFormatPlainUpdate_Initial(t *testing.T) {
	prev := InstanceUpdate{}
	curr := InstanceUpdate{
		CloudInstance: &db.CloudInstance{
			ID:               5,
			Status:           db.CloudInstanceStatusLaunching,
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
		CloudInstance: &db.CloudInstance{
			ID:               5,
			Status:           db.CloudInstanceStatusRunning,
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
		CloudInstance: &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning},
		Jobs:          []*db.Job{{ID: 88, Status: db.StatusQueued}},
	}
	curr := InstanceUpdate{
		CloudInstance: &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning},
		Jobs:          []*db.Job{{ID: 88, Status: db.StatusRunning}},
	}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "job 88 status=running") {
		t.Errorf("should contain job status change, got %q", output)
	}
}

func TestFormatPlainUpdate_NoChange(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning}
	jobs := []*db.Job{{ID: 88, Status: db.StatusRunning}}

	prev := InstanceUpdate{CloudInstance: ci, Jobs: jobs}
	curr := InstanceUpdate{CloudInstance: ci, Jobs: jobs}

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
		{"uploading:7", "uploading outputs (job 7)"},
		{"grace", "grace period"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		if got := InstancePhaseLabel(tt.phase); got != tt.want {
			t.Errorf("InstancePhaseLabel(%q) = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestFormatPlainUpdate_PhaseChange(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning}
	prev := InstanceUpdate{CloudInstance: ci, InstancePhase: "setup:42"}
	curr := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42"}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "phase: running job 42") {
		t.Errorf("should contain phase change, got %q", output)
	}
}

func TestFormatPlainUpdate_BootstrapStallWarning(t *testing.T) {
	ci := &db.CloudInstance{ID: 7, Status: db.CloudInstanceStatusRunning}
	prev := InstanceUpdate{CloudInstance: ci}
	curr := InstanceUpdate{
		CloudInstance: ci,
		StallMessage:  "bootstrap stalled — no activity after 15m0s",
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
	ci := &db.CloudInstance{ID: 7, Status: db.CloudInstanceStatusFailed}
	prev := InstanceUpdate{CloudInstance: &db.CloudInstance{ID: 7, Status: db.CloudInstanceStatusRunning}}
	curr := InstanceUpdate{
		CloudInstance: ci,
		StallMessage:  "bootstrap timeout after 20m0s — terminating instance, jobs reset to queued",
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
	ci := &db.CloudInstance{ID: 7, Status: db.CloudInstanceStatusRunning}
	msg := "bootstrap stalled — no activity after 15m0s"
	prev := InstanceUpdate{CloudInstance: ci, StallMessage: msg}
	curr := InstanceUpdate{CloudInstance: ci, StallMessage: msg}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "WARNING") {
		t.Errorf("should not repeat same stall warning, got %q", output)
	}
}

func TestWatchInstance_BootstrapTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a cloud instance, then set launched_at and provider_instance_id
	// (CreateCloudInstance doesn't persist these fields)
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	// Set launched_at to 25 minutes ago (beyond terminate threshold) and provider_instance_id
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	_, err = database.Exec(`UPDATE cloud_instances SET launched_at = ?, provider_instance_id = ? WHERE id = ?`,
		launchedAt, "test-123", instanceID)
	if err != nil {
		t.Fatalf("update launched_at: %v", err)
	}

	// Create a queued job assigned to this instance
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	var destroyed bool
	mockClient := &cloud.MockClient{
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{ProviderID: id, Status: "running"}, nil
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
	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusFailed)
	}
}

func TestWatchInstance_ShowInstanceErrorDoesNotMarkFailed(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	_, err = database.Exec(`UPDATE cloud_instances SET provider_instance_id = ? WHERE id = ?`,
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

	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.CloudInstanceStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.CloudInstanceStatusRunning)
	}
}

func TestFormatPlainUpdate_ProgressChange(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning}
	prev := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: -1}
	curr := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}

	output := FormatPlainUpdate(prev, curr)
	if !strings.Contains(output, "job 42 progress: 50%") {
		t.Errorf("should contain progress update, got %q", output)
	}
}

func TestFormatPlainUpdate_ProgressNoChange(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning}
	prev := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}
	curr := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: 50, JobProgressID: 42}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "progress") {
		t.Errorf("should not report unchanged progress, got %q", output)
	}
}

func TestFormatPlainUpdate_ProgressNoReport(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning}
	prev := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: -1}
	curr := InstanceUpdate{CloudInstance: ci, InstancePhase: "running:42", JobProgress: -1}

	output := FormatPlainUpdate(prev, curr)
	if strings.Contains(output, "progress") {
		t.Errorf("should not report when no progress, got %q", output)
	}
}

func TestJobDisplayStatus(t *testing.T) {
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
			name:     "running with outcome still shows running",
			status:   db.StatusRunning,
			outcomes: map[int64]string{1: db.AttemptOutcomeFailed},
			want:     db.StatusRunning,
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
			got := JobDisplayStatus(j, tt.outcomes)
			if got != tt.want {
				t.Errorf("JobDisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatPlainUpdate_JobDisplayStatusUsed(t *testing.T) {
	ci := &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusFailed}
	prev := InstanceUpdate{
		CloudInstance: &db.CloudInstance{ID: 5, Status: db.CloudInstanceStatusRunning},
		Jobs:          []*db.Job{{ID: 88, Status: db.StatusRunning}},
	}
	curr := InstanceUpdate{
		CloudInstance:      ci,
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
		{db.CloudInstanceStatusCompleted, true},
		{db.CloudInstanceStatusFailed, true},
		{db.CloudInstanceStatusCancelled, true},
		{db.CloudInstanceStatusRunning, false},
		{db.CloudInstanceStatusLaunching, false},
		{db.CloudInstanceStatusPlanned, false},
	}
	for _, tt := range tests {
		if got := IsInstanceTerminal(tt.status); got != tt.want {
			t.Errorf("IsInstanceTerminal(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
