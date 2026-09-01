package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

func TestState_PendingOperations(t *testing.T) {
	s := NewState()

	s.AddPending(1)
	s.AddPending(2)
	s.AddPending(3)

	if len(s.Pending) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(s.Pending))
	}

	// AddPending deduplicates
	s.AddPending(2)
	if len(s.Pending) != 3 {
		t.Fatalf("expected 3 pending after dedup, got %d", len(s.Pending))
	}
	// 2 should be at the end
	if s.Pending[2] != 2 {
		t.Errorf("expected 2 at end, got %v", s.Pending)
	}

	// Priority moves to front
	s.PriorityPending(3)
	if s.Pending[0] != 3 {
		t.Errorf("expected 3 at front, got %v", s.Pending)
	}

	// Pop
	id, ok := s.PopPending()
	if !ok || id != 3 {
		t.Errorf("expected pop=3, got %d, ok=%v", id, ok)
	}

	// Remove
	s.RemovePending(1)
	if len(s.Pending) != 1 || s.Pending[0] != 2 {
		t.Errorf("expected pending=[2], got %v", s.Pending)
	}

	// Pop last
	id, ok = s.PopPending()
	if !ok || id != 2 {
		t.Errorf("expected pop=2, got %d", id)
	}

	// Pop from empty
	_, ok = s.PopPending()
	if ok {
		t.Error("expected false from empty pop")
	}
}

func TestState_AddPendingClearsFinished(t *testing.T) {
	s := NewState()
	s.RecordFinished("42", 0, 1234)

	s.AddPending(42)

	if _, ok := s.Finished["42"]; ok {
		t.Fatal("expected AddPending to clear finished entry for requeued job")
	}
}

func TestState_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := NewState()
	s.SetCapabilities([]string{opsqueue.CapabilityJobPayloadV1})
	s.Cursor = "2024-01-01T00:00:00Z"
	s.CursorLine = 5
	s.AddPending(10)
	s.AddPending(20)
	id := int64(10)
	s.Current = &id
	s.AddRunning("10", RunningJobState{
		StartedAt:        1000,
		WarmupUntil:      1120,
		LocalAllotment:   30,
		GPUDevices:       []string{"0"},
		GPUMemGB:         20,
		RAMReservationKB: 32 * gibKB,
	})
	s.RecordFinished("5", 0, 999)

	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Cursor != s.Cursor {
		t.Errorf("cursor: got %q, want %q", loaded.Cursor, s.Cursor)
	}
	if !slices.Contains(loaded.Capabilities, opsqueue.CapabilityJobPayloadV1) {
		t.Fatalf("capabilities = %v", loaded.Capabilities)
	}
	if loaded.CursorLine != s.CursorLine {
		t.Errorf("cursor_line: got %d, want %d", loaded.CursorLine, s.CursorLine)
	}
	if len(loaded.Pending) != 2 {
		t.Errorf("pending: got %v, want [10,20]", loaded.Pending)
	}
	if len(loaded.Running) != 1 {
		t.Errorf("running: expected 1, got %d", len(loaded.Running))
	}
	rs := loaded.Running["10"]
	if rs.StartedAt != 1000 {
		t.Errorf("running.started_at: got %d, want 1000", rs.StartedAt)
	}
	if len(rs.GPUDevices) != 1 || rs.GPUDevices[0] != "0" {
		t.Errorf("running.gpu_devices: got %v, want [0]", rs.GPUDevices)
	}
	if rs.RAMReservationKB != 32*gibKB {
		t.Errorf("running.ram_reservation_kb: got %d, want %d", rs.RAMReservationKB, 32*gibKB)
	}
}

func TestState_LoadMissing(t *testing.T) {
	s, err := LoadState("/nonexistent/path")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 0 {
		t.Error("expected empty pending")
	}
}

