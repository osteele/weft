package terminal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/placement"
)

const listDBChangeDebounce = 200 * time.Millisecond
const listTUISyncInterval = TerminalSyncInterval
const listAutoLeaseTTL = 30 * time.Second

// backgroundSyncKey is the pendingSyncHosts sentinel for the non-worker
// startup / tick cloud sync, which is not tied to a specific host.
const backgroundSyncKey = ""

type listTUIModel struct {
	database                   *sql.DB
	args                       []string
	title                      string
	unprocessedView            bool
	statusView                 string
	jobs                       []*db.Job
	layout                     jobListLayout
	cursor                     int
	offset                     int
	width                      int
	height                     int
	syncEnabled                bool
	pendingSyncHosts           map[string]struct{}
	nextSyncTickAt             time.Time
	statusMessage              string
	daemonRestartInProgress    bool
	dbWatcher                  *dbwatch.Source
	debounceActive             bool
	debouncePending            bool
	syncWorker                 *hostsync.Worker
	ctx                        context.Context
	cancel                     context.CancelFunc
	groupMode                  listGroupMode
	groupedByStatus            bool
	groupedUnprocessedView     bool
	projectFilter              string
	projectInputActive         bool
	projectInputValue          string
	projectCandidates          []string
	projectCandidateCursor     int
	projectCandidatesLoading   bool
	hideStatusArea             bool
	groupedRows                []groupedStatusRow
	groupedSelectableRows      []int
	autoInProgress             bool
	autoPassStartedAt          time.Time
	autoPassPhase              autoPilotPhaseHint
	autoPersistentError        string
	autoPersistentBlocked      string
	autoPersistentBlockedN     int
	autoRunRateTargetCents     int
	autoRunRateInputActive     bool
	autoRunRateInputValue      string
	autoRunRateInputPhase      autoBudgetPhase
	autoDailyCapCents          int
	autoNextPassAt             time.Time
	autoLeaseOwner             string
	autoLeaseScope             string
	autoBlockReasons           map[int64]string
	autoBlockDetail            map[int64]*blockreason.Structured
	expandedBlocked            map[int64]bool
	showInstanceFailures       bool
	instanceFailuresScroll     int
	lastAutoPilotErrorRaw      string
	showAutoPilotErrorDetails  bool
	autopilotPaused            bool
	autopilotPausedReason      string
	launchLiveByID             map[int64]*db.LaunchLiveState
	launchStatusByID           map[int64]string
	placingJobIDs              map[int64]struct{}
	placementQueuedAtByJob     map[int64]int64
	placementStatusByJob       map[int64]jobview.PlacementStatus
	launchByID                 map[int64]*db.Launch
	launchBootstrapP50         time.Duration
	launchBootstrapSamples     int
	launchStageETAByName       map[string]groupedStatusLaunchingStageETA
	launchStageEnteredAtByID   map[int64]int64
	launchSpinner              spinner.Model
	launchSpinnerRunning       bool
	recentFailedInstances      *recentFailedInstances
	lastInstanceRunningAt      int64
	placementDaemonStopped     bool
	hostMetricsByName          map[string]*hostinfo.Host
	hostInfoByName             map[string]*db.CachedHostInfo
	cordonedHostsByName        map[string]bool
	overloadedHostsByName      map[string]bool
	quickLaunching             bool
	quickLaunchScope           string
	quickLaunchProgress        <-chan listQuickLaunchProgressMsg
	quickLaunchDone            <-chan listQuickLaunchDoneMsg
	quickLaunchStatusHoldUntil time.Time
	rebalanceProgress          <-chan rebalanceProgressMsg
	movePicker                 movePickerModel
	rebalancePreview           rebalancePreviewModel
	showHelp                   bool
	moveLookupPending          bool
	moveLookupRequestID        int64
	moveLookupJobID            int64
	moveLookupSeq              int64
	focused                    bool
	appConfig                  *config.Config
	cloudClients               []cloud.Client
	aiAssist                   *aiAssistState
}

type listDaemonRestartedMsg struct {
	pid    int
	action daemoncontrol.EnsureAction
	err    error
}

type listURLOpenedMsg struct {
	url string
	err error
}

type groupedViewLayout struct {
	visibleRows                 []groupedViewportLine
	statusLine                  string
	sharedStatusLines           []string
	autoPilotLine               string
	errorDetailsLines           []string
	instanceHealthLines         []string
	selectedDetails             []string
	budgetPanelLines            []string
	controlsLine                string
	maxBodyLines                int
	daemonStatusY               int
	daemonActionable            bool
	vastCreditWarningY          int
	vastCreditWarningActionable bool
}

var listRestartDaemonFunc = restartDaemonForListTUI
var listOpenURLFunc = openURLForListTUI

const vastaiBillingURL = "https://cloud.vast.ai/billing/"

type listGroupMode string

const (
	listGroupUngrouped listGroupMode = "ungrouped"
	listGroupStatus    listGroupMode = "status"
	listGroupProject   listGroupMode = "project"
	listGroupHost      listGroupMode = "host/instance"
)

func initialListGroupMode(groupedByStatus bool) listGroupMode {
	if groupedByStatus {
		return listGroupStatus
	}
	return listGroupUngrouped
}

func (m listTUIModel) effectiveGroupMode() listGroupMode {
	if m.groupMode != "" {
		return m.groupMode
	}
	if m.groupedByStatus {
		return listGroupStatus
	}
	return listGroupUngrouped
}

func (m listTUIModel) isGroupedView() bool {
	return m.effectiveGroupMode() != listGroupUngrouped
}

func (m listTUIModel) isStatusGroupedView() bool {
	return m.effectiveGroupMode() == listGroupStatus
}

func (m *listTUIModel) setGroupMode(mode listGroupMode) {
	if mode == "" {
		mode = listGroupUngrouped
	}
	m.groupMode = mode
	m.groupedByStatus = mode == listGroupStatus
	m.rebuildLayout()
	m.rebuildGroupedRows()
	m.clampCursor()
	m.adjustOffset()
}

func (m listTUIModel) nextGroupMode() listGroupMode {
	switch m.effectiveGroupMode() {
	case listGroupUngrouped:
		return listGroupStatus
	case listGroupStatus:
		return listGroupProject
	case listGroupProject:
		return listGroupHost
	default:
		return listGroupUngrouped
	}
}

func listGroupModeLabel(mode listGroupMode) string {
	switch mode {
	case listGroupStatus:
		return "status"
	case listGroupProject:
		return "project"
	case listGroupHost:
		return "host/instance"
	default:
		return "ungrouped"
	}
}

type listJobsLoadedMsg struct {
	jobs                     []*db.Job
	launchLiveByID           map[int64]*db.LaunchLiveState
	launchStatusByID         map[int64]string
	placingJobIDs            map[int64]struct{}
	placementQueuedAtByJob   map[int64]int64
	placementStatusByJob     map[int64]jobview.PlacementStatus
	launchByID               map[int64]*db.Launch
	launchBootstrapP50       time.Duration
	launchBootstrapSamples   int
	launchStageETAByName     map[string]groupedStatusLaunchingStageETA
	launchStageEnteredAtByID map[int64]int64
	recentFailedInstances    *recentFailedInstances
	lastInstanceRunningAt    int64
	placementDaemonStopped   bool
	hostInfoByName           map[string]*db.CachedHostInfo
	cordonedHostsByName      map[string]bool
	overloadedHostsByName    map[string]bool
	autoPassPhase            autoPilotPhaseHint
	autopilotPaused          bool
	autopilotPausedReason    string
	err                      error
}

type listSyncFinishedMsg struct {
	warnings []string
	full     bool
}

type listDBWatcherReadyMsg struct {
	source *dbwatch.Source
	err    error
}

type listDBWatchEventMsg struct {
	err error
}

type listDBRefreshTriggeredMsg struct{}
type listSyncTickMsg struct{}
type listSyncWorkerResultMsg struct {
	result hostsync.Result
}
type listAutoPilotDoneMsg struct {
	placed            int
	rebalanced        int
	launched          int
	launchedClass     string
	blockedReasons    map[int64]string
	structuredBlocked map[int64]*blockreason.Structured
	anotherHolding    bool
	paused            bool
	err               error
	// deferred marks a redelivery after the minimum-display hold so the handler
	// knows to apply the result rather than schedule another tick.
	deferred bool
}
type listQuickLaunchDoneMsg struct {
	instanceIDs  []int64
	movedJobs    int
	warning      string
	runningJobID int64
	err          error
}
type listQuickLaunchProgressMsg struct {
	message string
}

type listProjectCandidatesLoadedMsg struct {
	projects []string
	err      error
}

type rebalancePreviewLoadedMsg struct {
	moves []orchestration.QueueRebalanceMove
	err   error
}

type rebalanceProgressMsg struct {
	message string
}

type rebalanceAppliedMsg struct {
	count      int
	overBudget int
	err        error
}

type listMoveOptionsReadyMsg struct {
	requestID  int64
	jobID      int64
	options    []moveOption
	err        error
	newOnly    bool
	loadingNew bool
}

var (
	listTUITitleStyle    = lipgloss.NewStyle().Bold(true)
	listTUIHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUISelectedStyle = lipgloss.NewStyle().Reverse(true)
)

// renderSelectedRow applies the selected-row highlight to a row that may
// already contain inner ANSI escape sequences (e.g. the cyan rental glyph
// and job ID). lipgloss's Reverse style does not propagate across inner
// SGR resets, so the highlight visibly stops where the styled span ends.
// Strip inner styling first, then pad to the row width so the reverse
// background extends across the whole line.
func renderSelectedRow(row string, width int) string {
	plain := ansi.Strip(row)
	if width > 0 {
		if pad := width - lipgloss.Width(plain); pad > 0 {
			plain += strings.Repeat(" ", pad)
		}
	}
	return listTUISelectedStyle.Render(plain)
}

var (
	listTUIFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUIPromptStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
	listTUIEmptyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, projectFilter string) error {
	cfg, _ := config.Load()
	router := newListWatchRouterModel(database, cfg, args, jobs, title, syncEnabled, groupedByStatus, projectFilter)

	outputOpt, restore := InstallTUIStdioCapture()

	finalModel, err := tea.NewProgram(router, outputOpt, tea.WithAltScreen(), tea.WithReportFocus(), tea.WithMouseCellMotion()).Run()
	restore()
	if err != nil {
		return fmt.Errorf("run list TUI: %w", err)
	}
	if r, ok := finalModel.(watchRouterModel); ok {
		r.stopBanners()
		if summary := listTUIExitSummary(r.active); summary != "" {
			fmt.Fprint(os.Stdout, summary)
		}
	}
	return nil
}

func newListTUIModel(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, projectFilter string) listTUIModel {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, _ := config.Load()
	cloudClients, _ := buildCloudClients(cfg)

	var sw *hostsync.Worker
	if syncEnabled {
		r2Client, _ := buildR2Client(cfg)
		sw = hostsync.New(database, cloudClients, r2Client, cfg)
		sw.Start()
	}
	s := spinner.New()
	s.Spinner = spinner.Dot

	unprocessedView := listTitleHasPart(title, "unprocessed")
	statusView := listTitleStatusView(title)
	baseTitle := listTitleWithoutPart(listTitleWithoutPart(listTitleWithoutPart(title, "unprocessed"), "queued"), "draft")
	model := listTUIModel{
		database:               database,
		args:                   append([]string(nil), args...),
		title:                  baseTitle,
		unprocessedView:        unprocessedView,
		statusView:             statusView,
		jobs:                   jobs,
		syncEnabled:            syncEnabled,
		pendingSyncHosts:       map[string]struct{}{},
		nextSyncTickAt:         time.Now().Add(throttledInterval(listTUISyncInterval, true)),
		syncWorker:             sw,
		ctx:                    ctx,
		cancel:                 cancel,
		groupMode:              initialListGroupMode(groupedByStatus),
		groupedByStatus:        groupedByStatus,
		groupedUnprocessedView: unprocessedView,
		projectFilter:          projectFilter,
		autoLeaseOwner:         fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		autoLeaseScope:         buildListAutoLeaseScope(baseTitle),
		autoRunRateTargetCents: cfg.AutoRunRateSoftTargetCents(),
		autoDailyCapCents:      cfg.AutoRunawaySpendDailyCapCents(),
		quickLaunchScope:       "list_quick_launch:" + buildListAutoLeaseScope(baseTitle),
		focused:                true,
		appConfig:              cfg,
		cloudClients:           cloudClients,
		launchSpinner:          s,
	}
	model.rebuildGroupedRows()
	if syncEnabled {
		if sw != nil {
			for _, j := range jobs {
				if j != nil && j.Host != "" {
					model.pendingSyncHosts[j.Host] = struct{}{}
				}
			}
		} else {
			model.pendingSyncHosts[backgroundSyncKey] = struct{}{}
		}
	}
	return model
}

func (m *listTUIModel) shutdown() {
	m.shutdownImmediate()
	if m.syncWorker != nil {
		m.syncWorker.Stop()
	}
}

func (m *listTUIModel) shutdownImmediate() {
	_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
	_ = db.ReleaseAutoLease(m.database, m.quickLaunchScope, m.autoLeaseOwner)
	if m.dbWatcher != nil {
		_ = m.dbWatcher.Close()
	}
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *listTUIModel) shutdownForQuit() {
	m.shutdownImmediate()
	if m.syncWorker != nil {
		stopSyncWorkerAfterQuit(m.syncWorker)
	}
}

func (m listTUIModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.startDBWatcher(), m.reloadJobs(), m.scheduleListSyncTick()}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
		cmds = append(cmds,
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return listSyncWorkerResultMsg{result: r}
			}),
		)
		cmds = m.enqueueBackgroundCloudSync(cmds, false)
	} else if m.syncEnabled {
		cmds = append(cmds, m.runBackgroundSync(false))
	}
	if cmd := m.runAutoPilot(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.hasActiveLaunchingSpinner() {
		m.launchSpinnerRunning = true
		cmds = append(cmds, m.launchSpinner.Tick)
	}
	return tea.Batch(cmds...)
}

