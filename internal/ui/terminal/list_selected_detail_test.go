package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestSelectedJobDetail_InventoryHost(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	job := &db.Job{
		ID:        42,
		Host:      "cool30",
		GPU:       "0,1",
		Project:   "my-proj",
		Command:   "uv run scripts/train.py --epochs 10",
		Status:    db.StatusRunning,
		StartTime: now.Add(-5 * time.Minute).Unix(),
	}
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %d: %v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"#42", "host cool30", "GPU 0,1", "elapsed 5m"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in detail line, got: %s", want, line)
		}
	}
	for _, unwanted := range []string{"project", "my-proj", "train.py", "epochs"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("unexpected %q in detail line (project/command must be excluded), got: %s", unwanted, line)
		}
	}
}

func TestSelectedJobDetail_RentalInstance(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	launchID := int64(7)
	job := &db.Job{
		ID:        9,
		Host:      "cloud:vastai:123",
		LaunchID:  &launchID,
		Status:    db.StatusRunning,
		StartTime: now.Add(-90 * time.Second).Unix(),
	}
	live := &db.LaunchLiveState{InstancePhase: "running:316"}
	lines := renderSelectedJobDetail(job, live, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %v", lines)
	}
	line := lines[0]
	for _, want := range []string{"#9", "instance wi7", "phase running:316", "elapsed 1m"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in detail line, got: %s", want, line)
		}
	}
}

func TestSelectedJobDetail_Unplaced(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	memGB := 40
	job := &db.Job{
		ID:               5,
		GPUClass:         "ampere+",
		GPUMemGB:         &memGB,
		PlacementReasons: []string{"no capacity"},
		Status:           db.StatusQueued,
		QueueName:        "default",
		CreatedAt:        now.Add(-2 * time.Hour).Unix(),
	}
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %v", lines)
	}
	line := lines[0]
	for _, want := range []string{"unplaced", "ampere+", "≥40GB", "blocked: no capacity", "waiting 2h"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in detail line, got: %s", want, line)
		}
	}
	if strings.Contains(line, "queue") {
		t.Errorf("queue name must be excluded, got: %s", line)
	}
}

func TestSelectedJobDetail_Failed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	exit := 137
	job := &db.Job{
		ID:            3,
		Host:          "cool30",
		Status:        db.StatusFailed,
		ExitCode:      &exit,
		FailureReason: "oom",
	}
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 1 {
		t.Fatalf("expected 1 detail line, got %v", lines)
	}
	line := lines[0]
	for _, want := range []string{"#3", "host cool30", "exit 137", "oom"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in detail line, got: %s", want, line)
		}
	}
}

func TestSelectedJobDetail_NilJob(t *testing.T) {
	if lines := renderSelectedJobDetail(nil, nil, time.Now()); lines != nil {
		t.Fatalf("expected nil for nil job, got %v", lines)
	}
}
