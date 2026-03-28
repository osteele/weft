package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// watchMode selects which watch variant is active.
type watchMode int

const (
	watchModeCampaign  watchMode = iota // fixed instance IDs, campaign header, retry
	watchModeInstances                  // same as campaign but instance-centric header
	watchModeSystem                     // discover from DB, on-prem + unplaced sections
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
	syncWorker     *tui.SyncWorker

	// --- Instance-based mode fields ---
	campaignID          int64
	launchedAt          time.Time
	estimateSummaryLine string // pre-formatted estimate line from launch; empty if unavailable
	initInfo            map[int64]initialInstanceInfo
	reconciler          *campaign.Reconciler

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

	// --- Shared display state ---
	unplacedJobs []*db.Job
	flash        flashState
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

	var sw *tui.SyncWorker
	if cfg != nil {
		sw = tui.NewSyncWorker(database, nil, nil, cfg)
		sw.Start()
		go func() { <-ctx.Done(); sw.Stop() }()
	}

	allCloudClients, _ := buildCloudClients(cfg)

	m := watchModel{
		mode:           mode,
		database:       database,
		appConfig:      cfg,
		r2Client:       r2Client,
		ctx:            ctx,
		cancel:         cancel,
		spinner:        s,
		instanceIDs:    instanceIDs,
		updates:        make(map[int64]campaign.InstanceUpdate),
		channels:       make(map[int64]<-chan campaign.InstanceUpdate),
		clients:        clients,
		cloudClients:   allCloudClients,
		jobProgressHWM: make(map[int64]int),
		syncWorker:     sw,
		campaignID:     campaignID,
		launchedAt:     launchedAt,
		initInfo:       initInfo,
		reconciler:     campaign.NewReconciler(),
	}
	m.rebuildReplacementCache()
	return m
}

func newSystemWatchModel(database *sql.DB, cfg *config.Config, flashMessage string) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())
	r2Client, _ := buildR2Client(cfg)

	sw := tui.NewSyncWorker(database, nil, r2Client, cfg)
	sw.Start()

	allCloudClients, _ := buildCloudClients(cfg)

	model := watchModel{
		mode:           watchModeSystem,
		database:       database,
		appConfig:      cfg,
		r2Client:       r2Client,
		ctx:            ctx,
		cancel:         cancel,
		spinner:        s,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		cloudClients:   allCloudClients,
		jobProgressHWM: map[int64]int{},
		syncWorker:     sw,
		flash:          flashState{message: flashMessage},
	}

	snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
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
		sw.Request(tui.SyncRequest{
			Host: host.Name,
			Rate: tui.GetHostSyncRate(host.Jobs),
		})
	}

	return model
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
		cmds = append(cmds, scheduleSyncTick(), scheduleCheckDone())
		if m.syncWorker != nil {
			m.requestOnPremSyncs()
			cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
				return watchInstanceSyncResultMsg{}
			}))
		}
	case m.mode == watchModeSystem:
		cmds = append(cmds,
			scheduleWatchAllTick(),
			scheduleCheckDone(),
			m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
				return watchSyncResultMsg{result: r}
			}),
		)
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
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case watchUpdateMsg:
		return m.handleWatchUpdate(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	// --- Shared ---
	case flashExpiredMsg:
		m.flash.handleExpired()
		return m, nil

	case watchCheckDoneMsg:
		return m.handleCheckDone()

	case watchCheckDoneResultMsg:
		return m.handleCheckDoneResult(msg)

	case watchUnplaceDoneMsg:
		return m.handleUnplaceDone(msg)

	case watchSubmitDoneMsg:
		return m.handleSubmitDone(msg)

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
		return m, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
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
			m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
				return watchSyncResultMsg{result: r}
			}),
		)

	case watchOnPremRefreshedMsg:
		if m.mode != watchModeSystem {
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.onPremHosts = msg.onPremHosts
		m.unplacedJobs = msg.unplacedJobs
		m.clampCursor()
		return m, nil
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// Entry points (called from campaign_report.go and watch.go)
// ---------------------------------------------------------------------------

// watchInstances runs the interactive TUI watch for one or more cloud instances.
// Returns the final list of instance IDs (which may include auto-relaunched instances).
func watchInstances(database *sql.DB, mode watchMode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary) ([]int64, error) {
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	model := newWatchModelWithMode(mode, database, instanceIDs, r2Client, cfg)
	if estimateSummary != nil {
		model.estimateSummaryLine = estimateSummary.FormatLine()
	}

	restore := logging.Suppress()
	defer restore()

	p := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return instanceIDs, err
	}

	if finalView := renderWatchExitSnapshot(finalModel); finalView != "" {
		fmt.Print(finalView)
	}

	if m, ok := finalModel.(watchModel); ok {
		return m.instanceIDs, nil
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
