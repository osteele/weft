package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	flashmsg "github.com/osteele/weft/internal/app/flash"
	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/banner/monitor"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/vastai"
)

// switchToLaunchMsg is emitted by watchModel when the user presses 'l'.
type switchToLaunchMsg struct{}

// switchToSystemWatchMsg is emitted by listTUIModel when the user presses 'i'.
type switchToSystemWatchMsg struct{}

type systemWatchReadyMsg struct {
	model watchModel
}

// switchToListMsg is emitted by watchModel when the user presses 'J' or 'U'.
type switchToListMsg struct {
	groupedByStatus bool
}

// switchToWatchMsg is emitted by launchModel when it completes back to watch.
type switchToWatchMsg struct {
	flash string
}

// switchToAttemptsMsg is emitted by a job-listing TUI when the user presses 'a'
// on a selected job row. The router pushes the attempts drill-down screen and
// remembers the caller so it can be restored on switchBackFromAttemptsMsg.
type switchToAttemptsMsg struct {
	jobID int64
}

// switchBackFromAttemptsMsg is emitted by attemptsListModel when the user
// presses esc/q to leave the drill-down.
type switchBackFromAttemptsMsg struct{}

// schemaRelaunchRequestedMsg is dispatched by the schema-drift monitor's
// RelaunchSignal channel into the bubbletea program. The router responds
// by setting pendingExec and returning tea.Quit; the runner then invokes
// pendingExec after p.Run() returns, by which point bubbletea has restored
// the terminal.
type schemaRelaunchRequestedMsg struct{}

// processStartTime is captured at package init so the schema-drift monitor
// can compare it to the on-disk binary's mtime. A real Now() at the time
// each TUI starts would be slightly more accurate but functionally
// equivalent for the "is the binary newer than this process?" question.
var processStartTime = time.Now()

// launchPlanReadyMsg carries the prepared launch TUI model (or error).
type launchPlanReadyMsg struct {
	model *launchModel
	flash string
	err   error
}

// watchRouterModel is a composite model that delegates to whichever inner
// model (watchModel or launchModel) is currently active. A single
// tea.Program with tea.WithAltScreen() stays running throughout the session,
// eliminating the visual flash from alt-screen exit/re-enter on transitions.
type watchRouterModel struct {
	active        tea.Model
	database      *sql.DB
	config        *config.Config
	windowSize    tea.WindowSizeMsg
	homeMode      watchMode // which mode to return to after launch
	projectFilter string
	projectRecent time.Duration // recent window for project mode
	projectSync   bool          // preserve project watch sync mode across launch round-trips
	instanceIDs   []int64       // instance IDs for instance-based modes
	r2Client      *r2.Client    // R2 client for instance-based modes
	autoMode      bool          // initial auto-pilot state for new watch models
	listArgs      []string      // list query args (nil = default recent jobs)
	listTitle     string        // list title
	listSync      bool          // list sync mode
	attemptsPrev  tea.Model     // caller to restore when leaving attempts drill-down

	// Banner subsystem: surfaces schema drift, autopilot pause, and cloud
	// health across whichever child model is active. Set up in start() and
	// torn down in Stop(); nil-safe before start().
	bannerBus      *banner.Bus
	bannerView     *banner.View
	bannerSub      <-chan banner.Event
	bannerUnsub    func()
	monitorCancel  context.CancelFunc
	relaunchSignal chan struct{}
	// pendingExec is set when the router has decided that bubbletea should
	// quit and the binary should re-exec itself. The outer runner reads it
	// after p.Run() returns and performs the exec there, so bubbletea has
	// fully restored the terminal first.
	pendingExec func() error
}

type routerLoadingModel struct {
	message string
	width   int
}

func (m routerLoadingModel) Init() tea.Cmd { return nil }

func (m routerLoadingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
	}
	return m, nil
}

func (m routerLoadingModel) View() string {
	message := strings.TrimSpace(m.message)
	if message == "" {
		message = "Loading..."
	}
	width := m.width
	if width <= 0 {
		width = 120
	}
	return watchDimStyle.Render(truncateDisplayWidth(message, width)) + "\n"
}

func newWatchRouterModel(database *sql.DB, cfg *config.Config, flash string, autoMode bool) watchRouterModel {
	watch := newSystemWatchModel(database, cfg, flash)
	watch.autoMode = autoMode
	r := watchRouterModel{
		active:    watch,
		database:  database,
		config:    cfg,
		homeMode:  watchModeSystem,
		autoMode:  autoMode,
		listArgs:  nil,
		listTitle: "Jobs",
		listSync:  true,
	}
	return r.startBanners()
}

