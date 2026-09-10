package daemoncontrol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
)

// RecoveryWindow allows launchd's default 10-second ThrottleInterval plus a
// five-second startup allowance. It is a policy budget, not a latency estimate.
const RecoveryWindow = 15 * time.Second

// RecoverAbsent waits for the daemon endpoint, starting a confirmed-absent
// daemon at most once. It never stops a process, replaces a binary, or removes
// a socket. The caller's deadline can shorten the recovery window.
func RecoverAbsent(ctx context.Context, paths Paths) error {
	return recoverAbsent(ctx, paths, startAbsentDaemon)
}

func recoverAbsent(ctx context.Context, paths Paths, start func(context.Context, Paths) error) error {
	ctx, cancel := context.WithTimeout(ctx, RecoveryWindow)
	defer cancel()
	if paths.LockFile == "" || paths.PIDFile == "" || paths.SocketFile == "" {
		return fmt.Errorf("daemon recovery requires process lock, PID, and socket paths")
	}
	recoveryPaths := paths
	recoveryPaths.LockFile += ".recovery"
	var recoveryLock *Lock
	defer func() { recoveryLock.Close() }()
	started := false
	backoff := 100 * time.Millisecond
	var lastErr error
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("daemon recovery: %w; last observation: %v", ctx.Err(), lastErr)
		}
		probeCtx, cancelProbe := context.WithTimeout(ctx, 250*time.Millisecond)
		info, err := daemonapi.DialDaemonInfo(probeCtx, paths.SocketFile)
		cancelProbe()
		if err == nil {
			if info.PID <= 0 {
				return fmt.Errorf("daemon status unknown: socket reported invalid PID %d", info.PID)
			}
			return nil
		}
		lastErr = err
		if IsConfirmedStaleSocketError(err) && !started {
			if recoveryLock == nil {
				recoveryLock, err = AcquireLock(recoveryPaths)
				if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
					return fmt.Errorf("daemon recovery lock: %w", err)
				}
			}
			if recoveryLock != nil {
				started, err = startIfConfirmedAbsent(ctx, paths, start)
				if err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Second)
	}
}

func startIfConfirmedAbsent(ctx context.Context, paths Paths, start func(context.Context, Paths) error) (bool, error) {
	// A daemon starting before it has written its PID still holds this lock.
	// The separate recovery lock serializes waiters through startup/readiness;
	// the daemon's own lock remains its final admission gate against other
	// launchers, including launchd, racing the release-to-spawn gap.
	lock, err := AcquireLock(paths)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("daemon status unknown: process lock: %w", err)
	}
	pid, found, err := ReadPID(paths.PIDFile)
	if err != nil {
		lock.Close()
		return false, fmt.Errorf("daemon status unknown: %w", err)
	}
	if found {
		err = syscall.Kill(pid, 0)
		if err == nil {
			lock.Close()
			return false, nil
		}
		if !errors.Is(err, syscall.ESRCH) {
			lock.Close()
			return false, fmt.Errorf("daemon status unknown: probe PID %d: %w", pid, err)
		}
	}
	if err := lock.Close(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := start(ctx, paths); err != nil {
		return true, fmt.Errorf("start absent daemon: %w", err)
	}
	return true, nil
}

func startAbsentDaemon(ctx context.Context, paths Paths) error {
	if _, err := os.Stat(paths.PlistFile); err == nil {
		return Load(ctx, paths)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon installation status unknown: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := startDetachedProcess(paths)
	return err
}
