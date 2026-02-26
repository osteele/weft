package intent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSerializationRoundtrip(t *testing.T) {
	gpuMem := 24
	original := &Intent{
		Timestamp: time.Date(2026, 2, 26, 12, 0, 0, 0, time.UTC),
		Op:        "place",
		IntentID:  "abc-123",
		Source:    "laptop",
		Job: IntentJob{
			ID:      42,
			Cmd:     "python train.py",
			Dir:     "/mnt/code/project",
			Desc:    "Training run",
			Env:     []string{"CUDA_VISIBLE_DEVICES=0"},
			Inputs:  []string{"hf:meta-llama/Llama-3-8B"},
			Outputs: []string{"checkpoint:llama-ft-v1"},
			Constraints: IntentConstraints{
				GPUClass: "a100",
				GPUMemGB: 80,
			},
			Tags:     []string{"benchmark"},
			DepSpec:  "41",
			GPUMemGB: &gpuMem,
		},
	}

	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if parsed.IntentID != original.IntentID {
		t.Errorf("IntentID = %q, want %q", parsed.IntentID, original.IntentID)
	}
	if parsed.Op != original.Op {
		t.Errorf("Op = %q, want %q", parsed.Op, original.Op)
	}
	if parsed.Job.ID != original.Job.ID {
		t.Errorf("Job.ID = %d, want %d", parsed.Job.ID, original.Job.ID)
	}
	if parsed.Job.Cmd != original.Job.Cmd {
		t.Errorf("Job.Cmd = %q, want %q", parsed.Job.Cmd, original.Job.Cmd)
	}
	if parsed.Job.Constraints.GPUClass != "a100" {
		t.Errorf("Constraints.GPUClass = %q, want %q", parsed.Job.Constraints.GPUClass, "a100")
	}
	if parsed.Job.Constraints.GPUMemGB != 80 {
		t.Errorf("Constraints.GPUMemGB = %d, want %d", parsed.Job.Constraints.GPUMemGB, 80)
	}
	if parsed.Job.GPUMemGB == nil || *parsed.Job.GPUMemGB != 24 {
		t.Errorf("Job.GPUMemGB = %v, want 24", parsed.Job.GPUMemGB)
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "42-abc-123.json")

	intent := &Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  "abc-123",
		Source:    "laptop",
		Job: IntentJob{
			ID:  42,
			Cmd: "echo hello",
			Dir: "/tmp",
		},
	}

	data, err := intent.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	parsed, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if parsed.IntentID != "abc-123" {
		t.Errorf("IntentID = %q, want %q", parsed.IntentID, "abc-123")
	}
	if parsed.Job.Cmd != "echo hello" {
		t.Errorf("Job.Cmd = %q, want %q", parsed.Job.Cmd, "echo hello")
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		intent  Intent
		wantErr string
	}{
		{
			name:    "missing intent_id",
			intent:  Intent{Op: "place", Job: IntentJob{Cmd: "echo"}},
			wantErr: "missing intent_id",
		},
		{
			name:    "missing op",
			intent:  Intent{IntentID: "abc", Job: IntentJob{Cmd: "echo"}},
			wantErr: "missing op",
		},
		{
			name:    "missing command",
			intent:  Intent{IntentID: "abc", Op: "place", Job: IntentJob{Dir: "/tmp"}},
			wantErr: "missing job command",
		},
		{
			name:   "valid",
			intent: Intent{IntentID: "abc", Op: "place", Job: IntentJob{Cmd: "echo"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.intent.Validate()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q", tt.wantErr)
				}
				if got := err.Error(); got != "intent "+tt.wantErr {
					t.Errorf("error = %q, want to contain %q", got, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestFilename(t *testing.T) {
	intent := &Intent{
		IntentID: "abc-def-123",
		Job:      IntentJob{ID: 99},
	}
	want := "99-abc-def-123.json"
	if got := intent.Filename(); got != want {
		t.Errorf("Filename() = %q, want %q", got, want)
	}
}
