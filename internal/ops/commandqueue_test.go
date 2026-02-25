package ops

import (
	"encoding/json"
	"os"
	"os/exec"
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

func TestJqCanParseCommands(t *testing.T) {
	// Skip if jq is not installed
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}

	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "simple command",
			command: "echo hello",
		},
		{
			name: "multi-line command with continuations",
			command: `uv run compression-lab measure \
    --data ~/path/to/file \
    --codecs "blosc,parametric-ans,eg" \
    --quantize int16`,
		},
		{
			name:    "command with quotes and escapes",
			command: `python -c "import x;\nprint('hello\tworld')"`,
		},
		{
			name:    "command with special shell chars",
			command: `echo $HOME && ls -la | grep foo > /tmp/out`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewAddCommand(QueueEntry{
				JobID:       12345,
				WorkingDir:  "/Users/test/project",
				Command:     tt.command,
				Description: "Test job",
				EnvVars:     []string{"FOO=bar"},
			})

			jsonBytes, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Use jq to extract fields into separate variables using null delimiter
			// This handles multi-line content correctly
			jqScript := `
				job_id=$(echo "$1" | jq -r '.job.id')
				working_dir=$(echo "$1" | jq -r '.job.dir')
				command=$(echo "$1" | jq -r '.job.cmd')
				description=$(echo "$1" | jq -r '.job.desc')

				# Output with markers so we can parse reliably
				printf 'JOB_ID:%s\n' "$job_id"
				printf 'DIR:%s\n' "$working_dir"
				printf 'DESC:%s\n' "$description"
				# Command last since it may have newlines
				printf 'CMD_START\n%s\nCMD_END\n' "$command"
			`

			execCmd := exec.Command("bash", "-c", jqScript, "bash", string(jsonBytes))
			output, err := execCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("jq parsing failed: %v\noutput: %s", err, output)
			}

			outputStr := string(output)

			// Verify job_id
			if !strings.Contains(outputStr, "JOB_ID:12345") {
				t.Errorf("job_id not found or incorrect in output:\n%s", outputStr)
			}

			// Verify working_dir
			if !strings.Contains(outputStr, "DIR:/Users/test/project") {
				t.Errorf("working_dir not found or incorrect in output:\n%s", outputStr)
			}

			// Verify description
			if !strings.Contains(outputStr, "DESC:Test job") {
				t.Errorf("description not found or incorrect in output:\n%s", outputStr)
			}

			// Extract and verify command (between CMD_START and CMD_END)
			startIdx := strings.Index(outputStr, "CMD_START\n")
			endIdx := strings.Index(outputStr, "\nCMD_END")
			if startIdx == -1 || endIdx == -1 {
				t.Fatalf("could not find CMD markers in output:\n%s", outputStr)
			}
			extractedCmd := outputStr[startIdx+len("CMD_START\n") : endIdx]

			if extractedCmd != tt.command {
				t.Errorf("command mismatch:\ngot:  %q\nwant: %q", extractedCmd, tt.command)
			}
		})
	}
}

func TestBuildAppendShellCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "command with dollar expansion",
			command: `echo "$(hostname)" && nvidia-smi 2>/dev/null`,
		},
		{
			name:    "command with backticks",
			command: "echo `date`",
		},
		{
			name:    "command with single quotes",
			command: `echo 'hello world'`,
		},
		{
			name:    "command with newlines in output",
			command: "echo \"line1\nline2\nline3\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewAddCommand(QueueEntry{
				JobID:   42,
				Command: tt.command,
			})
			jsonBytes, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}
			jsonLine := string(jsonBytes)

			shellCmd := buildAppendShellCommand(jsonLine, "/tmp/test-commands")

			// Execute the shell command and verify the output is valid single-line JSON
			// with the command preserved exactly
			tmpDir := t.TempDir()
			outFile := filepath.Join(tmpDir, "commands")

			// Replace the target path in the command
			shellCmd = strings.Replace(shellCmd, "/tmp/test-commands", outFile, 1)
			// Also fix the mkdir path
			shellCmd = strings.Replace(shellCmd, "mkdir -p "+QueueDir, "mkdir -p "+tmpDir, 1)

			execCmd := exec.Command("bash", "-c", shellCmd)
			output, err := execCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("shell command failed: %v\noutput: %s\ncmd: %s", err, output, shellCmd)
			}

			// Read the file and verify it's exactly one line of valid JSON
			content, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("failed to read output: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(string(content)), "\n")
			if len(lines) != 1 {
				t.Errorf("expected 1 line, got %d:\n%s", len(lines), content)
			}

			// Verify the JSON is valid and the command is preserved
			var parsed QueueCommand
			if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
				t.Fatalf("output is not valid JSON: %v\nline: %s", err, lines[0])
			}

			if parsed.Job.Cmd != tt.command {
				t.Errorf("command not preserved:\ngot:  %q\nwant: %q", parsed.Job.Cmd, tt.command)
			}
		})
	}
}

