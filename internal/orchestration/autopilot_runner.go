package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Coarse, observable, externally-controllable layer above the per-scope
// auto_leases. Ensures only one autopilot pass runs at a time across all
// processes sharing a database, and exposes a sticky `paused` flag. See
// specs/campaign-lifecycle.allium § Autopilot singleton state.

const AutopilotPassStaleAfter = 30 * time.Second
const autopilotHeartbeatInterval = 5 * time.Second

// Cooldowns between auto-pilot passes; shared by every runner (TUI and CLI)
// so dispatch policy is defined once.
const (
	AutopilotCooldownProgress = 5 * time.Second
	AutopilotCooldownIdle     = 30 * time.Second
	AutopilotCooldownBlocked  = 30 * time.Second
	AutopilotCooldownError    = 60 * time.Second
	AutopilotCooldownContend  = 15 * time.Second
)

var ErrAutopilotPaused = errors.New("autopilot is paused")
var ErrAutopilotBusy = errors.New("autopilot pass is in progress on another runner")

// AutopilotRunner claims the singleton autopilot pass slot, heartbeats while a
// pass runs, and releases on completion.
type AutopilotRunner struct {
	database *sql.DB
	pid      int
	label    string
	host     string

	mu      sync.Mutex
	holding bool
	stopHB  chan struct{}
	doneHB  chan struct{}
}

func NewAutopilotRunner(database *sql.DB, label string) *AutopilotRunner {
	host, _ := os.Hostname()
	return &AutopilotRunner{
		database: database,
		pid:      os.Getpid(),
		label:    label,
		host:     host,
	}
}

// TryAcquire claims the pass slot. Returns ErrAutopilotPaused / ErrAutopilotBusy
// without holding any lock if the slot is unavailable. On success, the caller
// MUST call Release.
func (r *AutopilotRunner) TryAcquire() error {
	if r == nil || r.database == nil {
		return nil
	}
	claimed, paused, _, err := db.TryClaimAutopilotPass(r.database, r.pid, r.label, r.host, AutopilotPassStaleAfter)
	if err != nil {
		return err
	}
	if paused {
		return ErrAutopilotPaused
	}
	if !claimed {
		return ErrAutopilotBusy
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.holding = true
	r.stopHB = make(chan struct{})
	r.doneHB = make(chan struct{})
	go r.heartbeatLoop(r.stopHB, r.doneHB)
	return nil
}

func (r *AutopilotRunner) heartbeatLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(autopilotHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := db.HeartbeatAutopilotPass(r.database, r.pid); err != nil {
				// Slot taken or DB error — stop refreshing; Release becomes a no-op.
				return
			}
		}
	}
}

func (r *AutopilotRunner) Release(duration time.Duration, summary string, passErr error) error {
	if r == nil || r.database == nil {
		return nil
	}
	r.mu.Lock()
	holding := r.holding
	stop := r.stopHB
	done := r.doneHB
	r.holding = false
	r.stopHB = nil
	r.doneHB = nil
	r.mu.Unlock()
	if !holding {
		return nil
	}
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
	return db.ReleaseAutopilotPass(r.database, r.pid, duration, summary, passErr)
}

// RunGroupedAutoPilotPassGated wraps RunGroupedAutoPilotPass with the global
// pause/active-runner gate. On ErrAutopilotPaused or ErrAutopilotBusy the
// inner pass is not run.
func RunGroupedAutoPilotPassGated(ctx context.Context, database *sql.DB, scopedJobs []*db.Job, label string) (result *GroupedAutoPilotResult, err error) {
	runner := NewAutopilotRunner(database, label)
	if err := runner.TryAcquire(); err != nil {
		return nil, err
	}
	started := time.Now()
	defer func() {
		_ = runner.Release(time.Since(started), summarizeAutoPilotResult(result), err)
	}()
	result, err = RunGroupedAutoPilotPass(ctx, database, scopedJobs)
	return result, err
}

func summarizeAutoPilotResult(r *GroupedAutoPilotResult) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("placed=%d rebalanced=%d launched=%d blocked=%d",
		r.Placed, r.Rebalanced, r.Launched, len(r.BlockedReasons))
}

// IsAutopilotPaused is a single-column read for callers (e.g. watch_tui) that
// don't take the full pass lock but still want to short-circuit when paused.
func IsAutopilotPaused(database *sql.DB) (bool, error) {
	return db.IsAutopilotPaused(database)
}
