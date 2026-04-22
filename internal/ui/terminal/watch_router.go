package terminal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	flashmsg "github.com/osteele/weft/internal/app/flash"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
)

// switchToLaunchMsg is emitted by watchModel when the user presses 'l'.
type switchToLaunchMsg struct{}

// switchToSystemWatchMsg is emitted by listTUIModel when the user presses 'i'.
type switchToSystemWatchMsg struct{}

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
}

func newWatchRouterModel(database *sql.DB, cfg *config.Config, flash string, autoMode bool) watchRouterModel {
	watch := newSystemWatchModel(database, cfg, flash)
	watch.autoMode = autoMode
	return watchRouterModel{
		active:    watch,
		database:  database,
		config:    cfg,
		homeMode:  watchModeSystem,
		autoMode:  autoMode,
		listArgs:  nil,
		listTitle: "Jobs",
		listSync:  true,
	}
}

func newInstanceWatchRouterModel(database *sql.DB, cfg *config.Config, mode watchMode, instanceIDs []int64, r2Client *r2.Client, autoMode bool, projectFilter string) watchRouterModel {
	watch := newWatchModelWithMode(mode, database, instanceIDs, r2Client, cfg)
	watch.projectFilter = projectFilter
	watch.unplacedJobs = filterInstanceModeUnplacedJobs(watch.unplacedJobs, projectFilter)
	watch.autoMode = autoMode
	return watchRouterModel{
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
}

func newProjectWatchRouterModel(database *sql.DB, cfg *config.Config, recentWindow time.Duration, syncEnabled bool, projectFilter string, autoMode bool) watchRouterModel {
	watch := newProjectWatchModel(database, cfg, recentWindow, syncEnabled, projectFilter)
	watch.autoMode = autoMode
	return watchRouterModel{
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
}

func newListWatchRouterModel(database *sql.DB, cfg *config.Config, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool) watchRouterModel {
	list := newListTUIModel(database, args, jobs, title, syncEnabled, groupedByStatus, autoMode)
	return watchRouterModel{
		active:    list,
		database:  database,
		config:    cfg,
		homeMode:  watchModeSystem,
		autoMode:  autoMode,
		listArgs:  append([]string(nil), args...),
		listTitle: title,
		listSync:  syncEnabled,
	}
}

func (m watchRouterModel) Init() tea.Cmd {
	return m.active.Init()
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
		return m.switchTo(m.buildHomeWatch(""))

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

func (m watchRouterModel) View() string {
	return m.active.View()
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
		groups := campaign.PrepareGroups(jobs, database, "", r2Client)

		opts := campaign.LaunchOpts{Strategy: bidding.StrategyCheap, GPUWarmup: cfg.Campaign.GPUWarmup}
		if gracePeriod := cfg.DefaultGracePeriod(); gracePeriod != "0" {
			if d, err := time.ParseDuration(gracePeriod); err == nil {
				opts.GracePeriodSeconds = int(d.Seconds())
			}
		}

		clients, providerErr := buildCloudClients(cfg)
		predCfg := buildPredictorConfig(cfg)
		model := newLaunchModel(database, clients, providerErr, cfg, groups, opts, &predCfg, "", m.projectFilter, true, true, true)
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
