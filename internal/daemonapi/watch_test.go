package daemonapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/narrate"
	"github.com/osteele/weft/internal/ops"
)

func TestPrepareSocketPathPreservesExistingSocket(t *testing.T) {
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-existing-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	if err := prepareSocketPath(socketPath); err == nil {
		t.Fatal("prepareSocketPath accepted an existing socket")
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("prepareSocketPath removed existing socket: %v", err)
	}
}

func TestWatchJobsEmitsInitialAndTerminalSnapshots(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestJob(t, database, 101, db.StatusQueued)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	watcher, err := DialWatchJobs(ctx, server.socketPath, []int64{101}, 0)
	if err != nil {
		t.Fatalf("DialWatchJobs: %v", err)
	}
	defer watcher.Close()

	initial := nextWatchEvent(t, watcher)
	if initial.Type != EventSnapshot || len(initial.Jobs) != 1 || initial.Jobs[0].Status != db.StatusQueued {
		t.Fatalf("initial event = %+v, want queued snapshot", initial)
	}

	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), int64(101)); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	for {
		event := nextWatchEvent(t, watcher)
		if event.Type == EventSnapshot && len(event.Jobs) == 1 && event.Jobs[0].Status == db.StatusCompleted {
			return
		}
		if event.Type == EventDone && len(event.Jobs) == 1 && event.Jobs[0].Status == db.StatusCompleted {
			return
		}
	}
}

func TestSubscribeJobStatusEmitsVersionedSnapshots(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestJob(t, database, 101, db.StatusQueued)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	sub, err := DialSubscribeJobStatus(ctx, server.socketPath, []int64{101}, 0)
	if err != nil {
		t.Fatalf("DialSubscribeJobStatus: %v", err)
	}
	defer sub.Close()
	if sub.Ready.Type != EventSubscriptionReady || sub.Ready.APIVersion != 1 || sub.Ready.Resource != ResourceJobStatus {
		t.Fatalf("ready event = %+v", sub.Ready)
	}

	initial := nextSubscriptionEvent(t, sub)
	if initial.Type != EventSubscriptionSnapshot || initial.Snapshot == nil {
		t.Fatalf("initial event = %+v, want subscription snapshot", initial)
	}
	if got := initial.Snapshot.Jobs[0].Status; got != db.StatusQueued {
		t.Fatalf("initial status = %q, want queued", got)
	}

	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), int64(101)); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	for {
		event := nextSubscriptionEvent(t, sub)
		if event.Snapshot == nil || len(event.Snapshot.Jobs) != 1 {
			continue
		}
		if event.Snapshot.Jobs[0].Status == db.StatusCompleted {
			return
		}
	}
}

func TestSubscribeProjectWatchFiltersByProject(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestProjectJob(t, database, 201, "augur", db.StatusRunning)
	insertWatchTestProjectJob(t, database, 202, "other", db.StatusRunning)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	sub, err := DialSubscribe(ctx, server.socketPath, SubscriptionRequest{
		Resource: ResourceProjectWatch,
		Project:  "augur",
		Follow:   true,
	})
	if err != nil {
		t.Fatalf("DialSubscribe: %v", err)
	}
	defer sub.Close()

	event := nextSubscriptionEvent(t, sub)
	if event.Type != EventSubscriptionSnapshot || event.Snapshot == nil {
		t.Fatalf("event = %+v, want project watch snapshot", event)
	}
	if len(event.Snapshot.Jobs) != 1 {
		t.Fatalf("snapshot jobs = %+v, want one augur job", event.Snapshot.Jobs)
	}
	if got := event.Snapshot.Jobs[0].Project; got != "augur" {
		t.Fatalf("project = %q, want augur", got)
	}
}

func TestSubscribeActivityUsesNarrateSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestProjectJob(t, database, 301, "augur", db.StatusQueued)
	insertWatchTestProjectJob(t, database, 302, "other", db.StatusQueued)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUSpec:          "GPU",
		ResolvedGPUName:  "RTX 4090",
		GPUMemGB:         24,
		NumGPUs:          1,
		CostPerHourCents: 42,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServerWithOptions(ctx, database, socketPath, ServerOptions{
		BudgetCentsPerHour: 275,
	})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	sub, err := DialSubscribeActivity(ctx, server.socketPath, SubscriptionRequest{
		Project:       "augur",
		Follow:        true,
		IncludeDelta:  true,
		IncludeStatus: true,
	})
	if err != nil {
		t.Fatalf("DialSubscribeActivity: %v", err)
	}
	defer sub.Close()
	if sub.Ready.Resource != ResourceActivity {
		t.Fatalf("ready resource = %q, want activity", sub.Ready.Resource)
	}
	if sub.Ready.MaxEventBytes != DefaultActivityMaxEventBytes {
		t.Fatalf("ready max event bytes = %d, want %d", sub.Ready.MaxEventBytes, DefaultActivityMaxEventBytes)
	}

	event := nextSubscriptionEvent(t, sub)
	if event.Type != EventSubscriptionSnapshot || event.Activity == nil {
		t.Fatalf("event = %+v, want activity snapshot", event)
	}
	if event.Activity.Snapshot == nil {
		t.Fatalf("activity missing narrate snapshot: %+v", event.Activity)
	}
	if _, ok := event.Activity.Snapshot.Jobs[301]; !ok {
		t.Fatalf("activity snapshot missing augur job: %+v", event.Activity.Snapshot.Jobs)
	}
	if _, ok := event.Activity.Snapshot.Jobs[302]; ok {
		t.Fatalf("activity snapshot included other project job: %+v", event.Activity.Snapshot.Jobs)
	}
	inst, ok := event.Activity.Snapshot.Instances[launchID]
	if !ok {
		t.Fatalf("activity snapshot missing launch %d: %+v", launchID, event.Activity.Snapshot.Instances)
	}
	if inst.GPUSpec != "GPU" || inst.GPUDisplay != "RTX 4090 24GB" {
		t.Fatalf("activity instance GPU fields = (%q, %q), want constraint GPU and display RTX 4090 24GB", inst.GPUSpec, inst.GPUDisplay)
	}
	if event.Activity.StatusLine == nil || event.Activity.StatusLine.QueuedJobs != 1 {
		t.Fatalf("status line = %+v, want one queued job", event.Activity.StatusLine)
	}
	if got := event.Activity.StatusLine.BudgetUSDPerHour; got != 2.75 {
		t.Fatalf("status line budget = %v, want 2.75", got)
	}
	if event.Activity.Delta == nil || len(event.Activity.Delta.JobAdded) != 1 {
		t.Fatalf("delta = %+v, want one added job", event.Activity.Delta)
	}
	if event.Activity.FormattedSnapshot == "" || event.Activity.FormattedDelta == "" {
		t.Fatalf("formatted payload missing: %+v", event.Activity)
	}
}

func TestEncodeBoundedActivityEventAcceptsExactBoundary(t *testing.T) {
	event := Event{Type: EventSubscriptionSnapshot, APIVersion: 1, Resource: ResourceActivity, Activity: &ActivityPayload{FormattedSnapshot: "ok"}}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var output bytes.Buffer
	frameBytes := len(data) + 1
	if err := encodeBoundedActivityEvent(json.NewEncoder(&output), event, frameBytes); err != nil {
		t.Fatalf("encodeBoundedActivityEvent exact boundary: %v", err)
	}
	if got := output.Len(); got != frameBytes {
		t.Fatalf("encoded bytes = %d, want %d including delimiter", got, frameBytes)
	}

	output.Reset()
	err = encodeBoundedActivityEvent(json.NewEncoder(&output), event, frameBytes-1)
	sizeErr, ok := err.(*activityEventSizeError)
	if !ok || sizeErr.eventBytes != frameBytes || sizeErr.maxBytes != frameBytes-1 {
		t.Fatalf("boundary error = %#v, want event size %d max %d", err, frameBytes, frameBytes-1)
	}
	if output.Len() != 0 {
		t.Fatalf("oversized event emitted %d bytes", output.Len())
	}
}

