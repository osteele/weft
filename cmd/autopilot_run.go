package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var (
	autopilotRunOnce       bool
	autopilotRunInterval   time.Duration
	autopilotRunMaxPasses  int
	autopilotRunJSON       bool
	autopilotRunLabel      string
	autopilotRunPausedWait time.Duration
)

const autopilotLifecycleDebounce = 2 * time.Second

var autopilotRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the autopilot in a loop without the TUI",
	Long: `Repeatedly run autopilot passes (auto-place + auto-launch + rebalance) using
the same singleton lock and pause flag as the TUI runners. One line of summary
is printed per pass.

The cooldown between passes adapts to the previous result: short after
progress, longer when nothing is actionable, longest after errors. Use
--interval to pin it. Use --once for a single pass.

While the autopilot is paused (see "weft autopilot pause"), this command
loops without doing work, printing a paused notice each --paused-wait
interval. Resume with "weft autopilot resume" in another shell.

Use Ctrl-C to stop; the active pass is allowed to finish before exit.`,
	Args: cobra.NoArgs,
	RunE: runAutopilotRunLoop,
}

func init() {
	autopilotCmd.AddCommand(autopilotRunCmd)
	autopilotRunCmd.Flags().BoolVar(&autopilotRunOnce, "once", false, "Run a single pass and exit")
	autopilotRunCmd.Flags().DurationVar(&autopilotRunInterval, "interval", 0, "Fixed cooldown between passes (default: adaptive)")
	autopilotRunCmd.Flags().IntVar(&autopilotRunMaxPasses, "max-passes", 0, "Stop after this many passes (0 = unlimited)")
	autopilotRunCmd.Flags().BoolVar(&autopilotRunJSON, "json", false, "Emit one JSON object per pass instead of human text")
	autopilotRunCmd.Flags().StringVar(&autopilotRunLabel, "label", "", "Runner label recorded in autopilot state (default: autopilot-run/<pid>)")
	autopilotRunCmd.Flags().DurationVar(&autopilotRunPausedWait, "paused-wait", 30*time.Second, "Cooldown between paused-state checks")
}

// autopilotOutcome is a typed view of the per-pass outcome; the underlying
// string values are also the JSON contract emitted by `--json`. The policy
// that produces it is shared with the daemon and the TUI — see
// internal/orchestration/dispatch.go.
type autopilotOutcome = orchestration.PassOutcome

const (
	outcomeProgress = orchestration.OutcomeProgress
	outcomeIdle     = orchestration.OutcomeIdle
	outcomeBlocked  = orchestration.OutcomeBlocked
	outcomeError    = orchestration.OutcomeError
	outcomePaused   = orchestration.OutcomePaused
	outcomeBusy     = orchestration.OutcomeBusy
)

type autopilotRunPassEvent struct {
	Pass           int              `json:"pass"`
	StartedAt      string           `json:"started_at"`
	DurationMS     int64            `json:"duration_ms"`
	Outcome        autopilotOutcome `json:"outcome"`
	Placed         int              `json:"placed"`
	Rebalanced     int              `json:"rebalanced"`
	Launched       int              `json:"launched"`
	OverloadMoved  int              `json:"overload_moved"`
	LaunchedClass  string           `json:"launched_class,omitempty"`
	BlockedReasons map[int64]string `json:"blocked_reasons,omitempty"`
	Error          string           `json:"error,omitempty"`
	NextWaitMS     int64            `json:"next_wait_ms"`
	Note           string           `json:"note,omitempty"`
}

