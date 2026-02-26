package ops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueCommandSerialization(t *testing.T) {
	tests := []struct {
		name string
		cmd  QueueCommand
	}{
		{
			name: "add simple command",
			cmd: NewAddCommand(QueueEntry{
				JobID:       123,
				WorkingDir:  "/tmp/test",
				Command:     "echo hello",
				Description: "test job",
			}),
		},
		{
			name: "add multi-line command",
			cmd: NewAddCommand(QueueEntry{
				JobID:      456,
				WorkingDir: "/home/user/project",
				Command: `uv run compression-lab measure \
    --data ~/path/to/file \
    --codecs "blosc,ans" \
    --quantize int16`,
				Description: "Compression benchmark",
				EnvVars:     []string{"FOO=bar", "BAZ=qux"},
				DepSpec:     "455:success",
			}),
		},
		{
			name: "add command with special chars",
			cmd: NewAddCommand(QueueEntry{
				JobID:       789,
				WorkingDir:  "/path/with spaces/dir",
				Command:     `python -c "import x;\nprint(x)"`,
				Description: "Python with\ttabs\nand newlines",
			}),
		},
		{
			name: "priority command",
			cmd:  NewPriorityCommand(123),
		},
		{
			name: "cancel command",
			cmd:  NewCancelCommand(456),
		},
		{
			name: "stop command",
			cmd:  NewStopCommand(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Serialize to JSON
			jsonBytes, err := json.Marshal(tt.cmd)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Verify it's valid JSON
			var parsed map[string]interface{}
			if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
				t.Fatalf("produced invalid JSON: %v\njson: %s", err, jsonBytes)
			}

			// Verify required fields
			if _, ok := parsed["ts"]; !ok {
				t.Error("missing timestamp field")
			}
			if _, ok := parsed["op"]; !ok {
				t.Error("missing op field")
			}

			// Verify it's a single line (no embedded newlines)
			if strings.Contains(string(jsonBytes), "\n") {
				t.Error("JSON contains embedded newlines")
			}

			// Verify round-trip
			var roundTrip QueueCommand
			if err := json.Unmarshal(jsonBytes, &roundTrip); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if roundTrip.Op != tt.cmd.Op {
				t.Errorf("op mismatch: got %q, want %q", roundTrip.Op, tt.cmd.Op)
			}
		})
	}
}

func TestAppendCommandLocal(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	commandsFile := filepath.Join(tmpDir, "test.commands")

	// Append several commands
	commands := []QueueCommand{
		NewAddCommand(QueueEntry{JobID: 1, Command: "echo one"}),
		NewAddCommand(QueueEntry{JobID: 2, Command: "echo two"}),
		NewPriorityCommand(1),
		NewCancelCommand(2),
	}

	for _, cmd := range commands {
		if err := AppendCommandLocal(commandsFile, cmd); err != nil {
			t.Fatalf("AppendCommandLocal failed: %v", err)
		}
	}

	// Read and verify
	content, err := os.ReadFile(commandsFile)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 {
		t.Errorf("expected 4 lines, got %d", len(lines))
	}

	// Verify each line is valid JSON
	for i, line := range lines {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i+1, err, line)
		}
	}
}

func TestCommandFilePath(t *testing.T) {
	path := CommandsFilePath()
	if path != "~/.cache/weft/queue/default.commands" {
		t.Errorf("unexpected path: %s", path)
	}
}

func TestStateFilePath(t *testing.T) {
	path := StateFilePath()
	if path != "~/.cache/weft/queue/default.state.json" {
		t.Errorf("unexpected path: %s", path)
	}
}

