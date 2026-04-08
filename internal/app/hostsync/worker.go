package hostsync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/syncorch"
)

// SyncRate describes how aggressively a host should be refreshed.
type SyncRate int

const (
	RateIdle SyncRate = iota
	RateQueued
	RateRunning
	RateWarmup
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

// Request asks the worker to sync one host.
type Request struct {
	Host     string
	Rate     SyncRate
	Priority bool
	Mode     ops.SyncMode
}

// Result contains the outcome of a host sync or cloud reconciliation pass.
type Result struct {
	Host               string
	Updated            int
	QueueStarted       bool
	QueueDispatchError string
	QueueRunnerError   string
	HostWarning        string
	HostInfo           *db.CachedHostInfo
	HostFull           *hostinfo.Host
	QueueStatus        *queuerunner.StatusInfo
	Error              error
}

func buildWarning(result ops.HostSyncResult) string {
	var parts []string
	if result.QueueRunnerError != "" {
		parts = append(parts, result.QueueRunnerError)
	}
	if result.QueueDispatchError != "" {
		parts = append(parts, "queue dispatch failed: "+result.QueueDispatchError)
	}
	return strings.Join(parts, "; ")
}

// BuildWarning converts a sync result into a user-facing warning string.
func BuildWarning(result ops.HostSyncResult) string {
	return buildWarning(result)
}

type hostSyncState struct {
	lastSync      time.Time
	lastRepair    time.Time
	requestedRate SyncRate
	requestedMode ops.SyncMode
	inProgress    bool
}

// Worker manages background sync operations for inventory hosts and cloud reuse.
type Worker struct {
	database     *sql.DB
	cloudClients []cloud.Client
	r2Client     *r2.Client
	appConfig    *config.Config
	requests     chan Request
	results      chan Result

	mu         sync.Mutex
	hostState  map[string]*hostSyncState
	reconciler *campaign.Reconciler

	maxParallel int
	inFlight    int
	owner       string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a worker.
func New(database *sql.DB, cloudClients []cloud.Client, r2Client *r2.Client, appConfig *config.Config) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		database:     database,
		cloudClients: cloudClients,
		r2Client:     r2Client,
		appConfig:    appConfig,
		requests:     make(chan Request, 100),
		results:      make(chan Result, 100),
		hostState:    make(map[string]*hostSyncState),
		reconciler:   campaign.NewReconciler(),
		maxParallel:  3,
		owner:        syncOwnerID(),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Start begins the worker loop.
func (w *Worker) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop shuts down the worker.
func (w *Worker) Stop() {
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

// Request queues a host sync request.
func (w *Worker) Request(req Request) {
	select {
	case w.requests <- req:
	default:
	}
}

// Results returns the result stream.
func (w *Worker) Results() <-chan Result {
	return w.results
}

// SetCloudClients replaces the clients used for cloud reconciliation.
func (w *Worker) SetCloudClients(clients []cloud.Client) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cloudClients = append([]cloud.Client(nil), clients...)
}

// WaitForResult blocks until the next result arrives or either context ends.
func (w *Worker) WaitForResult(ctx context.Context, wrap func(Result) tea.Msg) tea.Cmd {
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

func (w *Worker) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	benchTicker := time.NewTicker(120 * time.Second)
	defer benchTicker.Stop()

	repairTicker := time.NewTicker(120 * time.Second)
	defer repairTicker.Stop()

	cloudReconcileTicker := time.NewTicker(60 * time.Second)
	defer cloudReconcileTicker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case req := <-w.requests:
			w.handleRequest(req)
		case <-ticker.C:
			w.processQueue()
		case <-repairTicker.C:
			w.processRepairQueue()
		case <-benchTicker.C:
			w.checkUnplacedJobs()
		case <-cloudReconcileTicker.C:
			w.reconcileCloudJobs()
		}
	}
}

