package opsqueue

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
			name: "add pinned source manifest",
			cmd: NewAddCommand(QueueEntry{
				JobID:       789,
				Command:     "true",
				SourceR2Key: "source-closures/v2/sha256/guard.json",
				SourceManifest: &SourceManifest{
					SHA256: strings.Repeat("a", 64),
					Roots: []SourceRoot{{
						MountBasename: "project",
						Hash:          strings.Repeat("b", 64),
						R2Key:         "sources/project.tar.gz",
					}},
				},
			}),
		},
		{name: "priority command", cmd: NewPriorityCommand(123)},
		{name: "cancel command", cmd: NewCancelCommand(456)},
		{name: "stop command", cmd: NewStopCommand()},
		{name: "restart command with env", cmd: NewRestartCommandWithEnv([]string{"WEFT_BENCHMARK_CPU=15", "WEFT_BENCHMARK_RAM=35"})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsonBytes, err := json.Marshal(tt.cmd)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(jsonBytes), "\n") {
				t.Fatalf("JSON contains embedded newline: %s", jsonBytes)
			}
			var roundTrip QueueCommand
			if err := json.Unmarshal(jsonBytes, &roundTrip); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if roundTrip.Op != tt.cmd.Op {
				t.Fatalf("op = %q, want %q", roundTrip.Op, tt.cmd.Op)
			}
			if tt.cmd.Job != nil && tt.cmd.Job.SourceManifest != nil {
				if roundTrip.Job == nil || roundTrip.Job.SourceManifest == nil || roundTrip.Job.SourceManifest.SHA256 != tt.cmd.Job.SourceManifest.SHA256 {
					t.Fatalf("source manifest did not survive round trip: %#v", roundTrip.Job)
				}
			}
		})
	}
}

func TestNewRestartCommandWithEnv(t *testing.T) {
	cmd := NewRestartCommandWithEnv([]string{"WEFT_BENCHMARK_CPU=15", "WEFT_BENCHMARK_RAM=35"})
	if cmd.Op != OpRestart {
		t.Fatalf("Op = %q, want %q", cmd.Op, OpRestart)
	}
	if len(cmd.Env) != 2 || cmd.Env[0] != "WEFT_BENCHMARK_CPU=15" || cmd.Env[1] != "WEFT_BENCHMARK_RAM=35" {
		t.Fatalf("Env = %#v", cmd.Env)
	}
}

func TestAppendCommandLocal(t *testing.T) {
	commandsFile := filepath.Join(t.TempDir(), "test.commands")
	commands := []QueueCommand{
		NewAddCommand(QueueEntry{JobID: 1, Command: "echo one"}),
		NewAddCommand(QueueEntry{JobID: 2, Command: "echo two"}),
		NewPriorityCommand(1),
		NewCancelCommand(2),
	}

	for _, cmd := range commands {
		if err := AppendCommandLocal(commandsFile, cmd); err != nil {
			t.Fatalf("AppendCommandLocal: %v", err)
		}
	}

	content, err := os.ReadFile(commandsFile)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != len(commands) {
		t.Fatalf("line count = %d, want %d", len(lines), len(commands))
	}
	for i, line := range lines {
		var parsed QueueCommand
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d invalid JSON: %v\n%s", i+1, err, line)
		}
	}
}

func TestCommandPaths(t *testing.T) {
	if got := CommandsFilePath(); got != "~/.cache/weft/queue/default.commands" {
		t.Fatalf("CommandsFilePath = %q", got)
	}
	if got := StateFilePath(); got != "~/.cache/weft/queue/default.state.json" {
		t.Fatalf("StateFilePath = %q", got)
	}
}

func TestNewAddCommandCarriesRAMReservation(t *testing.T) {
	cmd := NewAddCommand(QueueEntry{JobID: 42, Command: "true", RAMReservationKB: 123456})
	if cmd.Job == nil || cmd.Job.RAMReservationKB != 123456 {
		t.Fatalf("RAMReservationKB = %v, want 123456", cmd.Job)
	}
}
