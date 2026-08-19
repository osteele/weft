package daemonapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/narrate"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/watchevents"
)

const (
	RequestWatchJobs  = "watch_jobs"
	RequestSubscribe  = "subscribe"
	RequestDaemonInfo = "daemon_info"
	RequestShutdown   = "shutdown"
	RequestSubmitJob  = "submit_job"
	RequestMutate     = "mutate"

	EventSnapshot             = "snapshot"
	EventDone                 = "done"
	EventError                = "error"
	EventDaemonInfo           = "daemon_info"
	EventJobSubmitted         = "job_submitted"
	EventMutationResult       = "mutation_result"
	EventSubscriptionReady    = "subscription_ready"
	EventSubscriptionSnapshot = "subscription_snapshot"

	ResourceJobStatus    = "job_status"
	ResourceProjectWatch = "project_watch"
	ResourceActivity     = "activity"

	DefaultActivityMaxEventBytes = 64 << 20
	MinimumActivityMaxEventBytes = 1 << 10
	ErrorCodeFrameTooLarge       = "frame_too_large"
)

type Request struct {
	Type           string               `json:"type"`
	JobIDs         []int64              `json:"job_ids,omitempty"`
	TimeoutSeconds int64                `json:"timeout_seconds,omitempty"`
	ClientPID      int                  `json:"client_pid,omitempty"`
	Command        string               `json:"command,omitempty"`
	Subscribe      *SubscriptionRequest `json:"subscribe,omitempty"`
	SubmitJob      *SubmitJobRequest    `json:"submit_job,omitempty"`
	Mutation       *MutationRequest     `json:"mutation,omitempty"`
}

type SubmitJobRequest struct {
	Params ops.QueueJobParams `json:"params"`
}