func (m listTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.rebuildLayout()
		m.rebuildGroupedRows()
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case tea.KeyMsg:
		if m.rebalancePreview.active {
			return m.handleRebalancePreviewKey(msg)
		}
		if m.movePicker.active {
			return m.handleMovePickerKey(msg)
		}
		if m.aiAssist != nil {
			return m.handleAIAssistKey(msg)
		}
		if m.projectInputActive {
			return m.handleProjectFilterInputKey(msg)
		}
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "enter":
				m.showHelp = false
				return m, nil
			}
			return m, nil
		}
		if m.showInstanceFailures {
			page := max(1, m.height-3)
			switch msg.String() {
			case "f", "esc", "q", "enter":
				m.showInstanceFailures = false
			case "up", "k":
				m.instanceFailuresScroll = max(0, m.instanceFailuresScroll-1)
			case "down", "j":
				m.instanceFailuresScroll = min(m.instanceFailuresMaxScroll(), m.instanceFailuresScroll+1)
			case "pgup", "b":
				m.instanceFailuresScroll = max(0, m.instanceFailuresScroll-page)
			case "pgdown", " ":
				m.instanceFailuresScroll = min(m.instanceFailuresMaxScroll(), m.instanceFailuresScroll+page)
			}
			return m, nil
		}
		if m.isGroupedView() {
			return m.handleGroupedKey(msg)
		}
		if next, cmd, ok := handleListKeyBinding(m, msg.String(), listFlatKeyBindings()); ok {
			return next, cmd
		}
		switch msg.String() {
		case "A":
			if m.isGroupedView() {
				return m.toggleAutopilot()
			}
		case "n":
			if !m.isGroupedView() {
				break
			}
			if m.quickLaunching {
				m.statusMessage = "Launch already in progress..."
				return m, nil
			}
			if !computeGroupedETA(m.groupedJobsWithAutoReasons(), m.launchLiveByID, time.Now()).HasQueued {
				m.statusMessage = "No queued jobs to launch."
				return m, nil
			}
			m.clearAutoPilotPersistentState()
			m.quickLaunching = true
			m.statusMessage = "Launching new instance..."
			progressCh := make(chan listQuickLaunchProgressMsg, 16)
			doneCh := make(chan listQuickLaunchDoneMsg, 1)
			m.quickLaunchProgress = progressCh
			m.quickLaunchDone = doneCh
			return m, tea.Batch(
				m.startQuickLaunch(progressCh, doneCh),
				m.waitForQuickLaunchProgress(),
				m.waitForQuickLaunchDone(),
			)
		}
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case spinner.TickMsg:
		if !m.hasActiveLaunchingSpinner() {
			m.launchSpinnerRunning = false
			return m, nil
		}
		m.launchSpinnerRunning = true
		var cmd tea.Cmd
		m.launchSpinner, cmd = m.launchSpinner.Update(msg)
		m.rebuildGroupedRows()
		return m, cmd

	case moveLookupTickMsg:
		if !m.moveLookupPending || msg.requestID != m.moveLookupRequestID || !m.movePicker.active {
			return m, nil
		}
		m.movePicker.loadingFrame++
		return m, scheduleMoveLookupTick(msg.requestID)

	case aiAssistSyncStartMsg:
		return m.applyAIAssistSyncStart(msg)

	case aiAssistSyncDoneMsg:
		return m.applyAIAssistSyncDone(msg)

	case aiAssistResultMsg:
		return m.applyAIAssistResult(msg)

	case aiAssistChunkMsg:
		return m.applyAIAssistChunk(msg)

	case aiAssistDoneMsg:
		return m.applyAIAssistDone(msg)

	case listJobsLoadedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			return m, nil
		}
		m.jobs = m.visibleJobsForLoadedMsg(msg.jobs)
		m.launchLiveByID = msg.launchLiveByID
		m.launchStatusByID = msg.launchStatusByID
		m.placingJobIDs = msg.placingJobIDs
		m.placementQueuedAtByJob = msg.placementQueuedAtByJob
		m.placementStatusByJob = msg.placementStatusByJob
		m.launchByID = msg.launchByID
		m.launchBootstrapP50 = msg.launchBootstrapP50
		m.launchBootstrapSamples = msg.launchBootstrapSamples
		m.launchStageETAByName = msg.launchStageETAByName
		m.launchStageEnteredAtByID = msg.launchStageEnteredAtByID
		m.recentFailedInstances = msg.recentFailedInstances
		m.lastInstanceRunningAt = msg.lastInstanceRunningAt
		m.placementDaemonStopped = msg.placementDaemonStopped
		m.hostInfoByName = msg.hostInfoByName
		m.cordonedHostsByName = msg.cordonedHostsByName
		m.overloadedHostsByName = msg.overloadedHostsByName
		m.autoPassPhase = msg.autoPassPhase
		m.autopilotPaused = msg.autopilotPaused
		m.autopilotPausedReason = msg.autopilotPausedReason
		m.pruneAutoBlockReasons()
		if m.countAutoPilotActionableQueuedJobs() == 0 {
			m.autoPersistentBlocked = ""
			m.autoPersistentBlockedN = 0
		}
		m.rebuildLayout()
		m.rebuildGroupedRows()
		m.clampCursor()
		m.adjustOffset()
		if len(m.jobs) > 0 && strings.HasPrefix(m.statusMessage, "No jobs") {
			m.statusMessage = ""
		}
		cmds := []tea.Cmd{}
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.hasActiveLaunchingSpinner() && !m.launchSpinnerRunning {
			m.launchSpinnerRunning = true
			cmds = append(cmds, m.launchSpinner.Tick)
		}
		return m, tea.Batch(cmds...)

	case listSyncFinishedMsg:
		warnings := persistentListSyncWarnings(msg.warnings)
		if !msg.full {
			if len(warnings) > 0 {
				if !m.quickLaunchStatusProtected() {
					m.statusMessage = strings.Join(warnings, " | ")
				}
			} else {
				if !m.quickLaunchStatusProtected() {
					m.statusMessage = "Running full sync..."
				}
			}
			return m, tea.Batch(m.reloadJobs(), m.runBackgroundSync(true))
		}

		delete(m.pendingSyncHosts, backgroundSyncKey)
		if len(warnings) > 0 {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = strings.Join(warnings, " | ")
			}
		} else if len(m.jobs) == 0 {
			if !m.quickLaunchStatusProtected() {
				// The empty-state message renders in the body, not the status area.
				m.statusMessage = ""
			}
		} else {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = ""
			}
		}
		return m, m.reloadJobs()

	case listDBWatcherReadyMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
			return m, nil
		}
		m.dbWatcher = msg.source
		return m, m.waitForDBEvent()

	case listDBWatchEventMsg:
		cmds := []tea.Cmd{}
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
		} else {
			if !m.debounceActive {
				m.debounceActive = true
				cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
					return listDBRefreshTriggeredMsg{}
				}))
			} else {
				// Queue one trailing refresh so bursty DB writes don't leave stale rows.
				m.debouncePending = true
			}
		}
		if cmd := m.waitForDBEvent(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case listDBRefreshTriggeredMsg:
		cmds := []tea.Cmd{m.reloadJobs()}
		if m.debouncePending {
			m.debouncePending = false
			m.debounceActive = true
			cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
				return listDBRefreshTriggeredMsg{}
			}))
		} else {
			m.debounceActive = false
		}
		return m, tea.Batch(cmds...)

	case listSyncWorkerResultMsg:
		if msg.result.Host != "" {
			delete(m.pendingSyncHosts, msg.result.Host)
			if msg.result.HostFull != nil {
				if m.hostMetricsByName == nil {
					m.hostMetricsByName = map[string]*hostinfo.Host{}
				}
				m.hostMetricsByName[msg.result.Host] = msg.result.HostFull
			}
		}
		if msg.result.Error != nil {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = fmt.Sprintf("Sync error (%s): %v", msg.result.Host, msg.result.Error)
			}
		} else if msg.result.Updated > 0 || m.statusMessage == "Refreshing..." {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = ""
			}
		}
		return m, tea.Batch(
			m.reloadJobs(),
			m.requestGroupedMoveOptionsForActivePicker(),
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return listSyncWorkerResultMsg{result: r}
			}),
		)

	case listSyncTickMsg:
		m.nextSyncTickAt = time.Now().Add(throttledInterval(listTUISyncInterval, m.focused))
		cmds := []tea.Cmd{m.scheduleListSyncTick()}
		if cmd := m.ensureCurrentDaemonForTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.syncWorker != nil {
			m.requestActiveSyncs()
			cmds = append(cmds, m.reloadJobs())
			// The worker owns cloud result-marker sync; the TUI tick only needs
			// cloud instance reconciliation so it stays cheap and non-overlapping.
			cmds = m.enqueueBackgroundCloudSync(cmds, false)
		} else if m.syncEnabled && !m.syncInProgress() {
			m.pendingSyncHosts[backgroundSyncKey] = struct{}{}
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Refreshing..."
			}
			cmds = append(cmds, m.runBackgroundSync(true))
		}
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case listAutoPilotDoneMsg:
		if !msg.deferred {
			if remaining := listAutoPilotMinDisplayDuration - time.Since(m.autoPassStartedAt); remaining > 0 {
				held := msg
				held.deferred = true
				return m, tea.Tick(remaining, func(time.Time) tea.Msg { return held })
			}
		}
		m.autoInProgress = false
		if m.autopilotPaused {
			// Autopilot was disabled while this pass was in flight; drop
			// the result rather than acting on it.
			return m, nil
		}
		// Merge rather than replace: jobs temporarily absent from
		// msg.blockedReasons (because they just got a launch_id and
		// left the unplaced candidate list) keep their last known reason
		// until pruneAutoBlockReasons drops them once the job actually
		// starts running or reaches a terminal state.
		if len(msg.blockedReasons) > 0 {
			if m.autoBlockReasons == nil {
				m.autoBlockReasons = make(map[int64]string, len(msg.blockedReasons))
			}
			for jobID, reason := range msg.blockedReasons {
				m.autoBlockReasons[jobID] = reason
			}
		}
		if len(msg.structuredBlocked) > 0 {
			if m.autoBlockDetail == nil {
				m.autoBlockDetail = make(map[int64]*blockreason.Structured, len(msg.structuredBlocked))
			}
			for jobID, detail := range msg.structuredBlocked {
				m.autoBlockDetail[jobID] = detail
			}
		}
		if msg.err != nil {
			m.lastAutoPilotErrorRaw = msg.err.Error()
			m.showAutoPilotErrorDetails = false
			m.autoPersistentError = orchestration.SummarizeAutoPilotError(msg.err)
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot failed: " + orchestration.SummarizeAutoPilotError(msg.err)
			}
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownError)
			m.rebuildGroupedRows()
			return m, nil
		}
		m.lastAutoPilotErrorRaw = ""
		m.showAutoPilotErrorDetails = false
		m.autoPersistentError = ""
		if msg.paused {
			// The auto-pilot status line already reports the paused state
			// from autopilot_state (loaded in reloadJobs); don't duplicate
			// it in statusMessage.
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownContend)
			return m, nil
		}
		if msg.anotherHolding {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot: another runner is active"
			}
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownContend)
			return m, nil
		}
		m.autoPersistentBlocked = ""
		m.autoPersistentBlockedN = 0
		if summary := orchestration.AutoPilotBlockSummary(msg.blockedReasons); summary != "" {
			m.autoPersistentBlocked = summary
			m.autoPersistentBlockedN = len(msg.blockedReasons)
		}
		if msg.placed > 0 || msg.rebalanced > 0 || msg.launched > 0 {
			m.clearAutoPilotPersistentState()
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownProgress)
		} else if len(msg.blockedReasons) > 0 {
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownBlocked)
		} else {
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownIdle)
		}
		if !m.quickLaunchStatusProtected() {
			switch {
			case msg.placed > 0 && msg.rebalanced > 0 && msg.launched > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d, rebalanced %d, %s", msg.placed, msg.rebalanced, formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass))
			case msg.placed > 0 && msg.rebalanced > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d, rebalanced %d", msg.placed, msg.rebalanced)
			case msg.rebalanced > 0 && msg.launched > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: rebalanced %d, %s", msg.rebalanced, formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass))
			case msg.placed > 0 && msg.launched > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d, %s", msg.placed, formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass))
			case msg.placed > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d", msg.placed)
			case msg.rebalanced > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: rebalanced %d queued job(s).", msg.rebalanced)
			case msg.launched > 0:
				m.statusMessage = "Auto-pilot: " + formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass)
			case strings.HasPrefix(m.statusMessage, "Auto-pilot:"):
				m.statusMessage = ""
			}
		}
		return m, m.reloadJobs()

	case watchKillDoneMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Kill failed: %v", msg.err)
			return m, nil
		}
		if strings.TrimSpace(msg.message) != "" {
			m.statusMessage = msg.message
		} else {
			m.statusMessage = fmt.Sprintf("Job #%d killed", msg.jobID)
		}
		return m, m.reloadJobs()

	case watchCordonDoneMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Cordon failed: %v", msg.err)
			return m, nil
		}
		verb := "Uncordoned"
		if msg.cordoned {
			verb = "Cordoned"
		}
		m.statusMessage = fmt.Sprintf("%s %s", verb, msg.targetLabel)
		return m, m.reloadJobs()

	case watchUnplaceDoneMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Unplace failed: %v", msg.err)
			return m, nil
		}
		if strings.TrimSpace(msg.message) != "" {
			m.statusMessage = msg.message
		} else if msg.job != nil {
			m.statusMessage = fmt.Sprintf("Job #%d unplaced", msg.job.ID)
		}
		return m, m.reloadJobs()

	case watchProcessDoneMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Processed tag update failed: %v", msg.err)
			return m, nil
		}
		if strings.TrimSpace(msg.message) != "" {
			m.statusMessage = msg.message
		} else {
			m.statusMessage = fmt.Sprintf("Job #%d marked as processed", msg.jobID)
		}
		return m, m.reloadJobs()

	case moveOptionsReadyMsg:
		// Legacy path; grouped list uses listMoveOptionsReadyMsg with request IDs.
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Move: %v", msg.err)
			return m, nil
		}
		m.movePicker = movePickerModel{
			active:  true,
			jobID:   msg.jobID,
			options: msg.options,
			cursor:  firstEligibleMoveOption(msg.options),
		}
		return m, nil

	case listMoveOptionsReadyMsg:
		if !m.acceptListMoveOptionsResult(msg) {
			return m, nil
		}
		if msg.newOnly {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
			if !m.movePicker.active || m.movePicker.jobID != msg.jobID || m.movePicker.requestID != msg.requestID {
				return m, nil
			}
			if msg.err != nil {
				m.movePicker.loadingNew = false
				existingCount, newCount, disabledCount := countMoveOptions(m.movePicker.options)
				m.movePicker.status = fmt.Sprintf("%s Cloud offers failed: %v", movePickerStatus(existingCount, newCount, disabledCount, false), msg.err)
				m.statusMessage = "Move: cloud offer lookup failed"
				return m, nil
			}
			m.movePicker.addNewOptions(msg.options)
			existingCount, newCount, disabledCount := countMoveOptions(m.movePicker.options)
			m.movePicker.status = movePickerStatus(existingCount, newCount, disabledCount, false)
			m.statusMessage = m.movePicker.status
			return m, nil
		}
		if !msg.loadingNew {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
		}

		selected := m.selectedGroupedJob()
		if selected == nil || selected.ID != msg.jobID {
			if !m.movePicker.active || m.movePicker.jobID != msg.jobID || m.movePicker.requestID != msg.requestID {
				m.statusMessage = "Move lookup discarded (selection changed)"
				return m, nil
			}
		}
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Move: %v", msg.err)
			return m, nil
		}
		loadingNew := msg.loadingNew
		if m.movePicker.active && m.movePicker.requestID == msg.requestID && m.movePicker.jobID == msg.jobID {
			loadingNew = msg.loadingNew && m.movePicker.loadingNew
			m.movePicker.setExistingOptions(msg.options)
			m.movePicker.loadingNew = loadingNew
			m.movePicker.existingDone = true
		} else {
			m.movePicker = movePickerModel{
				active:       true,
				jobID:        msg.jobID,
				requestID:    msg.requestID,
				options:      msg.options,
				cursor:       firstEligibleMoveOption(msg.options),
				loadingNew:   loadingNew,
				loadingStart: m.movePicker.loadingStart,
				loadingFrame: m.movePicker.loadingFrame,
				existingDone: true,
			}
		}
		existingCount, newCount, disabledCount := countMoveOptions(m.movePicker.options)
		m.movePicker.status = movePickerStatus(existingCount, newCount, disabledCount, loadingNew)
		m.requestMovePickerStaleHostSyncs(msg.options)
		return m, nil

	case moveExecuteDoneMsg:
		if msg.err != nil {
			if msg.action == moveExecuteActionLaunchNew {
				m.statusMessage = fmt.Sprintf("Launch failed: %v", msg.err)
				return m, nil
			}
			m.statusMessage = fmt.Sprintf("Move failed: %v", msg.err)
			return m, nil
		}
		if msg.action == moveExecuteActionLaunchNew {
			m.statusMessage = fmt.Sprintf("Launched new instance %s for job #%d", msg.targetDesc, msg.jobID)
			return m, m.reloadJobs()
		}
		m.statusMessage = fmt.Sprintf("Moved job #%d to %s", msg.jobID, msg.targetDesc)
		return m, m.reloadJobs()

	case rebalancePreviewLoadedMsg:
		if !m.rebalancePreview.active || !m.rebalancePreview.loading {
			return m, nil
		}
		m.rebalancePreview.loading = false
		m.rebalancePreview.progressMessage = ""
		m.rebalanceProgress = nil
		if msg.err != nil {
			m.rebalancePreview.errMessage = msg.err.Error()
			m.statusMessage = "Rebalance preview failed"
			return m, nil
		}
		m.rebalancePreview.moves = msg.moves
		m.rebalancePreview.cursor = 0
		if len(msg.moves) == 0 {
			m.statusMessage = "No rebalance moves found."
		} else {
			m.statusMessage = fmt.Sprintf("Previewing %d rebalance move(s); %d over budget", len(msg.moves), countOverBudgetRebalanceMoves(msg.moves))
		}
		return m, nil

	case rebalanceAppliedMsg:
		if !m.rebalancePreview.active || !m.rebalancePreview.applying {
			return m, nil
		}
		m.rebalanceProgress = nil
		m.rebalancePreview.reset()
		m.resumeAutoPilotNow()
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Rebalance failed: %v", msg.err)
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Rebalanced %d job(s); %d over budget", msg.count, msg.overBudget)
		cmds := []tea.Cmd{m.reloadJobs()}
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case listQuickLaunchProgressMsg:
		if strings.TrimSpace(msg.message) != "" {
			m.statusMessage = msg.message
		}
		return m, m.waitForQuickLaunchProgress()

	case rebalanceProgressMsg:
		if strings.TrimSpace(msg.message) != "" {
			m.rebalancePreview.progressMessage = msg.message
		}
		return m, m.waitForRebalanceProgress()

	case listQuickLaunchDoneMsg:
		m.quickLaunching = false
		m.quickLaunchProgress = nil
		m.quickLaunchDone = nil
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Launch failed: %v", msg.err)
			m.quickLaunchStatusHoldUntil = time.Now().Add(10 * time.Second)
			return m, nil
		}
		if len(msg.instanceIDs) == 0 {
			m.statusMessage = "No new instance launched."
			m.quickLaunchStatusHoldUntil = time.Now().Add(10 * time.Second)
			return m, nil
		}
		status := fmt.Sprintf("Instance %s launched", ids.FormatInstanceID(msg.instanceIDs[0]))
		if msg.runningJobID > 0 {
			status = fmt.Sprintf("Instance %s: job #%d running", ids.FormatInstanceID(msg.instanceIDs[0]), msg.runningJobID)
		}
		if msg.movedJobs > 0 {
			status = fmt.Sprintf("%s; moved %d queued job(s)", status, msg.movedJobs)
		}
		if strings.TrimSpace(msg.warning) != "" {
			status = fmt.Sprintf("%s (%s)", status, msg.warning)
		}
		m.statusMessage = status
		m.quickLaunchStatusHoldUntil = time.Now().Add(10 * time.Second)
		return m, m.reloadJobs()

	case listDaemonRestartedMsg:
		m.daemonRestartInProgress = false
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Daemon restart failed: %v", msg.err)
			return m, nil
		}
		if msg.action == daemoncontrol.EnsureNoop {
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Daemon restarted (PID %d)", msg.pid)
		return m, nil

	case listURLOpenedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Open browser failed: %v", msg.err)
			return m, nil
		}
		m.statusMessage = "Opened Vast.ai billing"
		return m, nil

	case listProjectCandidatesLoadedMsg:
		m.projectCandidatesLoading = false
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Project filter: %v", msg.err)
			return m, nil
		}
		m.projectCandidates = msg.projects
		m.clampProjectCandidateCursor()
		return m, nil
	}

	return m, nil
}

