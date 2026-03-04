package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
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
	if !strings.Contains(output, "vastai=12345678") {
		t.Errorf("should contain vastai instance ID, got %q", output)
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
		Instance: &vastai.Instance{
			ID:      12345678,
			SSHHost: "ssh6.vast.ai",
			SSHPort: 34567,
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