func runAutopilotRunLoop(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Cloud sync runs as part of every pass: without it, R2 .complete and
	// .started markers accumulate but never propagate into job_attempts,
	// so the autopilot makes placement decisions on stale state and
	// reuses already-running jobs (creating supersede thrashing). The TUI
	// runners call SyncCloud on their own ticker; the headless runner
	// must do it itself.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "warning: load config for cloud sync: %v\n", cfgErr)
	}

	label := autopilotRunLabel
	if label == "" {
		label = fmt.Sprintf("autopilot-run/%d", os.Getpid())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	changeSource := openDBChangeSource("autopilot run")
	if changeSource != nil {
		defer changeSource.Close()
	}
	wakeSnapshot, snapshotErr := readAutopilotWakeSnapshot(database)
	if snapshotErr != nil {
		fmt.Fprintf(os.Stderr, "warning: read autopilot wake state: %v\n", snapshotErr)
	}

	pass := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Pre-check pause without claiming the singleton lock so paused mode
		// loops cheaply and uses the longer --paused-wait cadence. The Gated
		// call below still re-checks; transient races are fine.
		paused, perr := orchestration.IsAutopilotPaused(database)
		if perr == nil && paused {
			pass++
			emitPausedEvent(pass, autopilotRunPausedWait)
			if autopilotRunOnce {
				return nil
			}
			if _, done := waitForDBChangeOrTimeout(ctx, changeSource, autopilotRunPausedWait, "autopilot run"); done {
				return nil
			}
			continue
		}
		pass++

		// Sync first so the pass sees fresh job_attempts state. Bounded
		// timeout: if sync exceeds it, sync continues in the background
		// while the pass proceeds with the partially-synced view.
		if cfg != nil {
			syncCloudStateWithTimeout(cfg, database, nil, NormalCloudSyncTimeout, false)
		}

		started := time.Now()
		result, runErr := orchestration.RunGroupedAutoPilotPassGated(ctx, database, nil, label)
		duration := time.Since(started)

		ev := autopilotRunPassEvent{
			Pass:       pass,
			StartedAt:  started.Format(time.RFC3339),
			DurationMS: duration.Milliseconds(),
		}
		if result != nil {
			ev.Placed = result.Placed
			ev.Rebalanced = result.Rebalanced
			ev.Launched = result.Launched
			ev.OverloadMoved = result.OverloadMoved
			ev.LaunchedClass = result.LaunchedClass
			if len(result.BlockedReasons) > 0 {
				ev.BlockedReasons = result.BlockedReasons
			}
		}

		outcome, wait := classifyAutopilotPass(result, runErr, autopilotRunPausedWait)
		ev.Outcome = outcome
		if errors.Is(runErr, orchestration.ErrAutopilotBusy) {
			ev.Note = "another runner holds the autopilot pass slot"
		}
		if outcome == outcomeError {
			ev.Error = runErr.Error()
		}
		if autopilotRunInterval > 0 {
			wait = autopilotRunInterval
		}
		ev.NextWaitMS = wait.Milliseconds()

		emitPassEvent(ev)
		if nextSnapshot, err := readAutopilotWakeSnapshot(database); err == nil {
			wakeSnapshot = nextSnapshot
		} else {
			fmt.Fprintf(os.Stderr, "warning: read autopilot wake state: %v\n", err)
		}

		if autopilotRunOnce {
			if outcome == outcomeError {
				return runErr
			}
			return nil
		}
		if autopilotRunMaxPasses > 0 && pass >= autopilotRunMaxPasses {
			return nil
		}
		var blockedReasons map[int64]string
		if result != nil {
			blockedReasons = result.BlockedReasons
		}
		var reason autopilotWakeReason
		wakeSnapshot, reason = waitForAutopilotInvalidation(ctx, autopilotWaitParams{
			changeSource:  changeSource,
			database:      database,
			baseline:      wakeSnapshot,
			wait:          wait,
			timerRunsPass: autopilotTimerRunsPass(outcome, blockedReasons),
			syncCfg:       cfg,
			label:         "autopilot run",
		})
		if reason == autopilotWakeDone {
			return nil
		}
	}
}

// Thin adapters over the shared dispatch policy in internal/orchestration, so
// the headless runner, the daemon, and the TUI cannot drift on when a pass is
// worth running.

type autopilotWakeSnapshot = orchestration.WakeSnapshot

type autopilotWakeReason = orchestration.WakeReason

const (
	autopilotWakeTimer = orchestration.WakeTimer
	autopilotWakeDone  = orchestration.WakeDone
)

func readAutopilotWakeSnapshot(database *sql.DB) (autopilotWakeSnapshot, error) {
	return orchestration.ReadWakeSnapshot(database)
}

func autopilotTimerRunsPass(outcome autopilotOutcome, blockedReasons map[int64]string) bool {
	return orchestration.TimerRunsPass(outcome, blockedReasons)
}

func classifyAutopilotPass(result *orchestration.GroupedAutoPilotResult, err error, pausedWait time.Duration) (autopilotOutcome, time.Duration) {
	return orchestration.ClassifyPass(result, err, pausedWait)
}