func (m listTUIModel) visibleJobsForLoadedMsg(jobs []*db.Job) []*db.Job {
	if !m.unprocessedView || !m.isStatusGroupedView() {
		return jobs
	}
	return excludeEffectiveStatus(jobs, db.StatusCanceled)
}

func excludeEffectiveStatus(jobs []*db.Job, status string) []*db.Job {
	if len(jobs) == 0 {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.EffectiveStatus() == status {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

func (m listTUIModel) quickLaunchStatusPinned() bool {
	return !m.quickLaunchStatusHoldUntil.IsZero() && time.Now().Before(m.quickLaunchStatusHoldUntil)
}

func (m listTUIModel) quickLaunchStatusProtected() bool {
	return m.quickLaunching || m.quickLaunchStatusPinned()
}

func (m listTUIModel) waitForQuickLaunchProgress() tea.Cmd {
	ch := m.quickLaunchProgress
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m listTUIModel) waitForQuickLaunchDone() tea.Cmd {
	ch := m.quickLaunchDone
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m listTUIModel) View() string {
	if m.rebalancePreview.active {
		return m.rebalancePreview.View(m.width, m.height)
	}
	if m.movePicker.active {
		return m.movePicker.ViewOver(m.width, m.height, m.baseView())
	}
	return m.baseView()
}

func (m listTUIModel) baseView() string {
	if m.aiAssist != nil {
		return m.renderAIAssistOverlay()
	}
	if m.showHelp {
		return m.renderListHelpView()
	}
	if m.showInstanceFailures {
		return m.renderInstanceFailuresView()
	}
	if m.isGroupedView() {
		return m.groupedView()
	}

	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	layout := m.layout
	var b strings.Builder
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width, m.autoRunRateTargetCents)
	selectedDetailLines := m.selectedJobDetailLines()
	if m.hideStatusArea {
		sharedStatusLines = nil
		selectedDetailLines = nil
	}

	mateRows, matesActive := hostMatesForFlatView(m.jobs, m.cursor)
	rowWidth := m.width

	title := fmt.Sprintf("%s (%d)", m.displayTitle(), len(m.jobs))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")
	header := truncateDisplayWidth(formatJobListHeader(layout), rowWidth)
	b.WriteString(listTUIHeaderStyle.Render(truncateDisplayWidth(header, m.width)))
	b.WriteString("\n")

	footerBlockLines := len(sharedStatusLines) + len(selectedDetailLines) + 1
	if m.projectInputActive {
		footerBlockLines++
	}
	bodyRows := max(0, m.height-2-1-footerBlockLines)
	bodyLinesWritten := 0
	if len(m.jobs) == 0 {
		b.WriteString(listTUIEmptyStyle.Render(truncateDisplayWidth(m.emptyStateText(), m.width)))
		b.WriteString("\n")
		bodyLinesWritten = 1
		for i := 1; i < bodyRows; i++ {
			b.WriteString("\n")
			bodyLinesWritten++
		}
	} else {
		for i := 0; i < bodyRows; i++ {
			idx := m.offset + i
			if idx >= len(m.jobs) {
				b.WriteString("\n")
				bodyLinesWritten++
				continue
			}
			row := truncateDisplayWidth(formatJobListRow(layout, m.jobs[idx]), rowWidth)
			if matesActive && (idx == m.cursor || mateRows[idx]) {
				row = applyHostMateMarkerForJob(row, m.jobs[idx])
			}
			if idx == m.cursor {
				row = renderSelectedRow(row, m.width)
			}
			b.WriteString(row)
			b.WriteString("\n")
			bodyLinesWritten++
		}
	}
	for bodyLinesWritten < bodyRows {
		b.WriteString("\n")
		bodyLinesWritten++
	}

	// Always keep a visible separator above the status/footer block.
	b.WriteString("\n")
	if m.projectInputActive {
		for _, line := range m.projectFilterPromptLines() {
			b.WriteString(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)))
			b.WriteString("\n")
		}
	}
	for _, line := range selectedDetailLines {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(m.footerText(bodyRows), m.width)))
	return b.String()
}

func (m listTUIModel) displayTitle() string {
	title := m.title
	if m.projectFilter != "" && !strings.Contains(title, "project="+m.projectFilter) {
		title = strings.TrimSpace(title + " • project=" + m.projectFilter)
	}
	switch m.statusView {
	case db.StatusQueued:
		title = strings.TrimSpace(title + " • queued")
	case db.StatusDraft:
		title = strings.TrimSpace(title + " • draft")
	}
	if m.unprocessedView {
		return strings.TrimSpace(title + " • unprocessed")
	}
	return title
}

func (m listTUIModel) groupedView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	var b strings.Builder
	title := fmt.Sprintf("%s (%d) • group:%s", m.displayTitle(), len(m.jobs), listGroupModeLabel(m.effectiveGroupMode()))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")

	groupedJobs := m.groupedJobsWithAutoReasons()
	rows := m.groupedRows

	layout := m.buildGroupedViewLayout(rows, groupedJobs)
	selectedRow := m.selectedGroupedRow()
	mateJobs, matesActive := hostMatesForGroupedView(m.selectedGroupedJob(), m.jobs)
	rowWidth := m.width
	bodyLinesWritten := 0
	if len(rows) == 0 {
		// Empty view: the message goes in the body — under the title, separated
		// by a blank line — where jobs would otherwise be listed.
		b.WriteString("\n")
		b.WriteString(listTUIEmptyStyle.Render(truncateDisplayWidth(m.emptyStateBaseText(), m.width)))
		b.WriteString("\n")
		bodyLinesWritten = 2
	}
	for _, row := range layout.visibleRows {
		line := truncateDisplayWidth(row.text, rowWidth)
		if matesActive && row.rowIdx >= 0 && row.rowIdx < len(m.groupedRows) {
			if rj := m.groupedRows[row.rowIdx].job; rj != nil && mateJobs[rj.ID] {
				line = applyHostMateMarkerForJob(line, rj)
			}
		}
		if selectedRow >= 0 && row.rowIdx >= 0 && row.rowIdx == selectedRow {
			if matesActive {
				line = applyHostMateMarkerForJob(line, m.groupedRows[row.rowIdx].job)
			}
			line = renderSelectedRow(line, m.width)
		}
		b.WriteString(line)
		b.WriteString("\n")
		bodyLinesWritten++
	}
	for bodyLinesWritten < layout.maxBodyLines {
		b.WriteString("\n")
		bodyLinesWritten++
	}

	// Visually separate grouped job rows from footer lines.
	b.WriteString("\n")
	if m.projectInputActive {
		for _, line := range m.projectFilterPromptLines() {
			b.WriteString(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)))
			b.WriteString("\n")
		}
	}
	for _, line := range layout.errorDetailsLines {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	for _, line := range layout.selectedDetails {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	if layout.statusLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(layout.statusLine, m.width)))
		b.WriteString("\n")
	}
	for _, line := range layout.instanceHealthLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, line := range layout.sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if layout.autoPilotLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(layout.autoPilotLine, m.width)))
		b.WriteString("\n")
	}
	for _, line := range layout.budgetPanelLines {
		b.WriteString(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(layout.controlsLine, m.width)))
	return b.String()
}

func (m listTUIModel) buildGroupedViewLayout(rows []groupedStatusRow, groupedJobs []*db.Job) groupedViewLayout {
	// Build footer lines first so we can reserve space for them.
	// Per-job ETA now lives on the "Job:" line in selectedDetailLines, so
	// no standalone campaign-wide ETA line is rendered.
	eta := computeGroupedETA(groupedJobs, m.launchLiveByID, time.Now())
	statusLine := m.groupedStatusText()
	visibleRunning := countVisibleRunningJobs(groupedJobs)
	sharedStatus := renderSharedTUIStatusLinesView(m.database, m.width, visibleRunning, m.autoRunRateTargetCents, m.isUJGroupedView())
	autoPilotLine := m.groupedAutoPilotStatusText(visibleRunning)
	errorDetailsLines := m.groupedErrorDetailsLines()
	selectedDetailLines := m.selectedJobDetailLines()
	instanceHealthLines := buildInstanceHealthFooter(m.recentFailedInstances, m.width, time.Now(), m.lastInstanceRunningAt).lines
	if m.hideStatusArea {
		statusLine = ""
		sharedStatus.lines = nil
		sharedStatus.daemonLineIndex = -1
		sharedStatus.daemonActionable = false
		autoPilotLine = ""
		errorDetailsLines = nil
		selectedDetailLines = nil
		instanceHealthLines = nil
	}
	baseFooterLines := 2 // blank separator + controls
	if m.projectInputActive {
		baseFooterLines += len(m.projectFilterPromptLines())
	}
	if statusLine != "" {
		baseFooterLines++
	}
	baseFooterLines += len(sharedStatus.lines)
	baseFooterLines += len(selectedDetailLines)
	baseFooterLines += len(instanceHealthLines)
	if autoPilotLine != "" {
		baseFooterLines++
	}
	var budgetPanelLines []string
	if m.autoRunRateInputActive {
		budgetPanelLines = renderAutoBudgetPanel(m.currentBudgetState())
		baseFooterLines += len(budgetPanelLines)
	}
	availableForBodyAndDetails := max(0, m.height-1-baseFooterLines)
	if len(errorDetailsLines) > 0 {
		maxDetailLines := availableForBodyAndDetails
		if len(rows) > 0 && maxDetailLines > 0 {
			maxDetailLines--
		}
		if len(errorDetailsLines) > maxDetailLines {
			errorDetailsLines = truncateErrorDetailsLines(errorDetailsLines, maxDetailLines)
		}
	}
	footerLines := baseFooterLines + len(errorDetailsLines)
	maxBodyLines := max(0, m.height-1-footerLines) // 1 for title
	daemonStatusY := -1
	vastCreditWarningY := -1
	sharedStatusBaseY := 1 + maxBodyLines + 1
	if m.projectInputActive {
		sharedStatusBaseY += len(m.projectFilterPromptLines())
	}
	sharedStatusBaseY += len(errorDetailsLines)
	sharedStatusBaseY += len(selectedDetailLines)
	if statusLine != "" {
		sharedStatusBaseY++
	}
	sharedStatusBaseY += len(instanceHealthLines)
	if sharedStatus.daemonActionable && sharedStatus.daemonLineIndex >= 0 {
		daemonStatusY = sharedStatusBaseY + sharedStatus.daemonLineIndex
	}
	if sharedStatus.vastCreditWarningActionable && sharedStatus.vastCreditWarningLineIndex >= 0 {
		vastCreditWarningY = sharedStatusBaseY + sharedStatus.vastCreditWarningLineIndex
	}
	return groupedViewLayout{
		visibleRows:                 selectGroupedRowsForViewport(rows, maxBodyLines, m.selectedGroupedRow()),
		statusLine:                  statusLine,
		sharedStatusLines:           sharedStatus.lines,
		autoPilotLine:               autoPilotLine,
		errorDetailsLines:           errorDetailsLines,
		instanceHealthLines:         instanceHealthLines,
		selectedDetails:             selectedDetailLines,
		budgetPanelLines:            budgetPanelLines,
		controlsLine:                m.groupedControlsText(eta.HasQueued),
		maxBodyLines:                maxBodyLines,
		daemonStatusY:               daemonStatusY,
		daemonActionable:            sharedStatus.daemonActionable,
		vastCreditWarningY:          vastCreditWarningY,
		vastCreditWarningActionable: sharedStatus.vastCreditWarningActionable,
	}
}

// hasRecentInstanceFailures reports whether the `f` diagnose overlay has any
// content to show — abnormal terminations exist within the recent window.
func (m listTUIModel) hasRecentInstanceFailures() bool {
	return m.isStatusGroupedView() && summarizeRecentFailedInstances(m.recentFailedInstances, time.Now()).present
}

// instanceFailuresLines renders the full grouped-by-outcome failure section
// (the same body as the plain `weft jobs list` section) for the diagnose
// overlay, trimming the trailing blank separator.
func (m listTUIModel) instanceFailuresLines() []string {
	rows := appendRecentFailedInstanceRows(nil, m.recentFailedInstances, 0, m.width, time.Now())
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, truncateDisplayWidth(r.text, m.width))
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (m listTUIModel) instanceFailuresBodyHeight() int {
	return max(1, m.height-2) // title + footer
}

func (m listTUIModel) instanceFailuresMaxScroll() int {
	return max(0, len(m.instanceFailuresLines())-m.instanceFailuresBodyHeight())
}

// renderInstanceFailuresView is the read-only `f` diagnose overlay: a scrollable
// view of recent instance failures grouped by the fate of the jobs they carried,
// with a clear esc/q return to the list — the richer counterpart to the
// one-line footer.
func (m listTUIModel) renderInstanceFailuresView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}
	lines := m.instanceFailuresLines()
	bodyHeight := m.instanceFailuresBodyHeight()
	maxScroll := max(0, len(lines)-bodyHeight)
	scroll := min(m.instanceFailuresScroll, maxScroll)
	end := min(len(lines), scroll+bodyHeight)

	var b strings.Builder
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth("Instance failures — recent abnormal terminations", m.width)))
	b.WriteString("\n")
	shown := 0
	for _, ln := range lines[scroll:end] {
		b.WriteString(ln)
		b.WriteString("\n")
		shown++
	}
	for ; shown < bodyHeight; shown++ {
		b.WriteString("\n")
	}
	foot := "esc/q back · ↑/↓ scroll"
	if maxScroll > 0 {
		foot += fmt.Sprintf("  ·  %d–%d of %d", scroll+1, end, len(lines))
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(foot, m.width)))
	return b.String()
}

func (m listTUIModel) syncInProgress() bool {
	return len(m.pendingSyncHosts) > 0
}

