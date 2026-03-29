package cmd

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// switchToLaunchMsg is emitted by watchModel when the user presses 'l'.
type switchToLaunchMsg struct{}

// switchToWatchMsg is emitted by launchModel when it completes back to watch.
type switchToWatchMsg struct {
	flash string
}

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
	homeMode      watchMode     // which mode to return to after launch
	projectRecent time.Duration // recent window for project mode
	instanceIDs   []int64       // instance IDs for instance-based modes
	r2Client      *r2.Client    // R2 client for instance-based modes
	autoMode      bool          // initial auto-pilot state for new watch models
}

func newWatchRouterModel(database *sql.DB, cfg *config.Config, flash string, autoMode bool) watchRouterModel {
	watch := newSystemWatchModel(database, cfg, flash)
	watch.autoMode = autoMode
	return watchRouterModel{
		active:   watch,
		database: database,
		config:   cfg,
		homeMode: watchModeSystem,
		autoMode: autoMode,
	}
}

func newInstanceWatchRouterModel(database *sql.DB, cfg *config.Config, mode watchMode, instanceIDs []int64, r2Client *r2.Client, autoMode bool) watchRouterModel {
	watch := newWatchModelWithMode(mode, database, instanceIDs, r2Client, cfg)
	watch.autoMode = autoMode
	return watchRouterModel{
		active:      watch,
		database:    database,
		config:      cfg,
		homeMode:    mode,
		instanceIDs: instanceIDs,
		r2Client:    r2Client,
		autoMode:    autoMode,
	}
}

func newProjectWatchRouterModel(database *sql.DB, cfg *config.Config, recentWindow time.Duration, syncEnabled bool, autoMode bool) watchRouterModel {
	watch := newProjectWatchModel(database, cfg, recentWindow, syncEnabled)
	watch.autoMode = autoMode
	return watchRouterModel{
		active:        watch,
		database:      database,
		config:        cfg,
		homeMode:      watchModeProject,
		projectRecent: recentWindow,
		autoMode:      autoMode,
	}
}

func (m watchRouterModel) Init() tea.Cmd {
	return m.active.Init()
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
			w.cancel()
			if w.syncWorker != nil {
				w.syncWorker.Stop()
			}
		}
		return m, m.prepareLaunch()

	case launchPlanReadyMsg:
		if msg.err != nil {
			return m.switchTo(m.buildHomeWatch(fmt.Sprintf("Launch error: %v", msg.err)))
		}
		if msg.model == nil {
			return m.switchTo(m.buildHomeWatch(msg.flash))
		}
		return m.switchTo(*msg.model)

	case switchToWatchMsg:
		if w, ok := m.active.(watchModel); ok {
			m.autoMode = w.autoMode
		}
		return m.switchTo(m.buildHomeWatch(msg.flash))
	}

	// Delegate all other messages to the active model
	updated, cmd := m.active.Update(msg)
	m.active = updated
	return m, cmd
}

func (m watchRouterModel) View() string {
	return m.active.View()
}

// buildHomeWatch creates a watchModel for the router's home mode.
func (m watchRouterModel) buildHomeWatch(flash string) watchModel {
	switch {
	case m.homeMode.isInstanceBased():
		w := newWatchModelWithMode(m.homeMode, m.database, m.instanceIDs, m.r2Client, m.config)
		w.flash = tui.FlashState{Message: flash}
		w.autoMode = m.autoMode
		return w
	case m.homeMode == watchModeProject:
		w := newProjectWatchModel(m.database, m.config, m.projectRecent, true)
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
		if len(jobs) == 0 {
			return launchPlanReadyMsg{flash: "No jobs need rental GPUs."}
		}

		if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
			return launchPlanReadyMsg{err: fmt.Errorf("R2 not configured in ~/.config/weft/config.toml (vastai.r2)")}
		}

		groups := campaign.GroupByGPUSupremum(jobs)
		groups = campaign.SplitGroupsByImage(groups)
		r2Client, err := buildR2Client(cfg)
		if err != nil {
			slog.Warn("failed to build R2 client for disk estimation", "error", err)
		}
		for i := range groups {
			groups[i].DiskGB = campaign.EstimateGroupDisk(groups[i], database, r2Client)
		}

		opts := campaign.LaunchOpts{Strategy: bidding.StrategyCheap, GPUWarmup: cfg.Campaign.GPUWarmup}
		if gracePeriod := cfg.DefaultGracePeriod(); gracePeriod != "0" {
			if d, err := time.ParseDuration(gracePeriod); err == nil {
				opts.GracePeriodSeconds = int(d.Seconds())
			}
		}

		clients, providerErr := buildCloudClients(cfg)
		predCfg := buildPredictorConfig(cfg)
		model := newLaunchModel(database, clients, providerErr, cfg, groups, opts, &predCfg, "", true, true, true)
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
		instanceText = append(instanceText, fmt.Sprintf("%d", id))
	}
	message := "Launched instances: " + strings.Join(instanceText, ", ")
	if len(partialErrors) > 0 {
		message = fmt.Sprintf("%s (%d failed)", message, len(partialErrors))
	}
	return message
}
