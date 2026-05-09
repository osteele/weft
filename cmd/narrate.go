package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/narrate"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/slack"
	"github.com/osteele/weft/internal/status"
	"github.com/spf13/cobra"
)

var (
	narrateTickFlag    time.Duration
	narrateQuietFlag   time.Duration
	narrateProjectFlag string
	narrateModelFlag   string
	narrateOnceFlag    bool
	narrateDebugFlag   bool
	narrateNoSyncFlag  bool
	narrateNoAutopilot bool
	narrateSlackFlag   bool
	narrateSlackMin    time.Duration
)

var narrateCmd = &cobra.Command{
	Use:   "narrate",
	Short: "Stream a human-readable LLM narration of weft job and instance activity",
	Long: `Watches job, cloud-instance, campaign, and autopilot transitions and
streams an LLM-generated narration to stdout.

Each tick narrate also drives a cloud sync and an autopilot pass (using the
same singleton lock as the TUIs and ` + "`" + `--wait` + "`" + ` jobs), so it doubles as a
headless run-loop. Disable either with ` + "`" + `--no-sync` + "`" + ` / ` + "`" + `--no-autopilot` + "`" + `.

Requires a provider API key in the environment, in ~/.config/weft/config.toml,
or in ~/.config/weft/config as KEY=VALUE. Narration is commentary, not
authoritative status — use 'weft instance watch' / 'weft job watch' for the
structured truth.`,
	RunE: runNarrate,
}

func init() {
	rootCmd.AddCommand(narrateCmd)
	narrateCmd.Flags().DurationVar(&narrateTickFlag, "tick", 0, "Maximum interval between checks (default 30s, or config.ai.narrate.tick_seconds)")
	narrateCmd.Flags().DurationVar(&narrateQuietFlag, "quiet-window", 0, "DB-change quiet window before narrating (default 5s, or config.ai.narrate.quiet_seconds)")
	narrateCmd.Flags().StringVar(&narrateProjectFlag, "project", "", "Limit narration to a project (default: whole system)")
	narrateCmd.Flags().StringVar(&narrateModelFlag, "model", "", "Override provider model ID")
	narrateCmd.Flags().BoolVar(&narrateOnceFlag, "once", false, "Emit a single narration of current state, then exit")
	narrateCmd.Flags().BoolVar(&narrateDebugFlag, "debug", false, "Write raw delta payloads and per-tick cache stats to stderr")
	narrateCmd.Flags().BoolVar(&narrateNoSyncFlag, "no-sync", false, "Don't run a cloud sync each tick")
	narrateCmd.Flags().BoolVar(&narrateNoAutopilot, "no-autopilot", false, "Don't run an autopilot pass each tick")
	narrateCmd.Flags().BoolVar(&narrateSlackFlag, "slack", false, "Post narrate entries to the configured Slack webhook")
	narrateCmd.Flags().DurationVar(&narrateSlackMin, "slack-min-interval", 0, "Minimum interval between Slack posts (default 5m, or config.ai.narrate.slack_min_interval_seconds)")
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
	client           *narrate.Client
	session          *narrate.Session
	database         *sql.DB
	cfg              *config.Config
	opts             narrate.SnapshotOptions
	budgetCents      int
	width            int
	apLabel          string
	syncEnabled      bool
	apEnabled        bool
	slackEnabled     bool
	debug            bool
	slackMinInterval time.Duration
	lastSlackPost    time.Time
	slackPost        func(string) error
	now              func() time.Time
}