func (m listTUIModel) pendingHostList() []string {
	hosts := make([]string, 0, len(m.pendingSyncHosts))
	for h := range m.pendingSyncHosts {
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts
}

func (m listTUIModel) groupedStatusText() string {
	var parts []string
	status := orchestration.NormalizeStatusLineText(m.statusMessage)
	if status != "" &&
		!strings.HasPrefix(status, "Auto-pilot") &&
		status != "Auto-pilot ON" &&
		status != "Auto-pilot OFF" {
		parts = append(parts, status)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (m *listTUIModel) clearAutoPilotPersistentState() {
	m.autoPersistentError = ""
	m.autoPersistentBlocked = ""
	m.autoPersistentBlockedN = 0
}

func (m *listTUIModel) resumeAutoPilotNow() {
	m.autoNextPassAt = time.Time{}
}

func (m listTUIModel) hasOpenPlacementIntent(jobID int64) bool {
	status, ok := m.placementStatusByJob[jobID]
	if !ok {
		return false
	}
	return status.HasOpenIntent || (status.Move != nil && status.Move.State == db.MoveIntentStateOpen)
}

func (m listTUIModel) isAutopilotVisibleUnplaced(job *db.Job) bool {
	return job != nil && job.IsUnplacedAwaitingPlacement() && !m.hasOpenPlacementIntent(job.ID)
}

func (m listTUIModel) countUnplacedQueuedJobs() int {
	n := 0
	for _, job := range m.jobs {
		if m.isAutopilotVisibleUnplaced(job) {
			n++
		}
	}
	return n
}

func (m listTUIModel) countAutoPilotActionableQueuedJobs() int {
	n := 0
	for _, job := range m.jobs {
		if m.isAutopilotVisibleUnplaced(job) {
			n++
			continue
		}
		if job != nil && job.EffectiveStatus() == db.StatusQueued && job.IsRentalJob() && !m.hasOpenPlacementIntent(job.ID) {
			n++
		}
	}
	return n
}

func (m listTUIModel) groupedAutoPilotStatusText(visibleRunning int) string {
	// The list TUI has two blocked-reason sources: the persistent summary
	// from the last pass, and reasons hydrated onto the visible unplaced
	// jobs. Prefer the former (it carries a job count); fall back to the
	// latter. The shared formatter renders whichever the caller supplies.
	blockedSummary := strings.TrimSpace(m.autoPersistentBlocked)
	blockedJobs := m.autoPersistentBlockedN
	if blockedSummary == "" {
		if summary := orchestration.AutoPilotBlockSummary(m.visibleUnplacedBlockedReasons()); summary != "" {
			blockedSummary = summary
			blockedJobs = 0
		}
	}
	return autopilotStatusLine(autopilotDisplayInput{
		inputActive:     m.autoRunRateInputActive,
		paused:          m.autopilotPaused,
		pausedReason:    m.autopilotPausedReason,
		syncing:         m.syncInProgress(),
		syncHosts:       m.pendingHostList(),
		inFlight:        m.autoInProgress,
		passPhase:       m.autoPassPhase,
		passStartedAt:   m.autoPassStartedAt,
		persistentError: m.autoPersistentError,
		blockedSummary:  blockedSummary,
		blockedJobs:     blockedJobs,
		nextPassAt:      m.autoNextPassAt,
		nextSyncAt:      m.nextSyncTickAt,
		unplaced:        m.countUnplacedQueuedJobs(),
		running:         visibleRunning,
		targetCents:     m.autoRunRateTargetCents,
		suppressTarget:  true,
	})
}

func (m listTUIModel) groupedControlsText(hasQueued bool) string {
	if m.autoRunRateInputActive {
		// While the budget panel is open, the panel itself shows the
		// applicable keys; suppress the regular controls so they don't
		// muddle the picture.
		return ""
	}
	autoState := autopilotFooterState(m.autopilotPaused)
	// The group-mode indicator lives on the title line; the controls line is
	// keyboard shortcuts only.
	line := ""
	if m.isStatusGroupedView() {
		line = fmt.Sprintf("%s (%s)  ", listKeyGroupedAuto.footerToken(), autoState)
	}
	line += listKeyRefresh.footerToken()
	if m.selectedGroupedJob() != nil {
		line += "  " + listKeyAttempts.footerToken()
		line += "  " + listKeyKillCancel.footerToken()
		line += "  " + listKeyUnplace.footerToken()
		line += "  " + listKeyToggleProcessed.footerToken()
		if selected := m.selectedGroupedJob(); m.isStatusGroupedView() && selected != nil && selected.EffectiveStatus() == db.StatusQueued {
			line += "  " + listKeyMove.footerToken()
			line += "  " + listKeyLaunchSelected.footerToken()
		}
		if m.isStatusGroupedView() {
			line += "  " + listKeyPriority.footerToken()
		}
	}
	if m.isStatusGroupedView() && hasQueued {
		line += "  " + listKeyLaunchQueued.footerToken()
	}
	line += "  q:quit"
	if m.isStatusGroupedView() {
		line += "  " + listKeyRebalance.footerToken()
	}
	if m.isStatusGroupedView() && strings.TrimSpace(m.lastAutoPilotErrorRaw) != "" {
		if m.showAutoPilotErrorDetails {
			line += "  " + listKeyAutoHideError.footerToken()
		} else {
			line += "  " + listKeyAutoErrorDetails.footerToken()
		}
	}
	line += "  " + listKeyProjectFilter.footerToken()
	line += "  " + listKeyToggleStatusArea.footerToken()
	line += "  " + listKeyListView.footerToken()
	line += "  " + listKeyInstances.footerToken()
	line += "  ?:help"
	return line
}

func (m listTUIModel) selectedJobDetailLines() []string {
	var job *db.Job
	if m.isGroupedView() {
		job = m.selectedGroupedJob()
	} else if m.cursor >= 0 && m.cursor < len(m.jobs) {
		job = m.jobs[m.cursor]
	}
	if job == nil {
		if m.isGroupedView() {
			return renderSelectedLaunchDetail(m.selectedGroupedLaunch(), m.width, time.Now())
		}
		return nil
	}
	return renderSelectedJobDetail(job, selectedJobContext{
		launchLiveByID:        m.launchLiveByID,
		launchByID:            m.launchByID,
		hostMetricsByName:     m.hostMetricsByName,
		hostInfoByName:        m.hostInfoByName,
		cordonedHostsByName:   m.cordonedHostsByName,
		overloadedHostsByName: m.overloadedHostsByName,
		siblingJobs:           m.jobs,
		moveByJob:             moveDisplayMap(m.placementStatusByJob),
		openIntentJobIDs:      openIntentJobIDMap(m.placementStatusByJob),
		cloudConfigured:       len(m.cloudClients) > 0,
	}, time.Now())
}

func moveDisplayMap(statusByJob map[int64]jobview.PlacementStatus) map[int64]*jobview.MoveDisplay {
	if len(statusByJob) == 0 {
		return nil
	}
	out := make(map[int64]*jobview.MoveDisplay, len(statusByJob))
	for jobID, status := range statusByJob {
		if status.Move != nil {
			out[jobID] = status.Move
		}
	}
	return out
}

func openIntentJobIDMap(statusByJob map[int64]jobview.PlacementStatus) map[int64]struct{} {
	if len(statusByJob) == 0 {
		return nil
	}
	out := make(map[int64]struct{})
	for jobID, status := range statusByJob {
		if status.HasOpenIntent || (status.Move != nil && status.Move.State == db.MoveIntentStateOpen) {
			out[jobID] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m listTUIModel) selectedGroupedRow() int {
	if len(m.groupedSelectableRows) == 0 || m.cursor < 0 || m.cursor >= len(m.groupedSelectableRows) {
		return -1
	}
	return m.groupedSelectableRows[m.cursor]
}

// currentSelectedJob returns the job under the cursor in either grouped or
// ungrouped view, or nil if no row is selected.
func (m listTUIModel) currentSelectedJob() *db.Job {
	if m.isGroupedView() {
		return m.selectedGroupedJob()
	}
	if m.cursor >= 0 && m.cursor < len(m.jobs) {
		return m.jobs[m.cursor]
	}
	return nil
}

func listJobCanKillOrCancel(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusQueued, db.StatusDraft, db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return true
	default:
		return false
	}
}

func handleCordonToggle(m listTUIModel, job *db.Job) (tea.Model, tea.Cmd) {
	kind := db.JobTargetUnplaced
	if job != nil {
		kind = job.TargetKind()
	}
	if kind != db.JobTargetRentalInstance && kind != db.JobTargetInventoryHost {
		m.statusMessage = "Select a placed job to cordon its target"
		return m, nil
	}
	target := "host"
	if kind == db.JobTargetRentalInstance {
		target = "instance"
	}
	verb := "Cordoning"
	if m.cordonedTargets().IsCordoned(job) {
		verb = "Uncordoning"
	}
	m.clearAutoPilotPersistentState()
	m.statusMessage = fmt.Sprintf("%s %s %s...", verb, target, job.TargetDisplay())
	return m, requestWatchJobCordon(m.database, job, verb == "Cordoning")
}

func (m listTUIModel) selectedGroupedJob() *db.Job {
	row := m.selectedGroupedRow()
	if row < 0 || row >= len(m.groupedRows) {
		return nil
	}
	return m.groupedRows[row].job
}

func (m listTUIModel) selectedGroupedLaunch() *db.Launch {
	row := m.selectedGroupedRow()
	if row < 0 || row >= len(m.groupedRows) {
		return nil
	}
	return m.groupedRows[row].launch
}

func (m listTUIModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.rebalancePreview.active || m.movePicker.active || m.aiAssist != nil || m.showHelp || m.showInstanceFailures || m.autoRunRateInputActive {
		return m, nil
	}
	if m.isGroupedView() {
		if cmd := m.handleGroupedStatusClick(msg.Y); cmd != nil {
			return m, cmd
		}
		m.selectGroupedMouseRow(msg.Y)
		return m, nil
	}
	if cmd := m.handleFlatStatusClick(msg.Y); cmd != nil {
		return m, cmd
	}
	m.selectFlatMouseRow(msg.Y)
	return m, nil
}

func (m *listTUIModel) handleGroupedStatusClick(y int) tea.Cmd {
	if m.hideStatusArea {
		return nil
	}
	rows := m.groupedRows
	layout := m.buildGroupedViewLayout(rows, m.groupedJobsWithAutoReasons())
	if m.isUJGroupedView() && layout.daemonActionable && y == layout.daemonStatusY {
		if m.daemonRestartInProgress {
			return nil
		}
		m.daemonRestartInProgress = true
		m.statusMessage = "Restarting daemon..."
		return restartDaemonListCmd()
	}
	if layout.vastCreditWarningActionable && y == layout.vastCreditWarningY {
		m.statusMessage = "Opening Vast.ai billing..."
		return openURLListCmd(vastaiBillingURL)
	}
	return nil
}

func (m *listTUIModel) handleFlatStatusClick(y int) tea.Cmd {
	if m.hideStatusArea {
		return nil
	}
	warningY, actionable := m.flatVastCreditWarningY()
	if !actionable || y != warningY {
		return nil
	}
	m.statusMessage = "Opening Vast.ai billing..."
	return openURLListCmd(vastaiBillingURL)
}

func (m listTUIModel) flatVastCreditWarningY() (int, bool) {
	sharedStatus := renderSharedTUIStatusLinesView(m.database, m.width, -1, m.autoRunRateTargetCents, false)
	if !sharedStatus.vastCreditWarningActionable || sharedStatus.vastCreditWarningLineIndex < 0 {
		return -1, false
	}
	y := 2 + m.flatBodyRows() + 1
	if m.projectInputActive {
		y += len(m.projectFilterPromptLines())
	}
	y += len(m.selectedJobDetailLines())
	return y + sharedStatus.vastCreditWarningLineIndex, true
}

func (m listTUIModel) isUJGroupedView() bool {
	return m.groupedByStatus && m.groupedUnprocessedView
}

func (m *listTUIModel) selectFlatMouseRow(y int) {
	if y < 2 {
		return
	}
	row := y - 2
	if row >= m.flatBodyRows() {
		return
	}
	idx := m.offset + row
	if idx < 0 || idx >= len(m.jobs) {
		return
	}
	m.cursor = idx
	m.clampCursor()
	m.adjustOffset()
}

func (m listTUIModel) flatBodyRows() int {
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width, m.autoRunRateTargetCents)
	selectedDetailLines := m.selectedJobDetailLines()
	if m.hideStatusArea {
		sharedStatusLines = nil
		selectedDetailLines = nil
	}
	footerBlockLines := len(sharedStatusLines) + len(selectedDetailLines) + 1
	if m.projectInputActive {
		footerBlockLines += len(m.projectFilterPromptLines())
	}
	return max(0, m.height-2-1-footerBlockLines)
}

func (m *listTUIModel) selectGroupedMouseRow(y int) {
	if y < 1 {
		return
	}
	visibleRows := m.groupedViewportRows()
	bodyRow := y - 1
	if bodyRow < 0 || bodyRow >= len(visibleRows) {
		return
	}
	rowIdx := visibleRows[bodyRow].rowIdx
	if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
		return
	}
	row := m.groupedRows[rowIdx]
	if row.expandToggle == "" && ((row.job == nil && row.launch == nil) || row.isHeader || row.isBlocked) {
		return
	}
	for i, selectableRow := range m.groupedSelectableRows {
		if selectableRow == rowIdx {
			m.cursor = i
			m.clampGroupedCursor()
			return
		}
	}
}

func (m listTUIModel) groupedViewportRows() []groupedViewportLine {
	rows := m.groupedRows
	return m.buildGroupedViewLayout(rows, m.groupedJobsWithAutoReasons()).visibleRows
}

func (m listTUIModel) handleMovePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		if m.moveLookupPending {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
		}
		m.movePicker.reset()
		m.statusMessage = "Move lookup canceled"
		return m, nil
	case "up", "k":
		m.movePicker.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.movePicker.moveCursor(1)
		return m, nil
	case "enter":
		opt := m.movePicker.selectedOption()
		if opt == nil {
			return m, nil
		}
		jobID := m.movePicker.jobID
		selected := *opt
		if m.moveLookupPending {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
		}
		m.movePicker.reset()
		targetDesc := fmt.Sprintf("instance %s", ids.FormatInstanceID(selected.instanceID))
		if selected.kind == orchestration.OptionKindOnPrem {
			targetDesc = fmt.Sprintf("host %s", selected.host)
		} else if selected.isNew {
			targetDesc = fmt.Sprintf("new %s instance", selected.gpuName)
		}
		m.statusMessage = fmt.Sprintf("Moving job #%d to %s...", jobID, targetDesc)
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			return m, func() tea.Msg { return moveExecuteDoneMsg{jobID: jobID, err: cfgErr} }
		}
		cloudClients, clientsErr := buildCloudClients(cfg)
		if clientsErr != nil {
			return m, func() tea.Msg { return moveExecuteDoneMsg{jobID: jobID, err: clientsErr} }
		}
		r2Client, r2Err := buildR2Client(cfg)
		if r2Err != nil {
			return m, func() tea.Msg { return moveExecuteDoneMsg{jobID: jobID, err: r2Err} }
		}
		return m, requestMoveExecute(m.ctx, m.database, r2Client, cfg, cloudClients, jobID, selected)
	}
	return m, nil
}

func (m listTUIModel) handleRebalancePreviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "n", "q":
		if m.rebalancePreview.applying {
			return m, nil
		}
		m.rebalancePreview.reset()
		m.resumeAutoPilotNow()
		cmds := []tea.Cmd{}
		if cmd := m.runAutoPilot(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	case "up", "k":
		if !m.rebalancePreview.loading && !m.rebalancePreview.applying {
			m.rebalancePreview.moveCursor(-1)
		}
		return m, nil
	case "down", "j":
		if !m.rebalancePreview.loading && !m.rebalancePreview.applying {
			m.rebalancePreview.moveCursor(1)
		}
		return m, nil
	case "y":
		if m.rebalancePreview.loading || m.rebalancePreview.applying || len(m.rebalancePreview.moves) == 0 {
			return m, nil
		}
		m.rebalancePreview.applying = true
		m.rebalancePreview.progressMessage = ""
		progressCh := make(chan rebalanceProgressMsg, 16)
		m.rebalanceProgress = progressCh
		m.statusMessage = "Applying rebalance moves…"
		return m, tea.Batch(
			requestRebalanceApply(m.database, m.appConfig, progressCh),
			m.waitForRebalanceProgress(),
		)
	}
	return m, nil
}

func (m listTUIModel) handleGroupedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.autoRunRateInputActive {
		return m.handleAutoRunRateInputKey(msg)
	}
	if next, cmd, ok := handleListKeyBinding(m, msg.String(), listGroupedKeyBindings()); ok {
		return next, cmd
	}
	return m, nil
}

func (m listTUIModel) beginProjectFilterInput() (listTUIModel, tea.Cmd) {
	m.projectInputActive = true
	m.projectInputValue = m.projectFilter
	m.projectCandidates = nil
	m.projectCandidateCursor = 0
	m.projectCandidatesLoading = true
	m.statusMessage = "Enter project filter"
	return m, m.loadProjectFilterCandidates()
}

func (m listTUIModel) projectFilterPromptLines() []string {
	head := "project filter: " + m.projectInputValue
	if m.projectCandidatesLoading {
		return []string{head + "  loading..."}
	}
	matches := m.projectCandidateMatches()
	if strings.TrimSpace(m.projectInputValue) == "" {
		head += "  (empty clears)"
	}
	lines := []string{head}
	if len(matches) == 0 {
		if strings.TrimSpace(m.projectInputValue) != "" {
			lines = append(lines, "  no matching projects")
		}
		return lines
	}
	const maxProjectPromptMatches = 5
	start := 0
	if m.projectCandidateCursor >= maxProjectPromptMatches {
		start = m.projectCandidateCursor - maxProjectPromptMatches + 1
	}
	end := min(len(matches), start+maxProjectPromptMatches)
	for i := start; i < end; i++ {
		prefix := "  "
		if i == m.projectCandidateCursor {
			prefix = "> "
		}
		lines = append(lines, prefix+matches[i])
	}
	return lines
}

func (m listTUIModel) handleProjectFilterInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.projectInputActive = false
		m.projectInputValue = ""
		m.statusMessage = "Project filter unchanged"
		return m, nil
	case "up":
		m.projectCandidateCursor--
		m.clampProjectCandidateCursor()
		return m, nil
	case "down":
		m.projectCandidateCursor++
		m.clampProjectCandidateCursor()
		return m, nil
	case "enter":
		project := strings.TrimSpace(m.projectInputValue)
		matches := m.projectCandidateMatches()
		if project != "" {
			if len(matches) == 0 {
				m.statusMessage = "No matching project"
				return m, nil
			}
			m.clampProjectCandidateCursor()
			project = matches[m.projectCandidateCursor]
		}
		m.projectInputActive = false
		m.projectInputValue = ""
		m.projectFilter = project
		m.cursor = 0
		m.offset = 0
		if project == "" {
			m.statusMessage = "Project filter cleared"
		} else {
			m.statusMessage = "Project filter: " + project
		}
		return m, m.reloadJobs()
	case "backspace", "ctrl+h":
		if m.projectInputValue != "" {
			runes := []rune(m.projectInputValue)
			m.projectInputValue = string(runes[:len(runes)-1])
		}
		m.projectCandidateCursor = 0
		return m, nil
	}
	if len(msg.Runes) > 0 {
		m.projectInputValue += string(msg.Runes)
		m.projectCandidateCursor = 0
	}
	return m, nil
}

func (m listTUIModel) projectCandidateMatches() []string {
	query := strings.ToLower(strings.TrimSpace(m.projectInputValue))
	if query == "" {
		return append([]string(nil), m.projectCandidates...)
	}
	matches := make([]string, 0, len(m.projectCandidates))
	for _, project := range m.projectCandidates {
		if strings.Contains(strings.ToLower(project), query) {
			matches = append(matches, project)
		}
	}
	return matches
}

func (m *listTUIModel) clampProjectCandidateCursor() {
	matches := m.projectCandidateMatches()
	if len(matches) == 0 {
		m.projectCandidateCursor = 0
		return
	}
	if m.projectCandidateCursor < 0 {
		m.projectCandidateCursor = 0
	}
	if m.projectCandidateCursor >= len(matches) {
		m.projectCandidateCursor = len(matches) - 1
	}
}

func (m listTUIModel) loadProjectFilterCandidates() tea.Cmd {
	database := m.database
	args := append([]string(nil), m.args...)
	processedFilter := ""
	if m.unprocessedView {
		processedFilter = "unprocessed"
	}
	statusFilter := m.statusView
	return func() tea.Msg {
		jobs, err := collectJobsForListWithFilters(database, args, statusFilter, processedFilter, "")
		if err != nil {
			return listProjectCandidatesLoadedMsg{err: err}
		}
		return listProjectCandidatesLoadedMsg{projects: projectCandidatesFromJobs(jobs)}
	}
}