func (w *Worker) checkUnplacedJobs() {
	ok, err := db.AcquireAutoLease(w.database, "sync:unplaced:auto", w.owner, 180*time.Second)
	if err != nil || !ok {
		return
	}
	defer func() { _ = db.ReleaseAutoLease(w.database, "sync:unplaced:auto", w.owner) }()

	jobs, err := db.ListUnplacedJobs(w.database)
	if err != nil {
		slog.Warn("failed to list unplaced jobs", "component", "hostsync", "error", err)
		return
	}

	sources := []placement.CandidateSource{&placement.OnPremSource{}}
	w.mu.Lock()
	r2Client := w.r2Client
	w.mu.Unlock()
	if r2Client != nil {
		sources = append(sources, &campaign.ReuseSource{})
	}

	totalSubmitted := 0
	for _, j := range jobs {
		if j.LaunchID != nil && *j.LaunchID != 0 {
			continue
		}

		jobSources := sources
		if j.HasTag(db.TagInventory) {
			jobSources = []placement.CandidateSource{&placement.OnPremSource{}}
		}

		constraints := placement.ConstraintsFromJob(j)
		predict := placement.BuildJobPredictorFromConfig(w.appConfig, constraints)
		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: constraints,
			Predictor:   predict,
			Sources:     jobSources,
			Database:    w.database,
		})
		if err != nil || plan.Unplaced {
			continue
		}

		pick := plan.Fast
		if pick == nil {
			pick = plan.Cheap
		}
		if pick == nil {
			continue
		}

		switch pick.Kind {
		case placement.CandidateOnPrem:
			if pick.OnPrem == nil {
				continue
			}
			assigned, err := db.AssignJobHost(w.database, j.ID, pick.OnPrem.Host)
			if err != nil {
				slog.Warn("failed to assign unplaced job", "component", "hostsync", "job_id", j.ID, "error", err)
				continue
			}
			if assigned {
				slog.Info("assigned unplaced job to host", "component", "hostsync", "job_id", j.ID, "host", pick.OnPrem.Host)
				select {
				case w.results <- Result{Host: pick.OnPrem.Host, Updated: 1}:
				default:
				}
			}

		case placement.CandidateCloudReuse:
			if pick.Reuse == nil || r2Client == nil {
				continue
			}
			instanceID := pick.Reuse.InstanceID
			if err := campaign.SubmitJobsToInstance(w.ctx, w.database, r2Client, instanceID, []*db.Job{j}); err != nil {
				slog.Warn("cloud auto-reuse submit failed", "component", "hostsync", "instance", instanceID, "error", err)
				continue
			}
			slog.Info("cloud auto-reuse submitted job", "component", "hostsync", "job_id", j.ID, "instance", instanceID)
			totalSubmitted++

			inst, _ := db.GetLaunch(w.database, instanceID)
			if inst != nil && inst.Status == db.LaunchStatusGrace && inst.GraceDeadline != nil {
				remaining := time.Until(time.Unix(*inst.GraceDeadline, 0))
				if remaining < campaign.MinGraceRemaining {
					extendDur := 15 * time.Minute
					_, _ = controlplane.SendGraceExtend(w.ctx, r2Client, instanceID, extendDur)
					newDeadline := time.Now().Add(extendDur).Unix()
					_ = db.ExtendLaunchGrace(w.database, instanceID, newDeadline)
					slog.Info("auto-extended grace period", "component", "hostsync", "instance", instanceID, "duration", extendDur)
				}
			}
		}
	}

	if totalSubmitted > 0 {
		select {
		case w.results <- Result{Updated: totalSubmitted}:
		default:
		}
	}
}

func (w *Worker) reconcileCloudJobs() {
	w.mu.Lock()
	cloudClients := append([]cloud.Client(nil), w.cloudClients...)
	r2Client := w.r2Client
	w.mu.Unlock()

	if len(cloudClients) == 0 {
		resetMap, err := db.ResetJobsOnTerminalLaunches(w.database)
		if err != nil {
			slog.Warn("cloud reconcile failed", "component", "hostsync", "error", err)
			select {
			case w.results <- Result{Error: err}:
			default:
			}
			return
		}
		if len(resetMap) > 0 {
			slog.Info("cloud reconcile reset jobs on terminal instances", "component", "hostsync", "count", len(resetMap))
			select {
			case w.results <- Result{Updated: len(resetMap)}:
			default:
			}
		}
		return
	}

	clientAny := make([]any, 0, len(cloudClients))
	for _, client := range cloudClients {
		clientAny = append(clientAny, client)
	}
	result := syncorch.SyncCloud(nil, w.database, syncorch.CloudSyncOptions{
		Timeout:     0,
		Reconciler:  w.reconciler,
		Clients:     clientAny,
		R2Client:    r2Client,
		SyncResults: true,
	})
	if result.Updated > 0 {
		slog.Info("cloud reconcile completed", "component", "hostsync", "updated", result.Updated)
		select {
		case w.results <- Result{Updated: result.Updated}:
		default:
		}
	}
}

func (w *Worker) handleRequest(req Request) {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, exists := w.hostState[req.Host]
	if !exists {
		state = &hostSyncState{}
		w.hostState[req.Host] = state
	}

	if req.Rate < state.requestedRate || req.Priority {
		state.requestedRate = req.Rate
	}
	if req.Mode == "" {
		req.Mode = ops.SyncModeStatus
	}
	if req.Priority {
		state.requestedMode = ops.SyncModeFull
	} else if state.requestedMode == "" || req.Mode == ops.SyncModeFull {
		state.requestedMode = req.Mode
	}
	if req.Priority && !state.inProgress {
		w.maybeStartSync(req.Host, state)
	}
}

