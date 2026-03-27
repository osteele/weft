package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// watchMode selects which watch variant is active.
type watchMode int

const (
	watchModeCampaign watchMode = iota // fixed instance IDs, campaign header, retry
	watchModeSystem                    // discover from DB, on-prem + unplaced sections
)

// initialInstanceInfo holds pre-fetched DB data for instances that haven't
// received a channel update yet, avoiding repeated queries in View().
type initialInstanceInfo struct {
	ci       *db.Launch
	jobs     []*db.Job
	outcomes map[int64]string
}

// watchModel is the unified TUI model for both campaign and system watch.
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
	jobProgressHWM map[int64]int
	syncWorker     *tui.SyncWorker

	// --- Campaign-mode fields ---
	campaignID int64
	launchedAt time.Time
	initInfo   map[int64]initialInstanceInfo
	reconciler *campaign.Reconciler

	// Retry state (campaign mode)
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
	flashMessage string
	cursor       int // selectable row index; -1 when no selectable rows
	width        int
	height       int
	scrollOff    int // lines scrolled up from bottom (0 = pinned to bottom)

	// Replacement chain cache (both modes, recomputed when instanceIDs change)
	cachedHiddenIDs         map[int64]bool
	cachedReplacementChains map[int64][]*db.Launch
}

// Styles for the watch TUI (allocated once, not per-render).
var (
	watchTitleStyle     = lipgloss.NewStyle().Bold(true)
	watchStatusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	watchRunningStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	watchCompletedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	watchFailedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	watchDimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

var watchSelectedRowStyle = lipgloss.NewStyle().Background(lipgloss.Color("240"))

// ---------------------------------------------------------------------------
// Message types
// ---------------------------------------------------------------------------

type watchUpdateMsg struct {
	instanceID int64
	update     campaign.InstanceUpdate
	closed     bool
}

// watchSyncTickMsg triggers periodic cloud job result syncing (campaign mode).
type watchSyncTickMsg struct{}

// watchSyncDoneMsg is sent after syncCloudJobResults completes (campaign mode).
type watchSyncDoneMsg struct{}

// watchJobsRefreshedMsg carries refreshed job lists from a background DB query.
type watchJobsRefreshedMsg struct {
	cloudInstances map[int64]*db.Launch
	jobs           map[int64][]*db.Job
	outcomes       map[int64]map[int64]string
	quitAfter      bool
}

// watchCheckDoneMsg triggers a periodic DB-based check for all-terminal state.
type watchCheckDoneMsg struct{}

// watchCheckDoneResultMsg carries the result of a background DB terminal check.
type watchCheckDoneResultMsg struct{ allTerminal bool }

// campaignWatchSyncResultMsg is sent when the background SyncWorker produces a result (campaign mode).
type campaignWatchSyncResultMsg struct{}

// retryResultMsg carries the result of retrying failed instances.
type retryResultMsg struct {
	instanceIDs []int64
	skipped     int
	err         error
}

// retryBackoffMsg triggers a delayed retry attempt after no offers were found.
type retryBackoffMsg struct{}

// retryBackoffDelays defines the delay before each retry attempt.
var retryBackoffDelays = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// System-mode messages
type watchAllTickMsg struct{}

type watchAllRefreshedMsg struct {
	snapshot watchSystemSnapshot
	err      error
}

type watchSyncResultMsg struct {
	result tui.SyncResult
}

type watchOnPremRefreshedMsg struct {
	onPremHosts  []onPremHostSummary
	unplacedJobs []*db.Job
	err          error
}

type watchUnplaceDoneMsg struct {
	job     *db.Job
	message string
	err     error
}

// ---------------------------------------------------------------------------
// Render row types (system mode)
// ---------------------------------------------------------------------------

type watchRenderRow struct {
	text string
}

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

func newCampaignWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) watchModel {
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

	m := watchModel{
		mode:           watchModeCampaign,
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
		jobProgressHWM: map[int64]int{},
		syncWorker:     sw,
		flashMessage:   flashMessage,
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

	switch m.mode {
	case watchModeCampaign:
		cmds = append(cmds, scheduleSyncTick(), scheduleCheckDone())
		if m.syncWorker != nil {
			m.requestOnPremSyncs()
			cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
				return campaignWatchSyncResultMsg{}
			}))
		}
	case watchModeSystem:
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
	case watchCheckDoneMsg:
		return m.handleCheckDone()

	case watchCheckDoneResultMsg:
		return m.handleCheckDoneResult(msg)

	case watchUnplaceDoneMsg:
		return m.handleUnplaceDone(msg)

	// --- Campaign-only messages ---
	case watchSyncTickMsg:
		if m.mode != watchModeCampaign {
			return m, nil
		}
		return m.handleCampaignSyncTick()

	case watchSyncDoneMsg:
		if m.mode != watchModeCampaign {
			return m, nil
		}
		return m.handleCampaignSyncDone()

	case watchJobsRefreshedMsg:
		return m.handleJobsRefreshed(msg)

	case campaignWatchSyncResultMsg:
		if m.mode != watchModeCampaign || m.syncWorker == nil {
			return m, nil
		}
		return m, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
			return campaignWatchSyncResultMsg{}
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
// Key handling
// ---------------------------------------------------------------------------