type MutationRequest struct {
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type SubscriptionRequest struct {
	Resource         string  `json:"resource"`
	JobIDs           []int64 `json:"job_ids,omitempty"`
	Project          string  `json:"project,omitempty"`
	RecentSeconds    int64   `json:"recent_seconds,omitempty"`
	Follow           bool    `json:"follow,omitempty"`
	IncludeDelta     bool    `json:"include_delta,omitempty"`
	IncludeStatus    bool    `json:"include_status_line,omitempty"`
	IncludeFormatted *bool   `json:"include_formatted,omitempty"`
	MaxEventBytes    int     `json:"max_event_bytes,omitempty"`
	PollSeconds      int64   `json:"poll_seconds,omitempty"`
	TimeoutSeconds   int64   `json:"timeout_seconds,omitempty"`
}

type JobSnapshot struct {
	ID              int64  `json:"id"`
	Found           bool   `json:"found"`
	Status          string `json:"status,omitempty"`
	EffectiveStatus string `json:"effective_status,omitempty"`
	Host            string `json:"host,omitempty"`
}

type Event struct {
	Type           string                     `json:"type"`
	APIVersion     int                        `json:"api_version,omitempty"`
	Resource       string                     `json:"resource,omitempty"`
	SubscriptionID string                     `json:"subscription_id,omitempty"`
	Jobs           []JobSnapshot              `json:"jobs,omitempty"`
	Snapshot       *watchevents.SnapshotEvent `json:"snapshot,omitempty"`
	Activity       *ActivityPayload           `json:"activity,omitempty"`
	Daemon         *DaemonInfo                `json:"daemon,omitempty"`
	SubmittedJob   *SubmitJobResult           `json:"submitted_job,omitempty"`
	Mutation       *MutationResult            `json:"mutation,omitempty"`
	ErrorCode      string                     `json:"error_code,omitempty"`
	Error          string                     `json:"error,omitempty"`
	EventBytes     int                        `json:"event_bytes,omitempty"`
	MaxEventBytes  int                        `json:"max_event_bytes,omitempty"`
}

type SubmitJobResult struct {
	JobID int64 `json:"job_id"`
}

type MutationResult struct {
	Payload json.RawMessage `json:"payload,omitempty"`
}

type ActivityPayload struct {
	Snapshot          *narrate.Snapshot         `json:"snapshot,omitempty"`
	Delta             *narrate.Delta            `json:"delta,omitempty"`
	StatusLine        *narrate.StatusLine       `json:"status_line,omitempty"`
	RunawayBreakers   []RunawayBreakerView      `json:"runaway_breakers,omitempty"`
	Unprocessed       narrate.UnprocessedCounts `json:"unprocessed,omitempty"`
	UnprocessedJobs   []narrate.JobView         `json:"unprocessed_jobs,omitempty"`
	FormattedSnapshot string                    `json:"formatted_snapshot,omitempty"`
	FormattedDelta    string                    `json:"formatted_delta,omitempty"`
}

// RunawayBreakerView is the active give-up state for one autopilot scope.
// A non-empty ActivityPayload.RunawayBreakers means automatic relaunches have
// stopped in at least one scope visible to the subscription.
type RunawayBreakerView struct {
	Scope         string `json:"scope"`
	CampaignID    int64  `json:"campaign_id,omitempty"`
	Project       string `json:"project"`
	Reason        string `json:"reason"`
	TrippedAt     string `json:"tripped_at"`
	Chain         int    `json:"chain"`
	Orphaned      int    `json:"orphaned"`
	InfraFailures int    `json:"infra_failures"`
	SpendCents    int    `json:"spend_cents"`
	Window        string `json:"window"`
}

type Server struct {
	listener   net.Listener
	socketPath string
	closeOnce  sync.Once
	info       DaemonInfo
	shutdown   func()
	mutate     MutationHandler
	writer     *WriteExecutor

	budgetCentsPerHour int
	runawayBreakers    runawayBreakerCache
	activityInputs     activityInputsCache
}

const runawayBreakerRefreshInterval = 5 * time.Second

// activityHeartbeatInterval bounds silence between activity snapshots on an
// idle subscription; clients treat longer silence as staleness.
const activityHeartbeatInterval = 45 * time.Second

// activityInputsCacheTTL bounds how long project-scoped activity inputs are
// shared across subscribers. It matches the default activity poll interval
// (runActivitySubscriptionLoop polls once per second): subscribers tick once
// per interval, so a one-interval TTL collapses the per-subscriber duplicate
// builds within a tick into a single build while adding at most one tick of
// staleness.
const activityInputsCacheTTL = time.Second

type runawayBreakerCache struct {
	mu          sync.Mutex
	refreshedAt time.Time
	infos       []campaign.RunawayBreakerInfo
}

func (c *runawayBreakerCache) load(database *sql.DB, now time.Time) ([]campaign.RunawayBreakerInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.refreshedAt.IsZero() && now.Before(c.refreshedAt.Add(runawayBreakerRefreshInterval)) {
		return c.infos, nil
	}
	infos, err := campaign.LookupActiveRunawayBreakers(database)
	if err != nil {
		return nil, err
	}
	c.refreshedAt = now
	c.infos = infos
	return c.infos, nil
}

// activityInputs holds the project-scoped reads behind an activity payload:
// the narrate snapshot and the unprocessed-inbox counts and views. These
// depend only on the project scope, not on the subscriber, so subscribers
// over the same project share them.
//
// The snapshot is treated as immutable after build: DiffSnapshots,
// FormatSnapshot, BuildStatusLine, and the delta resolvers only read it, so
// one *narrate.Snapshot can be handed to every subscriber goroutine at once.
type activityInputs struct {
	snapshot *narrate.Snapshot
	counts   narrate.UnprocessedCounts
	views    []narrate.JobView
}

// activityInputsCache shares activityInputs across subscribers per project.
// The mutex covers the whole load so a cache miss runs one build even when
// several subscribers miss simultaneously.
type activityInputsCache struct {
	mu      sync.Mutex
	entries map[string]activityInputsEntry
}

type activityInputsEntry struct {
	refreshedAt time.Time
	inputs      activityInputs
}

func (c *activityInputsCache) load(database *sql.DB, project string, now time.Time) (activityInputs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[project]; ok && now.Before(entry.refreshedAt.Add(activityInputsCacheTTL)) {
		return entry.inputs, nil
	}
	inputs, err := loadActivityInputs(database, project)
	if err != nil {
		return activityInputs{}, err
	}
	if c.entries == nil {
		c.entries = map[string]activityInputsEntry{}
	}
	c.entries[project] = activityInputsEntry{refreshedAt: now, inputs: inputs}
	return inputs, nil
}

func loadActivityInputs(database *sql.DB, project string) (activityInputs, error) {
	snap, err := narrate.BuildSnapshot(database, narrate.SnapshotOptions{Project: project})
	if err != nil {
		return activityInputs{}, err
	}
	counts, views, err := narrate.LoadUnprocessedCountsAndViews(database, project)
	if err != nil {
		return activityInputs{}, fmt.Errorf("load unprocessed inbox: %w", err)
	}
	return activityInputs{snapshot: snap, counts: counts, views: views}, nil
}

type DaemonInfo struct {
	PID               int    `json:"pid"`
	Version           string `json:"version,omitempty"`
	Executable        string `json:"executable,omitempty"`
	ExecutableModTime int64  `json:"executable_mod_time,omitempty"`
	StartedAt         int64  `json:"started_at,omitempty"`
}

type ServerOptions struct {
	Info     DaemonInfo
	Shutdown func()
	Mutate   MutationHandler
	Writer   *WriteExecutor

	BudgetCentsPerHour int
}

type MutationHandler func(context.Context, *sql.DB, MutationRequest) (json.RawMessage, error)

func StartServer(ctx context.Context, database *sql.DB, socketPath string) (*Server, error) {
	return StartServerWithOptions(ctx, database, socketPath, ServerOptions{})
}

func StartServerWithOptions(ctx context.Context, database *sql.DB, socketPath string, opts ServerOptions) (*Server, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("empty daemon watch socket path")
	}
	if err := prepareSocketPath(socketPath); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon watch socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("chmod daemon watch socket: %w", err)
	}
	s := &Server{
		listener:   listener,
		socketPath: socketPath,
		info:       opts.Info,
		shutdown:   opts.Shutdown,
		mutate:     opts.Mutate,
		writer:     opts.Writer,

		budgetCentsPerHour: opts.BudgetCentsPerHour,
	}
	if s.writer == nil {
		s.writer = NewWriteExecutor(ctx, database)
	}
	go s.acceptLoop(ctx, database)
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.Close()
		_ = os.Remove(s.socketPath)
	})
	return err
}

