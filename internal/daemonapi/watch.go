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
}

const runawayBreakerRefreshInterval = 5 * time.Second

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
		subscribe(ctx, database, encoder, req, s.budgetCentsPerHour, &s.runawayBreakers)
	case RequestWatchJobs:
		watchJobs(ctx, database, encoder, req)
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

func subscribe(parent context.Context, database *sql.DB, encoder *json.Encoder, req Request, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache) {
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
		subscribeActivity(parent, database, encoder, id, *sub, budgetCentsPerHour, runawayBreakers)
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

func subscribeActivity(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache) {
	runActivitySubscriptionLoop(parent, database, encoder, id, sub, budgetCentsPerHour, runawayBreakers)
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

func runActivitySubscriptionLoop(parent context.Context, database *sql.DB, encoder *json.Encoder, id string, sub SubscriptionRequest, budgetCentsPerHour int, runawayBreakers *runawayBreakerCache) {
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

	includeStatus := sub.IncludeStatus || !sub.IncludeDelta
	var prev *narrate.Snapshot
	lastKey := ""
	for {
		payload, active, err := buildCachedActivityPayload(database, sub, prev, includeStatus, budgetCentsPerHour, runawayBreakers)
		if err != nil {
			_ = encoder.Encode(Event{Type: EventError, Resource: sub.Resource, SubscriptionID: id, Error: err.Error()})
			return
		}
		key := activityPayloadKey(payload)
		shouldEmit := key != lastKey || activityPayloadHasDelta(payload)
		if shouldEmit {
			lastKey = key
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
				return
			}
		}
		prev = payload.Snapshot
		if !sub.Follow && !active {
			if err := encodeBoundedActivityEvent(encoder, Event{Type: EventDone, APIVersion: 1, Resource: sub.Resource, SubscriptionID: id, Activity: payload}, sub.MaxEventBytes); err != nil {
				if sizeErr, ok := err.(*activityEventSizeError); ok {
					writeActivityEventSizeError(encoder, id, sub, sizeErr)
				}
			}
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

func buildCachedActivityPayload(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int, cache *runawayBreakerCache) (*ActivityPayload, bool, error) {
	if cache == nil {
		return buildActivityPayload(database, sub, prev, includeStatus, budgetCentsPerHour)
	}
	breakers, err := cache.load(database, time.Now())
	if err != nil {
		return nil, false, fmt.Errorf("load active runaway breakers: %w", err)
	}
	return buildActivityPayloadWithBreakers(database, sub, prev, includeStatus, budgetCentsPerHour, breakers)
}

func buildActivityPayload(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int) (*ActivityPayload, bool, error) {
	breakers, err := campaign.LookupActiveRunawayBreakers(database)
	if err != nil {
		return nil, false, fmt.Errorf("load active runaway breakers: %w", err)
	}
	return buildActivityPayloadWithBreakers(database, sub, prev, includeStatus, budgetCentsPerHour, breakers)
}

func buildActivityPayloadWithBreakers(database *sql.DB, sub SubscriptionRequest, prev *narrate.Snapshot, includeStatus bool, budgetCentsPerHour int, breakers []campaign.RunawayBreakerInfo) (*ActivityPayload, bool, error) {
	snap, err := narrate.BuildSnapshot(database, narrate.SnapshotOptions{Project: sub.Project})
	if err != nil {
		return nil, false, err
	}
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
		unprocessed, err := narrate.LoadUnprocessedCounts(database, sub.Project)
		if err != nil {
			return nil, false, fmt.Errorf("count unprocessed: %w", err)
		}
		unprocessedJobs, err := narrate.LoadUnprocessedJobViews(database, sub.Project)
		if err != nil {
			return nil, false, fmt.Errorf("list unprocessed jobs: %w", err)
		}
		statusLine := narrate.BuildStatusLine(snap, budgetCentsPerHour, unprocessed)
		payload.StatusLine = &statusLine
		payload.Unprocessed = unprocessed
		payload.UnprocessedJobs = unprocessedJobs
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
		Rows:        payload.UnprocessedJobs,
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
	return activitySnapshotKey{
		Jobs:      snapshot.Jobs,
		Instances: snapshot.Instances,
		Autopilot: snapshot.Autopilot,
	}
}

func stableStatusLineKey(statusLine *narrate.StatusLine) activityStatusLineKey {
	if statusLine == nil {
		return activityStatusLineKey{}
	}
	return activityStatusLineKey{
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
		AutopilotState:       statusLine.AutopilotState,
		UnprocessedCompleted: statusLine.UnprocessedCompleted,
		UnprocessedFailed:    statusLine.UnprocessedFailed,
		CompletedProjects:    statusLine.CompletedProjects,
		FailedProjects:       statusLine.FailedProjects,
	}
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