func (m watchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.cancel()
		return m, tea.Quit
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "home", "g":
		m.cursor = 0
		return m, nil
	case "end", "G":
		count := m.selectableRowCount()
		if count > 0 {
			m.cursor = count - 1
		}
		return m, nil
	case "r":
		if !m.retrying && m.hasRetryableFailures() {
			if m.database != nil {
				_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
					EventKind:  db.EventRetryManualTriggered,
					CampaignID: m.campaignID,
				})
			}
			m.retryAttempt = 0
			m.retrying = true
			m.retryResult = ""
			m.retryExtraAttempts = campaign.DefaultMaxCloudAttempts
			return m, m.retryFailedInstances(m.retryExtraAttempts)
		}
	case "u":
		job := m.selectedUnplacedJob()
		if job == nil {
			// Try on-prem job (system mode)
			if m.mode == watchModeSystem {
				job = m.selectedOnPremJob()
			}
		}
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			return m, nil
		}
		if job.HasInventoryHost() || job.Host == "" {
			return m, requestWatchJobUnplace(m.database, job.ID)
		}
	case "l":
		if m.mode == watchModeSystem {
			m.cancel()
			if m.syncWorker != nil {
				m.syncWorker.Stop()
			}
			return m, func() tea.Msg { return switchToLaunchMsg{} }
		}
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Update handlers: watch updates
// ---------------------------------------------------------------------------

func (m watchModel) handleWatchUpdate(msg watchUpdateMsg) (tea.Model, tea.Cmd) {
	if msg.closed {
		clearWatchJobProgressHWM(m.jobProgressHWM, m.updates[msg.instanceID])
		if m.mode == watchModeSystem {
			delete(m.channels, msg.instanceID)
			delete(m.clients, msg.instanceID)
		}
		return m, m.checkAllDone()
	}

	prev := m.updates[msg.instanceID]
	if msg.update.Launch != nil && campaign.IsInstanceTerminal(msg.update.Launch.Status) {
		msg.update.Jobs = preserveWatchCurrentJobs(msg.update.Launch.ID, prev.Jobs, msg.update.Jobs)
	}
	updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
	m.updates[msg.instanceID] = msg.update

	// Auto-relaunch on retryable infrastructure failure (campaign mode)
	ci := msg.update.Launch
	ch := m.channels[msg.instanceID]
	if ci != nil && db.IsRetryableTermination(ci) && !m.retrying && m.database != nil {
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:  db.EventRetryAutoTriggered,
			LaunchID:   ci.ID,
			CampaignID: m.campaignID,
			GPUSpec:    ci.GPUSpec,
			Detail:     ci.TerminationReason,
		})
		m.retrying = true
		m.retryResult = ""
		return m, tea.Batch(
			waitForUpdate(msg.instanceID, ch),
			m.retryFailedInstances(0),
		)
	}

	return m, waitForUpdate(msg.instanceID, ch)
}

// ---------------------------------------------------------------------------
// Update handlers: check-done
// ---------------------------------------------------------------------------

func (m watchModel) handleCheckDone() (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	return m, m.checkAllDone()
}

func (m watchModel) handleCheckDoneResult(msg watchCheckDoneResultMsg) (tea.Model, tea.Cmd) {
	if !msg.allTerminal {
		return m, scheduleCheckDone()
	}

	switch m.mode {
	case watchModeCampaign:
		if m.retrying {
			return m, scheduleCheckDone()
		}
		return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
	case watchModeSystem:
		// System mode: also check on-prem and unplaced
		if len(m.onPremHosts) > 0 || len(m.unplacedJobs) > 0 {
			return m, scheduleCheckDone()
		}
		// All empty — trigger final refresh and quit
		return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
	}
	return m, nil
}

func (m watchModel) checkAllDone() tea.Cmd {
	database := m.database
	instanceIDs := m.instanceIDs
	return func() tea.Msg {
		for _, id := range instanceIDs {
			ci, err := db.GetLaunch(database, id)
			if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
				return watchCheckDoneResultMsg{allTerminal: false}
			}
		}
		return watchCheckDoneResultMsg{allTerminal: true}
	}
}

// ---------------------------------------------------------------------------
// Update handlers: unplace
// ---------------------------------------------------------------------------

func (m watchModel) handleUnplaceDone(msg watchUnplaceDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.flashMessage = watchFailedStyle.Render(msg.err.Error())
		return m, nil
	}
	if m.mode == watchModeSystem {
		if msg.job != nil {
			m.removeOnPremJob(msg.job.ID)
			m.upsertUnplacedJob(msg.job)
			m.clampCursor()
		}
		m.flashMessage = msg.message
		m.refreshing = true
		return m, refreshWatchSystem(m.database, m.appConfig)
	}
	// Campaign mode: just refresh unplaced
	m.flashMessage = msg.message
	return m, nil
}

// ---------------------------------------------------------------------------
// Update handlers: campaign-specific
// ---------------------------------------------------------------------------

func (m watchModel) handleCampaignSyncTick() (tea.Model, tea.Cmd) {
	m.requestOnPremSyncs()
	return m, tea.Batch(
		func() tea.Msg {
			if _, err := db.ResetJobsOnTerminalLaunches(m.database); err != nil {
				// log suppressed in TUI mode
			}
			if _, err := campaign.ReconcileCampaigns(m.database); err != nil {
				// log suppressed in TUI mode
			}
			return watchSyncDoneMsg{}
		},
		scheduleSyncTick(),
	)
}

