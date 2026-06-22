package daemonapi

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
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
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, ?, ?, 0)`,
		id, "/tmp", "echo test"); err != nil {
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