func TestSubscribeActivityReportsOversizedCompleteSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestProjectJob(t, database, 303, "augur", db.StatusQueued)
	if _, err := database.Exec(`UPDATE jobs SET command = ? WHERE id = ?`, strings.Repeat("x", 8<<10), 303); err != nil {
		t.Fatalf("enlarge job command: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	includeFormatted := false
	sub, err := DialSubscribeActivity(ctx, socketPath, SubscriptionRequest{
		Project:          "augur",
		Follow:           true,
		IncludeDelta:     true,
		IncludeFormatted: &includeFormatted,
		MaxEventBytes:    MinimumActivityMaxEventBytes,
	})
	if err != nil {
		t.Fatalf("DialSubscribeActivity: %v", err)
	}
	defer sub.Close()
	if sub.Ready.MaxEventBytes != MinimumActivityMaxEventBytes {
		t.Fatalf("ready max event bytes = %d, want %d", sub.Ready.MaxEventBytes, MinimumActivityMaxEventBytes)
	}
	event := nextSubscriptionEvent(t, sub)
	if event.Type != EventError || event.ErrorCode != ErrorCodeFrameTooLarge {
		t.Fatalf("oversize event = %+v, want terminal frame_too_large error", event)
	}
	if event.EventBytes <= event.MaxEventBytes || event.MaxEventBytes != MinimumActivityMaxEventBytes {
		t.Fatalf("oversize evidence = %d/%d", event.EventBytes, event.MaxEventBytes)
	}
}

func TestActivityPayloadKeyIgnoresTimestamps(t *testing.T) {
	first := &ActivityPayload{
		Snapshot: &narrate.Snapshot{
			Time:      time.Unix(100, 0),
			Jobs:      map[int64]narrate.JobView{1: {ID: 1, Status: db.StatusRunning}},
			Instances: map[int64]narrate.InstanceView{},
			Autopilot: narrate.AutopilotView{State: "idle"},
		},
		StatusLine: &narrate.StatusLine{
			Now:         time.Unix(100, 0),
			RunningJobs: 1,
		},
	}
	second := &ActivityPayload{
		Snapshot: &narrate.Snapshot{
			Time:      time.Unix(200, 0),
			Jobs:      map[int64]narrate.JobView{1: {ID: 1, Status: db.StatusRunning}},
			Instances: map[int64]narrate.InstanceView{},
			Autopilot: narrate.AutopilotView{State: "idle"},
		},
		StatusLine: &narrate.StatusLine{
			Now:         time.Unix(200, 0),
			RunningJobs: 1,
		},
	}
	if activityPayloadKey(first) != activityPayloadKey(second) {
		t.Fatal("activity payload key changed for timestamp-only differences")
	}
	second.RunawayBreakers = []RunawayBreakerView{{
		Scope:     "global (project=<all>)",
		Project:   "<all>",
		Reason:    campaign.RunawayBreakerReasonLaunchFailures,
		TrippedAt: time.Unix(150, 0).UTC().Format(time.RFC3339),
	}}
	if activityPayloadKey(first) == activityPayloadKey(second) {
		t.Fatal("activity payload key did not change for runaway-breaker state")
	}
}

func TestActivityPayloadKeyIgnoresNowDerivedFields(t *testing.T) {
	stable := &ActivityPayload{
		Snapshot: &narrate.Snapshot{
			Jobs:      map[int64]narrate.JobView{1: {ID: 1, Status: db.StatusRunning, Explanation: "running: job is running", SuggestedAction: "monitor progress"}},
			Instances: map[int64]narrate.InstanceView{},
			Autopilot: narrate.AutopilotView{State: "running", PassAgeSeconds: 10},
		},
		StatusLine: &narrate.StatusLine{
			AutopilotState: "running",
			RunningJobs:    1,
		},
	}
	aged := &ActivityPayload{
		Snapshot: &narrate.Snapshot{
			Jobs:      map[int64]narrate.JobView{1: {ID: 1, Status: db.StatusRunning, Explanation: "running: dispatch [42s ago] blocked", SuggestedAction: "replan: waiting 42s"}},
			Instances: map[int64]narrate.InstanceView{},
			Autopilot: narrate.AutopilotView{State: "running", PassAgeSeconds: 300},
		},
		StatusLine: &narrate.StatusLine{
			AutopilotState: "running",
			RunningJobs:    1,
		},
	}
	if activityPayloadKey(stable) != activityPayloadKey(aged) {
		t.Fatal("activity payload key changed for now-derived fields only")
	}
	// Autopilot state is clock-derived but discrete: crossing a boundary
	// (running -> stale) is a rare, meaningful transition and must emit.
	aged.Snapshot.Autopilot.State = "stale"
	aged.StatusLine.AutopilotState = "stale"
	if activityPayloadKey(stable) == activityPayloadKey(aged) {
		t.Fatal("activity payload key did not change for an autopilot state transition")
	}
	aged.Snapshot.Autopilot.State = "running"
	aged.StatusLine.AutopilotState = "running"
	stable.Snapshot.Autopilot.LastSummary = "placed 30 jobs"
	if activityPayloadKey(stable) == activityPayloadKey(aged) {
		t.Fatal("activity payload key did not change for stored autopilot state")
	}
}

func TestActivitySubscriptionDedupsPassAgeChurn(t *testing.T) {
	database := db.SetupTestDB(t)
	passStartedAt := time.Now().Add(-10 * time.Second).Unix()
	if _, err := database.Exec(
		`UPDATE autopilot_state SET pass_started_at = ?, last_heartbeat = ? WHERE id = 1`,
		passStartedAt, time.Now().Unix()); err != nil {
		t.Fatalf("seed autopilot pass: %v", err)
	}

	includeFormatted := false
	sub := startActivityLoopTest(t, database, SubscriptionRequest{
		Resource:         ResourceActivity,
		Follow:           true,
		IncludeDelta:     false,
		IncludeStatus:    false,
		IncludeFormatted: &includeFormatted,
		PollSeconds:      1,
	}, time.Hour)

	first := nextSubscriptionEvent(t, sub)
	if first.Type != EventSubscriptionSnapshot || first.Activity == nil || first.Activity.Snapshot == nil {
		t.Fatalf("first event = %+v, want activity snapshot", first)
	}
	if got := first.Activity.Snapshot.Autopilot.PassAgeSeconds; got < 10 {
		t.Fatalf("emitted pass_age_seconds = %d, want >= 10", got)
	}

	// Two more poll ticks advance only the autopilot pass age. The stable key
	// must suppress re-emission until the heartbeat interval elapses.
	if err := sub.conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var extra Event
	if err := sub.decoder.Decode(&extra); err == nil {
		t.Fatalf("unexpected emission when only pass age changed: %+v", extra)
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("read after polls: %v", err)
	}
}

func TestActivitySubscriptionHeartbeatRefreshesIdleFeed(t *testing.T) {
	database := db.SetupTestDB(t)
	includeFormatted := false
	sub := startActivityLoopTest(t, database, SubscriptionRequest{
		Resource:         ResourceActivity,
		Follow:           true,
		IncludeDelta:     false,
		IncludeStatus:    false,
		IncludeFormatted: &includeFormatted,
		PollSeconds:      1,
	}, time.Second)

	first := nextSubscriptionEvent(t, sub)
	if first.Type != EventSubscriptionSnapshot || first.Activity == nil || first.Activity.Snapshot == nil {
		t.Fatalf("first event = %+v, want activity snapshot", first)
	}
	second := nextSubscriptionEvent(t, sub)
	if second.Type != EventSubscriptionSnapshot || second.Activity == nil || second.Activity.Snapshot == nil {
		t.Fatalf("second event = %+v, want heartbeat activity snapshot", second)
	}
	if got := second.Activity.Snapshot.Autopilot.State; got != "never" {
		t.Fatalf("heartbeat autopilot state = %q, want never", got)
	}
	if second.Activity.Snapshot.Time.IsZero() {
		t.Fatal("heartbeat snapshot missing refreshed time")
	}
}

func TestSubscriptionLoopsExitOnClientDisconnect(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestJob(t, database, 401, db.StatusQueued)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	cases := []struct {
		name string
		req  Request
	}{
		{name: "activity subscription", req: Request{
			Type: RequestSubscribe, ClientPID: os.Getpid(),
			Subscribe: &SubscriptionRequest{Resource: ResourceActivity, Follow: true},
		}},
		{name: "job_status subscription", req: Request{
			Type: RequestSubscribe, ClientPID: os.Getpid(),
			Subscribe: &SubscriptionRequest{Resource: ResourceJobStatus, JobIDs: []int64{401}},
		}},
		{name: "watch_jobs", req: Request{
			Type: RequestWatchJobs, ClientPID: os.Getpid(), JobIDs: []int64{401},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := runtime.NumGoroutine()
			conn, err := net.Dial("unix", server.socketPath)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			decoder := json.NewDecoder(conn)
			if err := json.NewEncoder(conn).Encode(tc.req); err != nil {
				conn.Close()
				t.Fatalf("encode request: %v", err)
			}
			for {
				var event Event
				if err := decoder.Decode(&event); err != nil {
					conn.Close()
					t.Fatalf("read initial event: %v", err)
				}
				if event.Type == EventSnapshot || event.Type == EventSubscriptionSnapshot {
					break
				}
			}
			// The subscription loop is now blocked on the connection-scoped
			// context. Sever without a clean close and require every
			// connection goroutine to exit.
			conn.Close()
			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > base {
				if time.Now().After(deadline) {
					t.Fatalf("subscription loop survived client disconnect: %d goroutines, want %d", runtime.NumGoroutine(), base)
				}
				time.Sleep(25 * time.Millisecond)
			}
		})
	}
}

func startActivityLoopTest(t *testing.T, database *sql.DB, sub SubscriptionRequest, heartbeat time.Duration) *Subscription {
	t.Helper()
	maxEventBytes, err := activityEventLimit(sub.MaxEventBytes)
	if err != nil {
		t.Fatalf("activity event limit: %v", err)
	}
	sub.MaxEventBytes = maxEventBytes
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runActivitySubscriptionLoop(ctx, database, json.NewEncoder(serverSide), "test-sub", sub, 0, nil, heartbeat)
	return &Subscription{conn: clientSide, decoder: json.NewDecoder(clientSide)}
}

func TestBuildActivityPayloadCanOmitFormattedFields(t *testing.T) {
	database := db.SetupTestDB(t)
	insertWatchTestProjectJob(t, database, 304, "augur", db.StatusQueued)
	includeFormatted := false
	payload, _, err := buildActivityPayload(database, SubscriptionRequest{
		Resource:         ResourceActivity,
		Project:          "augur",
		IncludeDelta:     true,
		IncludeFormatted: &includeFormatted,
	}, nil, true, 0)
	if err != nil {
		t.Fatalf("buildActivityPayload: %v", err)
	}
	if payload.FormattedSnapshot != "" || payload.FormattedDelta != "" {
		t.Fatalf("formatted fields were included: %+v", payload)
	}
	if payload.Snapshot == nil || payload.Delta == nil {
		t.Fatalf("structured fields missing: %+v", payload)
	}
}

func TestBuildActivityPayloadPublishesActiveRunawayBreakers(t *testing.T) {
	database := db.SetupTestDB(t)

	payload, _, err := buildActivityPayload(database, SubscriptionRequest{Resource: ResourceActivity}, nil, false, 0)
	if err != nil {
		t.Fatalf("build activity payload without breaker: %v", err)
	}
	if len(payload.RunawayBreakers) != 0 {
		t.Fatalf("runaway breakers = %+v, want none", payload.RunawayBreakers)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal activity payload: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal activity payload: %v", err)
	}
	if _, ok := fields["runaway_breakers"]; ok {
		t.Fatalf("empty runaway_breakers field was not omitted: %s", raw)
	}

	trippedAt := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: trippedAt.Unix(),
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: 42,
		Detail:     "project=augur; infra runaway: infra_failures=3 limit=3 chain=1 orphaned=2 spend=$1.25 window=24h0m0s",
	}); err != nil {
		t.Fatalf("insert runaway trip: %v", err)
	}

	payload, _, err = buildActivityPayload(database, SubscriptionRequest{
		Resource: ResourceActivity,
		Project:  "augur",
	}, nil, false, 0)
	if err != nil {
		t.Fatalf("build activity payload with breaker: %v", err)
	}
	if len(payload.RunawayBreakers) != 1 {
		t.Fatalf("runaway breakers = %+v, want one", payload.RunawayBreakers)
	}
	got := payload.RunawayBreakers[0]
	if got.Scope != "campaign 42, project=augur" || got.CampaignID != 42 || got.Project != "augur" {
		t.Fatalf("runaway breaker scope = %+v", got)
	}
	if got.Reason != campaign.RunawayBreakerReasonInfraFailures {
		t.Fatalf("runaway breaker reason = %q, want %q", got.Reason, campaign.RunawayBreakerReasonInfraFailures)
	}
	if got.TrippedAt != trippedAt.Format(time.RFC3339) {
		t.Fatalf("runaway breaker tripped_at = %q, want %q", got.TrippedAt, trippedAt.Format(time.RFC3339))
	}
	if got.Chain != 1 || got.Orphaned != 2 || got.InfraFailures != 3 || got.SpendCents != 125 || got.Window != "24h0m0s" {
		t.Fatalf("runaway breaker metrics = %+v", got)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: trippedAt.Add(time.Hour).Unix(),
		EventKind:  db.EventRelaunchRunawayResumed,
		CampaignID: 42,
		Detail:     "project=augur; test reset",
	}); err != nil {
		t.Fatalf("insert runaway reset: %v", err)
	}
	payload, _, err = buildActivityPayload(database, SubscriptionRequest{
		Resource: ResourceActivity,
		Project:  "augur",
	}, nil, false, 0)
	if err != nil {
		t.Fatalf("build activity payload after reset: %v", err)
	}
	if len(payload.RunawayBreakers) != 0 {
		t.Fatalf("runaway breakers after reset = %+v, want none", payload.RunawayBreakers)
	}
}