func (m watchModel) handleCampaignSyncDone() (tea.Model, tea.Cmd) {
	terminalIDs := make([]int64, 0)
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
			terminalIDs = append(terminalIDs, id)
		}
	}
	if len(terminalIDs) == 0 {
		return m, nil
	}
	return m, refreshWatchInstancesFromDB(m.database, terminalIDs, false)
}

func (m watchModel) handleJobsRefreshed(msg watchJobsRefreshedMsg) (tea.Model, tea.Cmd) {
	for id, ci := range msg.cloudInstances {
		u := m.updates[id]
		u.Launch = ci
		m.updates[id] = u
	}
	for id, jobs := range msg.jobs {
		u := m.updates[id]
		if u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
			u.Jobs = preserveWatchCurrentJobs(u.Launch.ID, u.Jobs, jobs)
		} else {
			u.Jobs = jobs
		}
		if outcomes, ok := msg.outcomes[id]; ok {
			u.JobAttemptOutcomes = outcomes
		}
		m.updates[id] = u
	}
	if msg.quitAfter {
		m.done = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return tea.QuitMsg{}
		})
	}
	return m, nil
}

func (m watchModel) handleRetryResult(msg retryResultMsg) (tea.Model, tea.Cmd) {
	m.retrying = false
	if msg.err != nil {
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:  db.EventRetryError,
			CampaignID: m.campaignID,
			ErrorText:  msg.err.Error(),
		})
		m.retryResult = fmt.Sprintf("Retry failed: %v", msg.err)
		return m, m.checkAllDone()
	}
	if len(msg.instanceIDs) == 0 {
		if msg.skipped > 0 {
			_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
				EventKind:  db.EventRetryMaxAttempts,
				CampaignID: m.campaignID,
				JobCount:   msg.skipped,
			})
			m.retryExtraAttempts = 0
			m.retryResult = fmt.Sprintf("Retry: %d job(s) exceeded max cloud attempts, giving up", msg.skipped)
			return m, m.checkAllDone()
		}
		if m.retryAttempt < len(retryBackoffDelays) {
			delay := retryBackoffDelays[m.retryAttempt]
			m.retryAttempt++
			_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
				EventKind:     db.EventRetryNoOffers,
				CampaignID:    m.campaignID,
				AttemptNumber: m.retryAttempt,
				MaxAttempts:   len(retryBackoffDelays) + 1,
				Detail:        fmt.Sprintf("backoff %s", delay),
			})
			m.retryResult = fmt.Sprintf("Retry: no offers available, retrying in %s (attempt %d/%d)",
				delay, m.retryAttempt+1, len(retryBackoffDelays)+1)
			return m, tea.Tick(delay, func(time.Time) tea.Msg { return retryBackoffMsg{} })
		}
		_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
			EventKind:     db.EventRetryExhausted,
			CampaignID:    m.campaignID,
			AttemptNumber: len(retryBackoffDelays) + 1,
		})
		m.retryExtraAttempts = 0
		m.retryResult = fmt.Sprintf("Retry: no instances launched after %d attempts (no offers available)", len(retryBackoffDelays)+1)
		return m, m.checkAllDone()
	}

	// Success
	_ = db.InsertLifecycleEvent(m.database, &db.LifecycleEvent{
		EventKind:  db.EventRetrySuccess,
		CampaignID: m.campaignID,
		JobCount:   len(msg.instanceIDs),
		Detail:     fmt.Sprintf("launched instances %v", msg.instanceIDs),
	})
	m.retryAttempt = 0
	m.retryExtraAttempts = 0

	var cmds []tea.Cmd
	for _, id := range msg.instanceIDs {
		m.instanceIDs = append(m.instanceIDs, id)
		// Pre-fetch initial info for display before first channel update
		if m.initInfo != nil {
			ci, _ := db.GetLaunch(m.database, id)
			jobs, _ := db.GetLaunchJobsIncludingAttempts(m.database, id)
			outcomes, _ := db.GetAttemptOutcomesByLaunch(m.database, id)
			m.initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
		}
		if cmd := m.startWatchingInstance(id); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	m.partialErrorJobs = nil
	m.partialErrorsRetried = true
	m.rebuildReplacementCache()
	m.scrollOff = 0
	return m, tea.Batch(cmds...)
}

// ---------------------------------------------------------------------------
// Update handlers: system-specific
// ---------------------------------------------------------------------------

func (m watchModel) handleSystemTick() (tea.Model, tea.Cmd) {
	if m.refreshing {
		return m, scheduleWatchAllTick()
	}
	m.refreshing = true
	return m, tea.Batch(
		refreshWatchSystem(m.database, m.appConfig),
		scheduleWatchAllTick(),
	)
}

func (m watchModel) handleSystemRefreshed(msg watchAllRefreshedMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.err = nil
	m.cloudInstances = msg.snapshot.Launches
	m.onPremHosts = msg.snapshot.OnPremHosts
	m.unplacedJobs = msg.snapshot.UnplacedJobs

	// Update instanceIDs from discovered instances
	m.instanceIDs = make([]int64, len(msg.snapshot.Launches))
	for i, ci := range msg.snapshot.Launches {
		m.instanceIDs[i] = ci.ID
	}

	cmds := m.mergeSnapshot(msg.snapshot)
	m.rebuildReplacementCache()

	for _, host := range m.onPremHosts {
		m.syncWorker.Request(tui.SyncRequest{
			Host: host.Name,
			Rate: tui.GetHostSyncRate(host.Jobs),
		})
	}
	m.clampCursor()
	return m, tea.Batch(cmds...)
}