func prepareSocketPath(socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create daemon watch socket directory: %w", err)
	}
	if _, err := os.Lstat(socketPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat daemon watch socket: %w", err)
	}
	return fmt.Errorf("daemon watch socket path already exists: %s", socketPath)
}

func (s *Server) acceptLoop(ctx context.Context, database *sql.DB) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.handleConn(ctx, database, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, database *sql.DB, conn net.Conn) {
	defer conn.Close()
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	var req Request
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
		return
	}
	go cancelOnConnClose(conn, cancelReq)
	switch req.Type {
	case RequestDaemonInfo:
		info := s.info
		_ = encoder.Encode(Event{Type: EventDaemonInfo, Daemon: &info})
	case RequestShutdown:
		_ = encoder.Encode(Event{Type: EventDone})
		if s.shutdown != nil {
			go s.shutdown()
		}
	case RequestSubmitJob:
		s.submitJob(reqCtx, database, encoder, req)
	case RequestMutate:
		s.mutateRequest(reqCtx, database, encoder, req)
	case RequestSubscribe:
		subscribe(reqCtx, database, encoder, req, s.budgetCentsPerHour, &s.runawayBreakers, &s.activityInputs)
	case RequestWatchJobs:
		watchJobs(reqCtx, database, encoder, req)
	default:
		_ = encoder.Encode(Event{Type: EventError, Error: fmt.Sprintf("unsupported request type %q", req.Type)})
	}
}

func cancelOnConnClose(conn net.Conn, cancel context.CancelFunc) {
	var b [1]byte
	if _, err := conn.Read(b[:]); err != nil {
		cancel()
		return
	}
	cancel()
}

func (s *Server) mutateRequest(ctx context.Context, database *sql.DB, encoder *json.Encoder, req Request) {
	if req.Mutation == nil {
		_ = encoder.Encode(Event{Type: EventError, Error: "mutate requires payload"})
		return
	}
	if req.Mutation.Op == "" {
		_ = encoder.Encode(Event{Type: EventError, Error: "mutate requires op"})
		return
	}
	if s.mutate == nil {
		_ = encoder.Encode(Event{Type: EventError, Error: "mutation API unavailable"})
		return
	}
	result, err := s.writer.Execute(ctx, func(ctx context.Context, database *sql.DB) (any, error) {
		return s.mutate(ctx, database, *req.Mutation)
	})
	if err != nil {
		_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
		return
	}
	payload, _ := result.(json.RawMessage)
	_ = encoder.Encode(Event{
		Type:     EventMutationResult,
		Mutation: &MutationResult{Payload: payload},
	})
}

func (s *Server) submitJob(ctx context.Context, database *sql.DB, encoder *json.Encoder, req Request) {
	if req.SubmitJob == nil {
		_ = encoder.Encode(Event{Type: EventError, Error: "submit_job requires payload"})
		return
	}
	result, err := s.writer.Execute(ctx, func(ctx context.Context, database *sql.DB) (any, error) {
		return ops.RecordQueuedJobContext(ctx, database, req.SubmitJob.Params)
	})
	if err != nil {
		_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
		return
	}
	jobID, _ := result.(int64)
	_ = encoder.Encode(Event{
		Type:         EventJobSubmitted,
		SubmittedJob: &SubmitJobResult{JobID: jobID},
	})
}