// startBanners initializes the banner Bus, View, monitor goroutines, and
// subscription channel. Idempotent: returns the model unchanged if banners
// were already started (e.g. for cheap re-construction in tests).
func (m watchRouterModel) startBanners() watchRouterModel {
	if m.bannerBus != nil {
		return m
	}
	m.bannerBus = banner.NewBus()
	m.bannerView = banner.NewView(nil)
	m.relaunchSignal = make(chan struct{}, 1)
	sub, unsub := m.bannerBus.Subscribe()
	m.bannerSub = sub
	m.bannerUnsub = unsub

	monCtx, cancel := context.WithCancel(context.Background())
	m.monitorCancel = cancel

	go monitor.WatchSchemaDrift(monCtx, monitor.SchemaDriftConfig{
		Database:       m.database,
		Bus:            m.bannerBus,
		ProcessStart:   processStartTime,
		RelaunchSignal: m.relaunchSignal,
	})
	go monitor.WatchAutopilotPaused(monCtx, monitor.AutopilotPausedConfig{
		Database: m.database,
		Bus:      m.bannerBus,
	})

	// Cloud-health monitor: only start probes for providers that are
	// configured. R2 needs bucket+credentials; Vast.ai needs the CLI
	// installed. Probing without configuration would just generate noise.
	cloudCfg := monitor.CloudHealthConfig{Bus: m.bannerBus}
	if cfg := m.config; cfg != nil && cfg.Vastai.R2.Bucket != "" {
		if r2Client, err := buildR2Client(cfg); err == nil && r2Client != nil {
			cloudCfg.R2 = r2Client
		}
	}
	cloudCfg.Vastai = vastai.NewClient()
	go monitor.WatchCloudHealth(monCtx, cloudCfg)

	return m
}

// stopBanners stops the monitor goroutines and unsubscribes from the bus.
// Safe to call multiple times.
func (m *watchRouterModel) stopBanners() {
	if m.monitorCancel != nil {
		m.monitorCancel()
		m.monitorCancel = nil
	}
	if m.bannerUnsub != nil {
		m.bannerUnsub()
		m.bannerUnsub = nil
	}
}

func newInstanceWatchRouterModel(database *sql.DB, cfg *config.Config, mode watchMode, instanceIDs []int64, r2Client *r2.Client, autoMode bool, projectFilter string) watchRouterModel {
	watch := newWatchModelWithMode(mode, database, instanceIDs, r2Client, cfg)
	watch.projectFilter = projectFilter
	watch.unplacedJobs = filterInstanceModeUnplacedJobs(watch.unplacedJobs, projectFilter)
	watch.autoMode = autoMode
	r := watchRouterModel{
		active:        watch,
		database:      database,
		config:        cfg,
		homeMode:      mode,
		instanceIDs:   instanceIDs,
		r2Client:      r2Client,
		projectFilter: projectFilter,
		autoMode:      autoMode,
		listArgs:      nil,
		listTitle:     "Jobs",
		listSync:      true,
	}
	return r.startBanners()
}

func newProjectWatchRouterModel(database *sql.DB, cfg *config.Config, recentWindow time.Duration, syncEnabled bool, projectFilter string, autoMode bool) watchRouterModel {
	watch := newProjectWatchModel(database, cfg, recentWindow, syncEnabled, projectFilter)
	watch.autoMode = autoMode
	r := watchRouterModel{
		active:        watch,
		database:      database,
		config:        cfg,
		homeMode:      watchModeProject,
		projectFilter: projectFilter,
		projectRecent: recentWindow,
		projectSync:   syncEnabled,
		autoMode:      autoMode,
		listArgs:      nil,
		listTitle:     "Jobs",
		listSync:      true,
	}
	return r.startBanners()
}

func newListWatchRouterModel(database *sql.DB, cfg *config.Config, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool, projectFilter string) watchRouterModel {
	list := newListTUIModel(database, args, jobs, title, syncEnabled, groupedByStatus, autoMode, projectFilter)
	r := watchRouterModel{
		active:        list,
		database:      database,
		config:        cfg,
		homeMode:      watchModeSystem,
		projectFilter: projectFilter,
		autoMode:      autoMode,
		listArgs:      append([]string(nil), args...),
		listTitle:     title,
		listSync:      syncEnabled,
	}
	return r.startBanners()
}

func (m watchRouterModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.active.Init()}
	if m.bannerSub != nil {
		cmds = append(cmds, banner.SubscribeNext(m.bannerSub))
	}
	if m.relaunchSignal != nil {
		cmds = append(cmds, waitForSchemaRelaunch(m.relaunchSignal))
	}
	return tea.Batch(cmds...)
}