func runNarrate(cmd *cobra.Command, _ []string) error {
	if !narrateDebugFlag {
		restoreLogs := logging.Suppress()
		defer restoreLogs()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	provider := cfg.NarrateProvider()
	apiKey := narrate.LookupProviderAPIKey(provider, cfg.LLM.APIKey)
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "weft narrate: %s API key not set; narration disabled\n", provider)
		fmt.Fprintln(os.Stderr, "  Set the provider key in env or config; OpenRouter uses OPENROUTER_API_KEY.")
		return nil
	}

	model := narrateModelFlag
	if model == "" {
		model = cfg.NarrateModelForProvider(provider)
	}
	tick := narrateTickFlag
	if tick <= 0 {
		tick = cfg.NarrateTickInterval()
	}
	quiet := narrateQuietFlag
	if quiet <= 0 {
		quiet = cfg.NarrateQuietWindow()
	}
	if quiet > tick {
		quiet = tick
	}
	slackMin := narrateSlackMin
	if slackMin <= 0 {
		slackMin = cfg.NarrateSlackMinInterval()
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
		Provider:        provider,
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
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go func() {
		<-ctx.Done()
		select {
		case <-shutdownDone:
			return
		case <-time.After(2 * time.Second):
			os.Exit(130)
		}
	}()

	r := &narrateRunner{
		client:           client,
		session:          sess,
		database:         database,
		cfg:              cfg,
		opts:             narrate.SnapshotOptions{Project: narrateProjectFlag},
		budgetCents:      cfg.AutoRunRateSoftTargetCentsPerHour(),
		width:            resolveTerminalWidth(),
		apLabel:          fmt.Sprintf("narrate/%d", os.Getpid()),
		syncEnabled:      !narrateNoSyncFlag,
		apEnabled:        !narrateNoAutopilot,
		slackEnabled:     cfg.NarrateSlackEnabled() || narrateSlackFlag,
		debug:            narrateDebugFlag,
		slackMinInterval: slackMin,
		slackPost:        slack.Post,
		now:              time.Now,
	}

	// Prime with the actual current snapshot and print it before the first
	// sync/autopilot pass, so startup status appears immediately even when
	// cloud sync is slow.
	initial, err := narrate.BuildSnapshot(database, r.opts)
	if err != nil {
		return err
	}
	sess.SetPrev(initial)
	if err := r.emitStartupOverview(os.Stdout, initial); err != nil {
		return err
	}
	r.driveSyncAndAutopilot(ctx)
	lastTick := time.Now()
	if narrateOnceFlag {
		return nil
	}

	changeSource := openDBChangeSource("narrate")
	if changeSource != nil {
		defer changeSource.Close()
	}
	for {
		wait := time.Until(lastTick.Add(tick))
		if wait <= 0 {
			wait = tick
		}
		changed, done := waitForDBChangeOrTimeout(ctx, changeSource, wait, "narrate")
		if done {
			fmt.Fprintln(os.Stderr, "weft narrate: stopping")
			return nil
		}
		if changed && waitForNarrateQuietWindow(ctx, changeSource, quiet, lastTick.Add(tick)) {
			fmt.Fprintln(os.Stderr, "weft narrate: stopping")
			return nil
		}
		if err := r.tick(ctx); err != nil {
			r.debugf("weft narrate: tick error: %v\n", err)
		}
		lastTick = time.Now()
	}
}

func waitForNarrateQuietWindow(ctx context.Context, changeSource *dbwatch.Source, quiet time.Duration, deadline time.Time) bool {
	if quiet <= 0 || changeSource == nil {
		return false
	}
	for {
		wait := quiet
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false
			}
			if wait > remaining {
				wait = remaining
			}
		}
		if wait <= 0 {
			return false
		}
		changed, done := waitForDBChangeOrTimeout(ctx, changeSource, wait, "narrate")
		if done {
			return true
		}
		if !changed {
			return false
		}
	}
}

// driveSyncAndAutopilot runs one cloud sync followed by one gated
// autopilot pass, mirroring `weft autopilot run`. Errors are logged to
// stderr but never fatal — narrate's primary job is to observe, not to
// drive.
func (r *narrateRunner) driveSyncAndAutopilot(ctx context.Context) {
	restoreLogs := logging.Suppress()
	defer restoreLogs()

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
		r.debugf("weft narrate: autopilot pass: %v\n", runErr)
	}
}

func (r *narrateRunner) debugf(format string, args ...any) {
	if r != nil && r.debug {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

func (r *narrateRunner) emitStartupOverview(out io.Writer, snap *narrate.Snapshot) error {
	if snap == nil {
		return nil
	}
	unprocessed, err := loadUnprocessedCounts(r.database, r.opts.Project)
	if err != nil {
		return fmt.Errorf("count unprocessed: %w", err)
	}
	statusLine := narrate.BuildStatusLine(snap, r.budgetCents, unprocessed)
	r.width = resolveTerminalWidth()
	statusChanged := r.session.UpdateStatus(statusLine)
	r.session.EmitEntry(out, statusLine, "", statusChanged, r.width)
	return nil
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
		r.maybePostSlack(statusLine, "", statusChanged)
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
	r.maybePostSlack(statusLine, report.Narration, statusChanged)
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

func (r *narrateRunner) maybePostSlack(statusLine narrate.StatusLine, narration string, statusChanged bool) {
	if !r.slackEnabled {
		return
	}
	narration = strings.TrimSpace(narration)
	if !statusChanged && narration == "" {
		return
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if r.slackMinInterval <= 0 {
		r.slackMinInterval = 5 * time.Minute
	}
	if !r.lastSlackPost.IsZero() && now.Sub(r.lastSlackPost) < r.slackMinInterval {
		return
	}
	post := r.slackPost
	if post == nil {
		post = slack.Post
	}
	message := formatNarrateSlackMessage(statusLine, narration, r.width)
	r.lastSlackPost = now
	if err := post(message); err != nil {
		fmt.Fprintf(os.Stderr, "weft narrate: slack post failed: %v\n", err)
	}
}

func formatNarrateSlackMessage(statusLine narrate.StatusLine, narration string, width int) string {
	lines := statusLine.HeaderLines(width)
	narration = strings.TrimSpace(narration)
	if narration != "" {
		lines = append(lines, narration)
	}
	return strings.Join(lines, "\n")
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
		if isFailedJob(j) {
			counts.Failed++
			if p := strings.TrimSpace(j.Project); p != "" {
				failedProjects[p] = struct{}{}
			}
			continue
		}
		if j.EffectiveStatus() == status.Completed {
			counts.Completed++
			if p := strings.TrimSpace(j.Project); p != "" {
				completedProjects[p] = struct{}{}
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
