package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/opsqueue"
)

func TestProcessCommands_Add(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	// Write an add command
	cmd := opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 42, Cmd: "echo hello", Dir: "/tmp"},
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

	cmd := opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpPriority,
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

	cmd := opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpCancel,
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

	cmd := opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpStop,
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
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpStop,
	})
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:02Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 99, Cmd: "echo work"},
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

	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 1, Cmd: "echo first"},
	})
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:02Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 2, Cmd: "echo second"},
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
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:03Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 3, Cmd: "echo third"},
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

func intPtr(n int) *int { return &n }

// assertResourceFields checks all GPU and artifact fields on a CommandJob.
func assertResourceFields(t *testing.T, got *opsqueue.CommandJob, gpu, gpuClass string, gpuMem int, outputDirs, produces, needs []string) {
	t.Helper()
	if got.GPU != gpu {
		t.Errorf("GPU: got %q, want %q", got.GPU, gpu)
	}
	if got.GPUClass != gpuClass {
		t.Errorf("GPUClass: got %q, want %q", got.GPUClass, gpuClass)
	}
	if got.GPUMem == nil || *got.GPUMem != gpuMem {
		t.Errorf("GPUMem: got %v, want %d", got.GPUMem, gpuMem)
	}
	if !slices.Equal(got.OutputDirs, outputDirs) {
		t.Errorf("OutputDirs: got %v, want %v", got.OutputDirs, outputDirs)
	}
	if !slices.Equal(got.Produces, produces) {
		t.Errorf("Produces: got %v, want %v", got.Produces, produces)
	}
	if !slices.Equal(got.Needs, needs) {
		t.Errorf("Needs: got %v, want %v", got.Needs, needs)
	}
}

func TestWriteJobFileMergesGPUFields(t *testing.T) {
	dir := t.TempDir()

	// First write: all GPU fields populated
	job1 := &opsqueue.CommandJob{
		ID:         50,
		Cmd:        "python train.py",
		GPU:        "2",
		GPUClass:   "a100",
		GPUMem:     intPtr(80),
		OutputDirs: []string{"out/"},
		Produces:   []string{"model.pt"},
		Needs:      []string{"data.csv:1"},
	}
	if err := writeJobFile(dir, job1); err != nil {
		t.Fatalf("first writeJobFile: %v", err)
	}

	// Second write: empty GPU fields (simulating duplicate add without resource data)
	job2 := &opsqueue.CommandJob{
		ID:  50,
		Cmd: "python train.py",
	}
	if err := writeJobFile(dir, job2); err != nil {
		t.Fatalf("second writeJobFile: %v", err)
	}

	// Read back and verify GPU fields were preserved from first write
	got, err := ReadJobFile(dir, 50)
	if err != nil {
		t.Fatalf("ReadJobFile: %v", err)
	}
	assertResourceFields(t, got, "2", "a100", 80, []string{"out/"}, []string{"model.pt"}, []string{"data.csv:1"})
}

func TestWriteJobFileNewDataTakesPrecedence(t *testing.T) {
	dir := t.TempDir()

	// First write
	job1 := &opsqueue.CommandJob{
		ID:       50,
		Cmd:      "python train.py",
		GPU:      "2",
		GPUClass: "a100",
		GPUMem:   intPtr(80),
	}
	if err := writeJobFile(dir, job1); err != nil {
		t.Fatalf("first writeJobFile: %v", err)
	}

	// Second write with different GPU fields — new non-empty data wins
	job2 := &opsqueue.CommandJob{
		ID:       50,
		Cmd:      "python train.py",
		GPU:      "3",
		GPUClass: "h100",
		GPUMem:   intPtr(40),
	}
	if err := writeJobFile(dir, job2); err != nil {
		t.Fatalf("second writeJobFile: %v", err)
	}

	got, err := ReadJobFile(dir, 50)
	if err != nil {
		t.Fatalf("ReadJobFile: %v", err)
	}
	if got.GPU != "3" {
		t.Errorf("GPU: got %q, want %q", got.GPU, "3")
	}
	if got.GPUClass != "h100" {
		t.Errorf("GPUClass: got %q, want %q", got.GPUClass, "h100")
	}
	if got.GPUMem == nil || *got.GPUMem != 40 {
		t.Errorf("GPUMem: got %v, want 40", got.GPUMem)
	}
}

func TestProcessCommands_AddWithGPUFields(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	cmd := opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job: &opsqueue.CommandJob{
			ID:         50,
			Cmd:        "python train.py",
			GPU:        "1",
			GPUClass:   "a100",
			GPUMem:     intPtr(80),
			OutputDirs: []string{"out/"},
			Produces:   []string{"model.pt"},
			Needs:      []string{"data.csv:1"},
		},
	}
	appendCmd(t, cmdFile, cmd)

	cp := NewCommandProcessor(cmdFile, dir)
	_, err := cp.ProcessCommands(state)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ReadJobFile(dir, 50)
	if err != nil {
		t.Fatalf("ReadJobFile: %v", err)
	}
	assertResourceFields(t, got, "1", "a100", 80, []string{"out/"}, []string{"model.pt"}, []string{"data.csv:1"})
}

func TestProcessCommands_AddHandlesLongJSONLine(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	largeDesc := strings.Repeat("x", 256*1024)
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job: &opsqueue.CommandJob{
			ID:   77,
			Cmd:  "echo huge",
			Desc: largeDesc,
		},
	})

	cp := NewCommandProcessor(cmdFile, dir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatalf("ProcessCommands: %v", err)
	}

	if len(state.Pending) != 1 || state.Pending[0] != 77 {
		t.Fatalf("pending = %v, want [77]", state.Pending)
	}

	jobData, err := ReadJobFile(dir, 77)
	if err != nil {
		t.Fatalf("ReadJobFile: %v", err)
	}
	if jobData.Desc != largeDesc {
		t.Fatalf("job description length = %d, want %d", len(jobData.Desc), len(largeDesc))
	}
}

func appendCmd(t *testing.T, path string, cmd opsqueue.QueueCommand) {
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