func subscribe(parent context.Context, database *sql.DB, encoder *json.Encoder, req Request, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache, activityInputs *activityInputsCache) {
	sub := req.Subscribe
	if sub == nil {
		sub = &SubscriptionRequest{
			Resource:       ResourceJobStatus,
			JobIDs:         append([]int64(nil), req.JobIDs...),
			TimeoutSeconds: req.TimeoutSeconds,
		}
	}
	if sub.TimeoutSeconds == 0 {
		sub.TimeoutSeconds = req.TimeoutSeconds
	}
	if sub.Resource == "" {
		_ = encoder.Encode(Event{Type: EventError, Error: "subscribe requires resource"})
		return
	}
	if sub.Resource != ResourceJobStatus && sub.Resource != ResourceProjectWatch && sub.Resource != ResourceActivity {
		_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, Error: fmt.Sprintf("unsupported subscription resource %q", sub.Resource)})
		return
	}
	if sub.Resource == ResourceActivity {
		maxEventBytes, err := activityEventLimit(sub.MaxEventBytes)
		if err != nil {
			_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, Error: err.Error()})
			return
		}
		sub.MaxEventBytes = maxEventBytes
	}
	id := subscriptionID(req.ClientPID, sub)
	if err := encoder.Encode(Event{
		Type:           EventSubscriptionReady,
		APIVersion:     1,
		Resource:       sub.Resource,
		SubscriptionID: id,
		MaxEventBytes:  sub.MaxEventBytes,
	}); err != nil {
		return
	}
	switch sub.Resource {
	case ResourceJobStatus:
		subscribeJobStatus(parent, database, encoder, id, *sub)
	case ResourceProjectWatch:
		subscribeProjectWatch(parent, database, encoder, id, *sub)
	case ResourceActivity:
		subscribeActivity(parent, database, encoder, id, *sub, budgetCentsPerHour, runawayBreakers, activityInputs)
	}
}

func subscriptionID(pid int, sub *SubscriptionRequest) string {
	data, err := json.Marshal(sub)
	if err != nil {
		return fmt.Sprintf("%d:%s", pid, sub.Resource)
	}
	return fmt.Sprintf("%d:%x", pid, data)
}

func subscribeJobStatus(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest) {
	if len(sub.JobIDs) == 0 {
		_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: "job_status subscription requires at least one job id"})
		return
	}
	runSubscriptionLoop(parent, encoder, id, sub, func(now time.Time) (*watchevents.SnapshotEvent, bool, error) {
		jobsByID, err := db.GetJobsByIDs(database, sub.JobIDs)
		if err != nil {
			return nil, false, err
		}
		jobs := make([]*db.Job, 0, len(sub.JobIDs))
		allDone := true
		for _, jobID := range sub.JobIDs {
			job := jobsByID[jobID]
			if job == nil {
				continue
			}
			jobs = append(jobs, job)
			if !isWaitTerminalStatus(job.EffectiveStatus()) {
				allDone = false
			}
		}
		snapshot := watchevents.BuildSnapshotEvent(jobs, now)
		return &snapshot, allDone, nil
	})
}

func subscribeProjectWatch(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest) {
	if sub.RecentSeconds <= 0 {
		sub.RecentSeconds = int64((24 * time.Hour).Seconds())
	}
	runSubscriptionLoop(parent, encoder, id, sub, func(now time.Time) (*watchevents.SnapshotEvent, bool, error) {
		jobs, active, err := projectWatchJobs(database, sub.Project, time.Duration(sub.RecentSeconds)*time.Second, now)
		if err != nil {
			return nil, false, err
		}
		snapshot := watchevents.BuildSnapshotEvent(jobs, now)
		return &snapshot, !sub.Follow && !active, nil
	})
}

func subscribeActivity(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache, inputs *activityInputsCache) {
	runActivitySubscriptionLoop(parent, database, encoder, id, sub, budgetCentsPerHour, runawayBreakers, inputs, activityHeartbeatInterval)
}

type subscriptionStep func(now time.Time) (*watchevents.SnapshotEvent, bool, error)

func runSubscriptionLoop(parent context.Context, encoder *json.Encoder, id string, sub SubscriptionRequest, step subscriptionStep) {
	ctx := parent
	cancel := func() {}
	if sub.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(sub.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	poll := time.Second
	if sub.PollSeconds > 0 {
		poll = time.Duration(sub.PollSeconds) * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	lastKey := ""
	for {
		snapshot, done, err := step(time.Now())
		if err != nil {
			_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: err.Error()})
			return
		}
		key := subscriptionSnapshotKey(snapshot)
		if key != lastKey {
			lastKey = key
			if err := encoder.Encode(Event{
				Type:           EventSubscriptionSnapshot,
				APIVersion:     1,
				Resource:       sub.Resource,
				SubscriptionID: id,
				Snapshot:       snapshot,
			}); err != nil {
				return
			}
		}
		if done {
			_ = encoder.Encode(Event{Type: EventDone, APIVersion: 1, Resource: sub.Resource, SubscriptionID: id, Snapshot: snapshot})
			return
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: ctx.Err().Error()})
			}
			return
		case <-ticker.C:
		}
	}
}

func subscriptionSnapshotKey(snapshot *watchevents.SnapshotEvent) string {
	if snapshot == nil {
		return ""
	}
	data, err := json.Marshal(snapshot.Jobs)
	if err != nil {
		return ""
	}
	return string(data)
}