func TestGPUFieldsSerialization(t *testing.T) {
	tenGB := 10
	tests := []struct {
		name    string
		gpu     string
		gpuMem  *int
		wantGPU string
		wantMem *int
	}{
		{
			name:    "no GPU fields",
			gpu:     "",
			gpuMem:  nil,
			wantGPU: "",
			wantMem: nil,
		},
		{
			name:    "GPU only",
			gpu:     "0",
			gpuMem:  nil,
			wantGPU: "0",
			wantMem: nil,
		},
		{
			name:    "GPU with memory",
			gpu:     "0,1",
			gpuMem:  &tenGB,
			wantGPU: "0,1",
			wantMem: &tenGB,
		},
		{
			name:    "memory without GPU",
			gpu:     "",
			gpuMem:  &tenGB,
			wantGPU: "",
			wantMem: &tenGB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewAddCommand(QueueEntry{
				JobID:    123,
				Command:  "echo hello",
				GPU:      tt.gpu,
				GPUMemGB: tt.gpuMem,
			})

			jsonBytes, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			var roundTrip QueueCommand
			if err := json.Unmarshal(jsonBytes, &roundTrip); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if roundTrip.Job == nil {
				t.Fatal("job is nil after round-trip")
			}

			if roundTrip.Job.GPU != tt.wantGPU {
				t.Errorf("GPU mismatch: got %q, want %q", roundTrip.Job.GPU, tt.wantGPU)
			}

			if tt.wantMem == nil {
				if roundTrip.Job.GPUMem != nil {
					t.Errorf("GPUMem: got %d, want nil", *roundTrip.Job.GPUMem)
				}
			} else {
				if roundTrip.Job.GPUMem == nil {
					t.Errorf("GPUMem: got nil, want %d", *tt.wantMem)
				} else if *roundTrip.Job.GPUMem != *tt.wantMem {
					t.Errorf("GPUMem mismatch: got %d, want %d", *roundTrip.Job.GPUMem, *tt.wantMem)
				}
			}
		})
	}
}

func TestGPUFieldsOmittedWhenEmpty(t *testing.T) {
	cmd := NewAddCommand(QueueEntry{
		JobID:   123,
		Command: "echo hello",
	})

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	jsonStr := string(jsonBytes)
	if strings.Contains(jsonStr, `"gpu"`) {
		t.Error("GPU field should be omitted when empty")
	}
	if strings.Contains(jsonStr, `"gpu_mem"`) {
		t.Error("gpu_mem field should be omitted when nil")
	}
}

func TestNewAddCommandWiresGPUFields(t *testing.T) {
	tenGB := 10
	entry := QueueEntry{
		JobID:    42,
		Command:  "python train.py",
		GPU:      "0,1",
		GPUMemGB: &tenGB,
	}

	cmd := NewAddCommand(entry)

	if cmd.Job.GPU != "0,1" {
		t.Errorf("GPU not wired: got %q, want %q", cmd.Job.GPU, "0,1")
	}
	if cmd.Job.GPUMem == nil || *cmd.Job.GPUMem != 10 {
		t.Errorf("GPUMem not wired: got %v, want 10", cmd.Job.GPUMem)
	}
}

func TestTagsSerialization(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		wantTags []string
	}{
		{
			name:     "no tags",
			tags:     nil,
			wantTags: nil,
		},
		{
			name:     "single tag",
			tags:     []string{"exclusive"},
			wantTags: []string{"exclusive"},
		},
		{
			name:     "multiple tags",
			tags:     []string{"exclusive", "gpu", "experiment-1"},
			wantTags: []string{"exclusive", "gpu", "experiment-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewAddCommand(QueueEntry{
				JobID:   123,
				Command: "echo hello",
				Tags:    tt.tags,
			})

			// Serialize to JSON
			jsonBytes, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Deserialize and verify tags
			var roundTrip QueueCommand
			if err := json.Unmarshal(jsonBytes, &roundTrip); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if roundTrip.Job == nil {
				t.Fatal("job is nil after round-trip")
			}

			// Check tags match
			if len(roundTrip.Job.Tags) != len(tt.wantTags) {
				t.Errorf("tags length mismatch: got %d, want %d", len(roundTrip.Job.Tags), len(tt.wantTags))
			}
			for i, tag := range roundTrip.Job.Tags {
				if tag != tt.wantTags[i] {
					t.Errorf("tag[%d] mismatch: got %q, want %q", i, tag, tt.wantTags[i])
				}
			}
		})
	}
}
