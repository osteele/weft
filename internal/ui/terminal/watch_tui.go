package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/flash"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
)

// watchMode selects which watch variant is active.
type watchMode int

const (
	watchModeCampaign  watchMode = iota // fixed instance IDs, campaign header, retry
	watchModeInstances                  // same as campaign but instance-centric header
	watchModeSystem                     // discover from DB, on-prem + unplaced sections
	watchModeProject                    // discover from DB, grouped by project
)

func (m watchMode) isInstanceBased() bool {
	return m == watchModeCampaign || m == watchModeInstances
}

// initialInstanceInfo holds pre-fetched DB data for instances that haven't
// received a channel update yet, avoiding repeated queries in View().
type initialInstanceInfo struct {
	ci       *db.Launch
	jobs     []*db.Job
	outcomes map[int64]string
}

// watchModel is the unified TUI model for campaign, instance, and system watch.
type watchModel struct {
	mode      watchMode
	database  *sql.DB
	appConfig *config.Config
	r2Client  *r2.Client
	ctx       context.Context
	cancel    context.CancelFunc
	spinner   spinner.Model
	done      bool
	err       error

	// --- Instance tracking (both modes) ---
	instanceIDs    []int64
	updates        map[int64]campaign.InstanceUpdate
	channels       map[int64]<-chan campaign.InstanceUpdate
	clients        map[int64]cloud.Client
	cloudClients   []cloud.Client // for orphan sweep
	jobProgressHWM map[int64]int
	syncWorker     *hostsync.Worker
	// Tracks instances where the view is showing preserved job attachment data
	// because snapshot/transition data was inconsistent.
	preservedJobAttachment map[int64]bool

	// --- Instance-based mode fields ---
	campaignID          int64
	launchedAt          time.Time
	estimateSummaryLine string // pre-formatted estimate line from launch; empty if unavailable
	initInfo            map[int64]initialInstanceInfo
	reconciler          *campaign.Reconciler
	launchPending       bool

	// Retry state (instance-based modes)
	retrying             bool
	retryResult          string
	retryAttempt         int
	retryExtraAttempts   int
	partialErrors        []string
	partialErrorJobs     []*db.Job
	partialErrorsRetried bool

	// --- System-mode fields ---
	cloudInstances []*db.Launch
	onPremHosts    []onPremHostSummary
	refreshing     bool

	// --- DB watcher (project + instance modes) ---
	dbWatcher        *fsnotify.Watcher
	dbWatcherTargets map[string]struct{}
	debounceActive   bool

	// --- Project-mode fields ---
	projectGroups  []projectGroup
	projectFilter  string
	projectRecent  time.Duration
	projectLines   []string // cached render lines
	projectSyncing bool
	projectStatus  string
	projectOffset  int // top visible line (offset-based scroll)

	// --- Auto-pilot mode ---
	autoMode      bool // when true, auto-relaunch, auto-place, and auto-launch are active
	autoLaunching bool // true while an auto-launch is in progress
	autoPlacing   bool // true while an auto-place is in progress

	// --- Move picker overlay ---
	movePicker movePickerModel

	// --- Shared display state ---
	unplacedJobs []*db.Job
	flash        flash.State
	cursor       int // selectable row index; -1 when no selectable rows
	width        int
	height       int
	scrollOff    int // lines scrolled up from bottom (0 = pinned to bottom)

	// Replacement chain cache (both modes, recomputed when instanceIDs change)
	cachedHiddenIDs         map[int64]bool
	cachedReplacementChains map[int64][]*db.Launch
}

// Aliases for shared TUI styles used throughout the watch TUI.
var (
	watchTitleStyle     = tuiTitleStyle
	watchStatusStyle    = tuiAccentStyle
	watchRunningStyle   = tuiRunningStyle
	watchCompletedStyle = tuiDimStyle
	watchFailedStyle    = tuiFailedStyle
	watchDimStyle       = tuiDimStyle
)

var watchSelectedRowStyle = tuiSelectedRowStyle

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

func newInstanceWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) watchModel {
	return newWatchModelWithMode(watchModeInstances, database, instanceIDs, r2Client, cfg)
}

func newCampaignWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) watchModel {
	return newWatchModelWithMode(watchModeCampaign, database, instanceIDs, r2Client, cfg)
}