func projectWatchJobs(database *sql.DB, project string, recentWindow time.Duration, now time.Time) ([]*db.Job, bool, error) {
	activeJobs := make([]*db.Job, 0)
	running, err := db.ListAllRunning(database)
	if err != nil {
		return nil, false, fmt.Errorf("list running jobs: %w", err)
	}
	activeJobs = append(activeJobs, filterProjectJobs(running, project)...)
	for _, status := range []string{db.StatusStarting, db.StatusPaused, db.StatusQueued, db.StatusPendingPlacement} {
		jobs, err := db.ListJobsByStatuses(database, []string{status}, "", project, 0, nil, "")
		if err != nil {
			return nil, false, fmt.Errorf("list %s jobs: %w", status, err)
		}
		activeJobs = append(activeJobs, jobs...)
	}
	recentJobs, err := db.ListRecentTerminalJobs(database, now.Add(-recentWindow).Unix())
	if err != nil {
		return nil, false, fmt.Errorf("list recent terminal jobs: %w", err)
	}
	recentJobs = filterProjectJobs(recentJobs, project)
	return watchevents.DedupeJobsByID(activeJobs, recentJobs), len(activeJobs) > 0, nil
}

func runActivitySubscriptionLoop(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache, inputs *activityInputsCache, heartbeat time.Duration) {
	includeStatus := sub.IncludeStatus || !sub.IncludeDelta
	build := func(prev *narrate.Snapshot) (*ActivityPayload, bool, error) {
		return buildCachedActivityPayload(database, sub, prev, includeStatus, budgetCentsPerHour, runawayBreakers, inputs)
	}
	runActivityLoop(parent, encoder, id, sub, heartbeat, build)
}

type activityBuildResult struct {
	payload *ActivityPayload
	active  bool
	err     error
}

// activityBuildFunc builds one activity payload against prev, the
// subscriber's most recent snapshot (nil before the first build). The delta
// is per-subscriber state, so each subscription builds its own payload even
// when the underlying project-scoped reads are shared.
type activityBuildFunc func(prev *narrate.Snapshot) (*ActivityPayload, bool, error)

// runActivityLoop emits activity snapshots on change and heartbeats on idle.
// Builds run in a worker goroutine, at most one at a time, so a build slower
// than the heartbeat interval cannot silence the feed: the loop selects over
// build results, the poll and heartbeat tickers, and cancellation. All
// encoder writes stay in this goroutine (json.Encoder is not
// concurrency-safe); the worker only builds and reports over a buffered
// channel so a build that outlives the subscription can always report and
// exit instead of leaking.
func runActivityLoop(parent context.Context, encoder *json.Encoder, id string, sub SubscriptionRequest, heartbeat time.Duration, build activityBuildFunc) {
	ctx := parent
	cancel := func() {}
	if sub.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(sub.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	poll := time.Second
	if sub.PollSeconds > 0 {
		poll = time.Duration(sub.PollSeconds) * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var heartbeatTicker *time.Ticker
	var heartbeatC <-chan time.Time
	if heartbeat > 0 {
		heartbeatTicker = time.NewTicker(heartbeat)
		defer heartbeatTicker.Stop()
		heartbeatC = heartbeatTicker.C
	}

	results := make(chan activityBuildResult, 1)
	buildInFlight := false
	startBuild := func(prev *narrate.Snapshot) {
		buildInFlight = true
		go func() {
			payload, active, err := build(prev)
			results <- activityBuildResult{payload: payload, active: active, err: err}
		}()
	}

	var prev *narrate.Snapshot
	var lastPayload *ActivityPayload
	lastKey := ""
	var lastEmit time.Time

	// emit writes a snapshot event and reports whether the connection is
	// still usable; false means the loop must exit.
	emit := func(payload *ActivityPayload) bool {
		if err := encodeBoundedActivityEvent(encoder, Event{
			Type:           EventSubscriptionSnapshot,
			APIVersion:     1,
			Resource:       sub.Resource,
			SubscriptionID: id,
			Activity:       payload,
		}, sub.MaxEventBytes); err != nil {
			if sizeErr, ok := err.(*activityEventSizeError); ok {
				writeActivityEventSizeError(encoder, id, sub, sizeErr)
			}
			return false
		}
		lastEmit = time.Now()
		// The heartbeat means "it has been at most one interval since the
		// client last heard from us", so every successful emission —
		// change-emission and heartbeat alike — restarts the interval.
		if heartbeatTicker != nil {
			heartbeatTicker.Reset(heartbeat)
		}
		return true
	}

	startBuild(nil)
	for {
		select {
		case result := <-results:
			buildInFlight = false
			if result.err != nil {
				_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: result.err.Error()})
				return
			}
			payload := result.payload
			key := activityPayloadKey(payload)
			if key != lastKey || activityPayloadHasDelta(payload) {
				lastKey = key
				if !emit(payload) {
					return
				}
			}
			lastPayload = payload
			prev = payload.Snapshot
			if !sub.Follow && !result.active {
				if err := encodeBoundedActivityEvent(encoder, Event{Type: EventDone, APIVersion: 1, Resource: sub.Resource, SubscriptionID: id, Activity: payload}, sub.MaxEventBytes); err != nil {
					if sizeErr, ok := err.(*activityEventSizeError); ok {
						writeActivityEventSizeError(encoder, id, sub, sizeErr)
					}
				}
				return
			}
		case <-ticker.C:
			if !buildInFlight {
				startBuild(prev)
			}
		case <-heartbeatC:
			// Invariant: a heartbeat re-emits the last built payload
			// unchanged — in particular snapshot.Time is preserved, never
			// refreshed. Refreshing a timestamp the daemon did not
			// re-observe would assert a freshness we have no evidence for:
			// the heartbeat proves the daemon is alive, and the preserved
			// timestamp tells the truth about how old the data is. Before
			// the first build completes there is no payload to re-emit, so
			// the tick is skipped (the client already has
			// subscription_ready). The re-emit goes through the same
			// bounded emit as any change emission, so a heartbeat never
			// exceeds the negotiated frame limit.
			//
			// The ticker resets on every emission, so a tick normally
			// arrives one full interval after the last emit. The
			// time.Since(lastEmit) guard is still load-bearing: the ticker
			// channel is buffered, so a tick can already be pending when an
			// emission resets the ticker (a tick that fired while the loop
			// was handling a build result). Without the guard that stale
			// tick would heartbeat immediately after a fresh emission.
			if lastPayload != nil && time.Since(lastEmit) >= heartbeat {
				if !emit(lastPayload) {
					return
				}
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: ctx.Err().Error()})
			}
			return
		}
	}
}