func (m *watchModel) mergeSnapshot(snapshot watchSystemSnapshot) []tea.Cmd {
	active := make(map[int64]bool, len(snapshot.Launches))
	var cmds []tea.Cmd

	for _, ci := range snapshot.Launches {
		active[ci.ID] = true
		snapUpdate := snapshot.InstanceUpdates[ci.ID]
		if existing, ok := m.updates[ci.ID]; ok {
			existing.Launch = snapUpdate.Launch
			existing.Jobs = snapUpdate.Jobs
			existing.JobAttemptOutcomes = snapUpdate.JobAttemptOutcomes
			m.updates[ci.ID] = existing
		} else {
			m.updates[ci.ID] = snapUpdate
		}
		if cmd := m.startWatchingInstance(ci.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	for id := range m.updates {
		if !active[id] {
			delete(m.updates, id)
		}
	}

	return cmds
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m watchModel) View() string {
	switch m.mode {
	case watchModeCampaign:
		content, cursorLine := m.renderCampaignView()
		return m.applyViewport(content, cursorLine)
	case watchModeSystem:
		content, cursorLine := m.renderSystemView()
		return m.applyViewport(content, cursorLine)
	}
	return ""
}

func (m watchModel) renderCampaignView() (string, int) {
	var b strings.Builder
	now := time.Now()
	width := m.width
	if width <= 0 {
		width = 100
	}
	selectableIndex := 0
	selectedVisualLine := -1
	lineCount := 0

	countLine := func() { lineCount++ }
	addLine := func(text string) {
		b.WriteString(text)
		b.WriteString("\n")
		countLine()
	}
	addSelectable := func(text string) {
		if selectableIndex == m.cursor {
			text = watchSelectedRowStyle.Render(padToWidth(text, width))
			selectedVisualLine = lineCount
		}
		b.WriteString(text)
		b.WriteString("\n")
		countLine()
		selectableIndex++
	}

	// Campaign header
	if m.campaignID > 0 {
		header := fmt.Sprintf("Campaign %d", m.campaignID)
		if !m.launchedAt.IsZero() {
			header += fmt.Sprintf(" — launched %s (%s ago)",
				m.launchedAt.Format("15:04"),
				now.Sub(m.launchedAt).Truncate(time.Second))
		}
		addLine(watchTitleStyle.Render(header))
		addLine("")
	}
	if summary := formatCampaignWatchSummaryLine(m.launchedAt, m.campaignViews(), now); summary != "" {
		addLine(summary)
		addLine("")
	}

	for _, id := range m.instanceIDs {
		if m.cachedHiddenIDs[id] {
			continue
		}
		donors := m.cachedReplacementChains[id]

		u, ok := m.updates[id]
		if !ok {
			info := m.initInfo[id]
			ci := info.ci
			jobs := info.jobs
			if ci != nil {
				update := campaign.InstanceUpdate{
					Launch:             ci,
					Jobs:               jobs,
					JobAttemptOutcomes: info.outcomes,
				}
				resolved := 0
				lines := formatWatchInstanceBlockLines(update, nil, watchInstanceBlockOptions{
					spinner:                  m.spinner.View(),
					showSpinnerIfNonTerminal: true,
					resolvedJobsOverride:     &resolved,
					dimJobStatuses:           true,
					predecessors:             donors,
				})
				if len(lines) > 0 {
					addSelectable(lines[0])
					for _, line := range lines[1:] {
						addLine(line)
					}
				}
			} else {
				addLine(m.spinner.View() + fmt.Sprintf(" Instance %d — waiting for data...", id))
			}
			addLine("")
			continue
		}

		lines := formatWatchInstanceBlockLines(u, m.jobProgressHWM, watchInstanceBlockOptions{
			predecessors: donors,
		})
		if len(lines) > 0 {
			addSelectable(lines[0])
			for _, line := range lines[1:] {
				addLine(line)
			}
		}
		addLine("")
	}

	// Unplaced jobs section (campaign mode)
	if len(m.unplacedJobs) > 0 {
		addLine(watchTitleStyle.Render(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs))))
		for _, job := range m.unplacedJobs {
			addSelectable("  " + truncate(m.formatUnplacedJobRow(job), max(width-2, 40)))
		}
		addLine("")
	}

	// Partial launch errors
	if len(m.partialErrors) > 0 && !m.partialErrorsRetried {
		b.WriteString(formatPartialErrors(m.partialErrors))
		b.WriteString("\n")
		countLine()
	}

	// Retry status
	if m.retrying {
		addLine(m.spinner.View() + fmt.Sprintf(" Retrying %d failed instance(s)...", m.countFailedInstances()))
	} else if m.retryResult != "" {
		addLine(m.retryResult)
	}

	if !m.done {
		hint := "j/k scroll  g/G top/bottom  q quit (instances continue in background)"
		if !m.retrying && m.hasRetryableFailures() {
			hint = "j/k scroll  g/G top/bottom  r retry  q quit (instances continue in background)"
		}
		addLine(watchDimStyle.Render(hint))
	}

	return b.String(), selectedVisualLine
}

