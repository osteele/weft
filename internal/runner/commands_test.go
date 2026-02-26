package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/ops"
)

func TestProcessCommands_Add(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	// Write an add command
	cmd := ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        ops.OpAdd,
		Job:       &ops.CommandJob{ID: 42, Cmd: "echo hello", Dir: "/tmp"},
	}
	appendCmd(t, cmdFile, cmd)

	cp := NewCommandProcessor(cmdFile, dir)
	result, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopRequested {
		t.Error("expected no stop")
	}

	if len(state.Pending) != 1 || state.Pending[0] != 42 {
		t.Errorf("expected pending=[42], got %v", state.Pending)
	}
	if state.CursorLine != 1 {
		t.Errorf("expected cursor_line=1, got %d", state.CursorLine)
	}

	// Verify job file was written
	jobData, err := ReadJobFile(dir, 42)
	if err != nil {
		t.Fatal(err)
	}
	if jobData.Cmd != "echo hello" {
		t.Errorf("expected cmd='echo hello', got %q", jobData.Cmd)
	}
}

func TestProcessCommands_Priority(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()
	state.AddPending(1)
	state.AddPending(2)
	state.AddPending(3)

	cmd := ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        ops.OpPriority,
		JobID:     3,
	}
	appendCmd(t, cmdFile, cmd)

	cp := NewCommandProcessor(cmdFile, dir)
	_, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}

	if len(state.Pending) != 3 || state.Pending[0] != 3 {
		t.Errorf("expected pending=[3,1,2], got %v", state.Pending)
	}
}

func TestProcessCommands_Cancel(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()
	state.AddPending(1)
	state.AddPending(2)

	cmd := ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        ops.OpCancel,
		JobID:     1,
	}
	appendCmd(t, cmdFile, cmd)

	cp := NewCommandProcessor(cmdFile, dir)
	_, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}

	if len(state.Pending) != 1 || state.Pending[0] != 2 {
		t.Errorf("expected pending=[2], got %v", state.Pending)
	}
}

func TestProcessCommands_Stop(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	cmd := ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        ops.OpStop,
	}
	appendCmd(t, cmdFile, cmd)

	cp := NewCommandProcessor(cmdFile, dir)
	result, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}

	if !result.StopRequested {
		t.Error("expected stop requested")
	}
	if !state.StopRequested {
		t.Error("expected state.StopRequested")
	}
}

func TestProcessCommands_StopCancelledByAdd(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	// Stop then add — the add should cancel the stop
	appendCmd(t, cmdFile, ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        ops.OpStop,
	})
	appendCmd(t, cmdFile, ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:02Z",
		Op:        ops.OpAdd,
		Job:       &ops.CommandJob{ID: 99, Cmd: "echo work"},
	})

	cp := NewCommandProcessor(cmdFile, dir)
	_, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}

	if state.StopRequested {
		t.Error("expected stop to be cancelled by add")
	}
}

func TestProcessCommands_SkipsAlreadyProcessed(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	appendCmd(t, cmdFile, ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        ops.OpAdd,
		Job:       &ops.CommandJob{ID: 1, Cmd: "echo first"},
	})
	appendCmd(t, cmdFile, ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:02Z",
		Op:        ops.OpAdd,
		Job:       &ops.CommandJob{ID: 2, Cmd: "echo second"},
	})

	cp := NewCommandProcessor(cmdFile, dir)

	// First pass: processes both
	_, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(state.Pending))
	}

	// Add a third command
	appendCmd(t, cmdFile, ops.QueueCommand{
		Timestamp: "2024-01-01T00:00:03Z",
		Op:        ops.OpAdd,
		Job:       &ops.CommandJob{ID: 3, Cmd: "echo third"},
	})

	// Second pass: only processes the new one
	_, err = cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 3 {
		t.Errorf("expected 3 pending, got %d", len(state.Pending))
	}
}

func TestProcessCommands_MissingFile(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "nonexistent.commands")
	state := NewState()

	cp := NewCommandProcessor(cmdFile, dir)
	result, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopRequested || result.RestartRequested {
		t.Error("expected no-op for missing file")
	}
}

func appendCmd(t *testing.T, path string, cmd ops.QueueCommand) {
	t.Helper()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}
