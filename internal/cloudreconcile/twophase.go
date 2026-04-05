package cloudreconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/osteele/weft/internal/db"
)

const (
	DefaultLeaseScope  = "sync:cloud:reconcile"
	DefaultLeaseTTL    = 180 * time.Second
	DefaultFastTimeout = 10 * time.Second
	DefaultFullTimeout = 60 * time.Second
	DefaultInterval    = 60 * time.Second
)

// Config controls two-phase cloud reconciliation.
type Config struct {
	Owner       string
	Scope       string
	LeaseTTL    time.Duration
	FastTimeout time.Duration
	FullTimeout time.Duration
}

// PhaseRunner executes one reconcile phase.
// full=false is the fast phase, full=true is the full phase.
type PhaseRunner func(ctx context.Context, full bool) (int, error)

// PhaseResult captures one phase outcome.
type PhaseResult struct {
	Acquired  bool
	Completed bool
	TimedOut  bool
	Updated   int
	Err       error
}

// Result captures a full two-phase pass outcome.
type Result struct {
	Fast PhaseResult
	Full PhaseResult
}

// RunTwoPhasePass runs fast then full cloud reconciliation.
// Each phase independently acquires/releases the same lease scope to support
// leader succession across multiple long-running processes.
func RunTwoPhasePass(ctx context.Context, database *sql.DB, cfg Config, run PhaseRunner) Result {
	cfg = normalizeConfig(cfg)
	logger := slog.Default().With("component", "cloudreconcile")

	result := Result{}
	result.Fast = runPhase(ctx, database, cfg, false, run, logger)
	if ctx.Err() != nil {
		return result
	}
	result.Full = runPhase(ctx, database, cfg, true, run, logger)
	return result
}

func runPhase(ctx context.Context, database *sql.DB, cfg Config, full bool, run PhaseRunner, logger *slog.Logger) PhaseResult {
	if ctx.Err() != nil {
		return PhaseResult{}
	}

	phase := "fast"
	timeout := cfg.FastTimeout
	if full {
		phase = "full"
		timeout = cfg.FullTimeout
	}

	ok, err := db.AcquireAutoLease(database, cfg.Scope, cfg.Owner, cfg.LeaseTTL)
	if err != nil {
		logger.Warn("cloud reconcile lease error", "phase", phase, "error", err)
		return PhaseResult{Err: fmt.Errorf("acquire lease: %w", err)}
	}
	if !ok {
		return PhaseResult{}
	}
	defer func() { _ = db.ReleaseAutoLease(database, cfg.Scope, cfg.Owner) }()

	updated, completed, timedOut, runErr := runWithTimeout(ctx, timeout, func(phaseCtx context.Context) (int, error) {
		return run(phaseCtx, full)
	})
	if runErr != nil {
		logger.Warn("cloud reconcile phase failed", "phase", phase, "error", runErr)
	}
	if timedOut {
		logger.Warn("cloud reconcile phase timed out", "phase", phase, "timeout", timeout)
	}
	return PhaseResult{
		Acquired:  true,
		Completed: completed,
		TimedOut:  timedOut,
		Updated:   updated,
		Err:       runErr,
	}
}

func runWithTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) (int, error)) (updated int, completed bool, timedOut bool, err error) {
	if timeout <= 0 {
		updated, err = fn(ctx)
		return updated, true, false, err
	}

	phaseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type runResult struct {
		updated int
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		u, runErr := fn(phaseCtx)
		done <- runResult{updated: u, err: runErr}
	}()

	select {
	case res := <-done:
		return res.updated, true, false, res.err
	case <-phaseCtx.Done():
		if errors.Is(phaseCtx.Err(), context.DeadlineExceeded) {
			return 0, false, true, nil
		}
		return 0, false, false, phaseCtx.Err()
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.Scope == "" {
		cfg.Scope = DefaultLeaseScope
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.FastTimeout <= 0 {
		cfg.FastTimeout = DefaultFastTimeout
	}
	if cfg.FullTimeout <= 0 {
		cfg.FullTimeout = DefaultFullTimeout
	}
	if cfg.Owner == "" {
		cfg.Owner = OwnerID()
	}
	return cfg
}

// OwnerID returns a stable process-unique owner identifier for lease ownership.
func OwnerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
}