func newWatchModelWithMode(mode watchMode, database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())

	clients := make(map[int64]cloud.Client)
	for _, id := range instanceIDs {
		clients[id] = clientForInstance(database, id)
	}

	initInfo := make(map[int64]initialInstanceInfo)
	for _, id := range instanceIDs {
		ci, _ := db.GetLaunch(database, id)
		jobs, _ := db.GetLaunchJobsIncludingAttempts(database, id)
		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, id)
		initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
	}

	campaignID, launchedAt := campaignInfoFromInstances(database, instanceIDs)

	var sw *hostsync.Worker
	if cfg != nil {
		sw = hostsync.New(database, nil, nil, cfg)
		sw.Start()
		go func() { <-ctx.Done(); sw.Stop() }()
	}

	allCloudClients, _ := buildCloudClients(cfg)
	if sw != nil && len(allCloudClients) > 0 {
		sw.SetCloudClients(allCloudClients)
	}

	unplaced, _ := db.ListUnplacedJobs(database)
	onPremJobs, _ := db.ListActiveOnPremJobs(database)
	queueblock.Apply(onPremJobs, queueblock.Fetch(onPremJobs, 5*time.Second))

	m := watchModel{
		mode:                   mode,
		database:               database,
		appConfig:              cfg,
		r2Client:               r2Client,
		ctx:                    ctx,
		cancel:                 cancel,
		spinner:                s,
		instanceIDs:            instanceIDs,
		updates:                make(map[int64]campaign.InstanceUpdate),
		channels:               make(map[int64]<-chan campaign.InstanceUpdate),
		clients:                clients,
		cloudClients:           allCloudClients,
		jobProgressHWM:         make(map[int64]int),
		syncWorker:             sw,
		preservedJobAttachment: map[int64]bool{},
		campaignID:             campaignID,
		launchedAt:             launchedAt,
		initInfo:               initInfo,
		reconciler:             campaign.NewReconciler(),
		onPremHosts:            groupOnPremHosts(onPremJobs),
		unplacedJobs:           unplaced,
	}
	m.rebuildReplacementCache()
	return m
}

func newSystemWatchModel(database *sql.DB, cfg *config.Config, flashMessage string) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())
	r2Client, _ := buildR2Client(cfg)

	sw := hostsync.New(database, nil, r2Client, cfg)
	sw.Start()

	allCloudClients, _ := buildCloudClients(cfg)

	model := watchModel{
		mode:                   watchModeSystem,
		database:               database,
		appConfig:              cfg,
		r2Client:               r2Client,
		ctx:                    ctx,
		cancel:                 cancel,
		spinner:                s,
		updates:                map[int64]campaign.InstanceUpdate{},
		channels:               map[int64]<-chan campaign.InstanceUpdate{},
		clients:                map[int64]cloud.Client{},
		cloudClients:           allCloudClients,
		jobProgressHWM:         map[int64]int{},
		syncWorker:             sw,
		preservedJobAttachment: map[int64]bool{},
		flash:                  flash.State{Message: flashMessage},
	}

	snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false, nil)
	if err != nil {
		model.err = err
		return model
	}
	model.cloudInstances = snapshot.Launches
	model.updates = snapshot.InstanceUpdates
	model.onPremHosts = snapshot.OnPremHosts
	model.unplacedJobs = snapshot.UnplacedJobs

	// Derive instanceIDs from discovered instances
	model.instanceIDs = make([]int64, len(snapshot.Launches))
	for i, ci := range snapshot.Launches {
		model.instanceIDs[i] = ci.ID
	}

	model.rebuildReplacementCache()

	// Request initial syncs for on-prem hosts
	for _, host := range model.onPremHosts {
		sw.Request(hostsync.Request{
			Host: host.Name,
			Rate: hostsync.GetHostSyncRate(host.Jobs),
		})
	}

	return model
}

func newProjectWatchModel(database *sql.DB, cfg *config.Config, recentWindow time.Duration, syncEnabled bool, projectFilter string) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())

	var sw *hostsync.Worker
	if syncEnabled && cfg != nil {
		sw = hostsync.New(database, nil, nil, cfg)
		sw.Start()
		go func() { <-ctx.Done(); sw.Stop() }()
	}

	return watchModel{
		mode:           watchModeProject,
		database:       database,
		appConfig:      cfg,
		ctx:            ctx,
		cancel:         cancel,
		spinner:        s,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
		syncWorker:     sw,
		projectFilter:  projectFilter,
		projectRecent:  recentWindow,
		projectSyncing: syncEnabled,
	}
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

func (m watchModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick}

	// Start watching all known instances
	for _, id := range m.instanceIDs {
		if cmd := m.startWatchingInstance(id); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	switch {
	case m.mode.isInstanceBased():
		cmds = append(cmds, m.startDBWatcher(), scheduleSyncTick(), scheduleCheckDone())
		if m.syncWorker != nil {
			m.requestOnPremSyncs()
			cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(hostsync.Result) tea.Msg {
				return watchInstanceSyncResultMsg{}
			}))
		}
	case m.mode == watchModeSystem:
		cmds = append(cmds,
			scheduleWatchAllTick(),
			scheduleCheckDone(),
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return watchSyncResultMsg{result: r}
			}),
		)
	case m.mode == watchModeProject:
		cmds = append(cmds, m.startDBWatcher(), m.reloadProjectGroups(), scheduleProjectSyncTick())
		if m.syncWorker != nil {
			m.requestProjectActiveSyncs()
			cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return watchProjectSyncResultMsg{result: r}
			}))
		} else if m.projectSyncing {
			cmds = append(cmds, m.runProjectBackgroundSync(false))
		}
	}

	// Schedule auto-pilot on startup if --auto was passed
	if m.autoMode && len(m.unplacedJobs) > 0 {
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return tea.Batch(cmds...)
}