type activityEventSizeError struct {
	eventBytes int
	maxBytes   int
}

func (e *activityEventSizeError) Error() string {
	return fmt.Sprintf("activity event is %d bytes, exceeding negotiated maximum %d", e.eventBytes, e.maxBytes)
}

func activityEventLimit(requested int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("max_event_bytes must not be negative")
	}
	if requested > 0 && requested < MinimumActivityMaxEventBytes {
		return 0, fmt.Errorf("max_event_bytes must be at least %d", MinimumActivityMaxEventBytes)
	}
	if requested == 0 || requested > DefaultActivityMaxEventBytes {
		return DefaultActivityMaxEventBytes, nil
	}
	return requested, nil
}

func encodeBoundedActivityEvent(encoder *json.Encoder, event Event, maxBytes int) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	eventBytes := len(data) + 1 // json.Encoder appends the NDJSON newline delimiter.
	if eventBytes > maxBytes {
		return &activityEventSizeError{eventBytes: eventBytes, maxBytes: maxBytes}
	}
	return encoder.Encode(json.RawMessage(data))
}

func writeActivityEventSizeError(encoder *json.Encoder, id string, sub SubscriptionRequest, sizeErr *activityEventSizeError) {
	_ = encoder.Encode(Event{
		Type:           EventError,
		APIVersion:     1,
		Resource:       sub.Resource,
		SubscriptionID: id,
		ErrorCode:      ErrorCodeFrameTooLarge,
		Error:          sizeErr.Error(),
		EventBytes:     sizeErr.eventBytes,
		MaxEventBytes:  sizeErr.maxBytes,
	})
}

func buildCachedActivityPayload(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int, breakerCache *runawayBreakerCache, inputsCache *activityInputsCache) (*ActivityPayload, bool, error) {
	var breakers []campaign.RunawayBreakerInfo
	var err error
	if breakerCache != nil {
		breakers, err = breakerCache.load(database, time.Now())
	} else {
		breakers, err = campaign.LookupActiveRunawayBreakers(database)
	}
	if err != nil {
		return nil, false, fmt.Errorf("load active runaway breakers: %w", err)
	}
	var inputs activityInputs
	if inputsCache != nil {
		inputs, err = inputsCache.load(database, sub.Project, time.Now())
	} else {
		inputs, err = loadActivityInputs(database, sub.Project)
	}
	if err != nil {
		return nil, false, err
	}
	return buildActivityPayloadWithBreakers(database, sub, prev, includeStatus, budgetCentsPerHour, breakers, inputs)
}

func buildActivityPayload(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int) (*ActivityPayload, bool, error) {
	breakers, err := campaign.LookupActiveRunawayBreakers(database)
	if err != nil {
		return nil, false, fmt.Errorf("load active runaway breakers: %w", err)
	}
	inputs, err := loadActivityInputs(database, sub.Project)
	if err != nil {
		return nil, false, err
	}
	return buildActivityPayloadWithBreakers(database, sub, prev, includeStatus, budgetCentsPerHour, breakers, inputs)
}

