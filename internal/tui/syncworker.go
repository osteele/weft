package tui

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queuerunner"
)

// SyncRate represents the desired sync frequency for a host
type SyncRate int

const (
	RateIdle    SyncRate = iota // No active jobs: 60s
	RateQueued                  // Queued but not running: 30s
	RateRunning                 // Jobs running: 10s
	RateWarmup                  // Job just started: 5s
)

func (r SyncRate) Interval() time.Duration {
	switch r {
	case RateWarmup:
		return 5 * time.Second
	case RateRunning:
		return 10 * time.Second
	case RateQueued:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}

func (r SyncRate) String() string {
	switch r {
	case RateWarmup:
		return "warmup"
	case RateRunning:
		return "running"
	case RateQueued:
		return "queued"
	default:
		return "idle"
	}
}

// SyncRequest is a request to sync a host at a given rate
type SyncRequest struct {
	Host     string
	Rate     SyncRate
	Priority bool // User-initiated gets priority
}

// SyncResult is the result of a sync operation
type SyncResult struct {
	Host             string
	Updated          int
	QueueStarted     bool
	QueueRunnerError string
	HostInfo         *db.CachedHostInfo      // Refreshed host info (nil on error)
	HostFull         *Host                   // Full host with dynamic metrics (nil on error)
	QueueStatus      *queuerunner.StatusInfo // Queue runner status (nil on error)
	Error            error
}

// hostSyncState tracks sync state for a single host
type hostSyncState struct {
	lastSync      time.Time
	requestedRate SyncRate
	inProgress    bool
}

// SyncWorker manages background sync operations
type SyncWorker struct {
	database *sql.DB
	requests chan SyncRequest
	results  chan SyncResult

	mu        sync.Mutex
	hostState map[string]*hostSyncState

	// Configuration
	maxParallel int
	inFlight    int

	// For graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSyncWorker creates a new sync worker
func NewSyncWorker(database *sql.DB) *SyncWorker {
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncWorker{
		database:    database,
		requests:    make(chan SyncRequest, 100),
		results:     make(chan SyncResult, 100),
		hostState:   make(map[string]*hostSyncState),
		maxParallel: 3,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start begins the sync worker goroutine
func (w *SyncWorker) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop gracefully shuts down the worker
func (w *SyncWorker) Stop() {
	w.cancel()
	w.wg.Wait()
	close(w.results)
}

// Request sends a sync request (non-blocking)
func (w *SyncWorker) Request(req SyncRequest) {
	select {
	case w.requests <- req:
	default:
		// Channel full, drop request (will be re-requested on next tick)
	}
}

// Results returns the channel for receiving sync results
func (w *SyncWorker) Results() <-chan SyncResult {
	return w.results
}

func (w *SyncWorker) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	benchTicker := time.NewTicker(60 * time.Second)
	defer benchTicker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return

		case req := <-w.requests:
			w.handleRequest(req)

		case <-ticker.C:
			w.processQueue()

		case <-benchTicker.C:
			w.checkUnplacedJobs()
		}
	}
}

// checkUnplacedJobs looks for unplaced queued jobs and tries to place them on
// hosts that have become available since the original placement failed.
func (w *SyncWorker) checkUnplacedJobs() {
	jobs, err := db.ListUnplacedJobs(w.database)
	if err != nil {
		log.Printf("unplaced re-placement: list unplaced: %v", err)
		return
	}

	for _, j := range jobs {
		if j.CloudInstanceID != nil && *j.CloudInstanceID != 0 {
			continue
		}

		constraints := placement.Constraints{
			GPUClass: j.GPUClass,
			Inputs:   j.Inputs,
			Tags:     j.Tags,
		}
		if j.GPUMemGB != nil {
			constraints.GPUMemGB = *j.GPUMemGB
		}

		result, err := placement.PlaceWithFallback(w.database, constraints, nil)
		if err != nil {
			continue
		}

		assigned, err := db.AssignJobHost(w.database, j.ID, result.Host)
		if err != nil {
			log.Printf("unplaced re-placement: assign job %d: %v", j.ID, err)
			continue
		}
		if assigned {
			log.Printf("unplaced re-placement: assigned job %d to %s", j.ID, result.Host)
			// Trigger a sync for the target host
			select {
			case w.results <- SyncResult{Host: result.Host, Updated: 1}:
			default:
			}
		}
	}
}

func (w *SyncWorker) handleRequest(req SyncRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, exists := w.hostState[req.Host]
	if !exists {
		state = &hostSyncState{}
		w.hostState[req.Host] = state
	}

	// Update requested rate (take the faster rate if multiple requests)
	if req.Rate < state.requestedRate || req.Priority {
		state.requestedRate = req.Rate
	}

	// Priority requests trigger immediate sync check
	if req.Priority && !state.inProgress {
		w.maybeStartSync(req.Host, state)
	}
}

func (w *SyncWorker) processQueue() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for host, state := range w.hostState {
		w.maybeStartSync(host, state)
	}
}