// autopilotWaitParams configures one wait between passes.
type autopilotWaitParams struct {
	changeSource *dbwatch.Source
	database     *sql.DB
	baseline     autopilotWakeSnapshot
	wait         time.Duration

	// timerRunsPass carries the shared policy verdict for the pass that just
	// finished.
	timerRunsPass bool

	// quietWait ends the wait with a timer reason even when timerRunsPass is
	// false. The daemon uses it to hold its sync cadence; the headless runner
	// leaves it zero because a wake it does not act on is wasted.
	quietWait time.Duration

	// syncCfg, when set, refreshes cloud state on a quiet timer expiry. A
	// sync that updates rows ends the wait, since it may have unblocked work
	// that produced no other local write.
	syncCfg *config.Config

	label string
}

func waitForAutopilotInvalidation(ctx context.Context, p autopilotWaitParams) (autopilotWakeSnapshot, autopilotWakeReason) {
	var waiter orchestration.ChangeWaiter
	if p.changeSource != nil {
		waiter = p.changeSource
	}
	label := p.label
	if label == "" {
		label = "autopilot"
	}

	opts := orchestration.InvalidationOptions{
		Waiter:        waiter,
		ReadSnapshot:  func() (autopilotWakeSnapshot, error) { return readAutopilotWakeSnapshot(p.database) },
		Baseline:      p.baseline,
		Wait:          p.wait,
		TimerRunsPass: p.timerRunsPass,
		QuietWait:     p.quietWait,
		Backstop:      orchestration.AutopilotQuietBackstop,
		Debounce:      autopilotLifecycleDebounce,
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "warning: %s wake state: %v\n", label, err)
		},
	}
	if p.syncCfg != nil {
		opts.OnQuietTimeout = func(context.Context) bool {
			result, completed := syncCloudStateWithTimeout(p.syncCfg, p.database, nil, NormalCloudSyncTimeout, false)
			return !completed || result.Updated > 0
		}
	}
	return orchestration.WaitForInvalidation(ctx, opts)
}

// waitOrDone sleeps for d, returning true if the context was canceled first.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

func emitPausedEvent(pass int, wait time.Duration) {
	if autopilotRunJSON {
		ev := autopilotRunPassEvent{
			Pass:       pass,
			StartedAt:  time.Now().Format(time.RFC3339),
			Outcome:    outcomePaused,
			NextWaitMS: wait.Milliseconds(),
			Note:       "autopilot is paused — resume with `weft autopilot resume`",
		}
		_ = json.NewEncoder(os.Stdout).Encode(ev)
		return
	}
	fmt.Printf("[%s] pass %d: paused (next check in %s)\n",
		time.Now().Format("15:04:05"), pass, wait.Truncate(time.Second))
}

func emitPassEvent(ev autopilotRunPassEvent) {
	if autopilotRunJSON {
		_ = json.NewEncoder(os.Stdout).Encode(ev)
		return
	}
	parts := []string{
		fmt.Sprintf("pass %d", ev.Pass),
		string(ev.Outcome),
		fmt.Sprintf("placed=%d", ev.Placed),
		fmt.Sprintf("launched=%d", ev.Launched),
		fmt.Sprintf("rebalanced=%d", ev.Rebalanced),
		fmt.Sprintf("overload_moved=%d", ev.OverloadMoved),
		fmt.Sprintf("blocked=%d", orchestration.AutoPilotBlockedReasonCount(ev.BlockedReasons)),
		fmt.Sprintf("dur=%s", time.Duration(ev.DurationMS)*time.Millisecond),
	}
	if ev.LaunchedClass != "" {
		parts = append(parts, fmt.Sprintf("class=%s", ev.LaunchedClass))
	}
	if ev.Error != "" {
		parts = append(parts, fmt.Sprintf("err=%q", ev.Error))
	}
	if ev.Note != "" {
		parts = append(parts, ev.Note)
	}
	fmt.Printf("[%s] %s (next in %s)\n",
		time.Now().Format("15:04:05"),
		strings.Join(parts, " "),
		time.Duration(ev.NextWaitMS)*time.Millisecond)

	if reason := anyBlockedReason(ev.BlockedReasons); reason != "" {
		fmt.Printf("    blocked sample: %s\n", reason)
	}
}

// anyBlockedReason returns the first non-blank value from m. Map iteration
// order is non-deterministic, so the choice is arbitrary among non-blank values.
func anyBlockedReason(m map[int64]string) string {
	for _, r := range m {
		if s := strings.TrimSpace(r); s != "" {
			if blockreason.ReasonKind(s) != blockreason.KindBlocked {
				continue
			}
			return s
		}
	}
	return ""
}