func (m watchModel) renderSystemView() (string, int) {
	width := m.width
	if width <= 0 {
		width = 100
	}

	rows := make([]watchRenderRow, 0, 8+len(m.cloudInstances)+len(m.unplacedJobs))
	selectedVisualIndex := -1
	selectableIndex := 0

	addHeader := func(text string) {
		rows = append(rows, watchRenderRow{text: watchTitleStyle.Render(text)})
	}
	addSelectable := func(text string) {
		if selectableIndex == m.cursor {
			text = watchSelectedRowStyle.Render(padToWidth(text, width))
			selectedVisualIndex = len(rows)
		}
		rows = append(rows, watchRenderRow{text: text})
		selectableIndex++
	}
	addPlain := func(text string) {
		rows = append(rows, watchRenderRow{text: text})
	}

	title := "System Watch"
	if m.refreshing {
		title += "  " + m.spinner.View() + " " + watchDimStyle.Render("refreshing")
	}
	addHeader(title)
	addPlain("")

	addHeader(fmt.Sprintf("Rental Instances (%d)", len(m.cloudInstances)))
	if len(m.cloudInstances) == 0 {
		addPlain(watchDimStyle.Render("  no active rental instances"))
	} else {
		views := make([]cloudInstanceView, 0, len(m.cloudInstances))
		for _, ci := range m.cloudInstances {
			update := normalizeWatchInstanceUpdate(m.updates[ci.ID], ci)
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
		}
		if summary := formatCloudAggregateSummary("  Summary:", summarizeLaunches(views, time.Now())); summary != "" {
			addPlain(summary)
			addPlain("")
		}
		for i, ci := range m.cloudInstances {
			if m.cachedHiddenIDs[ci.ID] {
				continue
			}
			if i > 0 {
				addPlain("")
			}
			update := normalizeWatchInstanceUpdate(m.updates[ci.ID], ci)
			donors := m.cachedReplacementChains[ci.ID]
			lines := formatWatchInstanceBlockLines(update, m.jobProgressHWM, watchInstanceBlockOptions{
				predecessors: donors,
			})
			if len(lines) == 0 {
				continue
			}
			addSelectable(truncate(lines[0], width))
			for _, line := range lines[1:] {
				addPlain(truncate(line, width))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Inventory Hosts (%d active)", len(m.onPremHosts)))
	if len(m.onPremHosts) == 0 {
		addPlain(watchDimStyle.Render("  no active inventory jobs"))
	} else {
		projectWidth := len("PROJECT")
		for _, host := range m.onPremHosts {
			for _, job := range host.Jobs {
				if w := len(campaign.JobProjectLabel(job)); w > projectWidth {
					projectWidth = w
				}
			}
		}
		for _, host := range m.onPremHosts {
			addPlain(watchStatusStyle.Render("  " + host.Name))
			for _, job := range host.Jobs {
				addSelectable("    " + truncate(m.formatOnPremJobRow(job, projectWidth), width-4))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs)))
	if len(m.unplacedJobs) == 0 {
		addPlain(watchDimStyle.Render("  no unplaced jobs"))
	} else {
		for _, job := range m.unplacedJobs {
			addSelectable("  " + truncate(m.formatUnplacedJobRow(job), width-2))
		}
	}

	// Build footer
	footerParts := make([]string, 0, 3)
	footerPrefixWidth := 0
	if m.err != nil {
		errText := fmt.Sprintf("Error: %v", m.err)
		footerParts = append(footerParts, watchFailedStyle.Render(errText))
		footerPrefixWidth = lipgloss.Width(errText)
	} else if m.flashMessage != "" {
		footerParts = append(footerParts, m.flashMessage)
		footerPrefixWidth = lipgloss.Width(m.flashMessage)
	}
	if detail := m.selectedStatusDetail(); detail != "" {
		detail = m.truncateFooterDetail(detail, footerPrefixWidth)
		if detail != "" {
			footerParts = append(footerParts, watchDimStyle.Render(detail))
		}
	}

	// Retry status in footer for system mode
	if m.retrying {
		footerParts = append(footerParts, m.spinner.View()+fmt.Sprintf(" Retrying %d failed instance(s)...", m.countFailedInstances()))
	} else if m.retryResult != "" {
		footerParts = append(footerParts, m.retryResult)
	}

	controls := "[u] unplace queued job  [l] launch  [r] retry  [q] quit"
	if !m.hasRetryableFailures() {
		controls = "[u] unplace queued job  [l] launch  [q] quit"
	}
	footerParts = append(footerParts, watchDimStyle.Render(controls))

	// Render rows into content string
	contentHeight := m.height - 1
	if contentHeight < 1 {
		contentHeight = len(rows)
	}
	start := watchScrollStart(len(rows), selectedVisualIndex, contentHeight)
	end := start + contentHeight
	if end > len(rows) {
		end = len(rows)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(rows[i].text)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if end-start < contentHeight {
		padLine := strings.Repeat(" ", max(0, m.width))
		for i := end - start; i < contentHeight; i++ {
			b.WriteString("\n")
			b.WriteString(padLine)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	footer := strings.Join(footerParts, "  ")
	b.WriteString(footer)

	// For system mode, scrolling is done via watchScrollStart above, so we
	// return -1 to skip applyViewport's cursor-based slicing.
	return b.String(), -1
}

// applyViewport slices rendered content to fit the terminal height.
// If cursorLine >= 0, it centers the viewport around that line.
// Otherwise, bottom-anchored: scrollOff=0 shows the bottom of the content.
func (m watchModel) applyViewport(content string, cursorLine int) string {
	if m.height <= 0 || m.done {
		return content
	}

	// System mode uses its own scrolling in renderSystemView
	if cursorLine < 0 && m.mode == watchModeSystem {
		return content
	}

	// Fast path: count newlines to check fit without allocating a []string
	if strings.Count(content, "\n") < m.height {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) <= m.height {
		return content
	}

	// If we have a cursor line, center viewport around it
	if cursorLine >= 0 {
		start := watchScrollStart(len(lines), cursorLine, m.height)
		end := start + m.height
		if end > len(lines) {
			end = len(lines)
		}
		visible := lines[start:end]
		if start > 0 {
			visible[0] = watchDimStyle.Render(fmt.Sprintf("↑ %d more lines above", start))
		}
		off := len(lines) - end
		if off > 0 {
			visible[len(visible)-1] = watchDimStyle.Render(fmt.Sprintf("↓ %d more lines below", off))
		}
		return strings.Join(visible, "\n")
	}

	// Bottom-anchored scrolling (fallback for campaign mode without cursor)
	maxOff := len(lines) - m.height
	off := m.scrollOff
	if off > maxOff {
		off = maxOff
	}

	end := len(lines) - off
	start := end - m.height
	if start < 0 {
		start = 0
	}

	visible := lines[start:end]

	if start > 0 {
		visible[0] = watchDimStyle.Render(fmt.Sprintf("↑ %d more lines above", start))
	}
	if off > 0 {
		visible[len(visible)-1] = watchDimStyle.Render(fmt.Sprintf("↓ %d more lines below", off))
	}

	return strings.Join(visible, "\n")
}

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

func (m watchModel) retryableJobs() []*db.Job {
	seen := make(map[int64]struct{})
	var jobs []*db.Job

	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if !ok || u.Launch == nil || u.Launch.Status != db.LaunchStatusFailed {
			continue
		}
		instanceJobs, err := db.GetLaunchJobsIncludingAttempts(m.database, id)
		if err != nil {
			continue
		}
		for _, j := range instanceJobs {
			if j == nil {
				continue
			}
			if _, ok := seen[j.ID]; ok {
				continue
			}
			fresh, err := db.GetJobByID(m.database, j.ID)
			if err != nil || fresh == nil {
				continue
			}
			if fresh.Status == db.StatusQueued && (fresh.Host == "" || fresh.LaunchID == nil) {
				seen[fresh.ID] = struct{}{}
				jobs = append(jobs, fresh)
			}
		}
	}

	for _, j := range m.partialErrorJobs {
		if j == nil {
			continue
		}
		if _, ok := seen[j.ID]; ok {
			continue
		}
		fresh, err := db.GetJobByID(m.database, j.ID)
		if err != nil || fresh == nil {
			continue
		}
		if fresh.Status == db.StatusQueued {
			seen[fresh.ID] = struct{}{}
			jobs = append(jobs, fresh)
		}
	}

	return jobs
}

func (m watchModel) retryFailedInstances(extraAttempts int) tea.Cmd {
	database := m.database
	cfg := m.appConfig
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, extraAttempts)
		if err != nil {
			return retryResultMsg{err: err}
		}
		if result == nil {
			return retryResultMsg{}
		}
		return retryResultMsg{instanceIDs: result.InstanceIDs, skipped: result.Skipped}
	}
}

func (m watchModel) hasRetryableFailures() bool {
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && db.IsRetryableTermination(u.Launch) {
			return true
		}
	}
	return len(m.partialErrors) > 0 && !m.partialErrorsRetried
}

func (m watchModel) countFailedInstances() int {
	count := 0
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && u.Launch.Status == db.LaunchStatusFailed {
			count++
		}
	}
	if len(m.partialErrorJobs) > 0 {
		count++
	}
	return count
}

// ---------------------------------------------------------------------------
// Replacement chain cache
// ---------------------------------------------------------------------------

func (m *watchModel) rebuildReplacementCache() {
	hiddenIDs := make(map[int64]bool)
	replacementChains := make(map[int64][]*db.Launch)

	getCI := func(id int64) *db.Launch {
		if u, ok := m.updates[id]; ok && u.Launch != nil {
			return u.Launch
		}
		if m.initInfo != nil {
			if info, ok := m.initInfo[id]; ok && info.ci != nil {
				return info.ci
			}
		}
		ci, _ := db.GetLaunch(m.database, id)
		return ci
	}

	for _, id := range m.instanceIDs {
		ci := getCI(id)
		if ci == nil || ci.ReplacedInstanceID == nil {
			continue
		}
		chain := collectReplacementChain(ci, getCI)
		if len(chain) > 0 {
			replacementChains[id] = chain
			for _, predecessor := range chain {
				hiddenIDs[predecessor.ID] = true
			}
		}
	}

	m.cachedHiddenIDs = hiddenIDs
	m.cachedReplacementChains = replacementChains
}

// ---------------------------------------------------------------------------
// Cursor / selection helpers
// ---------------------------------------------------------------------------

func (m *watchModel) moveCursor(delta int) {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m *watchModel) clampCursor() {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m watchModel) selectableRowCount() int {
	switch m.mode {
	case watchModeCampaign:
		count := 0
		for _, id := range m.instanceIDs {
			if !m.cachedHiddenIDs[id] {
				count++
			}
		}
		count += len(m.unplacedJobs)
		return count
	case watchModeSystem:
		count := len(m.unplacedJobs)
		for _, ci := range m.cloudInstances {
			if !m.cachedHiddenIDs[ci.ID] {
				count++
			}
		}
		for _, host := range m.onPremHosts {
			count += len(host.Jobs)
		}
		return count
	}
	return 0
}

func (m watchModel) visibleCloudInstanceCount() int {
	count := 0
	for _, ci := range m.cloudInstances {
		if !m.cachedHiddenIDs[ci.ID] {
			count++
		}
	}
	return count
}

func (m watchModel) selectedOnPremJob() *db.Job {
	if m.mode != watchModeSystem {
		return nil
	}
	index := m.cursor - m.visibleCloudInstanceCount()
	if index < 0 {
		return nil
	}
	for _, host := range m.onPremHosts {
		if index < len(host.Jobs) {
			return host.Jobs[index]
		}
		index -= len(host.Jobs)
	}
	return nil
}

func (m watchModel) selectedUnplacedJob() *db.Job {
	switch m.mode {
	case watchModeSystem:
		index := m.cursor - m.visibleCloudInstanceCount()
		if index < 0 {
			return nil
		}
		for _, host := range m.onPremHosts {
			if index < len(host.Jobs) {
				return nil
			}
			index -= len(host.Jobs)
		}
		if index < 0 || index >= len(m.unplacedJobs) {
			return nil
		}
		return m.unplacedJobs[index]
	case watchModeCampaign:
		// In campaign mode, selectable rows are: instances, then unplaced jobs
		visibleInstances := 0
		for _, id := range m.instanceIDs {
			if !m.cachedHiddenIDs[id] {
				visibleInstances++
			}
		}
		index := m.cursor - visibleInstances
		if index < 0 || index >= len(m.unplacedJobs) {
			return nil
		}
		return m.unplacedJobs[index]
	}
	return nil
}

func (m *watchModel) removeOnPremJob(jobID int64) {
	filteredHosts := m.onPremHosts[:0]
	for _, host := range m.onPremHosts {
		jobs := host.Jobs[:0]
		for _, job := range host.Jobs {
			if job != nil && job.ID == jobID {
				continue
			}
			jobs = append(jobs, job)
		}
		if len(jobs) == 0 {
			continue
		}
		host.Jobs = jobs
		filteredHosts = append(filteredHosts, host)
	}
	m.onPremHosts = filteredHosts
}

func (m *watchModel) upsertUnplacedJob(job *db.Job) {
	if job == nil {
		return
	}
	for i, existing := range m.unplacedJobs {
		if existing != nil && existing.ID == job.ID {
			m.unplacedJobs[i] = job
			return
		}
	}
	m.unplacedJobs = append(m.unplacedJobs, job)
	sort.SliceStable(m.unplacedJobs, func(i, j int) bool {
		return m.unplacedJobs[i].ID < m.unplacedJobs[j].ID
	})
}

// ---------------------------------------------------------------------------
// View helpers: campaign mode
// ---------------------------------------------------------------------------

func (m watchModel) campaignViews() []cloudInstanceView {
	views := make([]cloudInstanceView, 0, len(m.instanceIDs))
	for _, id := range m.instanceIDs {
		if update, ok := m.updates[id]; ok {
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
			continue
		}
		if m.initInfo != nil {
			if info, ok := m.initInfo[id]; ok {
				views = append(views, cloudInstanceView{Launch: info.ci})
			}
		}
	}
	return views
}

func formatCampaignWatchSummaryLine(launchedAt time.Time, views []cloudInstanceView, now time.Time) string {
	agg := summarizeLaunches(views, now)
	label := "Summary:"
	if !launchedAt.IsZero() {
		label = "Summary: uptime: " + now.Sub(launchedAt).Truncate(time.Second).String()
	}
	return formatCloudAggregateSummary(label, agg)
}

// ---------------------------------------------------------------------------
// View helpers: system mode
// ---------------------------------------------------------------------------

func (m watchModel) formatOnPremJobRow(job *db.Job, projectWidth int) string {
	status := job.EffectiveStatus()
	duration := "—"
	if job.StartTime > 0 {
		d := time.Duration(time.Now().Unix()-job.StartTime) * time.Second
		duration = d.Truncate(time.Second).String()
	}
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = campaign.TruncateCommand(job.Command, 50)
	}
	return fmt.Sprintf("#%-4d  %s  %-10s  %-*s  %s",
		job.ID,
		renderWatchJobStatusText(status, status, watchInstanceBlockOptions{}),
		duration,
		projectWidth, campaign.JobProjectLabel(job),
		desc,
	)
}

