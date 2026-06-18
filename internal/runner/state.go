package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// State tracks the runner's persistent state, saved to {queue}.state.json.
// Compatible with the bash runner's state format.
type State struct {
	mu             sync.RWMutex
	Cursor         string                      `json:"cursor"`
	CursorLine     int                         `json:"cursor_line"`
	Pending        []int64                     `json:"pending"`
	PendingReasons map[string]string           `json:"pending_reasons,omitempty"`
	Current        *int64                      `json:"current"`
	Running        map[string]RunningJobState  `json:"running,omitempty"`
	Finished       map[string]FinishedJobState `json:"finished,omitempty"`

	// StopRequested is not persisted — it's set from the command log each time.
	StopRequested bool `json:"-"`
}

// RunningJobState captures per-job runtime state for concurrent execution.
// Field names match the bash runner's JSON format for compatibility.
type RunningJobState struct {
	RunID          int64    `json:"run_id,omitempty"`
	StartedAt      int64    `json:"started_at"`
	WarmupUntil    int64    `json:"warmup_until"`
	LocalAllotment int      `json:"local_allotment"`
	Samples        []int    `json:"samples,omitempty"`
	OverCount      int      `json:"over_count"`
	UnderCount     int      `json:"under_count"`
	OverHist       []int    `json:"over_hist,omitempty"`
	UnderHist      []int    `json:"under_hist,omitempty"`
	GPUDevices     []string `json:"gpu_devices,omitempty"`
	GPUMemGB       int      `json:"gpu_mem_gb,omitempty"`
	DiskPath       string   `json:"disk_path,omitempty"`

	// Resource usage tracking (updated during sampling)
	RusageUserCPU string `json:"rusage_user_cpu,omitempty"`
	RusageSysCPU  string `json:"rusage_sys_cpu,omitempty"`
	RusagePeakRSS int64  `json:"rusage_peak_rss,omitempty"`
	RusageMaxGPU  int    `json:"rusage_max_gpu,omitempty"`

	// High-water marks (updated during sampling)
	PeakHostMemRatio float64          `json:"peak_host_mem_ratio,omitempty"`
	PeakRSSFromTS    int64            `json:"peak_rss_from_ts,omitempty"`
	FinalRSSKB       int64            `json:"final_rss_kb,omitempty"`
	PeakMemPressure  MemPressureLevel `json:"peak_mem_pressure,omitempty"`

	// Heartbeat / liveness (updated during sampling)
	LastHeartbeat int64 `json:"last_heartbeat,omitempty"`
	LastSample    int64 `json:"last_sample,omitempty"`

	// Previous counters for deriving per-sample throughput.
	TelemetryLastSampleAt    int64  `json:"telemetry_last_sample_at,omitempty"`
	TelemetryLastReadBytes   uint64 `json:"telemetry_last_read_bytes,omitempty"`
	TelemetryLastWriteBytes  uint64 `json:"telemetry_last_write_bytes,omitempty"`
	TelemetryIntervalSeconds int64  `json:"telemetry_interval_seconds,omitempty"`
	TelemetryAdvancedGPU     bool   `json:"telemetry_advanced_gpu,omitempty"`
}

// FinishedJobState records terminal state for deduplication.
type FinishedJobState struct {
	ExitCode   int   `json:"exit_code"`
	FinishedAt int64 `json:"finished_at"`
}

// NewState creates an empty state.
func NewState() *State {
	return &State{
		Pending:        []int64{},
		PendingReasons: make(map[string]string),
		Running:        make(map[string]RunningJobState),
		Finished:       make(map[string]FinishedJobState),
	}
}

// LoadState reads state from a JSON file.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if len(data) == 0 {
		return NewState(), nil
	}

	s := NewState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if s.Running == nil {
		s.Running = make(map[string]RunningJobState)
	}
	if s.Finished == nil {
		s.Finished = make(map[string]FinishedJobState)
	}
	if s.Pending == nil {
		s.Pending = []int64{}
	}
	if s.PendingReasons == nil {
		s.PendingReasons = make(map[string]string)
	}
	return s, nil
}

// Save writes state to a JSON file, pruning finished entries older than 24h.
func (s *State) Save(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneFinished()

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

// pruneFinished removes finished entries older than 24 hours.
func (s *State) pruneFinished() {
	cutoff := time.Now().Unix() - 86400
	for id, f := range s.Finished {
		if f.FinishedAt < cutoff {
			delete(s.Finished, id)
		}
	}
}

// CursorLineValue returns the last processed line from the command log.
func (s *State) CursorLineValue() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CursorLine
}

// SetCursorLine updates the command-log cursor line without changing the timestamp.
func (s *State) SetCursorLine(line int) {
	s.mu.Lock()
	s.CursorLine = line
	s.mu.Unlock()
}

// SetCursor updates the command-log cursor timestamp and line number together.
func (s *State) SetCursor(cursor string, line int) {
	s.mu.Lock()
	s.Cursor = cursor
	s.CursorLine = line
	s.mu.Unlock()
}

// AddPending adds a job ID to the end of the pending list.
// Removes any existing entry first to prevent duplicates.
func (s *State) AddPending(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addPendingLocked(jobID)
}

// AddPendingWithReason adds a job ID to the end of the pending list and records
// a transient gate reason for UI surfaces.
func (s *State) AddPendingWithReason(jobID int64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addPendingWithReasonLocked(jobID, reason)
}

// SetPendingReason records a transient gate reason without changing queue order.
func (s *State) SetPendingReason(jobID int64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobIDStr := fmt.Sprintf("%d", jobID)
	if !slices.Contains(s.Pending, jobID) {
		s.clearPendingReasonLocked(jobIDStr)
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		s.clearPendingReasonLocked(jobIDStr)
		return
	}
	s.PendingReasons[jobIDStr] = reason
}