func projectCandidatesFromJobs(jobs []*db.Job) []string {
	seen := map[string]struct{}{}
	projects := make([]string, 0)
	for _, job := range jobs {
		project := strings.TrimSpace(projectGroupLabel(job))
		if project == "" || project == "(no project)" {
			continue
		}
		if _, ok := seen[project]; ok {
			continue
		}
		seen[project] = struct{}{}
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

func (m listTUIModel) beginSelectedLaunchNew() (tea.Model, tea.Cmd) {
	job := m.selectedGroupedJob()
	if job == nil {
		m.statusMessage = "Select a queued job row to launch"
		return m, nil
	}
	if job.EffectiveStatus() != db.StatusQueued {
		m.statusMessage = "Can only launch queued jobs"
		return m, nil
	}
	m.clearAutoPilotPersistentState()
	m.autoInProgress = false
	_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
	m.statusMessage = fmt.Sprintf("Launching new instance for job #%d...", job.ID)
	return m, m.requestSelectedLaunchNew(job.ID)
}

func (m listTUIModel) toggleSelectedGroupedPriority() (tea.Model, tea.Cmd) {
	job := m.selectedGroupedJob()
	if job == nil {
		return m, nil
	}
	nextPriority := 1
	if job.Priority > 0 {
		nextPriority = 0
	}
	if err := db.SetJobPriority(m.database, job.ID, nextPriority); err != nil {
		m.statusMessage = fmt.Sprintf("Failed to update priority for job #%d: %v", job.ID, err)
		return m, nil
	}
	job.Priority = nextPriority
	if nextPriority > 0 && job.EffectiveStatus() == db.StatusQueued && job.Host != "" {
		_ = db.SetQueuedAtBefore(m.database, job.ID, job.Host)
	}
	if nextPriority > 0 {
		m.statusMessage = fmt.Sprintf("Job #%d marked priority", job.ID)
	} else {
		m.statusMessage = fmt.Sprintf("Job #%d priority cleared", job.ID)
	}
	return m, m.reloadJobs()
}

func (m listTUIModel) beginGroupedMove() (tea.Model, tea.Cmd) {
	job := m.selectedGroupedJob()
	if job == nil {
		return m, nil
	}
	status := job.EffectiveStatus()
	switch status {
	case db.StatusQueued, db.StatusPendingPlacement:
		// Standard move; no force semantics.
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		// Force-move: source attempt is atomically superseded inside the
		// launch; on success, the source process is terminated.
		m.statusMessage = fmt.Sprintf("Force-moving running job #%d — source will be killed on success", job.ID)
	default:
		m.statusMessage = fmt.Sprintf("Move not available for job in status %s", status)
		return m, nil
	}
	m.clearAutoPilotPersistentState()
	m.moveLookupSeq++
	reqID := m.moveLookupSeq
	m.moveLookupPending = true
	m.moveLookupRequestID = reqID
	m.moveLookupJobID = job.ID
	m.movePicker = movePickerModel{
		active:       true,
		jobID:        job.ID,
		requestID:    reqID,
		cursor:       -1,
		loadingNew:   true,
		loadingStart: time.Now(),
		status:       "Searching move destinations...",
		existingDone: false,
	}
	m.statusMessage = m.movePicker.status + " (Esc to cancel)"
	cmds := []tea.Cmd{
		scheduleMoveLookupTick(reqID),
		m.requestGroupedMoveOptions(reqID, job.ID),
		m.requestGroupedMoveNewOptions(reqID, job.ID),
	}
	return m, tea.Batch(cmds...)
}

func (m listTUIModel) requestSelectedLaunchNew(jobID int64) tea.Cmd {
	ctx := m.ctx
	database := m.database
	cfg := m.appConfig
	cachedCloudClients := append([]cloud.Client(nil), m.cloudClients...)
	return func() tea.Msg {
		if ctx == nil {
			ctx = context.Background()
		}
		if cfg == nil {
			var cfgErr error
			cfg, cfgErr = config.Load()
			if cfgErr != nil {
				return moveExecuteDoneMsg{jobID: jobID, action: moveExecuteActionLaunchNew, err: fmt.Errorf("load config: %w", cfgErr)}
			}
		}
		cloudClients := cachedCloudClients
		if len(cloudClients) == 0 {
			var clientsErr error
			cloudClients, clientsErr = buildCloudClients(cfg)
			if clientsErr != nil {
				return moveExecuteDoneMsg{jobID: jobID, action: moveExecuteActionLaunchNew, err: fmt.Errorf("build cloud clients: %w", clientsErr)}
			}
		}
		r2Client, r2Err := buildR2Client(cfg)
		if r2Err != nil {
			return moveExecuteDoneMsg{jobID: jobID, action: moveExecuteActionLaunchNew, err: fmt.Errorf("create R2 client: %w", r2Err)}
		}
		return requestLaunchNewForJob(ctx, database, r2Client, cfg, cloudClients, jobID)()
	}
}

func (m listTUIModel) requestMovePickerStaleHostSyncs(options []moveOption) {
	if m.syncWorker == nil {
		return
	}
	for _, opt := range options {
		if opt.kind != orchestration.OptionKindOnPrem || strings.TrimSpace(opt.host) == "" {
			continue
		}
		if strings.TrimSpace(opt.reason) != "host state stale" {
			continue
		}
		m.syncWorker.Request(hostsync.Request{
			Host:     opt.host,
			Rate:     hostsync.RateWarmup,
			Priority: true,
		})
	}
}

func (m listTUIModel) currentBudgetState() autoBudgetState {
	return autoBudgetState{
		Active:      m.autoRunRateInputActive,
		Phase:       m.autoRunRateInputPhase,
		Value:       m.autoRunRateInputValue,
		HourlyCents: m.autoRunRateTargetCents,
		DailyCents:  m.autoDailyCapCents,
	}
}

func (m *listTUIModel) applyBudgetState(s autoBudgetState) {
	m.autoRunRateInputActive = s.Active
	m.autoRunRateInputPhase = s.Phase
	m.autoRunRateInputValue = s.Value
	m.autoRunRateTargetCents = s.HourlyCents
	m.autoDailyCapCents = s.DailyCents
}

func (m listTUIModel) beginAutoRunRateInput() (tea.Model, tea.Cmd) {
	m.applyBudgetState(beginAutoBudget(m.autoRunRateTargetCents, m.autoDailyCapCents))
	return m, nil
}

func (m listTUIModel) handleAutoRunRateInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, eff := handleAutoBudgetKey(m.currentBudgetState(), msg, m.database)
	m.applyBudgetState(next)
	if eff.StatusText != "" {
		m.statusMessage = eff.StatusText
	}
	if eff.BreakerReset {
		m.clearAutoPilotPersistentState()
		m.autoBlockReasons = nil
		m.rebuildGroupedRows()
	}
	if eff.RetriggerPilot {
		m.resumeAutoPilotNow()
		if cmd := m.runAutoPilot(); cmd != nil {
			return m, cmd
		}
	}
	return m, nil
}

func (m listTUIModel) requestGroupedMoveOptions(requestID int64, jobID int64) tea.Cmd {
	return m.requestGroupedMoveOptionsWithCloudLoading(requestID, jobID, true)
}

func (m listTUIModel) requestGroupedMoveOptionsForActivePicker() tea.Cmd {
	if !m.movePicker.active || m.movePicker.jobID == 0 {
		return nil
	}
	return m.requestGroupedMoveOptionsWithCloudLoading(m.movePicker.requestID, m.movePicker.jobID, false)
}

func (m listTUIModel) requestGroupedMoveOptionsWithCloudLoading(requestID int64, jobID int64, loadingNew bool) tea.Cmd {
	database := m.database
	appCfg := m.appConfig
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)}
		}
		if job == nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("job %s not found", ids.FormatJobID(jobID))}
		}

		cfg := appCfg
		if cfg == nil {
			var cfgErr error
			cfg, cfgErr = config.Load()
			if cfgErr != nil {
				return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("load config: %w", cfgErr)}
			}
		}
		launches, err := db.ListRunningLaunches(database)
		if err != nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("list running launches: %w", err)}
		}
		capacities := make([]campaign.InstanceCapacity, 0, len(launches))
		queuedCounts := make(map[int64]int, len(launches))
		for _, ci := range launches {
			liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
			if jobsErr != nil {
				continue
			}
			if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(liveJobs)); ok {
				capacities = append(capacities, cap)
			}
			for _, j := range liveJobs {
				if j != nil && j.EffectiveStatus() == db.StatusQueued {
					queuedCounts[ci.ID]++
				}
			}
		}
		sourceInstanceID := int64(0)
		if job.LaunchID != nil {
			sourceInstanceID = *job.LaunchID
		}
		msg := requestMoveOptions(requestID, database, cfg, loadingNew, job, capacities, queuedCounts, sourceInstanceID)()
		ready, ok := msg.(moveOptionsReadyMsg)
		if !ok {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("unexpected move-options response")}
		}
		return listMoveOptionsReadyMsg{
			requestID:  requestID,
			jobID:      ready.jobID,
			options:    ready.options,
			err:        ready.err,
			loadingNew: ready.loadingNew,
		}
	}
}

func (m listTUIModel) acceptListMoveOptionsResult(msg listMoveOptionsReadyMsg) bool {
	if m.moveLookupPending {
		return msg.requestID == m.moveLookupRequestID
	}
	return m.movePicker.active && m.movePicker.requestID == msg.requestID && m.movePicker.jobID == msg.jobID
}

func (m listTUIModel) requestGroupedMoveNewOptions(requestID int64, jobID int64) tea.Cmd {
	appCfg := m.appConfig
	cachedCloudClients := append([]cloud.Client(nil), m.cloudClients...)
	return func() tea.Msg {
		job, err := db.GetJobByID(m.database, jobID)
		if err != nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err), newOnly: true}
		}
		if job == nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("job %s not found", ids.FormatJobID(jobID)), newOnly: true}
		}

		cfg := appCfg
		if cfg == nil {
			var cfgErr error
			cfg, cfgErr = config.Load()
			if cfgErr != nil {
				return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("load config: %w", cfgErr), newOnly: true}
			}
		}
		cloudClients := cachedCloudClients
		if len(cloudClients) == 0 {
			var clientsErr error
			cloudClients, clientsErr = buildCloudClients(cfg)
			if clientsErr != nil {
				return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("build cloud clients: %w", clientsErr), newOnly: true}
			}
		}
		options, err := orchestration.BuildNewOptionsWithSurvival(cloudClients, job, cfg.CampaignReliability(), nil, 0, nil)
		if err != nil {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: err, newOnly: true}
		}
		return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, options: moveOptionsFromOrchestration(options), newOnly: true}
	}
}

func requestRebalancePreview(database *sql.DB, cfg *config.Config, progress chan<- rebalanceProgressMsg) tea.Cmd {
	return func() tea.Msg {
		opts := orchestration.QueueRebalanceOptions{
			Apply:               false,
			CostCeilingOverride: 1e6,
			Operation:           "tui.rebalance",
			Progress:            makeRebalanceProgressFn(progress),
		}
		result, err := orchestration.RebalanceQueuedJobsAcrossTargets(context.Background(), database, cfg, opts)
		if progress != nil {
			close(progress)
		}
		if err != nil {
			return rebalancePreviewLoadedMsg{err: err}
		}
		return rebalancePreviewLoadedMsg{moves: result.Moves}
	}
}

func requestRebalanceApply(database *sql.DB, cfg *config.Config, progress chan<- rebalanceProgressMsg) tea.Cmd {
	return func() tea.Msg {
		opts := orchestration.QueueRebalanceOptions{
			Apply:               true,
			CostCeilingOverride: 1e6,
			Operation:           "tui.rebalance",
			Progress:            makeRebalanceProgressFn(progress),
		}
		result, err := orchestration.RebalanceQueuedJobsAcrossTargets(context.Background(), database, cfg, opts)
		if progress != nil {
			close(progress)
		}
		if err != nil {
			return rebalanceAppliedMsg{err: err}
		}
		return rebalanceAppliedMsg{
			count:      len(result.Moves),
			overBudget: countOverBudgetRebalanceMoves(result.Moves),
		}
	}
}

// makeRebalanceProgressFn returns a progress callback that non-blocking-sends
// to the given channel. Nil channel returns a nil callback.
func makeRebalanceProgressFn(ch chan<- rebalanceProgressMsg) func(string) {
	if ch == nil {
		return nil
	}
	return func(message string) {
		select {
		case ch <- rebalanceProgressMsg{message: message}:
		default:
			// Drop progress messages if the TUI hasn't drained the channel
			// yet — progress is best-effort, not ordered delivery.
		}
	}
}

func (m listTUIModel) waitForRebalanceProgress() tea.Cmd {
	ch := m.rebalanceProgress
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m listTUIModel) footerText(rows int) string {
	start := 0
	end := 0
	if len(m.jobs) > 0 {
		start = m.offset + 1
		end = min(len(m.jobs), m.offset+rows)
	}

	state := fmt.Sprintf("[%d-%d/%d]", start, end, len(m.jobs))
	if m.syncInProgress() {
		state += " syncing..."
	}
	if m.statusMessage != "" {
		state += "  " + m.statusMessage
	}
	state += "  up/down move  space/b page  g/G top/bottom"
	for _, binding := range []listKeyBinding{
		listKeyToggleQueuedDraft,
		listKeyToggleUnprocessed,
		listKeyToggleProcessed,
		listKeyKillCancel,
		listKeyToggleCordon,
		listKeyPriority,
		listKeyProjectFilter,
		listKeyToggleStatusArea,
		listKeyGroupedView,
		listKeyInstances,
		listKeyDashboard,
	} {
		state += "  " + binding.footerToken()
	}
	state += "  ? help  q quit"
	return state
}

func (m listTUIModel) renderListHelpView() string {
	viewBinding := listKeyGroupedView
	if m.isGroupedView() {
		viewBinding = listKeyListView
	}
	selectedJobLines := []string{
		"  a view attempts for selected job",
		"  c coding-assistant (progress / review / remediate, status-dependent)",
	}
	if m.isStatusGroupedView() {
		selectedJobLines = append(selectedJobLines,
			"  N launch selected on new instance",
			"  P toggle priority",
			"  x kill/cancel  C cordon target",
			"  u unplace selected",
			"  p toggle processed tag",
			"  m move selected",
		)
	} else if m.isGroupedView() {
		selectedJobLines = append(selectedJobLines,
			"  x kill/cancel  C cordon target",
			"  u unplace selected",
			"  p toggle processed tag",
		)
	} else {
		selectedJobLines = append(selectedJobLines,
			listKeyToggleProcessed.helpLine()+" tag",
			listKeyKillCancel.helpLine()+" selected job",
			listKeyToggleCordon.helpLine()+" selected job's target",
		)
	}

	sections := []keyHelpSection{
		{
			Title: "Navigation",
			Lines: []string{
				"  up/down (or j/k) move selection",
				"  pgup/pgdown (or b/space) page up/down",
				"  g/G jump top/bottom",
				"  S hide/show status area",
			},
		},
		{
			Title: "Views",
			Lines: []string{
				viewBinding.helpLine(),
				listKeyInstances.helpLine(),
				listKeyDashboard.helpLine(),
			},
		},
		{
			Title: "Selected job",
			Lines: selectedJobLines,
		},
		{
			Title: "General",
			Lines: []string{
				"  r refresh",
				"  q quit",
			},
		},
	}
	if !m.isGroupedView() {
		sections = append(sections,
			keyHelpSection{
				Title: "List filters",
				Lines: []string{
					"  d toggle queued/draft",
					"  U toggle unprocessed filter",
					"  / filter by project",
				},
			},
		)
	} else {
		sections = append(sections,
			keyHelpSection{
				Title: "List filters",
				Lines: []string{
					"  / filter by project",
				},
			},
		)
	}
	if m.isStatusGroupedView() {
		sections = append(sections,
			keyHelpSection{
				Title: "Queue and placement",
				Lines: []string{
					"  n launch queued jobs",
					"  R preview rebalance moves",
				},
			},
			keyHelpSection{
				Title: "Automation",
				Lines: []string{
					"  A toggle auto-pilot",
					"  $ set run-rate + daily cap",
					"    H/D clear; r resets breaker",
					"  e auto-pilot error details",
				},
			},
		)
	}
	return renderKeyHelp("Jobs List Keybindings", sections, m.width, m.height, listTUITitleStyle, listTUIFooterStyle)
}

func (m listTUIModel) triggerManualRefresh() (listTUIModel, tea.Cmd) {
	cmds := []tea.Cmd{m.reloadJobs()}
	if !m.quickLaunchStatusProtected() {
		m.statusMessage = "Refreshing..."
	}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
		cmds = m.enqueueBackgroundCloudSync(cmds, true)
	} else if m.syncEnabled && !m.syncInProgress() {
		m.pendingSyncHosts[backgroundSyncKey] = struct{}{}
		cmds = append(cmds, m.runBackgroundSync(true))
	}
	m.resumeAutoPilotNow()
	if cmd := m.runAutoPilot(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m listTUIModel) emptyStateText() string {
	if m.statusMessage != "" && !m.syncInProgress() {
		return "No jobs in this view. " + m.statusMessage
	}
	return m.emptyStateBaseText()
}