func TestCommandLogProcessing(t *testing.T) {
	// Skip if jq is not installed
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}

	// Create a series of commands
	commands := []QueueCommand{
		NewAddCommand(QueueEntry{JobID: 100, Command: "echo job100"}),
		NewAddCommand(QueueEntry{JobID: 101, Command: "echo job101"}),
		NewAddCommand(QueueEntry{JobID: 102, Command: "echo job102"}),
		NewPriorityCommand(102), // Move 102 to front
		NewCancelCommand(101),   // Cancel 101
	}

	// Build JSONL content
	var lines []string
	for _, cmd := range commands {
		jsonBytes, _ := json.Marshal(cmd)
		lines = append(lines, string(jsonBytes))
	}
	jsonlContent := strings.Join(lines, "\n")

	// Simulate what the shell script does to process commands
	shellScript := `
		pending=""

		add_pending() {
			if [ -z "$pending" ]; then
				pending="$1"
			else
				pending="$pending $1"
			fi
		}

		priority_pending() {
			pending=$(echo "$pending" | tr ' ' '\n' | grep -v "^$1$" | tr '\n' ' ')
			pending="$1 $pending"
		}

		remove_pending() {
			pending=$(echo "$pending" | tr ' ' '\n' | grep -v "^$1$" | tr '\n' ' ')
		}

		# Process each command
		echo "$1" | while IFS= read -r line; do
			[ -z "$line" ] && continue
			op=$(echo "$line" | jq -r '.op')
			case "$op" in
				add)
					job_id=$(echo "$line" | jq -r '.job.id')
					add_pending "$job_id"
					;;
				priority)
					job_id=$(echo "$line" | jq -r '.job_id')
					priority_pending "$job_id"
					;;
				cancel)
					job_id=$(echo "$line" | jq -r '.job_id')
					remove_pending "$job_id"
					;;
			esac
		done

		# Output final pending list (space-separated)
		echo "$pending" | tr -s ' ' | sed 's/^ //;s/ $//'
	`

	// This test verifies jq can parse all commands without error
	// The actual queue processing logic is more complex in the real script
	cmd := exec.Command("bash", "-c", shellScript, "bash", jsonlContent)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell processing failed: %v\noutput: %s", err, output)
	}

	// The shell script should process all commands without error
	// We're mainly testing that the JSON is parseable by jq
	t.Logf("Shell output: %s", output)
}

func TestCommandFilePath(t *testing.T) {
	path := CommandsFilePath()
	if path != "~/.cache/remote-jobs/queue/default.commands" {
		t.Errorf("unexpected path: %s", path)
	}
}

func TestStateFilePath(t *testing.T) {
	path := StateFilePath()
	if path != "~/.cache/remote-jobs/queue/default.state.json" {
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

func TestJqCanParseTagsFromCommands(t *testing.T) {
	// Skip if jq is not installed
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}

	cmd := NewAddCommand(QueueEntry{
		JobID:   12345,
		Command: "echo hello",
		Tags:    []string{"exclusive", "gpu", "experiment-1"},
	})

	jsonBytes, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Test jq extraction of tags
	jqScript := `echo "$1" | jq -r '.job.tags // [] | index("exclusive") != null'`
	execCmd := exec.Command("bash", "-c", jqScript, "bash", string(jsonBytes))
	output, err := execCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jq parsing failed: %v\noutput: %s", err, output)
	}

	result := strings.TrimSpace(string(output))
	if result != "true" {
		t.Errorf("expected jq to find exclusive tag, got: %s", result)
	}
}

func TestExclusiveTagLogic(t *testing.T) {
	// Skip if jq is not installed
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}

	// Test the job_has_exclusive_tag function logic
	tests := []struct {
		name     string
		tags     []string
		expected bool
	}{
		{"no tags", nil, false},
		{"empty tags", []string{}, false},
		{"other tag", []string{"gpu"}, false},
		{"exclusive tag", []string{"exclusive"}, true},
		{"exclusive with others", []string{"gpu", "exclusive", "test"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobData := map[string]interface{}{
				"id":   1,
				"cmd":  "echo test",
				"tags": tt.tags,
			}
			jsonBytes, _ := json.Marshal(jobData)

			// Test the same jq expression used in queue-runner.sh
			jqScript := `echo "$1" | jq -e '.tags // [] | index("exclusive") != null' &>/dev/null && echo "true" || echo "false"`
			execCmd := exec.Command("bash", "-c", jqScript, "bash", string(jsonBytes))
			output, err := execCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash failed: %v\noutput: %s", err, output)
			}

			result := strings.TrimSpace(string(output))
			expected := "false"
			if tt.expected {
				expected = "true"
			}
			if result != expected {
				t.Errorf("job_has_exclusive_tag: got %s, want %s", result, expected)
			}
		})
	}
}