// waitForSchemaRelaunch returns a tea.Cmd that blocks until the
// schema-drift monitor signals a relaunch is warranted. Bubbletea runs
// Cmds on background goroutines, so this doesn't stall the UI loop.
func waitForSchemaRelaunch(signal <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		_, ok := <-signal
		if !ok {
			return nil
		}
		return schemaRelaunchRequestedMsg{}
	}
}

func (m *watchRouterModel) cleanupActive() {
	switch active := m.active.(type) {
	case watchModel:
		if active.dbWatcher != nil {
			_ = active.dbWatcher.Close()
		}
		active.cancel()
		if active.syncWorker != nil {
			active.syncWorker.Stop()
		}
	case listTUIModel:
		active.shutdown()
	}
}

// switchTo replaces the active model with a new one, returning the Init cmd
// and a deferred WindowSizeMsg so the new model gets the current terminal size.
func (m *watchRouterModel) switchTo(model tea.Model) (watchRouterModel, tea.Cmd) {
	m.active = model
	cmds := []tea.Cmd{model.Init()}
	if m.windowSize.Width > 0 {
		ws := m.windowSize
		cmds = append(cmds, func() tea.Msg { return ws })
	}
	return *m, tea.Batch(cmds...)
}

func (m watchRouterModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case banner.TeaMsg:
		if m.bannerView != nil {
			m.bannerView.Apply(banner.Event(msg))
		}
		// Re-arm the subscription so the next event lands too. Without
		// this the TUI would only receive the first banner event.
		if m.bannerSub != nil {
			return m, banner.SubscribeNext(m.bannerSub)
		}
		return m, nil

	case schemaRelaunchRequestedMsg:
		// The monitor decided the on-disk binary is newer; quit
		// bubbletea so the terminal is restored, then exec.
		m.pendingExec = monitor.ExecSelf
		return m, tea.Quit

	case tea.WindowSizeMsg:
		m.windowSize = msg
		updated, cmd := m.active.Update(msg)
		m.active = updated
		return m, cmd

	case switchToLaunchMsg:
		// Clean up watch model and sync auto-pilot state
		if w, ok := m.active.(watchModel); ok {
			m.autoMode = w.autoMode
			if m.homeMode.isInstanceBased() {
				m.instanceIDs = append([]int64(nil), w.instanceIDs...)
			}
		}
		m.cleanupActive()
		return m, m.prepareLaunch()

	case switchToSystemWatchMsg:
		if l, ok := m.active.(listTUIModel); ok {
			m.autoMode = l.autoMode && l.groupedByStatus
		}
		m.homeMode = watchModeSystem
		m.cleanupActive()
		m.active = routerLoadingModel{message: "Opening instances watch..."}
		cmds := []tea.Cmd{m.prepareSystemWatch()}
		if m.windowSize.Width > 0 {
			ws := m.windowSize
			cmds = append(cmds, func() tea.Msg { return ws })
		}
		return m, tea.Batch(cmds...)

	case systemWatchReadyMsg:
		return m.switchTo(msg.model)

	case switchToListMsg:
		if w, ok := m.active.(watchModel); ok {
			m.autoMode = w.autoMode
		}
		m.cleanupActive()
		list := m.buildJobsList(msg.groupedByStatus)
		return m.switchTo(list)

	case launchPlanReadyMsg:
		if msg.err != nil {
			return m.switchTo(m.buildHomeWatch(fmt.Sprintf("Launch error: %v", msg.err)))
		}
		if msg.model == nil {
			return m.switchTo(m.buildHomeWatch(msg.flash))
		}
		return m.switchTo(*msg.model)

	case switchToWatchMsg:
		return m.switchTo(m.buildHomeWatch(msg.flash))

	case switchToAttemptsMsg:
		if m.attemptsPrev != nil {
			// Already inside the drill-down; ignore nested pushes so the
			// back-stack doesn't get clobbered.
			return m, nil
		}
		m.attemptsPrev = m.active
		attempts := newAttemptsListModel(m.database, msg.jobID)
		m.active = attempts
		cmds := []tea.Cmd{attempts.Init()}
		if m.windowSize.Width > 0 {
			ws := m.windowSize
			cmds = append(cmds, func() tea.Msg { return ws })
		}
		return m, tea.Batch(cmds...)

	case switchBackFromAttemptsMsg:
		if m.attemptsPrev == nil {
			return m, nil
		}
		prev := m.attemptsPrev
		m.attemptsPrev = nil
		m.active = prev
		if m.windowSize.Width > 0 {
			ws := m.windowSize
			return m, func() tea.Msg { return ws }
		}
		return m, nil
	}

	// Delegate all other messages to the active model
	updated, cmd := m.active.Update(msg)
	m.active = updated
	return m, cmd
}

