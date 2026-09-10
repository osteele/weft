package daemoncontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
)

func TestRecoverAbsentStartsOnceForConcurrentWaiters(t *testing.T) {
	paths := recoveryTestPaths(t)
	if err := WritePIDFile(paths.PIDFile, stalePID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var starts atomic.Int32
	start := func(context.Context, Paths) error {
		starts.Add(1)
		return serveRecoveryDaemon(t, paths)
	}
	gate := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() {
			<-gate
			results <- recoverAbsent(ctx, paths, start)
		}()
	}
	close(gate)
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("started %d daemons for concurrent waiters", starts.Load())
	}
	info, err := daemonapi.DialDaemonInfo(ctx, paths.SocketFile)
	if err != nil || info.PID != os.Getpid() {
		t.Fatalf("recovered endpoint = %+v, %v", info, err)
	}
}

func TestRecoverAbsentPreservesAmbiguousState(t *testing.T) {
	for _, state := range []string{"live-pid", "invalid-pid", "startup-lock", "unresponsive-socket", "foreign-socket"} {
		t.Run(state, func(t *testing.T) {
			paths := recoveryTestPaths(t)
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			switch state {
			case "live-pid":
				if err := WritePIDFile(paths.PIDFile, os.Getpid()); err != nil {
					t.Fatal(err)
				}
			case "invalid-pid":
				if err := os.WriteFile(paths.PIDFile, []byte("unreadable identity\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "startup-lock":
				lock, err := AcquireLock(paths)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "unresponsive-socket", "foreign-socket":
				listener, err := net.Listen("unix", paths.SocketFile)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				// The endpoint accepts connections but cannot establish its
				// identity. It must survive the failed recovery unchanged.
				if state == "foreign-socket" {
					go func() {
						for {
							conn, err := listener.Accept()
							if err != nil {
								return
							}
							conn.SetDeadline(time.Now().Add(time.Second))
							var req daemonapi.Request
							if err := json.NewDecoder(conn).Decode(&req); err == nil {
								_ = json.NewEncoder(conn).Encode(daemonapi.Event{Type: daemonapi.EventError, Error: "unsupported protocol"})
							}
							conn.Close()
						}
					}()
				}
			}
			starts := 0
			err := recoverAbsent(ctx, paths, func(context.Context, Paths) error {
				starts++
				return errors.New("unexpected process start")
			})
			if err == nil || starts != 0 {
				t.Fatalf("recovery of %s: err=%v starts=%d", state, err, starts)
			}
			if state == "unresponsive-socket" || state == "foreign-socket" {
				if _, err := os.Lstat(paths.SocketFile); err != nil {
					t.Fatalf("ambiguous socket removed: %v", err)
				}
			}
			if state == "live-pid" {
				pid, found, err := ReadPID(paths.PIDFile)
				if err != nil || !found || pid != os.Getpid() {
					t.Fatalf("live PID changed: %d %v %v", pid, found, err)
				}
			}
		})
	}
}

func TestRecoverAbsentLeavesStaleSocketForLockedStartup(t *testing.T) {
	paths := recoveryTestPaths(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.SocketFile, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = recoverAbsent(ctx, paths, func(context.Context, Paths) error {
		if _, err := os.Lstat(paths.SocketFile); err != nil {
			return fmt.Errorf("observer removed stale socket before startup: %w", err)
		}
		return serveRecoveryDaemon(t, paths)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecoverAbsentDoesNotRestartLoop(t *testing.T) {
	paths := recoveryTestPaths(t)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	starts := 0
	err := recoverAbsent(ctx, paths, func(context.Context, Paths) error {
		starts++
		// Simulate an admitted process exiting before opening its socket.
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || starts != 1 {
		t.Fatalf("recovery = %v, starts=%d; want bounded single start", err, starts)
	}
	recoveryPaths := paths
	recoveryPaths.LockFile += ".recovery"
	lock, err := AcquireLock(recoveryPaths)
	if err != nil {
		t.Fatalf("recovery retained its lock: %v", err)
	}
	lock.Close()
}

func TestRecoverAbsentCanceledBeforeAdmission(t *testing.T) {
	paths := recoveryTestPaths(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := recoverAbsent(ctx, paths, func(context.Context, Paths) error {
		t.Fatal("started daemon after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery = %v, want cancellation", err)
	}
}

func TestRecoverAbsentWaitsForLiveDaemonStartup(t *testing.T) {
	paths := recoveryTestPaths(t)
	if err := WritePIDFile(paths.PIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			ready <- ctx.Err()
		case <-time.After(2200 * time.Millisecond):
			ready <- serveRecoveryDaemon(t, paths)
		}
	}()
	err := recoverAbsent(ctx, paths, func(context.Context, Paths) error {
		t.Error("restarted an existing daemon during startup")
		return errors.New("unexpected start")
	})
	if serverErr := <-ready; serverErr != nil {
		t.Fatal(serverErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func recoveryTestPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		PIDFile:    filepath.Join(dir, "daemon.pid"),
		LockFile:   filepath.Join(dir, "daemon.lock"),
		SocketFile: shortTestSocketPath(t),
		PlistFile:  filepath.Join(dir, "daemon.plist"),
		StdoutLog:  filepath.Join(dir, "stdout.log"),
		StderrLog:  filepath.Join(dir, "stderr.log"),
	}
}

func serveRecoveryDaemon(t *testing.T, paths Paths) error {
	t.Helper()
	lock, err := AcquireLock(paths)
	if err != nil {
		return err
	}
	t.Cleanup(func() { lock.Close() })
	if err := EnsureSocketAvailable(paths, 100*time.Millisecond); err != nil {
		return err
	}
	pid, found, err := ReadPID(paths.PIDFile)
	if err != nil {
		return err
	}
	if !found || pid != os.Getpid() {
		if err := WritePIDFile(paths.PIDFile, os.Getpid()); err != nil {
			return err
		}
	}
	server, err := daemonapi.StartServerWithOptions(t.Context(), nil, paths.SocketFile, daemonapi.ServerOptions{
		Info: daemonapi.DaemonInfo{PID: os.Getpid(), Version: "recovery-test"},
	})
	if err != nil {
		return err
	}
	t.Cleanup(func() { server.Close() })
	return nil
}