// emptyStateBaseText is the sync-aware empty-view message without the
// statusMessage variant. The grouped view uses it directly because its footer
// already carries the status and auto-pilot lines.
func (m listTUIModel) emptyStateBaseText() string {
	if m.syncInProgress() {
		return "No jobs in this view yet. Waiting for startup sync and DB updates..."
	}
	return "No jobs match this view."
}

func (m *listTUIModel) rebuildLayout() {
	m.layout = newJobListLayoutWithCordon(max(20, m.width-2), m.jobs, nil, false, m.cordonedTargets())
}

func (m listTUIModel) cordonedTargets() cordonedTargets {
	out := cordonedTargets{hosts: m.cordonedHostsByName, overloadedHosts: m.overloadedHostsByName}
	if len(m.launchByID) > 0 {
		launches := map[int64]bool{}
		for id, l := range m.launchByID {
			if l != nil && l.Cordoned {
				launches[id] = true
			}
		}
		if len(launches) > 0 {
			out.launches = launches
		}
	}
	return out
}

func (m *listTUIModel) rebuildGroupedRows() {
	if !m.isGroupedView() {
		m.groupedRows = nil
		m.groupedSelectableRows = nil
		return
	}
	if m.isStatusGroupedView() {
		groupedJobs := m.groupedJobsWithAutoReasons()
		m.groupedRows = buildGroupedStatusRowsWithOptions(groupedJobs, m.width, groupedStatusRenderOptions{
			launchLiveByID:         m.launchLiveByID,
			launchStatusByID:       m.launchStatusByID,
			placingJobIDs:          m.placingJobIDs,
			placementQueuedAtByJob: m.placementQueuedAtByJob,
			placementStatusByJob:   m.placementStatusByJob,
			overloadedHostsByName:  m.overloadedHostsByName,
			failedInstances:        m.recentFailedInstances,
			launchByID:             m.launchByID,
			now:                    time.Now(),
			launchSpinner:          m.launchSpinner.View(),
			launchingETA: groupedStatusLaunchingETA{
				totalP50:               m.launchBootstrapP50,
				totalSamples:           m.launchBootstrapSamples,
				stageByName:            m.launchStageETAByName,
				stageEnteredAtByLaunch: m.launchStageEnteredAtByID,
			},
			blockedDetail:   m.effectiveBlockedDetail(),
			expandedBlocked: m.expandedBlocked,
			interactive:     true,
			daemonStopped:   m.placementDaemonStopped,
		})
	} else {
		m.groupedRows = buildListGroupedRows(m.jobs, m.effectiveGroupMode(), m.width, m.layout)
	}
	m.groupedSelectableRows = m.groupedSelectableRows[:0]
	for i, row := range m.groupedRows {
		if row.expandToggle != "" || ((row.job != nil || row.launch != nil) && !row.isHeader && !row.isBlocked) {
			m.groupedSelectableRows = append(m.groupedSelectableRows, i)
		}
	}
	m.clampGroupedCursor()
}

func (m listTUIModel) hasActiveLaunchingSpinner() bool {
	if !m.isStatusGroupedView() {
		return false
	}
	for _, job := range m.groupedJobsWithAutoReasons() {
		if groupedStatusBucketWithOptions(job, m.launchesWithActiveJob(), jobview.LaunchesEverReady(m.launchByID), groupedStatusRenderOptions{
			launchStatusByID:     m.launchStatusByID,
			placingJobIDs:        m.placingJobIDs,
			placementStatusByJob: m.placementStatusByJob,
			now:                  time.Now(),
		}) != string(jobview.BucketLaunching) {
			continue
		}
		live := launchLiveStateForLaunchingJob(job, m.launchLiveByID)
		if live != nil && strings.EqualFold(strings.TrimSpace(live.BootstrapStage), "ready") {
			continue
		}
		return true
	}
	return false
}

func (m listTUIModel) launchesWithActiveJob() map[int64]bool {
	return computeLaunchesWithActiveJob(m.groupedJobsWithAutoReasons(), m.launchLiveByID)
}

func placementDisplayMaps(statusByJob map[int64]jobview.PlacementStatus) (map[int64]struct{}, map[int64]int64) {
	placingJobIDs := make(map[int64]struct{})
	placementQueuedAtByJob := make(map[int64]int64)
	for jobID, ps := range statusByJob {
		if ps.HasOpenIntent {
			placingJobIDs[jobID] = struct{}{}
		}
		if ps.DisplayAt > 0 {
			placementQueuedAtByJob[jobID] = ps.DisplayAt
		}
	}
	return placingJobIDs, placementQueuedAtByJob
}

func (m *listTUIModel) clampCursor() {
	if m.isGroupedView() {
		m.clampGroupedCursor()
		return
	}
	if len(m.jobs) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.jobs) {
		m.cursor = len(m.jobs) - 1
	}
}

func (m *listTUIModel) clampGroupedCursor() {
	if len(m.groupedSelectableRows) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.groupedSelectableRows) {
		m.cursor = len(m.groupedSelectableRows) - 1
	}
}

func (m *listTUIModel) adjustOffset() {
	if m.isGroupedView() {
		return
	}
	pageSize := m.pageSize()
	if pageSize <= 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+pageSize {
		m.offset = m.cursor - pageSize + 1
	}
	maxOffset := max(0, len(m.jobs)-pageSize)
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m listTUIModel) pageSize() int {
	if m.height <= 0 {
		return 10
	}
	if m.isGroupedView() {
		return max(1, len(m.groupedViewportRows()))
	}
	return max(1, m.flatBodyRows())
}

func (m listTUIModel) reloadJobs() tea.Cmd {
	database := m.database
	args := append([]string(nil), m.args...)
	processedFilter := ""
	if m.unprocessedView {
		processedFilter = "unprocessed"
	}
	statusFilter := m.statusView
	projectFilter := m.projectFilter
	return func() tea.Msg {
		jobs, err := collectJobsForListWithFilters(database, args, statusFilter, processedFilter, projectFilter)
		if err != nil {
			return listJobsLoadedMsg{jobs: jobs, err: err}
		}
		orchestration.HydrateUnplacedBlockedReasons(database, jobs)
		launchIDs := make([]int64, 0, len(jobs))
		seen := make(map[int64]bool, len(jobs))
		for _, job := range jobs {
			if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
				continue
			}
			if seen[*job.LaunchID] {
				continue
			}
			seen[*job.LaunchID] = true
			launchIDs = append(launchIDs, *job.LaunchID)
		}
		launchLiveByID, liveErr := db.GetLaunchLiveStates(database, launchIDs)
		if liveErr != nil {
			launchLiveByID = map[int64]*db.LaunchLiveState{}
		}
		launchStatusByID, statusErr := db.GetLaunchStatuses(database, launchIDs)
		if statusErr != nil {
			launchStatusByID = map[int64]string{}
		}
		launchByID, launchErr := db.GetLaunchesByIDs(database, launchIDs)
		if launchErr != nil {
			launchByID = map[int64]*db.Launch{}
		}
		bootstrapP50, bootstrapSamples, bootstrapErr := db.TotalBootstrapPercentile(database, 0.5)
		if bootstrapErr != nil {
			bootstrapP50 = 0
			bootstrapSamples = 0
		}
		stageETAByName, stageEnteredAtByID := loadLaunchingStageETA(database, jobs, launchLiveByID)
		placementStatusByJob, placementErr := jobview.PlacementStatusForJobs(database, jobs, time.Now())
		if placementErr != nil {
			placementStatusByJob = map[int64]jobview.PlacementStatus{}
		}
		placingJobIDs, placementQueuedAtByJob := placementDisplayMaps(placementStatusByJob)
		hostInfoByName := loadInventoryHostInfo(database, jobs)
		cordonedHostsByName := loadCordonedHostsByName(database, jobs)
		overloadedHostsByName := loadOverloadedHostsByName(database, jobs)
		autopilotPaused, autopilotPausedReason := autopilotPauseState(database)
		// 0 on error keeps clusters red (fail toward showing the alert).
		lastInstanceRunningAt, _ := db.LatestInstanceRunningAt(database)
		return listJobsLoadedMsg{
			jobs:                     jobs,
			launchLiveByID:           launchLiveByID,
			launchStatusByID:         launchStatusByID,
			placingJobIDs:            placingJobIDs,
			placementQueuedAtByJob:   placementQueuedAtByJob,
			placementStatusByJob:     placementStatusByJob,
			launchByID:               launchByID,
			launchBootstrapP50:       bootstrapP50,
			launchBootstrapSamples:   bootstrapSamples,
			launchStageETAByName:     stageETAByName,
			launchStageEnteredAtByID: stageEnteredAtByID,
			recentFailedInstances:    loadRecentFailedInstances(database, recentFailedInstanceWindow, time.Now()),
			lastInstanceRunningAt:    lastInstanceRunningAt,
			placementDaemonStopped:   placementDaemonStopped(),
			hostInfoByName:           hostInfoByName,
			cordonedHostsByName:      cordonedHostsByName,
			overloadedHostsByName:    overloadedHostsByName,
			autoPassPhase:            loadLatestAutoPilotPhase(database),
			autopilotPaused:          autopilotPaused,
			autopilotPausedReason:    autopilotPausedReason,
		}
	}
}

const recentFailedInstanceWindow = 24 * time.Hour

func loadLaunchingStageETA(database *sql.DB, jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState) (map[string]groupedStatusLaunchingStageETA, map[int64]int64) {
	stageByName := make(map[string]groupedStatusLaunchingStageETA)
	enteredAtByLaunch := make(map[int64]int64)
	if database == nil || len(jobs) == 0 {
		return stageByName, enteredAtByLaunch
	}
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		live := launchLiveStateForLaunchingJob(job, launchLiveByID)
		if live == nil {
			continue
		}
		stage := strings.TrimSpace(live.BootstrapStage)
		if stage == "" {
			continue
		}
		if _, ok := stageByName[stage]; !ok {
			p50, samples, oldest, err := db.BootstrapStagePercentile(database, stage, 0.5)
			if err == nil {
				stageByName[stage] = groupedStatusLaunchingStageETA{
					p50:     p50,
					samples: samples,
					oldest:  oldest,
				}
			}
		}
		if _, ok := enteredAtByLaunch[*job.LaunchID]; ok {
			continue
		}
		enteredAt, err := db.LatestBootstrapStageEnteredAt(database, *job.LaunchID, stage)
		if err == nil && enteredAt > 0 {
			enteredAtByLaunch[*job.LaunchID] = enteredAt
		}
	}
	return stageByName, enteredAtByLaunch
}

func loadRecentFailedInstances(database *sql.DB, window time.Duration, now time.Time) *recentFailedInstances {
	if database == nil || window <= 0 {
		return nil
	}
	since := now.Add(-window)
	items, err := db.ListRecentAbnormalFailedInstances(database, since.Unix())
	if err != nil {
		slog.Warn("load recent failed instances", "component", "ui.list", "error", err)
		return nil
	}
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it != nil {
			ids = append(ids, it.ID)
		}
	}
	projects, err := db.ProjectsByLaunchIDs(database, ids)
	if err != nil {
		slog.Warn("load launch projects", "component", "ui.list", "error", err)
		projects = map[int64]string{}
	}
	outcomes, err := db.JobOutcomesByLaunchIDs(database, ids)
	if err != nil {
		slog.Warn("load launch job outcomes", "component", "ui.list", "error", err)
		outcomes = map[int64]db.LaunchJobOutcome{}
	}
	chainTerminals, err := db.LaunchChainTerminalStatuses(database, ids)
	if err != nil {
		slog.Warn("load launch chain terminals", "component", "ui.list", "error", err)
		chainTerminals = map[int64]string{}
	}
	return &recentFailedInstances{
		items:                   items,
		projectByLaunchID:       projects,
		jobOutcomeByLaunchID:    outcomes,
		chainTerminalByLaunchID: chainTerminals,
	}
}

// loadInventoryHostInfo returns CachedHostInfo keyed by host name, filtered
// to the distinct inventory hosts referenced by the given jobs. One DB
// query (LoadAllCachedHosts) regardless of host count. Rental hosts are
// skipped — they don't populate host_info_cache. Errors return an empty
// map; callers fall back to the bare Host: <name> form.
func loadInventoryHostInfo(database *sql.DB, jobs []*db.Job) map[string]*db.CachedHostInfo {
	names := map[string]struct{}{}
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() {
			continue
		}
		host := strings.TrimSpace(job.Host)
		if host == "" {
			continue
		}
		names[host] = struct{}{}
	}
	if len(names) == 0 {
		return nil
	}
	all, err := db.LoadAllCachedHosts(database)
	if err != nil {
		return nil
	}
	out := make(map[string]*db.CachedHostInfo, len(names))
	for _, info := range all {
		if info == nil {
			continue
		}
		if _, want := names[info.Name]; want {
			out[info.Name] = info
		}
	}
	return out
}

func loadCordonedHostsByName(database *sql.DB, jobs []*db.Job) map[string]bool {
	want := map[string]struct{}{}
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() {
			continue
		}
		host := strings.TrimSpace(job.Host)
		if host != "" {
			want[host] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	// Targeted read — db.ListExecutionTargets runs SyncExecutionTargets
	// (multi-write upsert) and we're on the snapshot tick.
	placeholders := make([]string, 0, len(want))
	args := make([]any, 0, len(want)+1)
	args = append(args, db.ExecutionTargetInventoryHost)
	for h := range want {
		placeholders = append(placeholders, "?")
		args = append(args, h)
	}
	query := fmt.Sprintf(
		`SELECT host FROM execution_targets
		 WHERE kind = ? AND cordoned = 1 AND host IN (%s)`,
		strings.Join(placeholders, ","),
	)
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil
		}
		out[host] = true
	}
	return out
}

func loadOverloadedHostsByName(database *sql.DB, jobs []*db.Job) map[string]bool {
	want := map[string]struct{}{}
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() {
			continue
		}
		host := strings.TrimSpace(job.Host)
		if host != "" {
			want[host] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(want))
	for host := range want {
		hosts = append(hosts, host)
	}
	assessments := placement.OverloadedHostsFromRecent(database, hosts)
	if len(assessments) == 0 {
		return nil
	}
	out := make(map[string]bool, len(assessments))
	for host := range assessments {
		out[host] = true
	}
	return out
}

func (m listTUIModel) runBackgroundSync(full bool) tea.Cmd {
	return m.runBackgroundSyncFn(full, syncListTUIData)
}

// runBackgroundCloudSync runs only the cloud-side sync. Used when a
// hostsync.Worker is handling host-side syncing on its own tick, so the cloud
// reconciler (which performs orphan sweep + termination reconcile) still runs
// every list-TUI tick.
func (m listTUIModel) runBackgroundCloudSync(full bool) tea.Cmd {
	return m.runBackgroundSyncFn(full, syncCloudStateForTUI)
}

func (m *listTUIModel) enqueueBackgroundCloudSync(cmds []tea.Cmd, full bool) []tea.Cmd {
	if m.pendingSyncHosts == nil {
		m.pendingSyncHosts = make(map[string]struct{})
	}
	if _, ok := m.pendingSyncHosts[backgroundSyncKey]; ok {
		return cmds
	}
	m.pendingSyncHosts[backgroundSyncKey] = struct{}{}
	return append(cmds, m.runBackgroundCloudSync(full))
}

func (m listTUIModel) runBackgroundSyncFn(full bool, fn func(*sql.DB, bool) []string) tea.Cmd {
	database := m.database
	ctx := m.ctx
	return func() tea.Msg {
		done := make(chan listSyncFinishedMsg, 1)
		go func() {
			done <- listSyncFinishedMsg{warnings: fn(database, full), full: full}
		}()
		select {
		case msg := <-done:
			return msg
		case <-ctx.Done():
			return listSyncFinishedMsg{full: true} // terminal msg so no further syncs are triggered
		}
	}
}

func syncListTUIData(database *sql.DB, full bool) []string {
	timeout := FastSyncTimeout
	if full {
		timeout = NormalSyncTimeout
	}
	completed, unreachable, slow, warnings := performSyncWithTimeoutForHostsDetailedWithOptions(database, nil, timeout, false, full)
	if !completed {
		if note := buildStaleDataNote(database, unreachable, slow); note != "" {
			warnings = append(warnings, note)
		}
	}
	warnings = append(warnings, syncCloudStateForTUI(database, full)...)
	return compactWarnings(warnings)
}

func (m listTUIModel) requestActiveSyncs() {
	if m.syncWorker == nil {
		return
	}
	hosts := make(map[string][]*db.Job)
	for _, job := range m.jobs {
		if job != nil && job.Host != "" {
			hosts[job.Host] = append(hosts[job.Host], job)
		}
	}
	for host, jobs := range hosts {
		m.pendingSyncHosts[host] = struct{}{}
		m.syncWorker.Request(listHostSyncRequest(host, jobs))
	}
}

func listHostSyncRequest(host string, jobs []*db.Job) hostsync.Request {
	return hostsync.Request{
		Host: host,
		Rate: hostsync.GetHostSyncRate(jobs),
		Mode: hostsync.GetHostSyncMode(jobs),
	}
}

func (m listTUIModel) scheduleListSyncTick() tea.Cmd {
	return tea.Tick(throttledInterval(listTUISyncInterval, m.focused), func(time.Time) tea.Msg {
		return listSyncTickMsg{}
	})
}

func buildListAutoLeaseScope(title string) string {
	scope := strings.TrimSpace(title)
	if scope == "" {
		scope = "jobs"
	}
	return "list_grouped_status:" + scope
}

