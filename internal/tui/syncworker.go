package tui

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
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
	Host               string
	Updated            int
	QueueStarted       bool
	QueueDispatchError string
	QueueRunnerError   string
	HostWarning        string
	HostInfo           *db.CachedHostInfo      // Refreshed host info (nil on error)
	HostFull           *Host                   // Full host with dynamic metrics (nil on error)
	QueueStatus        *queuerunner.StatusInfo // Queue runner status (nil on error)
	Error              error
}

func buildSyncWarning(result ops.HostSyncResult) string {
	var parts []string
	if result.QueueRunnerError != "" {
		parts = append(parts, result.QueueRunnerError)
	}
	if result.QueueDispatchError != "" {
		parts = append(parts, "queue dispatch failed: "+result.QueueDispatchError)
	}
	return strings.Join(parts, "; ")
}

// hostSyncState tracks sync state for a single host
type hostSyncState struct {
	lastSync      time.Time
	requestedRate SyncRate
	inProgress    bool
}

// SyncWorker manages background sync operations
type SyncWorker struct {
	database     *sql.DB
	cloudClients []cloud.Client
	r2Client     *r2.Client
	appConfig    *config.Config
	requests     chan SyncRequest
	results      chan SyncResult

	mu         sync.Mutex
	hostState  map[string]*hostSyncState
	reconciler *campaign.Reconciler

	// Configuration
	maxParallel int
	inFlight    int

	// For graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSyncWorker creates a new sync worker
func NewSyncWorker(database *sql.DB, cloudClients []cloud.Client, r2Client *r2.Client, appConfig *config.Config) *SyncWorker {
	ctx, cancel := context.WithCancel(context.Background())
	return &SyncWorker{
		database:     database,
		cloudClients: cloudClients,
		r2Client:     r2Client,
		appConfig:    appConfig,
		requests:     make(chan SyncRequest, 100),
		results:      make(chan SyncResult, 100),
		hostState:    make(map[string]*hostSyncState),
		reconciler:   campaign.NewReconciler(),
		maxParallel:  3,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Start begins the sync worker goroutine
func (w *SyncWorker) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop gracefully shuts down the worker, waiting up to 500ms for in-flight syncs.
func (w *SyncWorker) Stop() {
	w.cancel()
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
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

// SetCloudClients updates the cloud clients used by periodic cloud reconciliation.
func (w *SyncWorker) SetCloudClients(clients []cloud.Client) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cloudClients = append([]cloud.Client(nil), clients...)
}

func (w *SyncWorker) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	benchTicker := time.NewTicker(60 * time.Second)
	defer benchTicker.Stop()

	cloudReconcileTicker := time.NewTicker(10 * time.Second)
	defer cloudReconcileTicker.Stop()

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

		case <-cloudReconcileTicker.C:
			w.reconcileCloudJobs()
		}
	}
}

// checkUnplacedJobs looks for unplaced queued jobs and tries to place them on
// hosts that have become available since the original placement failed, then
// tries to submit remaining jobs to running cloud instances.
func (w *SyncWorker) checkUnplacedJobs() {
	jobs, err := db.ListUnplacedJobs(w.database)
	if err != nil {
		slog.Warn("failed to list unplaced jobs", "component", "tui", "error", err)
		return
	}

	var cloudEligible []*db.Job
	for _, j := range jobs {
		if j.LaunchID != nil && *j.LaunchID != 0 {
			continue
		}

		constraints := placement.Constraints{
			GPUClass: j.GPUClass,
			Inputs:   j.Inputs,
			Command:  j.Command,
			Project:  j.Project,
			Tags:     j.Tags,
		}
		if j.GPUMemGB != nil {
			constraints.GPUMemGB = *j.GPUMemGB
		}

		predict := placement.BuildJobPredictorFromConfig(w.appConfig, constraints)
		result, err := placement.PlaceWithFallback(w.database, constraints, predict)
		if err != nil {
			if !j.HasTag(db.TagInventory) {
				cloudEligible = append(cloudEligible, j)
			}
			continue
		}

		assigned, err := db.AssignJobHost(w.database, j.ID, result.Host)
		if err != nil {
			slog.Warn("failed to assign unplaced job", "component", "tui", "job_id", j.ID, "error", err)
			continue
		}
		if assigned {
			slog.Info("assigned unplaced job to host", "component", "tui", "job_id", j.ID, "host", result.Host)
			// Trigger a sync for the target host
			select {
			case w.results <- SyncResult{Host: result.Host, Updated: 1}:
			default:
			}
		}
	}

	if len(cloudEligible) > 0 {
		w.tryCloudReuseForJobs(cloudEligible)
	}
}

