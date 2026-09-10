package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/watchevents"
)

func TestDaemonWaitReconnect(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo hi", "watch reconnect", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	listener := daemonWaitTestListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	serverDone := make(chan error, 1)
	go func() {
		for attempt := range 3 {
			conn, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			var req daemonapi.Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				serverDone <- err
				return
			}
			if attempt == 1 {
				// A retiring daemon can accept the reconnect, then close
				// it before its readiness reply. Observation should retry.
				conn.Close()
				continue
			}
			if attempt == 2 {
				exitCode := 0
				if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitCode, time.Now().Unix()); err != nil {
					conn.Close()
					serverDone <- err
					return
				}
			}
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				conn.Close()
				serverDone <- err
				return
			}
			snapshot := watchevents.BuildSnapshotEvent([]*db.Job{job}, time.Now())
			encoder := json.NewEncoder(conn)
			err = encoder.Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionReady})
			if err == nil {
				err = encoder.Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionSnapshot, Snapshot: &snapshot})
			}
			conn.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	final := map[int64]*db.Job{}
	pending := map[int64]struct{}{jobID: {}}
	used, err := waitForJobsCompletionViaDaemon(ctx, database, final, pending, []int64{jobID}, map[int64]string{}, 2*time.Second)
	if err != nil {
		t.Fatalf("wait after transport disconnect: %v", err)
	}
	if !used || len(pending) != 0 || final[jobID] == nil || final[jobID].Status != db.StatusCompleted {
		t.Fatalf("wait did not observe completion: used=%v pending=%v final=%v", used, pending, final)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonWaitReconnectDeadline(t *testing.T) {
	database := db.SetupTestDB(t)
	listener := daemonWaitTestListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	serverDone := make(chan error, 1)
	go func() {
		for attempt := range 2 {
			conn, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			var req daemonapi.Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				serverDone <- err
				return
			}
			if attempt == 0 {
				enc := json.NewEncoder(conn)
				if err := enc.Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionReady}); err != nil {
					conn.Close()
					serverDone <- err
					return
				}
				if err := enc.Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionSnapshot}); err != nil {
					conn.Close()
					serverDone <- err
					return
				}
				conn.Close()
			} else {
				// Accept the reconnect but never send readiness. The original wait
				// deadline must interrupt the handshake, not only the initial dial.
				var ignored daemonapi.Request
				err := json.NewDecoder(conn).Decode(&ignored)
				conn.Close()
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					serverDone <- fmt.Errorf("client did not close handshake at deadline")
					return
				}
			}
		}
		serverDone <- nil
	}()
	used, err := waitForJobsCompletionViaDaemon(ctx, database, map[int64]*db.Job{}, map[int64]struct{}{1: {}}, []int64{1}, map[int64]string{}, 300*time.Millisecond)
	if !used || !errors.Is(err, errWaitTimeout) {
		t.Fatalf("wait = %v, %v; want wait timeout", used, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonWaitRepeatedEOFFallsBackToPolling(t *testing.T) {
	database := db.SetupTestDB(t)
	listener := daemonWaitTestListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					err = nil
				}
				serverDone <- err
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			var req daemonapi.Request
			err = json.NewDecoder(conn).Decode(&req)
			if err == nil {
				err = json.NewEncoder(conn).Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionReady})
			}
			conn.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
	}()
	// There is no CLI wait timeout. The parent deadline is a test safety
	// bound. A stream that never delivers a snapshot must not fail the
	// wait: observation degrades to remote status sync and database polling,
	// so the watcher lets the caller poll (wb116/wb117).
	used, err := waitForJobsCompletionViaDaemon(ctx, database, map[int64]*db.Job{}, map[int64]struct{}{1: {}}, []int64{1}, map[int64]string{}, 0)
	listener.Close()
	if used || err != nil {
		t.Fatalf("wait = %v, %v; want polling fallback without error", used, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonWaitFallbackPreservesDeadline(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp", "echo hi", "watch fallback deadline", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedToRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	listener := daemonWaitTestListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()

	const timeout = 2 * time.Second
	const daemonDelay = time.Second
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		var req daemonapi.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverDone <- err
			return
		}
		if err := json.NewEncoder(conn).Encode(daemonapi.Event{Type: daemonapi.EventSubscriptionReady}); err != nil {
			serverDone <- err
			return
		}
		// Spend part of the wait budget watching, then break the stream.
		select {
		case <-time.After(daemonDelay):
			_, err = fmt.Fprintln(conn, "{invalid JSON")
		case <-ctx.Done():
			err = ctx.Err()
		}
		serverDone <- err
	}()

	originalSync := syncJobForStatusWaitFunc
	syncCalls := 0
	syncJobForStatusWaitFunc = func(_ *sql.DB, _ *db.Job, _ ops.SyncOptions) (ops.SyncResult, error) {
		syncCalls++
		return ops.SyncResult{}, nil
	}
	t.Cleanup(func() { syncJobForStatusWaitFunc = originalSync })
	started := time.Now()
	final, err := waitForJobsCompletion(ctx, database, []jobStatusRequest{{ID: jobID, Job: job}}, timeout, nil)
	elapsed := time.Since(started)
	if serverErr := <-serverDone; serverErr != nil {
		t.Fatal(serverErr)
	}
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("wait error = %v, want wait timeout", err)
	}
	if syncCalls == 0 {
		t.Fatal("wait never fell back to remote status sync")
	}
	if final[jobID] == nil || final[jobID].Status != db.StatusRunning {
		t.Fatalf("timeout changed the pending job: %+v", final[jobID])
	}
	// A fresh polling timeout would expire at least daemonDelay later.
	// Allow scheduling slack without accepting that reset.
	if elapsed >= timeout+daemonDelay/2 {
		t.Fatalf("wait took %s; timeout %s was reset after daemon fallback", elapsed, timeout)
	}
}

func daemonWaitTestListener(t *testing.T) net.Listener {
	t.Helper()
	// Keep Unix socket paths below the macOS sockaddr limit.
	home, err := os.MkdirTemp("/tmp", "weft-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	paths := daemoncontrol.DefaultPaths()
	if err := os.MkdirAll(filepath.Dir(paths.SocketFile), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.SocketFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	original := ensureDaemonStartedFunc
	ensureDaemonStartedFunc = func(daemoncontrol.Paths, time.Duration) (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		return daemoncontrol.Status{}, daemoncontrol.EnsureNoop, nil
	}
	t.Cleanup(func() { ensureDaemonStartedFunc = original })
	return listener
}