// toggleAutopilot flips the singleton autopilot enabled/disabled state. The
// A key is a control for the one global flag — there is no per-surface
// toggle — so it writes autopilot_state, which every surface then reflects.
func (m listTUIModel) toggleAutopilot() (tea.Model, tea.Cmd) {
	if m.database == nil {
		return m, nil
	}
	if m.autopilotPaused {
		if _, err := db.ResumeAutopilot(m.database); err != nil {
			m.statusMessage = "Auto-pilot: resume failed: " + err.Error()
			return m, nil
		}
		m.autopilotPaused = false
		m.autopilotPausedReason = ""
		m.clearAutoPilotPersistentState()
		m.resumeAutoPilotNow()
		m.statusMessage = "Auto-pilot enabled"
		return m, tea.Batch(m.runAutoPilot(), m.reloadJobs())
	}
	if _, err := db.PauseAutopilot(m.database, autopilotActor(), ""); err != nil {
		m.statusMessage = "Auto-pilot: pause failed: " + err.Error()
		return m, nil
	}
	m.autopilotPaused = true
	m.autoInProgress = false
	m.autoBlockReasons = nil
	m.clearAutoPilotPersistentState()
	_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
	m.statusMessage = "Auto-pilot paused"
	return m, m.reloadJobs()
}

func (m *listTUIModel) runAutoPilot() tea.Cmd {
	if !m.isStatusGroupedView() || m.autopilotPaused || m.autoInProgress || m.database == nil {
		return nil
	}
	if !m.autoNextPassAt.IsZero() && time.Now().Before(m.autoNextPassAt) {
		return nil
	}
	if m.countAutoPilotActionableQueuedJobs() == 0 {
		// Nothing to evaluate; skip the pass so the status line doesn't flicker.
		return nil
	}
	m.autoInProgress = true
	m.autoPassStartedAt = time.Now()

	database := m.database
	ctx := m.ctx
	owner := m.autoLeaseOwner
	scope := m.autoLeaseScope
	jobs := append([]*db.Job(nil), m.jobs...)

	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				oplog.Log("auto_pilot.panic", oplog.WithErrorStr(fmt.Sprintf("%v", r)))
				msg = listAutoPilotDoneMsg{err: fmt.Errorf("auto-pilot panic: %v", r)}
			}
		}()
		acquired, err := db.AcquireAutoLease(database, scope, owner, listAutoLeaseTTL)
		if err != nil {
			return listAutoPilotDoneMsg{err: err}
		}
		if !acquired {
			return listAutoPilotDoneMsg{anotherHolding: true}
		}
		defer db.ReleaseAutoLease(database, scope, owner)
		placed, rebalanced, launched, launchedClass, blockedReasons, structuredBlocked, runErr := runGatedAutoPilotPass(ctx, database, jobs, "list-tui")
		if runErr != nil {
			if errors.Is(runErr, orchestration.ErrAutopilotPaused) {
				return listAutoPilotDoneMsg{paused: true}
			}
			if errors.Is(runErr, orchestration.ErrAutopilotBusy) {
				return listAutoPilotDoneMsg{anotherHolding: true}
			}
		}
		return listAutoPilotDoneMsg{
			placed:            placed,
			rebalanced:        rebalanced,
			launched:          launched,
			launchedClass:     launchedClass,
			blockedReasons:    blockedReasons,
			structuredBlocked: structuredBlocked,
			err:               runErr,
		}
	}
}

// listAutoPilotMinDisplayDuration is the minimum time the "evaluating..."
// auto-pilot status line stays visible, even when the pass completes sooner.
// Prevents rapid flicker between "evaluating" and idle text on every sync tick.
const listAutoPilotMinDisplayDuration = 1200 * time.Millisecond

// Aliases for the shared autopilot cooldowns; preserves call sites in this
// file and the existing list_ui_test.go references.
const (
	listAutoPilotCooldownError    = orchestration.AutopilotCooldownError
	listAutoPilotCooldownBlocked  = orchestration.AutopilotCooldownBlocked
	listAutoPilotCooldownIdle     = orchestration.AutopilotCooldownIdle
	listAutoPilotCooldownProgress = orchestration.AutopilotCooldownProgress
	listAutoPilotCooldownContend  = orchestration.AutopilotCooldownContend
)

func runGroupedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job) (int, int, int, string, map[int64]string, error) {
	result, err := orchestration.RunGroupedAutoPilotPass(ctx, database, scopedJobs)
	if result != nil {
		return result.Placed, result.Rebalanced, result.Launched, result.LaunchedClass, result.BlockedReasons, err
	}
	if err != nil {
		return 0, 0, 0, "", nil, err
	}
	return 0, 0, 0, "", nil, nil
}

// runGatedAutoPilotPass wraps the orchestration pass with the singleton
// pause/active-runner gate. It returns ErrAutopilotPaused or ErrAutopilotBusy
// (via the runner package) without running a pass when those conditions hold.
func runGatedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job, label string) (int, int, int, string, map[int64]string, map[int64]*blockreason.Structured, error) {
	result, err := orchestration.RunGroupedAutoPilotPassGatedWithOptions(ctx, database, scopedJobs, label, orchestration.AutopilotRunnerOptions{
		AllowStaleBinary: true,
	})
	if result != nil {
		return result.Placed, result.Rebalanced, result.Launched, result.LaunchedClass, result.BlockedReasons, result.StructuredBlocked, err
	}
	if err != nil {
		return 0, 0, 0, "", nil, nil, err
	}
	return 0, 0, 0, "", nil, nil, nil
}

func launchedClassFromResult(database *sql.DB, instanceIDs []int64) string {
	if database == nil || len(instanceIDs) != 1 {
		return ""
	}
	launch, err := db.GetLaunch(database, instanceIDs[0])
	if err != nil || launch == nil {
		return ""
	}
	if label := strings.TrimSpace(launch.DisplayGPUBrief()); label != "" {
		return label
	}
	if label := strings.TrimSpace(launch.DisplayGPUSpec()); label != "" {
		return label
	}
	return ""
}

func formatAutoPilotLaunchedSummary(count int, launchedClass string) string {
	switch {
	case count <= 0:
		return "launched 0 instances"
	case count == 1:
		if strings.TrimSpace(launchedClass) != "" {
			return fmt.Sprintf("launched 1 %s instance", strings.TrimSpace(launchedClass))
		}
		return "launched 1 instance"
	default:
		return fmt.Sprintf("launched %d instances", count)
	}
}

func restartDaemonListCmd() tea.Cmd {
	return func() tea.Msg {
		status, action, err := listRestartDaemonFunc()
		return listDaemonRestartedMsg{pid: status.PID, action: action, err: err}
	}
}

func (m *listTUIModel) ensureCurrentDaemonForTick() tea.Cmd {
	if m.daemonRestartInProgress {
		return nil
	}
	status, err := daemoncontrol.CurrentStatus(daemonStatusPaths())
	if err != nil || !status.ActiveBinaryStale {
		return nil
	}
	m.daemonRestartInProgress = true
	m.statusMessage = "Restarting daemon..."
	return restartDaemonListCmd()
}

func openURLListCmd(url string) tea.Cmd {
	return func() tea.Msg {
		return listURLOpenedMsg{url: url, err: listOpenURLFunc(url)}
	}
}

func openURLForListTUI(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	default:
		return nil
	}
}

func restartDaemonForListTUI() (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
	return daemoncontrol.EnsureCurrent(daemoncontrol.DefaultPaths(), 2*time.Second)
}

func (m listTUIModel) startQuickLaunch(progress chan<- listQuickLaunchProgressMsg, done chan<- listQuickLaunchDoneMsg) tea.Cmd {
	database := m.database
	scopeOwner := m.autoLeaseOwner
	scope := m.quickLaunchScope
	jobs := append([]*db.Job(nil), m.jobs...)
	ctx := m.ctx

	return func() tea.Msg {
		go func() {
			defer close(progress)
			defer close(done)
			done <- runQuickLaunchWithProgress(ctx, database, scopeOwner, scope, jobs, progress)
		}()
		return nil
	}
}

func runQuickLaunchWithProgress(
	ctx context.Context,
	database *sql.DB,
	scopeOwner string,
	scope string,
	jobs []*db.Job,
	progress chan<- listQuickLaunchProgressMsg,
) listQuickLaunchDoneMsg {
	res, err := orchestration.RunQuickLaunch(ctx, database, scopeOwner, scope, jobs, func(message string) {
		select {
		case progress <- listQuickLaunchProgressMsg{message: message}:
		default:
		}
	})
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	return listQuickLaunchDoneMsg{
		instanceIDs: res.InstanceIDs,
		movedJobs:   res.MovedJobs,
		warning:     res.Warning,
	}
}

func formatQuickLaunchEventLine(event campaign.LaunchEvent) string {
	switch event.Kind {
	case campaign.LaunchEventCampaignStatus:
		return strings.TrimSpace(event.Phase)
	case campaign.LaunchEventGroupAssets:
		return fmt.Sprintf("%s: staging (%d/%d assets ready)", event.Group.GPUSpec(), event.AssetsReady, event.AssetsTotal)
	case campaign.LaunchEventGroupRetry:
		return fmt.Sprintf("%s: retrying with replacement offer (attempt %d/%d)", event.Group.GPUSpec(), event.RetryAttempt, event.RetryMax)
	case campaign.LaunchEventGroupReplan:
		return fmt.Sprintf("%s: replanning with fresh offer (chain %d/%d)", event.Group.GPUSpec(), event.RetryAttempt, event.RetryMax)
	case campaign.LaunchEventGroupPhase:
		if strings.TrimSpace(event.Phase) != "" {
			return fmt.Sprintf("%s: %s", event.Group.GPUSpec(), strings.TrimSpace(event.Phase))
		}
	}
	return ""
}

// effectiveBlockedDetail merges the freshest structured blocked breakdown for
// every unplaced job. The in-memory result of this session's last autopilot
// pass takes precedence; the persisted placement_blocked column covers jobs no
// local pass has seen yet (before the first pass, or when another sync path ran
// the pass).
func (m listTUIModel) effectiveBlockedDetail() map[int64]*blockreason.Structured {
	detail := make(map[int64]*blockreason.Structured)
	for _, job := range m.jobs {
		if !m.isAutopilotVisibleUnplaced(job) {
			continue
		}
		if d := m.autoBlockDetail[job.ID]; d != nil {
			detail[job.ID] = d
			continue
		}
		if d := blockreason.ForJob(job); d != nil && d.IsPlacementFailure() {
			detail[job.ID] = d
		}
	}
	return detail
}

func (m *listTUIModel) pruneAutoBlockReasons() {
	if len(m.autoBlockReasons) == 0 && len(m.autoBlockDetail) == 0 && len(m.expandedBlocked) == 0 {
		return
	}
	// visibleUnplaced: jobs that are actively unplaced and awaiting placement.
	// visibleBlockedReasons: jobs whose cached block reason should be retained.
	// This extends beyond visibleUnplaced to include jobs temporarily assigned
	// to a cloud instance (TargetKind == rental_instance) that haven't started
	// running yet, so reasons survive the transient window where the AP assigns
	// a launch_id but the job hasn't actually executed. Reasons are dropped once
	// the job starts running, gets an on-prem host, pauses, or reaches a terminal
	// state.
	visibleUnplaced := make(map[int64]struct{}, len(m.jobs))
	visibleBlockedReasons := make(map[int64]struct{}, len(m.jobs))
	for _, job := range m.jobs {
		if m.isAutopilotVisibleUnplaced(job) {
			visibleUnplaced[job.ID] = struct{}{}
			visibleBlockedReasons[job.ID] = struct{}{}
			continue
		}
		s := job.EffectiveStatus()
		if (s == db.StatusQueued || s == db.StatusPendingPlacement) &&
			job.TargetKind() == db.JobTargetRentalInstance {
			visibleBlockedReasons[job.ID] = struct{}{}
		}
	}
	for jobID := range m.autoBlockDetail {
		if _, ok := visibleUnplaced[jobID]; !ok {
			delete(m.autoBlockDetail, jobID)
		}
	}
	for jobID := range m.expandedBlocked {
		if _, ok := visibleUnplaced[jobID]; !ok {
			delete(m.expandedBlocked, jobID)
		}
	}
	if len(m.autoBlockReasons) == 0 {
		return
	}
	headroom, hasRunRateHeadroom := m.currentRunRateHeadroomCents()
	changed := false
	for jobID, reason := range m.autoBlockReasons {
		if _, ok := visibleBlockedReasons[jobID]; !ok {
			delete(m.autoBlockReasons, jobID)
			changed = true
			continue
		}
		if blockreason.IsReuseOnlyDiagnostic(reason) {
			delete(m.autoBlockReasons, jobID)
			changed = true
			continue
		}
		if hasRunRateHeadroom {
			if pruned, prunedChanged := pruneStaleRunRateBlockReason(reason, headroom); prunedChanged {
				if pruned == "" {
					delete(m.autoBlockReasons, jobID)
				} else {
					m.autoBlockReasons[jobID] = pruned
				}
				changed = true
			}
		}
	}
	if changed {
		m.autoPersistentBlocked = ""
		m.autoPersistentBlockedN = 0
		if summary := orchestration.AutoPilotBlockSummary(m.autoBlockReasons); summary != "" {
			m.autoPersistentBlocked = summary
			m.autoPersistentBlockedN = len(m.autoBlockReasons)
		}
	}
}

func (m listTUIModel) currentRunRateHeadroomCents() (int, bool) {
	return orchestration.RunRateHeadroom(m.database, m.autoRunRateTargetCents)
}

func staleRunRateBlockReason(reason string, headroomCents int) bool {
	return orchestration.IsStaleRunRateBlockReason(reason, headroomCents)
}

func pruneStaleRunRateBlockReason(reason string, headroomCents int) (string, bool) {
	return orchestration.PruneStaleRunRateBlockReason(reason, headroomCents)
}

func parseRunRateBlockedNeedCents(reason string) (int, bool) {
	return orchestration.ParseRunRateBlockedNeedCents(reason)
}

func normalizeStatusLineText(msg string) string {
	return orchestration.NormalizeStatusLineText(msg)
}

func (m listTUIModel) groupedJobsWithAutoReasons() []*db.Job {
	baseJobs := m.jobs
	if m.isStatusGroupedView() && m.groupedUnprocessedView {
		baseJobs = excludeJobsWithStatus(baseJobs, db.StatusCanceled)
	}

	blockReasons := m.visibleUnplacedBlockedReasonsForJobs(baseJobs)
	if len(blockReasons) == 0 {
		return baseJobs
	}
	decorated := make([]*db.Job, 0, len(baseJobs))
	for _, job := range baseJobs {
		if job == nil {
			decorated = append(decorated, nil)
			continue
		}
		reason, ok := blockReasons[job.ID]
		if !ok || strings.TrimSpace(reason) == "" || !m.isAutopilotVisibleUnplaced(job) {
			decorated = append(decorated, job)
			continue
		}
		copyJob := *job
		copyJob.QueueBlockedReason = reason
		decorated = append(decorated, &copyJob)
	}
	return decorated
}

func (m listTUIModel) visibleUnplacedBlockedReasons() map[int64]string {
	return m.visibleUnplacedBlockedReasonsForJobs(m.jobs)
}

func (m listTUIModel) visibleUnplacedBlockedReasonsForJobs(jobs []*db.Job) map[int64]string {
	reasons := make(map[int64]string)
	headroom, hasRunRateHeadroom := m.currentRunRateHeadroomCents()
	for _, job := range jobs {
		if !m.isAutopilotVisibleUnplaced(job) {
			continue
		}
		displayJob := jobWithDisplayPlacementReasons(job, headroom, hasRunRateHeadroom)
		result := blockreason.Resolve(displayJob, blockreason.Options{
			AutoPilotReason: m.autoBlockReasons[job.ID],
			Compact:         true,
		})
		if result.Blocked {
			reasons[job.ID] = result.Reason
		}
	}
	return reasons
}

func jobWithDisplayPlacementReasons(job *db.Job, headroomCents int, hasRunRateHeadroom bool) *db.Job {
	if job == nil || !hasRunRateHeadroom || len(job.PlacementReasons) == 0 {
		return job
	}
	reasons := make([]string, 0, len(job.PlacementReasons))
	changed := false
	for _, reason := range job.PlacementReasons {
		pruned, prunedChanged := pruneStaleRunRateBlockReason(reason, headroomCents)
		if prunedChanged {
			changed = true
			if strings.TrimSpace(pruned) == "" {
				continue
			}
			reasons = append(reasons, pruned)
			continue
		}
		reasons = append(reasons, reason)
	}
	if !changed {
		return job
	}
	copyJob := *job
	copyJob.PlacementReasons = reasons
	return &copyJob
}

func groupedStatusUnprocessedView(title string) bool {
	return listTitleHasPart(title, "unprocessed")
}

func listTitleHasPart(title, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, part := range strings.Split(strings.ToLower(title), "•") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func listTitleStatusView(title string) string {
	for _, part := range strings.Split(strings.ToLower(title), "•") {
		part = strings.TrimSpace(part)
		switch part {
		case "queued", "status=queued":
			return db.StatusQueued
		case "draft", "status=draft":
			return db.StatusDraft
		}
	}
	return ""
}

func listTitleWithoutPart(title, drop string) string {
	drop = strings.ToLower(strings.TrimSpace(drop))
	parts := strings.Split(title, "•")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)
		if part == "" || lower == drop || lower == "status="+drop {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return "Jobs"
	}
	return strings.Join(kept, " • ")
}