// maybeStartSync checks if a host should be synced and starts it
// Must be called with w.mu held
func (w *SyncWorker) maybeStartSync(host string, state *hostSyncState) {
	if state.inProgress {
		return
	}

	if w.inFlight >= w.maxParallel {
		return
	}

	interval := state.requestedRate.Interval()
	if time.Since(state.lastSync) < interval {
		return
	}

	// Start sync
	state.inProgress = true
	state.lastSync = time.Now()
	w.inFlight++

	go w.doSync(host)
}

func (w *SyncWorker) doSync(host string) {
	result := SyncResult{Host: host}

	defer func() {
		w.mu.Lock()
		if state, exists := w.hostState[host]; exists {
			state.inProgress = false
		}
		w.inFlight--
		w.mu.Unlock()

		// Send result (non-blocking)
		select {
		case w.results <- result:
		default:
		}
	}()

	syncResult, err := ops.SyncHost(w.database, host, ops.HostSyncOptions{
		Timeout:      ops.DefaultSyncOptions().Timeout,
		UseBatchSync: true,
	}, ensureQueueRunnerStartedTUI)
	if err != nil {
		result.Error = err
		return
	}

	result.Updated = syncResult.Updated
	result.QueueStarted = syncResult.QueueStarted
	result.QueueRunnerError = syncResult.QueueRunnerError

	// Fetch host info and queue status in a single SSH call (best-effort)
	if hostStatus, err := ops.FetchHostStatusCombined(w.database, host, queuerunner.StatusCommand(), 10*time.Second); err == nil {
		result.HostInfo = hostStatus.HostInfo
		result.HostFull = hostStatus.Host
		if hostStatus.ExtraOutput != "" {
			result.QueueStatus = queuerunner.ParseStatus(hostStatus.ExtraOutput)
		}
	}
}

// syncResultMsg wraps a SyncResult for the TUI message loop
type syncResultMsg struct {
	result SyncResult
}

// Model methods for sync worker integration

// requestSyncsForActiveHosts sends sync requests for all hosts with jobs
func (m *Model) requestSyncsForActiveHosts() {
	if m.syncWorker == nil {
		return
	}

	// Group jobs by host
	jobsByHost := make(map[string][]*db.Job)
	for _, job := range m.allJobs {
		if job != nil {
			jobsByHost[job.Host] = append(jobsByHost[job.Host], job)
		}
	}

	// Request sync for each host at appropriate rate
	for host, jobs := range jobsByHost {
		rate := GetHostSyncRate(jobs)
		m.syncWorker.Request(SyncRequest{
			Host: host,
			Rate: rate,
		})
	}

	// Also request sync for all known hosts at idle rate
	// This ensures hosts with queued jobs (that may be filtered out of m.allJobs)
	// still get synced and have their queue runners started
	for _, host := range m.hosts {
		if _, hasJobs := jobsByHost[host.Name]; !hasJobs {
			m.syncWorker.Request(SyncRequest{
				Host: host.Name,
				Rate: RateIdle,
			})
		}
	}
}

// requestSyncAllHosts requests sync for all known hosts
func (m *Model) requestSyncAllHosts(priority bool) {
	if m.syncWorker == nil {
		return
	}

	// Get unique hosts
	hosts := make(map[string]bool)
	for _, job := range m.allJobs {
		if job != nil {
			hosts[job.Host] = true
		}
	}
	for _, host := range m.hosts {
		hosts[host.Name] = true
	}

	// Request sync for each host
	for host := range hosts {
		m.syncWorker.Request(SyncRequest{
			Host:     host,
			Rate:     RateRunning, // Use fast rate for manual sync
			Priority: priority,
		})
	}
}