// startWatchingInstance starts a WatchInstance channel if one isn't already running.
func (m *watchModel) startWatchingInstance(instanceID int64) tea.Cmd {
	if _, ok := m.channels[instanceID]; ok {
		return nil
	}
	client := clientForInstance(m.database, instanceID)
	ch := campaign.WatchInstance(m.ctx, client, m.database, instanceID, 2*time.Second, 10*time.Second, m.r2Client)
	m.clients[instanceID] = client
	m.channels[instanceID] = ch
	return waitForUpdate(instanceID, ch)
}

func (m *watchModel) addInstance(instanceID int64) tea.Cmd {
	for _, existing := range m.instanceIDs {
		if existing == instanceID {
			return nil
		}
	}
	m.instanceIDs = append(m.instanceIDs, instanceID)
	if m.initInfo == nil {
		m.initInfo = make(map[int64]initialInstanceInfo)
	}
	ci, _ := db.GetLaunch(m.database, instanceID)
	jobs, _ := db.GetLaunchJobsIncludingAttempts(m.database, instanceID)
	outcomes, _ := db.GetAttemptOutcomesByLaunch(m.database, instanceID)
	m.initInfo[instanceID] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
	if m.campaignID == 0 {
		if campaignID, launchedAt := campaignInfoFromInstances(m.database, []int64{instanceID}); campaignID > 0 {
			m.campaignID = campaignID
			if m.launchedAt.IsZero() {
				m.launchedAt = launchedAt
			}
		}
	}
	m.rebuildReplacementCache()
	return m.startWatchingInstance(instanceID)
}