func TestState_JSONCompatibility(t *testing.T) {
	// Verify the Go state JSON is compatible with what the bash runner writes
	bashState := `{
		"cursor": "2024-01-01T00:00:00Z",
		"cursor_line": 3,
		"pending": [10, 20],
		"current": 10,
		"running": {
			"10": {
				"started_at": 1000,
				"warmup_until": 1120,
				"local_allotment": 30,
				"samples": [25, 30, 28],
				"over_count": 0,
				"under_count": 0,
				"gpu_devices": ["0", "1"],
				"gpu_mem_gb": 20
			}
		},
		"finished": {
			"5": {"exit_code": 0, "finished_at": 999}
		}
	}`

	s := NewState()
	if err := json.Unmarshal([]byte(bashState), s); err != nil {
		t.Fatal(err)
	}

	if s.Cursor != "2024-01-01T00:00:00Z" {
		t.Errorf("cursor: %q", s.Cursor)
	}
	if s.CursorLine != 3 {
		t.Errorf("cursor_line: %d", s.CursorLine)
	}
	if len(s.Pending) != 2 {
		t.Errorf("pending: %v", s.Pending)
	}
	if s.Current == nil || *s.Current != 10 {
		t.Errorf("current: %v", s.Current)
	}
	rs := s.Running["10"]
	if rs.StartedAt != 1000 {
		t.Errorf("started_at: %d", rs.StartedAt)
	}
	if len(rs.GPUDevices) != 2 {
		t.Errorf("gpu_devices: %v", rs.GPUDevices)
	}
	if rs.GPUMemGB != 20 {
		t.Errorf("gpu_mem_gb: %d", rs.GPUMemGB)
	}
	if len(rs.Samples) != 3 {
		t.Errorf("samples: %v", rs.Samples)
	}

	// Re-serialize and verify it's valid JSON
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}

	var roundTripped State
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("roundtrip failed: %v\nJSON: %s", err, data)
	}
}

func TestState_TotalAllotment(t *testing.T) {
	s := NewState()
	s.AddRunning("1", RunningJobState{LocalAllotment: 30})
	s.AddRunning("2", RunningJobState{LocalAllotment: 25})

	if total := s.TotalAllotment(); total != 55 {
		t.Errorf("expected 55, got %d", total)
	}
}

func TestState_StaleAttemptUpdateCannotRestoreReleasedSlot(t *testing.T) {
	s := NewState()
	first := RunningJobState{RunID: 11, StartedAt: 100, LocalAllotment: 25}
	s.AddRunning("42", first)

	if !s.FinishRunningAttempt("42", first, 0, 200) {
		t.Fatal("expected first attempt to release its slot")
	}
	stale := first
	stale.LocalAllotment = 50
	if s.UpdateRunningAttempt("42", first, stale) {
		t.Fatal("stale sampler restored a released slot")
	}
	if s.RunningCount() != 0 {
		t.Fatalf("running count = %d, want 0", s.RunningCount())
	}

	second := RunningJobState{RunID: 12, StartedAt: 300, LocalAllotment: 30}
	s.AddRunning("42", second)
	if s.UpdateRunningAttempt("42", first, stale) {
		t.Fatal("stale sampler updated a newer attempt")
	}
	got, ok := s.GetRunning("42")
	if !ok || got.RunID != second.RunID || got.LocalAllotment != second.LocalAllotment {
		t.Fatalf("new attempt changed: %+v", got)
	}
	if s.FinishRunningAttempt("42", first, 1, 400) {
		t.Fatal("old waiter released a newer attempt")
	}
}

func TestState_UnfencedAttemptCanUpdateButCannotReleaseSlot(t *testing.T) {
	s := NewState()
	legacy := RunningJobState{LocalAllotment: 25}
	s.AddRunning("42", legacy)

	updated := legacy
	updated.LocalAllotment = 40
	if !s.UpdateRunningAttempt("42", legacy, updated) {
		t.Fatal("legacy unfenced entry rejected an in-place observation update")
	}
	if got, _ := s.GetRunning("42"); got.LocalAllotment != 40 {
		t.Fatalf("local allotment = %d, want 40", got.LocalAllotment)
	}
	if s.FinishRunningAttempt("42", updated, 0, 200) {
		t.Fatal("unfenced terminal observation released scheduler occupancy")
	}
	if s.RunningCount() != 1 {
		t.Fatalf("running count = %d, want 1", s.RunningCount())
	}
}

func TestState_PruneFinished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := NewState()
	// Old entry (should be pruned)
	s.RecordFinished("1", 0, 1000)
	// Recent entry (should survive)
	s.RecordFinished("2", 0, 99999999999)

	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	// Reload
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}

	// Read the raw JSON to check
	data, _ := os.ReadFile(path)
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	var finished map[string]json.RawMessage
	json.Unmarshal(raw["finished"], &finished)

	if _, ok := finished["1"]; ok {
		t.Error("expected old finished entry to be pruned")
	}
	if _, ok := finished["2"]; !ok {
		t.Error("expected recent finished entry to survive")
	}
	_ = loaded
}

func TestState_SaveConcurrentWithMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := NewState()

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			jobID := strconv.Itoa(i % 8)
			state.AddRunning(jobID, RunningJobState{
				StartedAt:      time.Now().Unix(),
				LocalAllotment: (i % 50) + 1,
			})
			state.RecordFinished(jobID, 0, time.Now().Unix())
			state.RemoveRunning(jobID)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			state.AddPending(int64(i))
			if i > 0 {
				state.RemovePending(int64(i - 1))
			}
			_ = state.TotalAllotment()
			_, _ = state.CurrentJobID()
			_ = state.RunningIDs()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if err := state.Save(path); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
