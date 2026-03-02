package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"
)

// State tracks the runner's persistent state, saved to {queue}.state.json.
// Compatible with the bash runner's state format.
type State struct {
	Cursor     string                      `json:"cursor"`
	CursorLine int                         `json:"cursor_line"`
	Pending    []int64                     `json:"pending"`
	Current    *int64                      `json:"current"`
	Running    map[string]RunningJobState  `json:"running,omitempty"`
	Finished   map[string]FinishedJobState `json:"finished,omitempty"`

	// StopRequested is not persisted — it's set from the command log each time.
	StopRequested bool `json:"-"`
}

// RunningJobState captures per-job runtime state for concurrent execution.
// Field names match the bash runner's JSON format for compatibility.
type RunningJobState struct {
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

	// Resource usage tracking (updated during sampling)
	RusageUserCPU string `json:"rusage_user_cpu,omitempty"`
	RusageSysCPU  string `json:"rusage_sys_cpu,omitempty"`
	RusagePeakRSS string `json:"rusage_peak_rss,omitempty"`
	RusageMaxGPU  string `json:"rusage_max_gpu,omitempty"`

	// High-water marks (updated during sampling)
	PeakHostMemRatio float64 `json:"peak_host_mem_ratio,omitempty"`
	PeakRSSFromTS    int64   `json:"peak_rss_from_ts,omitempty"`
	PeakMemPressure  string  `json:"peak_mem_pressure,omitempty"`

	// Heartbeat / liveness (updated during sampling)
	LastHeartbeat int64 `json:"last_heartbeat,omitempty"`
	LastSample    int64 `json:"last_sample,omitempty"`
}

// FinishedJobState records terminal state for deduplication.
type FinishedJobState struct {
	ExitCode   int   `json:"exit_code"`
	FinishedAt int64 `json:"finished_at"`
}

// NewState creates an empty state.
func NewState() *State {
	return &State{
		Pending:  []int64{},
		Running:  make(map[string]RunningJobState),
		Finished: make(map[string]FinishedJobState),
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
	return s, nil
}

// Save writes state to a JSON file, pruning finished entries older than 24h.
func (s *State) Save(path string) error {
	s.pruneFinished()

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
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

// AddPending adds a job ID to the end of the pending list.
// Removes any existing entry first to prevent duplicates.
func (s *State) AddPending(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
	s.Pending = append(s.Pending, jobID)
}

// PriorityPending moves a job to the front of the pending list.
func (s *State) PriorityPending(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
	s.Pending = slices.Insert(s.Pending, 0, jobID)
}

// RemovePending removes a job from the pending list.
func (s *State) RemovePending(jobID int64) {
	s.Pending = slices.DeleteFunc(s.Pending, func(id int64) bool { return id == jobID })
}

// PopPending removes and returns the first job from the pending list.
// Returns 0, false if the list is empty.
func (s *State) PopPending() (int64, bool) {
	if len(s.Pending) == 0 {
		return 0, false
	}
	jobID := s.Pending[0]
	s.Pending = s.Pending[1:]
	return jobID, true
}

// PendingEmpty returns true if there are no pending jobs.
func (s *State) PendingEmpty() bool {
	return len(s.Pending) == 0
}

// RunningCount returns the number of running jobs.
func (s *State) RunningCount() int {
	return len(s.Running)
}

// RunningIDs returns the IDs of all running jobs.
func (s *State) RunningIDs() []string {
	ids := make([]string, 0, len(s.Running))
	for id := range s.Running {
		ids = append(ids, id)
	}
	return ids
}

// AddRunning adds a job to the running set.
func (s *State) AddRunning(jobID string, state RunningJobState) {
	s.Running[jobID] = state
	s.updateCurrent()
}

// RemoveRunning removes a job from the running set.
func (s *State) RemoveRunning(jobID string) {
	delete(s.Running, jobID)
	s.updateCurrent()
}

// RecordFinished records a job's terminal state.
func (s *State) RecordFinished(jobID string, exitCode int, finishedAt int64) {
	s.Finished[jobID] = FinishedJobState{
		ExitCode:   exitCode,
		FinishedAt: finishedAt,
	}
}

// TotalAllotment returns the sum of all running jobs' local_allotment values.
func (s *State) TotalAllotment() int {
	total := 0
	for _, rs := range s.Running {
		total += rs.LocalAllotment
	}
	return total
}

// updateCurrent sets Current to the most recently started running job.
func (s *State) updateCurrent() {
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