func (w *Worker) processQueue() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for host, state := range w.hostState {
		w.maybeStartSync(host, state)
	}
}

func (w *Worker) processRepairQueue() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for host, state := range w.hostState {
		w.maybeStartRepairSync(host, state)
	}
}

func (w *Worker) maybeStartSync(host string, state *hostSyncState) {
	if state.inProgress || w.inFlight >= w.maxParallel {
		return
	}
	if time.Since(state.lastSync) < state.requestedRate.Interval() {
		return
	}

	state.inProgress = true
	state.lastSync = time.Now()
	w.inFlight++
	mode := state.requestedMode
	if mode == "" {
		mode = ops.SyncModeStatus
	}
	if mode == ops.SyncModeFull {
		state.requestedMode = ops.SyncModeStatus
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.doSync(host, mode)
	}()
}

func (w *Worker) maybeStartRepairSync(host string, state *hostSyncState) {
	if state.inProgress || w.inFlight >= w.maxParallel {
		return
	}
	if time.Since(state.lastRepair) < 120*time.Second {
		return
	}

	state.inProgress = true
	state.lastRepair = time.Now()
	w.inFlight++

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.doSync(host, ops.SyncModeRepair)
	}()
}

func (w *Worker) doSync(host string, mode ops.SyncMode) {
	result := Result{Host: host}

	defer func() {
		w.mu.Lock()
		if state, exists := w.hostState[host]; exists {
			state.inProgress = false
		}
		w.inFlight--
		w.mu.Unlock()

		select {
		case w.results <- result:
		default:
		}
	}()

	scope := hostLeaseScope(mode, host)
	ttl := hostLeaseTTL(mode)
	ok, err := db.AcquireAutoLease(w.database, scope, w.owner, ttl)
	if err != nil {
		result.Error = err
		return
	}
	if !ok {
		return
	}
	defer func() { _ = db.ReleaseAutoLease(w.database, scope, w.owner) }()

	if db.IsLaunchHost(host) {
		hostStatus, err := ops.FetchLaunchHostStatusFromDB(w.database, host)
		if err != nil {
			result.Error = err
			return
		}
		result.HostInfo = hostStatus.HostInfo
		result.HostFull = hostStatus.Host
		return
	}

	syncResult, err := ops.SyncHost(w.database, host, ops.HostSyncOptions{
		Timeout:      ops.DefaultSyncOptions().Timeout,
		UseBatchSync: true,
		Mode:         mode,
		Logger:       ops.NewSilentSyncLogger(),
	}, EnsureQueueRunnerStarted)
	if err != nil {
		result.Error = err
		return
	}

	result.Updated = syncResult.Updated
	result.QueueStarted = syncResult.QueueStarted
	result.QueueDispatchError = syncResult.QueueDispatchError
	result.QueueRunnerError = syncResult.QueueRunnerError
	result.HostWarning = buildWarning(syncResult)

	if hostStatus, err := ops.FetchHostStatusCombined(w.database, host, queuerunner.StatusCommand(), 10*time.Second); err == nil {
		result.HostInfo = hostStatus.HostInfo
		result.HostFull = hostStatus.Host
		if hostStatus.ExtraOutput != "" {
			result.QueueStatus = queuerunner.ParseStatus(hostStatus.ExtraOutput)
			if result.HostFull != nil {
				result.HostFull.QueueStatus = hostinfo.QueueCheckChecked
				result.HostFull.QueueRunnerActive = result.QueueStatus.RunnerActive
				result.HostFull.QueuedJobCount = result.QueueStatus.QueuedJobCount
				result.HostFull.CurrentQueueJob = result.QueueStatus.CurrentJob
				result.HostFull.QueueStopPending = result.QueueStatus.StopPending
				result.HostFull.BlockedQueueJobs = result.QueueStatus.BlockedReasons
			}
		}
	}
}

func hostLeaseScope(mode ops.SyncMode, host string) string {
	switch mode {
	case ops.SyncModeRepair:
		return fmt.Sprintf("sync:host:repair:%s", host)
	case ops.SyncModeFull:
		return fmt.Sprintf("sync:host:full:%s", host)
	default:
		return fmt.Sprintf("sync:host:fast:%s", host)
	}
}

func hostLeaseTTL(mode ops.SyncMode) time.Duration {
	switch mode {
	case ops.SyncModeRepair:
		return 3 * time.Minute
	case ops.SyncModeFull:
		return 15 * time.Minute
	default:
		return 30 * time.Second
	}
}

func syncOwnerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
}

// GetHostSyncRate returns the sync cadence for a host based on active jobs.
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