// PriorityPending moves a job to the front of the pending list.
func (s *State) PriorityPending(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priorityPendingLocked(jobID)
}

// RemovePending removes a job from the pending list.
func (s *State) RemovePending(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removePendingLocked(jobID)
}

// PopPending removes and returns the first job from the pending list.
// Returns 0, false if the list is empty.
func (s *State) PopPending() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.popPendingLocked()
}

// PeekPending returns the first pending job without changing queue order.
func (s *State) PeekPending() (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.Pending) == 0 {
		return 0, false
	}
	return s.Pending[0], true
}

// PendingSnapshot returns the current pending queue order.
func (s *State) PendingSnapshot() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.Pending)
}

func (s *State) popPendingLocked() (int64, bool) {
	if len(s.Pending) == 0 {
		return 0, false
	}
	jobID := s.Pending[0]
	s.Pending = s.Pending[1:]
	s.clearPendingReasonLocked(fmt.Sprintf("%d", jobID))
	return jobID, true
}

// PendingEmpty returns true if there are no pending jobs.
func (s *State) PendingEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Pending) == 0
}

// RunningCount returns the number of running jobs.
func (s *State) RunningCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Running)
}

// RunningIDs returns the IDs of all running jobs.
func (s *State) RunningIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runningIDsLocked()
}

func (s *State) runningIDsLocked() []string {
	ids := make([]string, 0, len(s.Running))
	for id := range s.Running {
		ids = append(ids, id)
	}
	return ids
}

// RunningSnapshot returns a copy of the running jobs map for safe iteration.
func (s *State) RunningSnapshot() map[string]RunningJobState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshot := make(map[string]RunningJobState, len(s.Running))
	for id, rs := range s.Running {
		snapshot[id] = rs
	}
	return snapshot
}

// GetRunning returns the running-state entry for a job.
func (s *State) GetRunning(jobID string) (RunningJobState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs, ok := s.Running[jobID]
	return rs, ok
}

// SetRunning updates the running-state entry for a job.
func (s *State) SetRunning(jobID string, state RunningJobState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Running[jobID] = state
	s.clearPendingReasonLocked(jobID)
	s.updateCurrentLocked()
}

// AddRunning adds a job to the running set.
func (s *State) AddRunning(jobID string, state RunningJobState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Running[jobID] = state
	s.clearPendingReasonLocked(jobID)
	s.updateCurrentLocked()
}

// RemoveRunning removes a job from the running set.
func (s *State) RemoveRunning(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRunningLocked(jobID)
}

// RecordFinished records a job's terminal state.
func (s *State) RecordFinished(jobID string, exitCode int, finishedAt int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFinishedLocked(jobID, exitCode, finishedAt)
}

// IsStopRequested reports whether the runner should stop after draining work.
func (s *State) IsStopRequested() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.StopRequested
}

// SetStopRequested updates the stop-requested flag.
func (s *State) SetStopRequested(requested bool) {
	s.mu.Lock()
	s.StopRequested = requested
	s.mu.Unlock()
}

// CurrentJobID returns the current job marker, if any.
func (s *State) CurrentJobID() (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Current == nil {
		return 0, false
	}
	return *s.Current, true
}

func (s *State) addPendingLocked(jobID int64) {
	s.queuePendingLocked(jobID)
	s.clearPendingReasonLocked(fmt.Sprintf("%d", jobID))
}

func (s *State) addPendingWithReasonLocked(jobID int64, reason string) {
	s.queuePendingLocked(jobID)
	jobIDStr := fmt.Sprintf("%d", jobID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		s.clearPendingReasonLocked(jobIDStr)
		return
	}
	s.PendingReasons[jobIDStr] = reason
}

func (s *State) priorityPendingLocked(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
	s.Pending = slices.Insert(s.Pending, 0, jobID)
	s.clearFinishedLocked(fmt.Sprintf("%d", jobID))
	s.clearPendingReasonLocked(fmt.Sprintf("%d", jobID))
}

func (s *State) removePendingLocked(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
	s.clearPendingReasonLocked(fmt.Sprintf("%d", jobID))
}

func (s *State) removeRunningLocked(jobID string) {
	delete(s.Running, jobID)
	s.updateCurrentLocked()
}

func (s *State) recordFinishedLocked(jobID string, exitCode int, finishedAt int64) {
	s.Finished[jobID] = FinishedJobState{
		ExitCode:   exitCode,
		FinishedAt: finishedAt,
	}
	s.clearPendingReasonLocked(jobID)
}

func (s *State) clearFinishedLocked(jobID string) {
	delete(s.Finished, jobID)
}

func (s *State) clearPendingReasonLocked(jobID string) {
	delete(s.PendingReasons, jobID)
}

func (s *State) queuePendingLocked(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
	s.Pending = append(s.Pending, jobID)
	s.clearFinishedLocked(fmt.Sprintf("%d", jobID))
}

// TotalAllotment returns the sum of all running jobs' local_allotment values.
func (s *State) TotalAllotment() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, rs := range s.Running {
		total += rs.LocalAllotment
	}
	return total
}

// updateCurrent sets Current to the most recently started running job.
func (s *State) updateCurrentLocked() {
	if len(s.Running) == 0 {
		s.Current = nil
		return
	}
	var bestID string
	var bestStart int64
	for id, rs := range s.Running {
		if rs.StartedAt > bestStart {
			bestStart = rs.StartedAt
			bestID = id
		}
	}
	if bestID != "" {
		// Parse the ID back to int64 for the Current field
		var id int64
		if _, err := fmt.Sscanf(bestID, "%d", &id); err == nil {
			s.Current = &id
		}
	}
}