// checkSyncResults returns a command that checks for sync results
func (m *Model) checkSyncResults() tea.Cmd {
	if m.syncWorker == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case result, ok := <-m.syncWorker.Results():
			if ok {
				return syncResultMsg{result: result}
			}
		default:
			// No results available
		}
		return nil
	}
}

// handleSyncResult handles a sync result from the worker
func (m Model) handleSyncResult(msg syncResultMsg) (Model, tea.Cmd) {
	result := msg.result

	// Update host sync times
	now := time.Now()
	m.lastHostSyncTimes[result.Host] = now
	m.hostSyncTimes[result.Host] = now

	if result.Error != nil {
		// Hysteresis: only mark offline after consecutive failures
		m.hostFailCount[result.Host]++
		if m.hostFailCount[result.Host] >= 3 {
			for i, h := range m.hosts {
				if h.Name == result.Host {
					m.hosts[i].Status = HostStatusOffline
					m.hosts[i].Error = result.Error.Error()
					break
				}
			}
		}
		return m, m.setFlash(fmt.Sprintf("Sync error (%s): %v", result.Host, result.Error), true)
	}
	m.hostFailCount[result.Host] = 0

	var cmds []tea.Cmd

	// Apply host info from sync result (prefer HostFull for dynamic metrics)
	if result.HostFull != nil || result.HostInfo != nil {
		var host *Host
		if result.HostFull != nil {
			host = result.HostFull
			host.Status = HostStatusOnline
		} else {
			host = hostFromCachedInfo(result.HostInfo)
		}
		m.hostsQueriedThisSession[result.Host] = true
		found := false
		for i, h := range m.hosts {
			if h.Name == result.Host {
				m.hosts[i].UpdateFrom(host)
				found = true
				break
			}
		}
		if !found {
			m.hosts = append(m.hosts, host)
		}
	}

	// Apply queue status from sync result
	if result.QueueStatus != nil {
		for i, h := range m.hosts {
			if h.Name == result.Host {
				m.hosts[i].QueueStatus = QueueCheckChecked
				m.hosts[i].QueueRunnerActive = result.QueueStatus.RunnerActive
				m.hosts[i].QueuedJobCount = result.QueueStatus.QueuedJobCount
				m.hosts[i].CurrentQueueJob = result.QueueStatus.CurrentJob
				m.hosts[i].QueueStopPending = result.QueueStatus.StopPending

				if !result.QueueStatus.RunnerActive && !m.queueStoppedWarnedHosts[result.Host] {
					queuedCount, _ := db.CountQueuedByHost(m.database, result.Host)
					if queuedCount > 0 {
						m.queueStoppedWarnedHosts[result.Host] = true
						cmds = append(cmds, m.setFlash(
							fmt.Sprintf("Warning: Queue runner on %s is not running but %d job(s) are waiting. Press 'S' to start it.", result.Host, queuedCount),
							true,
						))
					}
				}
				break
			}
		}
	}

	// Flash message for significant events
	if result.QueueStarted {
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Started queue on %s", result.Host), false))
	}
	if result.QueueRunnerError != "" {
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Queue runner error (%s): %s", result.Host, result.QueueRunnerError), true))
	}

	// Refresh jobs if updates occurred
	if result.Updated > 0 {
		cmds = append(cmds, m.refreshJobs())
	}

	// Check for more results
	cmds = append(cmds, m.checkSyncResults())

	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

// GetHostSyncRate determines the appropriate sync rate for a host based on job status
func GetHostSyncRate(jobs []*db.Job) SyncRate {
	hasRunning := false
	hasQueued := false
	hasRecentStart := false

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch job.Status {
		case db.StatusRunning, db.StatusStarting:
			hasRunning = true
			if job.StartTime > 0 {
				startTime := time.Unix(job.StartTime, 0)
				if time.Since(startTime) < 2*time.Minute {
					hasRecentStart = true
				}
			}
		case db.StatusQueued:
			hasQueued = true
		}
	}

	if hasRecentStart {
		return RateWarmup
	}
	if hasRunning {
		return RateRunning
	}
	if hasQueued {
		return RateQueued
	}
	return RateIdle
}