func (m watchModel) formatUnplacedJobRow(job *db.Job) string {
	return fmt.Sprintf("#%-4d %-12s %-28s %s",
		job.ID,
		campaign.JobProjectLabel(job),
		truncate(job.EffectiveDescription(), 28),
		formatWatchGPUConstraint(job),
	)
}

func (m watchModel) selectedStatusDetail() string {
	job := m.selectedUnplacedJob()
	if job == nil || len(job.PlacementReasons) == 0 {
		return ""
	}
	return fmt.Sprintf("#%d unplaced: %s", job.ID, strings.Join(job.PlacementReasons, " | "))
}

func (m watchModel) truncateFooterDetail(detail string, prefixWidth int) string {
	if m.width <= 0 {
		return detail
	}
	controlsWidth := lipgloss.Width("[u] unplace queued job  [l] launch  [r] retry  [q] quit")
	available := m.width - controlsWidth
	if prefixWidth > 0 {
		available -= prefixWidth + lipgloss.Width("  ")
	}
	available -= lipgloss.Width("  ")
	if available <= 0 {
		return ""
	}
	if available < 16 {
		return truncate(detail, max(available, 3))
	}
	return truncate(detail, available)
}

// ---------------------------------------------------------------------------
// On-prem sync helpers
// ---------------------------------------------------------------------------

