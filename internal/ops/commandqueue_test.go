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
	path := CommandsFilePath("myqueue")
	if path != "~/.cache/remote-jobs/queue/myqueue.commands" {
		t.Errorf("unexpected path: %s", path)
	}

	path = CommandsFilePath("default")
	if path != "~/.cache/remote-jobs/queue/default.commands" {
		t.Errorf("unexpected path: %s", path)
	}
}

func TestStateFilePath(t *testing.T) {
	path := StateFilePath("myqueue")
	if path != "~/.cache/remote-jobs/queue/myqueue.state.json" {
		t.Errorf("unexpected path: %s", path)
	}
}