func buildActivityPayloadWithBreakers(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int, breakers []campaign.RunawayBreakerInfo, inputs activityInputs) (*ActivityPayload, bool, error) {
	snap := inputs.snapshot
	payload := &ActivityPayload{Snapshot: snap}
	includeFormatted := sub.IncludeFormatted == nil || *sub.IncludeFormatted
	if includeFormatted {
		payload.FormattedSnapshot = narrate.FormatSnapshot(snap)
	}
	payload.RunawayBreakers = runawayBreakerViews(breakers, sub.Project)
	if sub.IncludeDelta {
		delta := narrate.DiffSnapshots(prev, snap)
		if err := delta.ResolveRemovedJobs(database); err != nil {
			return nil, false, fmt.Errorf("resolve removed jobs: %w", err)
		}
		if err := delta.AddRecentTerminalJobs(database, prev, sub.Project); err != nil {
			return nil, false, fmt.Errorf("resolve recent terminal jobs: %w", err)
		}
		if err := delta.ResolveRemovedInstances(database); err != nil {
			return nil, false, fmt.Errorf("resolve removed instances: %w", err)
		}
		payload.Delta = &delta
		if includeFormatted {
			payload.FormattedDelta = narrate.FormatDelta(delta)
		}
	}
	if includeStatus {
		statusLine := narrate.BuildStatusLine(snap, budgetCentsPerHour, inputs.counts)
		payload.StatusLine = &statusLine
		payload.Unprocessed = inputs.counts
		payload.UnprocessedJobs = inputs.views
	}
	active := len(snap.Jobs) > 0 || len(snap.Instances) > 0
	return payload, active, nil
}

func runawayBreakerViews(infos []campaign.RunawayBreakerInfo, project string) []RunawayBreakerView {
	var views []RunawayBreakerView
	for _, info := range infos {
		if project != "" && info.Project != "<all>" && info.Project != project {
			continue
		}
		views = append(views, RunawayBreakerView{
			Scope:         info.ScopeLabel(),
			CampaignID:    info.CampaignID,
			Project:       info.Project,
			Reason:        info.Reason,
			TrippedAt:     info.TrippedAt.Format(time.RFC3339),
			Chain:         info.Chain,
			Orphaned:      info.Orphaned,
			InfraFailures: info.InfraFails,
			SpendCents:    info.SpendCents,
			Window:        info.Window.String(),
		})
	}
	return views
}

func activityPayloadHasDelta(payload *ActivityPayload) bool {
	return payload != nil && payload.Delta != nil && !payload.Delta.Empty()
}

func activityPayloadKey(payload *ActivityPayload) string {
	if payload == nil {
		return ""
	}
	snapshot := stableSnapshotKey(payload.Snapshot)
	statusLine := stableStatusLineKey(payload.StatusLine)
	rows := make([]narrate.JobView, 0, len(payload.UnprocessedJobs))
	for _, job := range payload.UnprocessedJobs {
		rows = append(rows, stableJobView(job))
	}
	data, err := json.Marshal(struct {
		Snapshot    activitySnapshotKey       `json:"snapshot,omitempty"`
		StatusLine  activityStatusLineKey     `json:"status_line,omitempty"`
		Breakers    []RunawayBreakerView      `json:"runaway_breakers,omitempty"`
		Unprocessed narrate.UnprocessedCounts `json:"unprocessed,omitempty"`
		Rows        []narrate.JobView         `json:"unprocessed_jobs,omitempty"`
	}{
		Snapshot:    snapshot,
		StatusLine:  statusLine,
		Breakers:    payload.RunawayBreakers,
		Unprocessed: payload.Unprocessed,
		Rows:        rows,
	})
	if err != nil {
		return ""
	}
	return string(data)
}

type activitySnapshotKey struct {
	Jobs      map[int64]narrate.JobView      `json:"jobs,omitempty"`
	Instances map[int64]narrate.InstanceView `json:"instances,omitempty"`
	Autopilot narrate.AutopilotView          `json:"autopilot"`
}

type activityStatusLineKey struct {
	RunningJobs          int      `json:"running_jobs"`
	QueuedJobs           int      `json:"queued_jobs"`
	StartingJobs         int      `json:"starting_jobs"`
	PendingPlacement     int      `json:"pending_placement"`
	ActiveInstances      int      `json:"active_instances"`
	GraceInstances       int      `json:"grace_instances"`
	LaunchingInst        int      `json:"launching_instances"`
	RunRateUSDPerHour    float64  `json:"run_rate_usd_per_hour"`
	BudgetUSDPerHour     float64  `json:"budget_usd_per_hour"`
	Projects             []string `json:"projects,omitempty"`
	AutopilotState       string   `json:"autopilot_state,omitempty"`
	UnprocessedCompleted int      `json:"unprocessed_completed"`
	UnprocessedFailed    int      `json:"unprocessed_failed"`
	CompletedProjects    []string `json:"completed_projects,omitempty"`
	FailedProjects       []string `json:"failed_projects,omitempty"`
}