func persistentListSyncWarnings(warnings []string) []string {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if degraded.IsCloudSyncTimeoutWarning(warning) {
			continue
		}
		out = append(out, warning)
	}
	return out
}

func excludeJobsWithStatus(jobs []*db.Job, status string) []*db.Job {
	if len(jobs) == 0 {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.EffectiveStatus() == status {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

func (m listTUIModel) groupedErrorDetailsLines() []string {
	if !m.isStatusGroupedView() || !m.showAutoPilotErrorDetails {
		return nil
	}
	raw := strings.TrimSpace(m.lastAutoPilotErrorRaw)
	if raw == "" {
		return nil
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := []string{"Auto-pilot error details:"}
	lines = append(lines, strings.Split(raw, "\n")...)
	return lines
}

func truncateErrorDetailsLines(lines []string, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	if len(lines) <= maxLines {
		return lines
	}
	if maxLines == 1 {
		return []string{"…"}
	}
	truncated := append([]string(nil), lines[:maxLines-1]...)
	truncated = append(truncated, "…")
	return truncated
}

type groupedViewportLine struct {
	text   string
	rowIdx int // -1 means synthesized (ellipsis)
}

type groupedViewportSection struct {
	title          string
	headerRowIndex int
	itemCount      int
	rows           []groupedStatusRow
	shownRows      int
	ellipsis       bool
	summaryOnly    bool
}

func (s groupedViewportSection) abbreviated() bool {
	return s.ellipsis || s.summaryOnly
}

func parseGroupedViewportSections(rows []groupedStatusRow) []groupedViewportSection {
	sections := make([]groupedViewportSection, 0, 8)
	current := groupedViewportSection{}
	active := false
	for i, row := range rows {
		if row.isHeader {
			if active {
				current.shownRows = len(current.rows)
				sections = append(sections, current)
			}
			title := strings.TrimSpace(strings.TrimSuffix(row.text, ":"))
			if idx := strings.Index(title, " ("); idx > 0 {
				title = strings.TrimSpace(title[:idx])
			}
			current = groupedViewportSection{
				title:          title,
				headerRowIndex: i,
				rows:           make([]groupedStatusRow, 0, 8),
			}
			active = true
			continue
		}
		if !active || strings.TrimSpace(row.text) == "" {
			continue
		}
		current.rows = append(current.rows, row)
		if (row.job != nil || row.launch != nil) && !row.isBlocked {
			current.itemCount++
		}
	}
	if active {
		current.shownRows = len(current.rows)
		sections = append(sections, current)
	}
	return sections
}

// sectionFalloffPow controls how sharply non-cursor section budgets fall off
// with index distance from the cursor section. The allocation weight is
// 1/(1+d^sectionFalloffPow); a higher power concentrates rows nearer the cursor.
const sectionFalloffPow = 2

// absDistance returns |a-b|.
func absDistance(a, b int) int {
	if d := a - b; d < 0 {
		return -d
	} else {
		return d
	}
}

// sectionMinVisible is the 0-collapse floor for a section: it is either
// collapsed to its header (0 job rows) or shows at least this many job rows.
// Sections with 1-2 rows are shown whole (an ellipsis would save nothing);
// larger sections show at least 2 rows before a trailing "...".
func sectionMinVisible(total int) int {
	if total <= 2 {
		return total
	}
	return 2
}

// sectionAlloc describes one section's allocation request for
// allocateSectionBudgets.
type sectionAlloc struct {
	total      int // selectable/job rows available in the section
	minVisible int // 0-collapse floor (see sectionMinVisible)
}

// allocateSectionBudgets distributes available job-row lines across sections,
// biased toward the cursor's section so navigation always has room to reveal the
// selected row. Each returned count is either 0 (collapse to header) or in
// [minVisible, total]. Non-cursor sections are weighted by index distance from
// the cursor (1/(1+d^2)) so nearer sections keep rows while distant sections
// collapse first; a single tall nearby section is trimmed rather than collapsed.
func allocateSectionBudgets(secs []sectionAlloc, cursorIdx, available int) []int {
	n := len(secs)
	out := make([]int, n)
	if n == 0 {
		return out
	}
	if cursorIdx < 0 || cursorIdx >= n {
		cursorIdx = 0
	}
	if available < 0 {
		available = 0
	}
	// Reserve the cursor section's minimum first.
	cursorMin := min(secs[cursorIdx].minVisible, secs[cursorIdx].total)
	cursorMin = min(cursorMin, available)
	out[cursorIdx] = cursorMin
	remaining := available - cursorMin

	// Precompute each section's distance-falloff weight once (read repeatedly by
	// the sort comparators below).
	weights := make([]float64, n)
	for i := range secs {
		w := 1.0
		for p := 0; p < sectionFalloffPow; p++ {
			w *= float64(absDistance(i, cursorIdx))
		}
		weights[i] = 1.0 / (1.0 + w)
	}

	// Phase 1: fractional ideal per non-cursor section, proportional to weight.
	var wsum float64
	for i := range secs {
		if i == cursorIdx || secs[i].total == 0 {
			continue
		}
		wsum += weights[i]
	}
	ideal := make([]float64, n)
	for i := range secs {
		if i == cursorIdx || secs[i].total == 0 {
			continue
		}
		f := 0.0
		if wsum > 0 {
			f = float64(remaining) * weights[i] / wsum
		}
		ideal[i] = min(f, float64(secs[i].total))
	}

	// Phase 2: snap each fractional value to 0 or [minVisible, total].
	spent := 0
	for i := range secs {
		if i == cursorIdx || secs[i].total == 0 {
			continue
		}
		f := ideal[i]
		mv := secs[i].minVisible
		switch {
		case f < 1:
			out[i] = 0
		case f < float64(mv):
			if f < float64(mv)-f { // closer to 0 than to minVisible
				out[i] = 0
			} else {
				out[i] = mv
			}
		default:
			out[i] = min(int(f), secs[i].total)
		}
		spent += out[i]
	}

	// Phase 3: reconcile snap drift against the remaining budget.
	order := make([]int, 0, n)
	for i := range secs {
		if i != cursorIdx && secs[i].total > 0 {
			order = append(order, i)
		}
	}
	switch {
	case spent > remaining: // over budget: cut from furthest (lowest weight) first
		sort.SliceStable(order, func(a, b int) bool { return weights[order[a]] < weights[order[b]] })
		over := spent - remaining
		for _, i := range order {
			if over <= 0 {
				break
			}
			if out[i] == 0 {
				continue
			}
			mv := secs[i].minVisible
			if reducible := out[i] - mv; reducible > 0 {
				cut := min(reducible, over)
				out[i] -= cut
				over -= cut
			}
			if over > 0 && out[i] == mv {
				out[i] = 0
				over -= mv
			}
		}
	case spent < remaining: // under budget: add to nearest (highest weight) first
		sort.SliceStable(order, func(a, b int) bool { return weights[order[a]] > weights[order[b]] })
		under := remaining - spent
		for _, i := range order {
			if under <= 0 {
				break
			}
			if out[i] == 0 {
				mv := min(secs[i].minVisible, secs[i].total)
				if mv > 0 && mv <= under {
					out[i] = mv
					under -= mv
				}
				continue
			}
			if room := secs[i].total - out[i]; room > 0 {
				add := min(room, under)
				out[i] += add
				under -= add
			}
		}
	}

	// Hand any budget the non-cursor sections did not absorb to the cursor
	// section, so a lone or dominant section fills the available rows.
	used := 0
	for i, b := range out {
		if i != cursorIdx {
			used += b
		}
	}
	if extra := min(available-used, secs[cursorIdx].total); extra > out[cursorIdx] {
		out[cursorIdx] = extra
	}
	return out
}

// viewportContains reports whether any emitted line maps to rowIdx. A negative
// rowIdx (no selection) is vacuously contained.
func viewportContains(lines []groupedViewportLine, rowIdx int) bool {
	if rowIdx < 0 {
		return true
	}
	for _, l := range lines {
		if l.rowIdx == rowIdx {
			return true
		}
	}
	return false
}

// capViewportKeepingRow hard-truncates lines to maxLines while keeping the line
// for rowIdx on screen. Reached only when a single section is taller than the
// entire viewport.
func capViewportKeepingRow(lines []groupedViewportLine, maxLines, rowIdx int) []groupedViewportLine {
	if len(lines) <= maxLines {
		return lines
	}
	ci := -1
	for i, l := range lines {
		if l.rowIdx == rowIdx {
			ci = i
			break
		}
	}
	if ci < 0 {
		return lines[:maxLines]
	}
	start, end := visibleWindow(ci, len(lines), maxLines)
	return lines[start:end]
}

// selectGroupedRowsForViewport keeps grouped sections in order and abbreviates
// them to fit maxLines. When cursorRowIdx is a valid groupedRows index the fit is
// cursor-aware: the cursor's section is guaranteed room and a window anchored on
// the cursor, while distant sections collapse first (see allocateSectionBudgets).
// The returned viewport always contains cursorRowIdx, so keyboard navigation can
// never land on an unrendered row. When cursorRowIdx < 0 the legacy positional
// abbreviation (collapse the last section first) is used.
func selectGroupedRowsForViewport(rows []groupedStatusRow, maxLines, cursorRowIdx int) []groupedViewportLine {
	if maxLines <= 0 || len(rows) == 0 {
		return nil
	}
	type sectionState struct {
		groupedViewportSection
		rowIdxs         []int
		windowStart     int
		leadingEllipsis bool
	}
	parsed := parseGroupedViewportSections(rows)
	sections := make([]sectionState, 0, len(parsed))
	for _, section := range parsed {
		rowIdxs := make([]int, 0, len(rows))
		for i := range rows {
			if i <= section.headerRowIndex {
				continue
			}
			if rows[i].isHeader {
				break
			}
			if strings.TrimSpace(rows[i].text) == "" {
				continue
			}
			rowIdxs = append(rowIdxs, i)
		}
		sections = append(sections, sectionState{
			groupedViewportSection: section,
			rowIdxs:                rowIdxs,
		})
	}
	render := func() []groupedViewportLine {
		out := make([]groupedViewportLine, 0, maxLines)
		prevAbbreviated := false
		for idx := range sections {
			section := sections[idx]
			currAbbreviated := section.abbreviated() || section.leadingEllipsis
			// Collapsed (header-only) sections pack tightly: no blank above them.
			if idx > 0 && !(prevAbbreviated && currAbbreviated) && !section.summaryOnly {
				out = append(out, groupedViewportLine{text: "", rowIdx: -1})
			}
			if section.summaryOnly {
				out = append(out, groupedViewportLine{text: fmt.Sprintf("%s (%d)", section.title, section.itemCount), rowIdx: -1})
			} else {
				out = append(out, groupedViewportLine{text: rows[section.headerRowIndex].text, rowIdx: section.headerRowIndex})
				if section.leadingEllipsis {
					out = append(out, groupedViewportLine{text: "...", rowIdx: -1})
				}
				for i := 0; i < section.shownRows; i++ {
					ri := section.windowStart + i
					if ri < 0 || ri >= len(section.rows) || ri >= len(section.rowIdxs) {
						break
					}
					out = append(out, groupedViewportLine{text: section.rows[ri].text, rowIdx: section.rowIdxs[ri]})
				}
				if section.ellipsis {
					out = append(out, groupedViewportLine{text: "...", rowIdx: -1})
				}
			}
			prevAbbreviated = currAbbreviated
		}
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1].text) == "" {
			out = out[:len(out)-1]
		}
		return out
	}
	normalizeSectionWindow := func(s *sectionState) {
		total := len(s.rowIdxs)
		if total == 0 || s.summaryOnly {
			return
		}
		indicatorLines := 0
		if s.leadingEllipsis {
			indicatorLines++
		}
		if s.ellipsis {
			indicatorLines++
		}
		// If the ellipsis markers cost as many lines as the hidden rows, show
		// the rows instead.
		if s.shownRows+indicatorLines >= total {
			s.windowStart = 0
			s.leadingEllipsis = false
			s.ellipsis = false
			s.shownRows = total
		}
	}

	// Locate the cursor's section and its offset within that section.
	cursorSection, cursorOffset := -1, -1
	if cursorRowIdx >= 0 {
		for si := range sections {
			for oi, ri := range sections[si].rowIdxs {
				if ri == cursorRowIdx {
					cursorSection, cursorOffset = si, oi
					break
				}
			}
			if cursorSection >= 0 {
				break
			}
		}
	}

	if cursorSection < 0 {
		// Legacy positional abbreviation: collapse from the last section back.
		lines := render()
		for len(lines) > maxLines {
			changed := false
			for i := len(sections) - 1; i >= 0; i-- {
				s := &sections[i]
				total := len(s.rows)
				if s.summaryOnly || total == 0 {
					continue
				}
				switch {
				case s.ellipsis:
					if s.shownRows > 1 {
						s.shownRows--
						changed = true
					} else {
						s.summaryOnly = true
						s.ellipsis = false
						s.shownRows = 0
						changed = true
					}
				case total >= 3:
					s.ellipsis = true
					s.shownRows = total - 2
					changed = true
				default:
					s.summaryOnly = true
					s.shownRows = 0
					changed = true
				}
				if changed {
					break
				}
			}
			if !changed {
				break
			}
			lines = render()
		}
		if len(lines) > maxLines {
			lines = lines[:maxLines]
		}
		return lines
	}

	// Cursor-aware allocation: hand each section a job-row budget, then anchor a
	// window on the cursor within its section.
	allocs := make([]sectionAlloc, len(sections))
	for i := range sections {
		total := len(sections[i].rowIdxs)
		allocs[i] = sectionAlloc{total: total, minVisible: sectionMinVisible(total)}
	}
	budgets := allocateSectionBudgets(allocs, cursorSection, max(0, maxLines-len(sections)))
	for i := range sections {
		s := &sections[i]
		total := len(s.rowIdxs)
		s.windowStart = 0
		s.leadingEllipsis = false
		switch b := budgets[i]; {
		case total == 0:
			// A header-only section (e.g. the collapsed "Recent failed
			// instances" summary) has no rows to hide. Collapsing it would
			// only drop its blank separator and append a bogus "(0)", so it
			// is never summaryOnly: render the header verbatim.
			s.summaryOnly = false
			s.ellipsis = false
			s.shownRows = 0
		case b <= 0:
			s.summaryOnly = true
			s.ellipsis = false
			s.shownRows = 0
		case b >= total:
			s.summaryOnly = false
			s.ellipsis = false
			s.shownRows = total
		default:
			s.summaryOnly = false
			s.ellipsis = true
			s.shownRows = b
			normalizeSectionWindow(s)
		}
	}
	// anchorCursorWindow positions the cursor section's visible window so it
	// contains the cursor, centering it (via visibleWindow) and setting the
	// leading/trailing ellipsis flags. Safe to re-call after shrinking shownRows.
	anchorCursorWindow := func(s *sectionState) {
		total := len(s.rowIdxs)
		s.summaryOnly = false
		if s.shownRows < 1 {
			s.shownRows = 1
		}
		b := s.shownRows
		if b >= total {
			s.shownRows = total
			s.windowStart = 0
			s.ellipsis = false
			s.leadingEllipsis = false
			return
		}
		start, _ := visibleWindow(cursorOffset, total, b)
		s.windowStart = start
		s.leadingEllipsis = start > 0
		s.ellipsis = start+b < total
		normalizeSectionWindow(s)
	}
	// Show the cursor's row only when the viewport has room beyond one header per
	// section; at the extreme where headers alone fill the viewport, preserve the
	// section skeleton (budget 0 for every section) rather than dropping a header.
	if budgets[cursorSection] > 0 {
		anchorCursorWindow(&sections[cursorSection])
	}

	lines := render()
	for len(lines) > maxLines {
		// Collapse the furthest non-cursor, non-collapsed section first.
		victim, bestDist := -1, -1
		for i := range sections {
			if i == cursorSection || sections[i].summaryOnly || len(sections[i].rowIdxs) == 0 {
				continue
			}
			if d := absDistance(i, cursorSection); d > bestDist {
				bestDist = d
				victim = i
			}
		}
		if victim < 0 {
			// Only the cursor section (plus collapsed others) remains and it is
			// still too tall: shrink its window and re-anchor around the cursor.
			s := &sections[cursorSection]
			if s.shownRows <= 1 {
				break
			}
			s.shownRows--
			anchorCursorWindow(s)
		} else {
			v := &sections[victim]
			v.summaryOnly = true
			v.ellipsis = false
			v.leadingEllipsis = false
			v.shownRows = 0
		}
		lines = render()
	}
	if len(lines) > maxLines {
		lines = capViewportKeepingRow(lines, maxLines, cursorRowIdx)
	}
	return lines
}

func (m listTUIModel) startDBWatcher() tea.Cmd {
	return func() tea.Msg {
		source, err := dbwatch.OpenChangeSource()
		if err != nil {
			return listDBWatcherReadyMsg{err: err}
		}
		if source == nil {
			return nil
		}
		return listDBWatcherReadyMsg{source: source}
	}
}

func (m listTUIModel) waitForDBEvent() tea.Cmd {
	if m.dbWatcher == nil {
		return nil
	}
	source := m.dbWatcher
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	return func() tea.Msg {
		_, err := source.Wait(ctx, 0)
		if err != nil && ctx.Err() == nil {
			return listDBWatchEventMsg{err: err}
		}
		return listDBWatchEventMsg{}
	}
}