func TestRunawayBreakerCacheBoundsRefreshes(t *testing.T) {
	database := db.SetupTestDB(t)
	var cache runawayBreakerCache
	now := time.Now()

	infos, err := cache.load(database, now)
	if err != nil {
		t.Fatalf("initial cache load: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("initial runaway breakers = %+v, want none", infos)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Unix(),
		EventKind:  db.EventRelaunchRunawayTripped,
		Detail:     "project=<all>; no-progress runaway: chain=3 orphaned=3 spend=$0.00 window=24h0m0s",
	}); err != nil {
		t.Fatalf("insert runaway trip: %v", err)
	}

	infos, err = cache.load(database, now.Add(runawayBreakerRefreshInterval-time.Millisecond))
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("cache refreshed before interval: %+v", infos)
	}
	infos, err = cache.load(database, now.Add(runawayBreakerRefreshInterval))
	if err != nil {
		t.Fatalf("refreshed load: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("runaway breakers after refresh = %+v, want one", infos)
	}
}

func TestBuildActivityPayloadIncludesFastTerminalJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	prev := &narrate.Snapshot{
		Time:      time.Now().Add(-2 * time.Second),
		Jobs:      map[int64]narrate.JobView{},
		Instances: map[int64]narrate.InstanceView{},
	}
	jobID, err := db.RecordQueued(database, "cool30", "/tmp/quick", "echo ok", "quick")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "quick-project"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	payload, active, err := buildActivityPayload(database, SubscriptionRequest{
		Resource:     ResourceActivity,
		Project:      "quick-project",
		IncludeDelta: true,
	}, prev, false, 0)
	if err != nil {
		t.Fatalf("buildActivityPayload: %v", err)
	}
	if active {
		t.Fatal("fast terminal job should not make the live activity snapshot active")
	}
	if payload.Delta == nil || len(payload.Delta.JobFinished) != 1 {
		t.Fatalf("delta = %+v, want one finished job", payload.Delta)
	}
	if got := payload.Delta.JobFinished[0]; got.ID != jobID || got.Status != db.StatusCompleted {
		t.Fatalf("terminal job = %+v, want completed job %d", got, jobID)
	}
	if !activityPayloadHasDelta(payload) {
		t.Fatal("fast terminal payload should be emitted as a delta-only event")
	}
}

