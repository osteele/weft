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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

func TestProcessCommands_AddPreservesSourceSHAOnDuplicate(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 42, Cmd: "echo one", SourceSHA: "abc123"},
	})
	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:01Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 42, Cmd: "echo two"},
	})

	cp := NewCommandProcessor(cmdFile, dir, dir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatal(err)
	}

	jobData, err := ReadJobFile(dir, 42)
	if err != nil {
		t.Fatal(err)
	}
	if jobData.SourceSHA != "abc123" {
		t.Fatalf("SourceSHA = %q, want abc123", jobData.SourceSHA)
	}
}

func TestProcessCommands_AddSkipsPendingWhenJobFileWriteFails(t *testing.T) {
	rootDir := t.TempDir()
	cmdFile := filepath.Join(rootDir, "default.commands")
	queueDir := filepath.Join(rootDir, "missing", "queue")
	state := NewState()

	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: 42, Cmd: "echo hello", Dir: "/tmp"},
	})

	cp := NewCommandProcessor(cmdFile, queueDir, queueDir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatal(err)
	}

	if len(state.Pending) != 0 {
		t.Fatalf("expected no pending jobs when job file write fails, got %v", state.Pending)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)

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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

	cp := NewCommandProcessor(cmdFile, dir, dir)
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

// TestProcessCommands_Add_ArchivesPriorArtifacts verifies the regression
// that wedged wj2301 on cool30: a leftover primary <id>.status file from a
// prior on-host attempt used to make JobCompleted() return true for the next
// add, so the runner silently dropped the re-added job. (paths.Status is
// written by the wrapper-exit trap, StartProcess failure, waitForJob,
// dep-failed skip, and orphan recovery — NOT by rejectPreflight, which
// writes only failure_reason and preflight_rejected.) The OpAdd handler
// must now archive any prior artifacts so JobCompleted is false when
// startJob runs.
func TestProcessCommands_Add_ArchivesPriorArtifacts(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	// Stage a leftover primary status file from a previous attempt — e.g.
	// the wrapper's signal-trap write at the end of a killed attempt, or
	// the dep-failed skip path in startJob.
	const jobID int64 = 4242
	priorStatus := filepath.Join(dir, "4242.status")
	if err := os.WriteFile(priorStatus, []byte("1\n"), 0644); err != nil {
		t.Fatalf("seed prior status: %v", err)
	}
	priorLog := filepath.Join(dir, "4242.log")
	if err := os.WriteFile(priorLog, []byte("prior attempt log\n"), 0644); err != nil {
		t.Fatalf("seed prior log: %v", err)
	}

	// Sanity: before the add op, JobCompleted is true.
	if !JobCompleted(dir, jobID) {
		t.Fatalf("precondition: expected JobCompleted=true with seeded status file")
	}

	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: jobID, Cmd: "echo hi", Dir: "/tmp"},
	})

	cp := NewCommandProcessor(cmdFile, dir, dir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatalf("ProcessCommands: %v", err)
	}

	if !slices.Contains(state.Pending, jobID) {
		t.Fatalf("expected job %d in pending, got %v", jobID, state.Pending)
	}

	// Primary status must have been archived (so startJob's JobCompleted
	// check no longer fires) and the archived copy must still exist.
	if JobCompleted(dir, jobID) {
		t.Fatalf("expected JobCompleted=false after OpAdd archived prior status")
	}
	if _, err := os.Stat(priorStatus); !os.IsNotExist(err) {
		t.Fatalf("expected primary status file to be removed, stat err=%v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "4242-*.status"))
	if err != nil {
		t.Fatalf("glob archived status: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected an archived <id>-<seq>.status file under %s", dir)
	}
}

// TestProcessCommands_Add_SkipsArchiveForLiveJob verifies that a duplicate
// OpAdd for a job currently in state.Running does not rename the live
// attempt's in-flight artifacts (.heartbeat, .timeseries.jsonl, .pid, …).
// Duplicate OpAdds are legitimate (host_sync forward reconcile, coordinator
// retries) — mergeResourceFields in writeJobFile already handles them.
func TestProcessCommands_Add_SkipsArchiveForLiveJob(t *testing.T) {
	dir := t.TempDir()
	cmdFile := filepath.Join(dir, "default.commands")
	state := NewState()

	const jobID int64 = 7777
	// Seed in-flight heartbeat as if the job were currently running.
	liveHeartbeat := filepath.Join(dir, "7777.heartbeat")
	if err := os.WriteFile(liveHeartbeat, []byte("alive\n"), 0644); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
	state.AddRunning("7777", RunningJobState{})

	appendCmd(t, cmdFile, opsqueue.QueueCommand{
		Timestamp: "2024-01-01T00:00:00Z",
		Op:        opsqueue.OpAdd,
		Job:       &opsqueue.CommandJob{ID: jobID, Cmd: "echo dup", Dir: "/tmp"},
	})

	cp := NewCommandProcessor(cmdFile, dir, dir)
	if _, err := cp.ProcessCommands(state); err != nil {
		t.Fatalf("ProcessCommands: %v", err)
	}

	if _, err := os.Stat(liveHeartbeat); err != nil {
		t.Fatalf("live heartbeat must not be archived while job is running: %v", err)
	}
	archived, _ := filepath.Glob(filepath.Join(dir, "7777-*.heartbeat"))
	if len(archived) != 0 {
		t.Fatalf("expected no archived heartbeat for live job, got %v", archived)
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