// waitForUpdate reads the next value from an instance watch channel.
func waitForUpdate(instanceID int64, ch <-chan campaign.InstanceUpdate) tea.Cmd {
	return func() tea.Msg {
		update, ok := <-ch
		return watchUpdateMsg{instanceID: instanceID, update: update, closed: !ok}
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.mode == watchModeProject {
			m.projectLines = m.computeProjectLines()
			m.clampCursor()
			m.adjustProjectOffset()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.moveCursor(-3)
		case tea.MouseButtonWheelDown:
			m.moveCursor(3)
		}
		return m, nil

	case watchUpdateMsg:
		return m.handleWatchUpdate(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	// --- Shared ---
	case flash.ExpiredMsg:
		m.flash.HandleExpired()
		return m, nil

	case watchCheckDoneMsg:
		return m.handleCheckDone()

	case watchCheckDoneResultMsg:
		return m.handleCheckDoneResult(msg)

	case watchUnplaceDoneMsg:
		return m.handleUnplaceDone(msg)

	case watchSubmitDoneMsg:
		return m.handleSubmitDone(msg)

	case watchKillDoneMsg:
		return m.handleActionDone("Kill", msg.message, msg.err)

	case watchTerminateDoneMsg:
		return m.handleActionDone("Terminate", msg.message, msg.err)

	// --- Move picker messages ---
	case moveOptionsReadyMsg:
		return m.handleMoveOptionsReady(msg)

	case moveExecuteDoneMsg:
		return m.handleMoveExecuteDone(msg)

	// --- Auto-pilot messages ---
	case autoPlaceDoneMsg:
		return m.handleAutoPlaceDone(msg)

	case autoLaunchDoneMsg:
		return m.handleAutoLaunchDone(msg)

	// --- Campaign/instance-only messages ---
	case watchSyncTickMsg:
		if !m.mode.isInstanceBased() {
			return m, nil
		}
		return m.handleInstanceSyncTick()

	case watchSyncDoneMsg:
		if !m.mode.isInstanceBased() {
			return m, nil
		}
		return m.handleInstanceSyncDone()

	case watchJobsRefreshedMsg:
		return m.handleJobsRefreshed(msg)

	case watchInstanceSyncResultMsg:
		if !m.mode.isInstanceBased() || m.syncWorker == nil {
			return m, nil
		}
		return m, m.syncWorker.WaitForResult(m.ctx, func(hostsync.Result) tea.Msg {
			return watchInstanceSyncResultMsg{}
		})

	case retryResultMsg:
		return m.handleRetryResult(msg)

	case retryBackoffMsg:
		m.retrying = true
		m.retryResult = ""
		return m, m.retryFailedInstances(m.retryExtraAttempts)

	// --- System-only messages ---
	case watchAllTickMsg:
		if m.mode != watchModeSystem {
			return m, nil
		}
		return m.handleSystemTick()

	case watchAllRefreshedMsg:
		if m.mode != watchModeSystem {
			return m, nil
		}
		return m.handleSystemRefreshed(msg)

	case watchSyncResultMsg:
		if m.mode != watchModeSystem {
			return m, nil
		}
		return m, tea.Batch(
			refreshWatchOnPrem(m.database),
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return watchSyncResultMsg{result: r}
			}),
		)

	case watchOnPremRefreshedMsg:
		if msg.err != nil {
			if m.mode == watchModeSystem {
				m.err = msg.err
			}
			return m, nil
		}
		if m.mode == watchModeSystem || m.mode.isInstanceBased() {
			m.onPremHosts = msg.onPremHosts
		}
		if m.mode == watchModeSystem || m.mode.isInstanceBased() {
			m.unplacedJobs = msg.unplacedJobs
			m.clampCursor()
		}
		if m.autoMode && m.mode.isInstanceBased() {
			return m, m.runAutoPilot()
		}
		return m, nil

	// --- Project-mode messages ---
	case watchProjectLoadedMsg:
		if m.mode != watchModeProject {
			return m, nil
		}
		return m.handleProjectLoaded(msg)

	case watchProjectSyncFinishedMsg:
		if m.mode != watchModeProject {
			return m, nil
		}
		return m.handleProjectSyncFinished(msg)

	case watchProjectSyncResultMsg:
		if m.mode != watchModeProject || m.syncWorker == nil {
			return m, nil
		}
		return m.handleProjectSyncWorkerResult(msg)

	case watchDBWatcherReadyMsg:
		if m.mode == watchModeProject || m.mode.isInstanceBased() {
			return m.handleDBWatcherReady(msg)
		}
		return m, nil

	case watchDBWatchEventMsg:
		switch {
		case m.mode == watchModeProject:
			return m.handleDBWatchEvent(msg, watchProjectDBRefreshTriggeredMsg{})
		case m.mode.isInstanceBased():
			return m.handleDBWatchEvent(msg, watchInstanceDBRefreshTriggeredMsg{})
		}
		return m, nil

	case watchProjectDBRefreshTriggeredMsg:
		if m.mode != watchModeProject {
			return m, nil
		}
		m.debounceActive = false
		return m, m.reloadProjectGroups()

	case watchInstanceDBRefreshTriggeredMsg:
		if !m.mode.isInstanceBased() {
			return m, nil
		}
		m.debounceActive = false
		// Lightweight refresh: sync on-prem hosts and refresh unplaced jobs,
		// but do NOT call handleInstanceSyncTick() which schedules another
		// scheduleSyncTick(). Doing so from the DB watcher (which fires every
		// ~200ms) causes exponential timer accumulation and 300%+ CPU.
		// The full sweep (reconcile campaigns, orphan sweep) already runs on
		// independent schedules via SyncWorker (10s) and the periodic tick (15s).
		m.requestOnPremSyncs()
		return m, tea.Batch(
			refreshWatchUnplacedJobs(m.database),
			func() tea.Msg {
				if _, err := db.ResetJobsOnTerminalLaunches(m.database); err != nil {
					// log suppressed in TUI mode
				}
				return watchSyncDoneMsg{}
			},
		)

	case watchProjectSyncTickMsg:
		if m.mode != watchModeProject {
			return m, nil
		}
		return m.handleProjectSyncTick()
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// Entry points (called from campaign_report.go and watch.go)
// ---------------------------------------------------------------------------

// watchInstances runs the interactive TUI watch for one or more cloud instances.
// Returns the final list of instance IDs (which may include auto-relaunched instances).
func watchInstances(database *sql.DB, mode watchMode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary, autoMode bool) ([]int64, error) {
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	router := newInstanceWatchRouterModel(database, cfg, mode, instanceIDs, r2Client, autoMode)
	if estimateSummary != nil {
		if w, ok := router.active.(watchModel); ok {
			w.estimateSummaryLine = estimateSummary.FormatLine()
			router.active = w
		}
	}

	restore := logging.Suppress()
	defer restore()

	p := tea.NewProgram(router, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	if err != nil {
		return instanceIDs, err
	}

	// Extract the final watchModel from the router
	if r, ok := finalModel.(watchRouterModel); ok {
		if m, ok := r.active.(watchModel); ok {
			if m.done {
				fmt.Print(m.View())
			}
			if m.syncWorker != nil {
				m.syncWorker.Stop()
			}
			return m.instanceIDs, nil
		}
	}
	return instanceIDs, nil
}

func renderWatchExitSnapshot(model tea.Model) string {
	m, ok := model.(watchModel)
	if !ok || !m.done {
		return ""
	}
	return m.View()
}