func stableSnapshotKey(snapshot *narrate.Snapshot) activitySnapshotKey {
	if snapshot == nil {
		return activitySnapshotKey{}
	}
	autopilot := snapshot.Autopilot
	// PassAgeSeconds is an age counter recomputed from the build time, so it
	// churns whenever the project-scoped inputs are rebuilt; it must not
	// drive emit-on-change. State stays in the key:
	// it is also clock-derived but only crosses discrete boundaries
	// (running/stale/idle), and those rare transitions are exactly what a
	// status client wants emitted promptly.
	autopilot.PassAgeSeconds = 0
	jobs := make(map[int64]narrate.JobView, len(snapshot.Jobs))
	for id, job := range snapshot.Jobs {
		jobs[id] = stableJobView(job)
	}
	return activitySnapshotKey{
		Jobs:      jobs,
		Instances: snapshot.Instances,
		Autopilot: autopilot,
	}
}

// stableJobView strips build-time-derived narration so elapsed time alone
// never reads as a state change.
func stableJobView(job narrate.JobView) narrate.JobView {
	job.Explanation = ""
	job.SuggestedAction = ""
	return job
}

func stableStatusLineKey(statusLine *narrate.StatusLine) activityStatusLineKey {
	if statusLine == nil {
		return activityStatusLineKey{}
	}
	key := activityStatusLineKey{
		RunningJobs:          statusLine.RunningJobs,
		QueuedJobs:           statusLine.QueuedJobs,
		StartingJobs:         statusLine.StartingJobs,
		PendingPlacement:     statusLine.PendingPlacement,
		ActiveInstances:      statusLine.ActiveInstances,
		GraceInstances:       statusLine.GraceInstances,
		LaunchingInst:        statusLine.LaunchingInst,
		RunRateUSDPerHour:    statusLine.RunRateUSDPerHour,
		BudgetUSDPerHour:     statusLine.BudgetUSDPerHour,
		Projects:             statusLine.Projects,
		UnprocessedCompleted: statusLine.UnprocessedCompleted,
		UnprocessedFailed:    statusLine.UnprocessedFailed,
		CompletedProjects:    statusLine.CompletedProjects,
		FailedProjects:       statusLine.FailedProjects,
		AutopilotState:       statusLine.AutopilotState,
	}
	return key
}

func filterProjectJobs(jobs []*db.Job, project string) []*db.Job {
	if project == "" {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.Project == project {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func watchJobs(parent context.Context, database *sql.DB, encoder *json.Encoder, req Request) {
	if len(req.JobIDs) == 0 {
		_ = encoder.Encode(Event{Type: EventError, Error: "watch_jobs requires at least one job id"})
		return
	}
	ctx := parent
	cancel := func() {}
	if req.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(req.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lastKey := ""
	for {
		snapshots, allDone, err := jobSnapshots(database, req.JobIDs)
		if err != nil {
			_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
			return
		}
		key := snapshotsKey(snapshots)
		if key != lastKey {
			lastKey = key
			if err := encoder.Encode(Event{Type: EventSnapshot, Jobs: snapshots}); err != nil {
				return
			}
		}
		if allDone {
			_ = encoder.Encode(Event{Type: EventDone, Jobs: snapshots})
			return
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = encoder.Encode(Event{Type: EventError, Error: ctx.Err().Error()})
			}
			return
		case <-ticker.C:
		}
	}
}

func jobSnapshots(database *sql.DB, jobIDs []int64) ([]JobSnapshot, bool, error) {
	jobs, err := db.GetJobsByIDs(database, jobIDs)
	if err != nil {
		return nil, false, err
	}
	snapshots := make([]JobSnapshot, 0, len(jobIDs))
	allDone := true
	for _, id := range jobIDs {
		job := jobs[id]
		if job == nil {
			snapshots = append(snapshots, JobSnapshot{ID: id, Found: false})
			continue
		}
		effective := job.EffectiveStatus()
		snapshots = append(snapshots, JobSnapshot{
			ID:              id,
			Found:           true,
			Status:          job.Status,
			EffectiveStatus: effective,
			Host:            job.Host,
		})
		if !isWaitTerminalStatus(effective) {
			allDone = false
		}
	}
	return snapshots, allDone, nil
}

func isWaitTerminalStatus(s string) bool {
	switch s {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return true
	default:
		return false
	}
}

func snapshotsKey(snapshots []JobSnapshot) string {
	data, err := json.Marshal(snapshots)
	if err != nil {
		return ""
	}
	return string(data)
}