func TestBuildActivityPayloadIncludesUnprocessedJobDetails(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "cool30", "/tmp/done", "echo ok", "done")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "done-project"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	payload, _, err := buildActivityPayload(database, SubscriptionRequest{
		Resource:      ResourceActivity,
		Project:       "done-project",
		IncludeStatus: true,
	}, nil, true, 0)
	if err != nil {
		t.Fatalf("buildActivityPayload: %v", err)
	}
	if payload.Unprocessed.Completed != 1 {
		t.Fatalf("unprocessed counts = %+v, want one completed", payload.Unprocessed)
	}
	if len(payload.UnprocessedJobs) != 1 {
		t.Fatalf("unprocessed job details = %+v, want one row", payload.UnprocessedJobs)
	}
	got := payload.UnprocessedJobs[0]
	if got.ID != jobID || got.Status != db.StatusCompleted || got.Project != "done-project" {
		t.Fatalf("unprocessed row = %+v, want completed job %d in done-project", got, jobID)
	}
	if got.EffectiveStatus != db.StatusCompleted || got.Description != "done" || got.CommandFull != "echo ok" {
		t.Fatalf("unprocessed stable fields = %+v", got)
	}
	if got.Source == nil || got.Source.WorkingDir != "/tmp/done" {
		t.Fatalf("unprocessed source provenance = %+v", got.Source)
	}
	if got.PlacementBucket != string(jobview.BucketCompletions) || got.EndTime == nil || got.StateSince != *got.EndTime {
		t.Fatalf("unprocessed timing = bucket:%q end:%v state_since:%d", got.PlacementBucket, got.EndTime, got.StateSince)
	}
}

func TestDaemonInfoAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	shutdown := make(chan struct{}, 1)
	want := DaemonInfo{
		PID:               os.Getpid(),
		Version:           "test-version",
		Executable:        "/tmp/weft",
		ExecutableModTime: 123,
		StartedAt:         456,
	}
	server, err := StartServerWithOptions(ctx, nil, socketPath, ServerOptions{
		Info: want,
		Shutdown: func() {
			shutdown <- struct{}{}
		},
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()

	got, err := DialDaemonInfo(ctx, socketPath)
	if err != nil {
		t.Fatalf("DialDaemonInfo: %v", err)
	}
	if got != want {
		t.Fatalf("DialDaemonInfo = %+v, want %+v", got, want)
	}
	if err := DialShutdown(ctx, socketPath); err != nil {
		t.Fatalf("DialShutdown: %v", err)
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func TestMutationsRunThroughSingleWriterExecutor(t *testing.T) {
	database := db.SetupTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	calls := 0
	server, err := StartServerWithOptions(ctx, database, socketPath, ServerOptions{
		Mutate: func(ctx context.Context, database *sql.DB, req MutationRequest) (json.RawMessage, error) {
			calls++
			if calls == 1 {
				close(firstEntered)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return nil, nil
			}
			secondEntered <- struct{}{}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- DialMutation(ctx, socketPath, "first", map[string]string{"x": "1"}, nil)
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not enter handler")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- DialMutation(ctx, socketPath, "second", map[string]string{"x": "2"}, nil)
	}()
	select {
	case <-secondEntered:
		t.Fatal("second mutation entered before first completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second mutation did not enter after first completed")
	}
}

func insertWatchTestJob(t *testing.T, database *sql.DB, id int64, status string) {
	t.Helper()
	insertWatchTestProjectJob(t, database, id, "", status)
}

func insertWatchTestProjectJob(t *testing.T, database *sql.DB, id int64, project string, status string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, command, project, tombstoned) VALUES (?, ?, ?, ?, 0)`,
		id, "/tmp", "echo test", project); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, status, queued_at) VALUES (?, 1, ?, ?)`,
		id, status, time.Now().Unix()); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func TestSubmitJobRecordsThroughDaemon(t *testing.T) {
	database := db.SetupTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer server.Close()

	jobID, err := DialSubmitJob(ctx, server.socketPath, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo hi",
		Description: "daemon submit",
		Tags:        []string{"EXP-350"},
		Inputs:      []string{"hf:meta-llama/Llama-3.1-8B"},
	})
	if err != nil {
		t.Fatalf("DialSubmitJob: %v", err)
	}
	if jobID <= 0 {
		t.Fatalf("jobID = %d, want positive", jobID)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatal("submitted job missing")
	}
	if job.Command != "echo hi" || job.Description != "daemon submit" {
		t.Fatalf("job = %+v", job)
	}
	if got := job.Inputs; len(got) != 1 || got[0] != "hf:meta-llama/Llama-3.1-8B" {
		t.Fatalf("inputs = %v", got)
	}
	if got := job.Tags; len(got) != 1 || got[0] != "EXP-350" {
		t.Fatalf("tags = %v", got)
	}
}

func nextWatchEvent(t *testing.T, watcher *Watcher) Event {
	t.Helper()
	type result struct {
		event Event
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		event, err := watcher.Next()
		ch <- result{event: event, err: err}
	}()
	select {
	case result := <-ch:
		if result.err != nil {
			t.Fatalf("watcher.Next: %v", result.err)
		}
		return result.event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch event")
		return Event{}
	}
}

func nextSubscriptionEvent(t *testing.T, sub *Subscription) Event {
	t.Helper()
	type result struct {
		event Event
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		event, err := sub.Next()
		ch <- result{event: event, err: err}
	}()
	select {
	case result := <-ch:
		if result.err != nil {
			t.Fatalf("subscription.Next: %v", result.err)
		}
		return result.event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscription event")
		return Event{}
	}
}