// tryCloudReuseForJobs attempts to submit unplaced jobs to compatible running
// cloud instances, closing the race where jobs are created while instances are
// launching.
func (w *SyncWorker) tryCloudReuseForJobs(jobs []*db.Job) {
	w.mu.Lock()
	r2Client := w.r2Client
	w.mu.Unlock()

	if r2Client == nil {
		return
	}

	instances, err := campaign.FindReusableInstances(w.database)
	if err != nil || len(instances) == 0 {
		return
	}

	assignments, _ := campaign.PlanReuse(jobs, instances)
	if len(assignments) == 0 {
		return
	}

	// Group assignments by instance ID
	byInstance := make(map[int64][]*db.Job)
	for _, a := range assignments {
		byInstance[a.Instance.Instance.ID] = append(byInstance[a.Instance.Instance.ID], a.Job)
	}

	totalSubmitted := 0
	ctx := w.ctx
	for instanceID, instJobs := range byInstance {
		if err := campaign.SubmitJobsToInstance(ctx, w.database, r2Client, instanceID, instJobs); err != nil {
			slog.Warn("cloud auto-reuse submit failed", "component", "tui", "instance", instanceID, "error", err)
			continue
		}

		jobIDs := make([]string, len(instJobs))
		for i, j := range instJobs {
			jobIDs[i] = fmt.Sprintf("#%d", j.ID)
		}
		slog.Info("cloud auto-reuse submitted jobs", "component", "tui", "count", len(instJobs), "instance", instanceID, "jobs", strings.Join(jobIDs, ", "))
		totalSubmitted += len(instJobs)

		// Auto-extend grace if deadline is close
		inst, _ := db.GetLaunch(w.database, instanceID)
		if inst != nil && inst.Status == db.LaunchStatusGrace && inst.GraceDeadline != nil {
			remaining := time.Until(time.Unix(*inst.GraceDeadline, 0))
			if remaining < campaign.MinGraceRemaining {
				extendDur := 15 * time.Minute
				extendKey := r2keys.GraceExtend(instanceID)
				_ = r2Client.PutObject(ctx, extendKey, strings.NewReader(extendDur.String()), "text/plain")
				newDeadline := time.Now().Add(extendDur).Unix()
				_ = db.ExtendLaunchGrace(w.database, instanceID, newDeadline)
				slog.Info("auto-extended grace period", "component", "tui", "instance", instanceID, "duration", extendDur)
			}
		}
	}

	if totalSubmitted > 0 {
		select {
		case w.results <- SyncResult{Updated: totalSubmitted}:
		default:
		}
	}
}

// reconcileCloudJobs runs full cloud reconciliation: queries provider APIs,
// discovers dead instances, resets orphaned jobs, and auto-closes campaigns.
func (w *SyncWorker) reconcileCloudJobs() {
	w.mu.Lock()
	cloudClients := append([]cloud.Client(nil), w.cloudClients...)
	r2Client := w.r2Client
	w.mu.Unlock()

	if len(cloudClients) == 0 {
		// No cloud clients — fall back to DB-only reconciliation
		resetMap, err := db.ResetJobsOnTerminalLaunches(w.database)
		if err != nil {
			slog.Warn("cloud reconcile failed", "component", "tui", "error", err)
			return
		}
		if len(resetMap) > 0 {
			slog.Info("cloud reconcile reset jobs on terminal instances", "component", "tui", "count", len(resetMap))
			select {
			case w.results <- SyncResult{Updated: len(resetMap)}:
			default:
			}
		}
		return
	}

	result := cloudsync.SyncState(w.database, w.reconciler, cloudClients, r2Client, nil)
	if result.ReconcileResult != nil && result.ReconcileResult.Reconciled > 0 {
		slog.Info("cloud reconcile completed", "component", "tui", "reconciled", result.ReconcileResult.Reconciled)
		select {
		case w.results <- SyncResult{Updated: result.ReconcileResult.Reconciled}:
		default:
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

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.doSync(host)
	}()
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
		Logger:       ops.NewSilentSyncLogger(),
	}, ensureQueueRunnerStartedTUI)
	if err != nil {
		result.Error = err
		return
	}

	result.Updated = syncResult.Updated
	result.QueueStarted = syncResult.QueueStarted
	result.QueueDispatchError = syncResult.QueueDispatchError
	result.QueueRunnerError = syncResult.QueueRunnerError
	result.HostWarning = buildSyncWarning(syncResult)

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

// WaitForResult returns a cmd that blocks until the next sync result is
// available or ctx is cancelled, then wraps it with the provided function.
func (w *SyncWorker) WaitForResult(ctx context.Context, wrap func(SyncResult) tea.Msg) tea.Cmd {
	if w == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case result, ok := <-w.Results():
			if !ok {
				return nil
			}
			return wrap(result)
		case <-w.ctx.Done():
			return nil
		case <-ctx.Done():
			return nil
		}
	}
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
	hostRef := func() *Host {
		for _, host := range m.hosts {
			if host.Name == result.Host {
				return host
			}
		}
		return nil
	}
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
	if host := hostRef(); host != nil {
		host.SyncWarning = result.HostWarning
	} else if result.HostWarning != "" {
		m.hosts = append(m.hosts, &Host{
			Name:        result.Host,
			SyncWarning: result.HostWarning,
		})
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

				if result.HostWarning == "" && !result.QueueStatus.RunnerActive && !m.queueStoppedWarnedHosts[result.Host] {
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
	if result.HostWarning != "" {
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Sync warning (%s): %s", result.Host, result.HostWarning), true))
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
		switch job.EffectiveStatus() {
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
