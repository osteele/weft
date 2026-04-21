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
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (Job + Host), got %d: %v", len(lines), lines)
	}
	job0, host1 := lines[0], lines[1]
	for _, want := range []string{"Job: wj42", "elapsed 5m"} {
		if !strings.Contains(job0, want) {
			t.Errorf("expected %q in Job line, got: %s", want, job0)
		}
	}
	if !strings.Contains(host1, "Host: cool30") {
		t.Errorf("expected 'Host: cool30' in Host line, got: %s", host1)
	}
	for _, unwanted := range []string{"project", "my-proj", "train.py", "epochs"} {
		joined := strings.Join(lines, " | ")
		if strings.Contains(joined, unwanted) {
			t.Errorf("unexpected %q in detail lines (project/command must be excluded): %s", unwanted, joined)
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
		Tags:      []string{"provider:vastai"},
		Status:    db.StatusRunning,
		StartTime: now.Add(-90 * time.Second).Unix(),
	}
	lines := renderSelectedJobDetail(job, nil, now)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	if !strings.Contains(lines[0], "Job: wj9") || !strings.Contains(lines[0], "elapsed 1m") {
		t.Errorf("expected 'Job: wj9 · elapsed 1m', got: %s", lines[0])
	}
	for _, want := range []string{"Host:", "wi7", "provider: Vast.ai"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("expected %q in Host line, got: %s", want, lines[1])
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
		t.Fatalf("unplaced jobs should have no Host line (got %d lines): %v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"Job: wj5", "unplaced", "ampere+", "≥40GB", "blocked: no capacity", "waiting 2h"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in Job line, got: %s", want, line)
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
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %v", lines)
	}
	for _, want := range []string{"Job: wj3", "exit 137", "oom"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("expected %q in Job line, got: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "Host: cool30") {
		t.Errorf("expected 'Host: cool30' in Host line, got: %s", lines[1])
	}
}

func TestSelectedJobDetail_NilJob(t *testing.T) {
	if lines := renderSelectedJobDetail(nil, nil, time.Now()); lines != nil {
		t.Fatalf("expected nil for nil job, got %v", lines)
	}
}

func TestProviderDisplayName(t *testing.T) {
	for provider, want := range map[string]string{
		"vastai": "Vast.ai",
		"runpod": "RunPod",
		"":       "",
		"gcp":    "",
	} {
		if got := providerDisplayName(provider); got != want {
			t.Errorf("providerDisplayName(%q) = %q, want %q", provider, got, want)
		}
	}
}
