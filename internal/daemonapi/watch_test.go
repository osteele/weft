package daemonapi

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/narrate"
)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := fmt.Sprintf("/tmp/weft-daemonapi-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartServer(ctx, database, socketPath)
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
	if event.Activity.StatusLine == nil || event.Activity.StatusLine.QueuedJobs != 1 {
		t.Fatalf("status line = %+v, want one queued job", event.Activity.StatusLine)
	}
	if event.Activity.Delta == nil || len(event.Activity.Delta.JobAdded) != 1 {
		t.Fatalf("delta = %+v, want one added job", event.Activity.Delta)
	}
	if event.Activity.FormattedSnapshot == "" || event.Activity.FormattedDelta == "" {
		t.Fatalf("formatted payload missing: %+v", event.Activity)
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
	}, prev, false)
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
	}, nil, true)
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