func (m watchRouterModel) prepareSystemWatch() tea.Cmd {
	database := m.database
	cfg := m.config
	autoMode := m.autoMode
	return func() tea.Msg {
		w := newSystemWatchModel(database, cfg, "")
		w.autoMode = autoMode
		return systemWatchReadyMsg{model: w}
	}
}

func (m watchRouterModel) View() string {
	body := m.active.View()
	if m.bannerView == nil || !m.bannerView.HasAny() {
		return body
	}
	return m.bannerView.Render() + "\n" + body
}

func (m watchRouterModel) buildJobsList(groupedByStatus bool) listTUIModel {
	return newListTUIModel(
		m.database,
		append([]string(nil), m.listArgs...),
		nil,
		m.listTitle,
		m.listSync,
		groupedByStatus,
		m.autoMode,
		m.projectFilter,
	)
}

// buildHomeWatch creates a watchModel for the router's home mode.
func (m watchRouterModel) buildHomeWatch(flash string) watchModel {
	switch {
	case m.homeMode.isInstanceBased():
		w := newWatchModelWithMode(m.homeMode, m.database, m.instanceIDs, m.r2Client, m.config)
		w.projectFilter = m.projectFilter
		w.unplacedJobs = filterInstanceModeUnplacedJobs(w.unplacedJobs, m.projectFilter)
		w.flash = flashmsg.State{Message: flash}
		w.autoMode = m.autoMode
		return w
	case m.homeMode == watchModeProject:
		w := newProjectWatchModel(m.database, m.config, m.projectRecent, m.projectSync, m.projectFilter)
		w.projectStatus = flash
		w.autoMode = m.autoMode
		return w
	default:
		w := newSystemWatchModel(m.database, m.config, flash)
		w.autoMode = m.autoMode
		return w
	}
}

// prepareLaunch runs the launch planner logic as a tea.Cmd, building the
// launchModel without blocking the UI.
func (m watchRouterModel) prepareLaunch() tea.Cmd {
	database := m.database
	cfg := m.config
	return func() tea.Msg {
		jobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return launchPlanReadyMsg{err: fmt.Errorf("list unplaced jobs: %w", err)}
		}
		jobs = filterRentalLaunchJobs(jobs)
		jobs = filterLaunchJobsForScope(jobs, m.projectFilter)
		if len(jobs) == 0 {
			msg := "No jobs need rental GPUs."
			if n, err := db.CountJobsWaitingOnInstances(database); err == nil && n > 0 {
				msg += fmt.Sprintf(" (%d job(s) waiting on instances still setting up)", n)
			}
			return launchPlanReadyMsg{flash: msg}
		}

		if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
			return launchPlanReadyMsg{err: fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")}
		}

		r2Client, err := buildR2Client(cfg)
		if err != nil {
			slog.Warn("failed to build R2 client for disk estimation", "error", err)
		}
		groups := campaign.PrepareGroupsWithConfig(jobs, database, cfg, "", r2Client)

		opts := campaign.LaunchOpts{Strategy: bidding.StrategyCheap, GPUWarmup: cfg.Campaign.GPUWarmup}
		if gracePeriod := cfg.DefaultGracePeriod(); gracePeriod != "0" {
			if d, err := time.ParseDuration(gracePeriod); err == nil {
				opts.GracePeriodSeconds = int(d.Seconds())
			}
		}

		clients, providerErr := buildCloudClients(cfg)
		predCfg := buildPredictorConfig(cfg)
		model := newLaunchModel(database, clients, providerErr, cfg, groups, opts, &predCfg, "", m.projectFilter, nil, true, true, true)
		return launchPlanReadyMsg{model: &model}
	}
}

// formatLaunchResultFlash builds a flash message from launch results.
func formatLaunchResultFlash(instanceIDs []int64, partialErrors []string) string {
	if len(instanceIDs) == 0 {
		return "Launch canceled."
	}
	instanceText := make([]string, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instanceText = append(instanceText, ids.FormatInstanceID(id))
	}
	message := "Launched instances: " + strings.Join(instanceText, ", ")
	if len(partialErrors) > 0 {
		message = fmt.Sprintf("%s (%d failed)", message, len(partialErrors))
	}
	return message
}