func (m watchModel) requestOnPremSyncs() {
	if m.syncWorker == nil {
		return
	}
	jobs, err := db.ListActiveOnPremJobs(m.database)
	if err != nil {
		return
	}
	byHost := make(map[string][]*db.Job)
	for _, job := range jobs {
		if job != nil && job.Host != "" {
			byHost[job.Host] = append(byHost[job.Host], job)
		}
	}
	for host, hostJobs := range byHost {
		m.syncWorker.Request(tui.SyncRequest{
			Host: host,
			Rate: tui.GetHostSyncRate(hostJobs),
		})
	}
}

// ---------------------------------------------------------------------------
// Job preservation
// ---------------------------------------------------------------------------

func preserveWatchCurrentJobs(instanceID int64, prevJobs, jobs []*db.Job) []*db.Job {
	if instanceID == 0 || len(jobs) == 0 {
		return jobs
	}

	currentJobIDs := make(map[int64]struct{})
	for _, job := range prevJobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID != instanceID {
			continue
		}
		currentJobIDs[job.ID] = struct{}{}
	}
	if len(currentJobIDs) == 0 {
		return jobs
	}

	preserved := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			preserved = append(preserved, nil)
			continue
		}
		if _, ok := currentJobIDs[job.ID]; !ok {
			preserved = append(preserved, job)
			continue
		}
		if job.LaunchID != nil && *job.LaunchID == instanceID {
			preserved = append(preserved, job)
			continue
		}

		jobCopy := *job
		preservedInstanceID := instanceID
		jobCopy.LaunchID = &preservedInstanceID
		preserved = append(preserved, &jobCopy)
	}
	return preserved
}

