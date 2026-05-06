package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/narrate"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/status"
	"github.com/spf13/cobra"
)

var (
	narrateTickFlag    time.Duration
	narrateProjectFlag string
	narrateModelFlag   string
	narrateOnceFlag    bool
	narrateDebugFlag   bool
	narrateNoSyncFlag  bool
	narrateNoAutopilot bool
)

var narrateCmd = &cobra.Command{
	Use:   "narrate",
	Short: "Stream a human-readable LLM narration of weft job and instance activity",
	Long: `Watches job, cloud-instance, campaign, and autopilot transitions and
streams an Anthropic-generated narration to stdout.

Each tick narrate also drives a cloud sync and an autopilot pass (using the
same singleton lock as the TUIs and ` + "`" + `--wait` + "`" + ` jobs), so it doubles as a
headless run-loop. Disable either with ` + "`" + `--no-sync` + "`" + ` / ` + "`" + `--no-autopilot` + "`" + `.

Requires ANTHROPIC_API_KEY in the environment or in ~/.config/weft/config as
"ANTHROPIC_API_KEY=...". Narration is commentary, not authoritative status —
use 'weft instance watch' / 'weft job watch' for the structured truth.`,
	RunE: runNarrate,
}

func init() {
	rootCmd.AddCommand(narrateCmd)
	narrateCmd.Flags().DurationVar(&narrateTickFlag, "tick", 0, "Polling interval (default 30s, or config.ai.narrate.tick_seconds)")
	narrateCmd.Flags().StringVar(&narrateProjectFlag, "project", "", "Limit narration to a project (default: whole system)")
	narrateCmd.Flags().StringVar(&narrateModelFlag, "model", "", "Override Anthropic model ID")
	narrateCmd.Flags().BoolVar(&narrateOnceFlag, "once", false, "Emit a single narration of current state, then exit")
	narrateCmd.Flags().BoolVar(&narrateDebugFlag, "debug", false, "Write raw delta payloads and per-tick cache stats to stderr")
	narrateCmd.Flags().BoolVar(&narrateNoSyncFlag, "no-sync", false, "Don't run a cloud sync each tick")
	narrateCmd.Flags().BoolVar(&narrateNoAutopilot, "no-autopilot", false, "Don't run an autopilot pass each tick")
}

func resolveTerminalWidth() int {
	if cols, _, err := term.GetSize(os.Stdout.Fd()); err == nil && cols > 20 {
		return cols
	}
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 20 {
			return n
		}
	}
	return 100
}

type narrateRunner struct {
	client      *narrate.Client
	session     *narrate.Session
	database    *sql.DB
	cfg         *config.Config
	opts        narrate.SnapshotOptions
	budgetCents int
	width       int
	apLabel     string
	syncEnabled bool
	apEnabled   bool
}

func runNarrate(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	apiKey := narrate.LookupAPIKey()
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "weft narrate: ANTHROPIC_API_KEY not set; narration disabled")
		fmt.Fprintln(os.Stderr, "  Set ANTHROPIC_API_KEY in env or in ~/.config/weft/config (KEY=VALUE format).")
		return nil
	}

	model := narrateModelFlag
	if model == "" {
		model = cfg.NarrateModel()
	}
	tick := narrateTickFlag
	if tick <= 0 {
		tick = cfg.NarrateTickInterval()
	}

	sess, err := narrate.NewSession(narrate.SessionOptions{
		CompactionThreshold: cfg.NarrateCompactionThreshold(),
		Debug:               narrateDebugFlag,
		DebugSink:           os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	client := narrate.NewClient(narrate.ClientConfig{
		APIKey:          apiKey,
		Model:           model,
		MaxOutputTokens: cfg.NarrateMaxOutputTokens(),
	})

	// narrate runs a writable DB connection because it drives sync and an
	// autopilot pass each tick (gated by the singleton claim, so it's safe
	// to run alongside other TUIs / `--wait` jobs).
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r := &narrateRunner{
		client:      client,
		session:     sess,
		database:    database,
		cfg:         cfg,
		opts:        narrate.SnapshotOptions{Project: narrateProjectFlag},
		budgetCents: cfg.AutoRunRateSoftTargetCentsPerHour(),
		width:       resolveTerminalWidth(),
		apLabel:     fmt.Sprintf("narrate/%d", os.Getpid()),
		syncEnabled: !narrateNoSyncFlag,
		apEnabled:   !narrateNoAutopilot,
	}

	// Prime with the actual current snapshot so the first run-loop tick
	// produces an empty delta (status-only entry) rather than narrating
	// every currently-active job as freshly added. --once forces emission
	// and falls back to the prompt's "empty CHANGES → one status sentence"
	// rule.
	r.driveSyncAndAutopilot(ctx)
	initial, err := narrate.BuildSnapshot(database, r.opts)
	if err != nil {
		return err
	}
	sess.SetPrev(initial)

	if err := r.tick(ctx); err != nil {
		return err
	}
	if narrateOnceFlag {
		return nil
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "weft narrate: stopping")
			return nil
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "weft narrate: tick error: %v\n", err)
			}
		}
	}
}

// driveSyncAndAutopilot runs one cloud sync followed by one gated
// autopilot pass, mirroring `weft autopilot run`. Errors are logged to
// stderr but never fatal — narrate's primary job is to observe, not to
// drive.
func (r *narrateRunner) driveSyncAndAutopilot(ctx context.Context) {
	if r.syncEnabled && r.cfg != nil {
		syncCloudStateWithTimeout(r.cfg, r.database, nil, NormalCloudSyncTimeout, false)
	}
	if !r.apEnabled {
		return
	}
	paused, perr := orchestration.IsAutopilotPaused(r.database)
	if perr == nil && paused {
		return
	}
	_, runErr := orchestration.RunGroupedAutoPilotPassGated(ctx, r.database, nil, r.apLabel)
	if runErr != nil && !errors.Is(runErr, orchestration.ErrAutopilotBusy) && !errors.Is(runErr, orchestration.ErrAutopilotPaused) {
		fmt.Fprintf(os.Stderr, "weft narrate: autopilot pass: %v\n", runErr)
	}
}

func (r *narrateRunner) tick(ctx context.Context) error {
	r.driveSyncAndAutopilot(ctx)

	snap, err := narrate.BuildSnapshot(r.database, r.opts)
	if err != nil {
		return err
	}
	delta := narrate.DiffSnapshots(r.session.Prev(), snap)
	if err := delta.ResolveRemovedJobs(r.database); err != nil {
		return fmt.Errorf("resolve removed jobs: %w", err)
	}
	if err := delta.ResolveRemovedInstances(r.database); err != nil {
		return fmt.Errorf("resolve removed instances: %w", err)
	}

	unprocessed, err := loadUnprocessedCounts(r.database, r.opts.Project)
	if err != nil {
		return fmt.Errorf("count unprocessed: %w", err)
	}
	statusLine := narrate.BuildStatusLine(snap, r.budgetCents, unprocessed)
	r.width = resolveTerminalWidth()

	if delta.Empty() && !narrateOnceFlag && r.session.Prev() != nil {
		statusChanged := r.session.UpdateStatus(statusLine)
		r.session.EmitEntry(os.Stdout, statusLine, "", statusChanged, r.width)
		r.session.SetPrev(snap)
		return nil
	}

	prevTime := time.Time{}
	if p := r.session.Prev(); p != nil {
		prevTime = p.Time
	}
	priorRecap := narrate.FormatPriorRecap(r.session.Recaps())

	tick := narrate.Tick{
		PriorRecap:      priorRecap,
		CurrentSnapshot: narrate.FormatSnapshot(snap),
		Delta:           narrate.FormatDelta(delta),
		Now:             snap.Time,
		Since:           prevTime,
	}

	r.session.EmitDebug("delta", delta)
	report, usage, err := r.client.Narrate(ctx, tick)
	if err != nil {
		return err
	}
	statusChanged := r.session.UpdateStatus(statusLine)
	r.session.EmitEntry(os.Stdout, statusLine, report.Narration, statusChanged, r.width)
	r.session.EmitDebug("usage", usage)

	if shouldCompact := r.session.AppendRecap(report.StateRecap); shouldCompact {
		fmt.Fprintln(os.Stderr, "weft narrate: compacting recap chain")
		compacted, _, err := r.client.CompactRecaps(ctx, narrate.FormatPriorRecap(r.session.Recaps()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft narrate: compaction failed (continuing without): %v\n", err)
		} else {
			r.session.ReplaceWithCompacted(compacted)
		}
	}
	r.session.SetPrev(snap)
	return nil
}

// loadUnprocessedCounts queries terminal jobs in the unprocessed inbox and
// splits the count into successes (completed) vs failures (failed / dead /
// killed / canceled). Optionally scoped to a project. Bounded to the last
// 14 days so the inbox doesn't drag in ancient history.
func loadUnprocessedCounts(database *sql.DB, project string) (narrate.UnprocessedCounts, error) {
	const maxAgeDays = 14
	jobs, err := db.ListJobsWithMaxAge(database, "", "", 0, maxAgeDays, nil, "unprocessed")
	if err != nil {
		return narrate.UnprocessedCounts{}, err
	}
	if project != "" {
		jobs = db.FilterJobsByProject(jobs, project)
	}
	var counts narrate.UnprocessedCounts
	completedProjects := map[string]struct{}{}
	failedProjects := map[string]struct{}{}
	for _, j := range jobs {
		switch j.Status {
		case status.Completed:
			counts.Completed++
			if p := strings.TrimSpace(j.Project); p != "" {
				completedProjects[p] = struct{}{}
			}
		case status.Failed, status.Dead, status.Killed, status.Canceled:
			counts.Failed++
			if p := strings.TrimSpace(j.Project); p != "" {
				failedProjects[p] = struct{}{}
			}
		}
	}
	counts.CompletedProjects = sortedMapKeys(completedProjects)
	counts.FailedProjects = sortedMapKeys(failedProjects)
	return counts, nil
}

func sortedMapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