// ---------------------------------------------------------------------------
// Refresh commands
// ---------------------------------------------------------------------------

func refreshWatchInstancesFromDB(database *sql.DB, instanceIDs []int64, quitAfter bool) tea.Cmd {
	return func() tea.Msg {
		cloudInstances := make(map[int64]*db.Launch, len(instanceIDs))
		jobs := make(map[int64][]*db.Job, len(instanceIDs))
		outcomes := make(map[int64]map[int64]string, len(instanceIDs))
		for _, id := range instanceIDs {
			if ci, err := db.GetLaunch(database, id); err == nil && ci != nil {
				cloudInstances[id] = ci
			}
			if instanceJobs, err := db.GetLaunchJobsIncludingAttempts(database, id); err == nil && instanceJobs != nil {
				jobs[id] = instanceJobs
			}
			if instanceOutcomes, err := db.GetAttemptOutcomesByLaunch(database, id); err == nil {
				outcomes[id] = instanceOutcomes
			}
		}
		return watchJobsRefreshedMsg{
			cloudInstances: cloudInstances,
			jobs:           jobs,
			outcomes:       outcomes,
			quitAfter:      quitAfter,
		}
	}
}

func refreshWatchSystem(database *sql.DB, cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
		return watchAllRefreshedMsg{snapshot: snapshot, err: err}
	}
}

func requestWatchJobUnplace(database *sql.DB, jobID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("job %d not found", jobID)}
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return watchUnplaceDoneMsg{err: err}
		}
		updatedJob, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("reload job %d: %w", jobID, err)}
		}
		return watchUnplaceDoneMsg{job: updatedJob, message: result.Message}
	}
}

func refreshWatchOnPrem(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		onPremJobs, err := db.ListActiveOnPremJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			onPremHosts:  groupOnPremHosts(onPremJobs),
			unplacedJobs: unplacedJobs,
		}
	}
}

// ---------------------------------------------------------------------------
// Tick scheduling
// ---------------------------------------------------------------------------

func scheduleSyncTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

func scheduleCheckDone() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return watchCheckDoneMsg{}
	})
}

func scheduleWatchAllTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchAllTickMsg{}
	})
}

// ---------------------------------------------------------------------------
// Scroll helpers
// ---------------------------------------------------------------------------

func watchScrollStart(totalRows, selectedIndex, viewportHeight int) int {
	if viewportHeight <= 0 || totalRows <= viewportHeight || selectedIndex < 0 {
		return 0
	}
	start := selectedIndex - viewportHeight/2
	if start < 0 {
		start = 0
	}
	maxStart := totalRows - viewportHeight
	if start > maxStart {
		start = maxStart
	}
	return start
}

func padToWidth(s string, width int) string {
	current := lipgloss.Width(s)
	if current >= width {
		return truncate(s, width)
	}
	return s + strings.Repeat(" ", width-current)
}

// ---------------------------------------------------------------------------
// Entry points (called from campaign_report.go and watch.go)
// ---------------------------------------------------------------------------

// watchInstances runs the interactive TUI watch for one or more cloud instances.
// Returns the final list of instance IDs (which may include auto-relaunched instances).
func watchInstances(database *sql.DB, instanceIDs []int64) ([]int64, error) {
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	model := newCampaignWatchModel(database, instanceIDs, r2Client, cfg)

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

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
