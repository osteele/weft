package terminal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/orchestration"
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
	dbWatcher                  *fsnotify.Watcher
	dbWatcherTargets           map[string]struct{}
	debounceActive             bool
	debouncePending            bool
	syncWorker                 *hostsync.Worker
	ctx                        context.Context
	cancel                     context.CancelFunc
	groupedByStatus            bool
	groupedUnprocessedView     bool
	groupedRows                []groupedStatusRow
	groupedSelectableRows      []int
	autoMode                   bool
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
	lastAutoPilotErrorRaw      string
	showAutoPilotErrorDetails  bool
	launchLiveByID             map[int64]*db.LaunchLiveState
	launchStatusByID           map[int64]string
	placingJobIDs              map[int64]struct{}
	placementQueuedAtByJob     map[int64]int64
	launchByID                 map[int64]*db.Launch
	hostInfoByName             map[string]*db.CachedHostInfo
	quickLaunching             bool
	quickLaunchScope           string
	quickLaunchProgress        <-chan listQuickLaunchProgressMsg
	quickLaunchDone            <-chan listQuickLaunchDoneMsg
	quickLaunchStatusHoldUntil time.Time
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

type listJobsLoadedMsg struct {
	jobs                   []*db.Job
	launchLiveByID         map[int64]*db.LaunchLiveState
	launchStatusByID       map[int64]string
	placingJobIDs          map[int64]struct{}
	placementQueuedAtByJob map[int64]int64
	launchByID             map[int64]*db.Launch
	hostInfoByName         map[string]*db.CachedHostInfo
	autoPassPhase          autoPilotPhaseHint
	err                    error
}

type listSyncFinishedMsg struct {
	warnings []string
	full     bool
}

type listDBWatcherReadyMsg struct {
	watcher *fsnotify.Watcher
	targets map[string]struct{}
	err     error
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
	placed         int
	rebalanced     int
	launched       int
	launchedClass  string
	blockedReasons map[int64]string
	anotherHolding bool
	paused         bool
	err            error
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

type rebalancePreviewLoadedMsg struct {
	moves []orchestration.QueueRebalanceMove
	err   error
}

type rebalanceAppliedMsg struct {
	count      int
	overBudget int
	err        error
}

type listMoveOptionsReadyMsg struct {
	requestID int64
	jobID     int64
	options   []moveOption
	err       error
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
	graceAckKeyPattern = regexp.MustCompile(`grace/(\d+)/acks/`)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool) error {
	cfg, _ := config.Load()
	router := newListWatchRouterModel(database, cfg, args, jobs, title, syncEnabled, groupedByStatus, autoMode)

	outputOpt, restore := InstallTUIStdioCapture()
	defer restore()

	_, err := tea.NewProgram(router, outputOpt, tea.WithAltScreen(), tea.WithReportFocus(), tea.WithMouseCellMotion()).Run()
	if err != nil {
		return fmt.Errorf("run list TUI: %w", err)
	}
	return nil
}

func newListTUIModel(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool) listTUIModel {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, _ := config.Load()
	cloudClients, _ := buildCloudClients(cfg)

	var sw *hostsync.Worker
	if syncEnabled {
		r2Client, _ := buildR2Client(cfg)
		sw = hostsync.New(database, cloudClients, r2Client, cfg)
		sw.Start()
	}

	model := listTUIModel{
		database:               database,
		args:                   append([]string(nil), args...),
		title:                  title,
		jobs:                   jobs,
		syncEnabled:            syncEnabled,
		pendingSyncHosts:       map[string]struct{}{},
		nextSyncTickAt:         time.Now().Add(throttledInterval(listTUISyncInterval, true)),
		syncWorker:             sw,
		ctx:                    ctx,
		cancel:                 cancel,
		groupedByStatus:        groupedByStatus,
		groupedUnprocessedView: groupedStatusUnprocessedView(title),
		autoMode:               groupedByStatus && autoMode,
		autoLeaseOwner:         fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		autoLeaseScope:         buildListAutoLeaseScope(title),
		autoRunRateTargetCents: loadAutoRunRateSoftTargetCentsPerHour(cfg),
		autoDailyCapCents:      loadAutoRunawaySpendDailyCapCents(cfg),
		quickLaunchScope:       "list_quick_launch:" + buildListAutoLeaseScope(title),
		focused:                true,
		appConfig:              cfg,
		cloudClients:           cloudClients,
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
	_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
	_ = db.ReleaseAutoLease(m.database, m.quickLaunchScope, m.autoLeaseOwner)
	if m.dbWatcher != nil {
		_ = m.dbWatcher.Close()
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.syncWorker != nil {
		m.syncWorker.Stop()
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
			m.runBackgroundCloudSync(false),
		)
	} else if m.syncEnabled {
		cmds = append(cmds, m.runBackgroundSync(false))
	}
	if cmd := m.runAutoPilot(); cmd != nil {
		cmds = append(cmds, cmd)
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
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "enter":
				m.showHelp = false
				return m, nil
			}
			return m, nil
		}
		if m.groupedByStatus {
			return m.handleGroupedKey(msg)
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.shutdown()
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.jobs)-1 {
				m.cursor++
			}
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			if len(m.jobs) > 0 {
				m.cursor = len(m.jobs) - 1
			}
		case "pgdown", "space":
			m.cursor += m.pageSize()
			if m.cursor >= len(m.jobs) {
				m.cursor = max(0, len(m.jobs)-1)
			}
		case "pgup", "b":
			m.cursor -= m.pageSize()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "r":
			return m.triggerManualRefresh()
		case "v":
			if m.moveLookupPending {
				m.moveLookupPending = false
				m.moveLookupRequestID = 0
			}
			m.groupedByStatus = !m.groupedByStatus
			m.rebuildLayout()
			m.rebuildGroupedRows()
			m.clampCursor()
			m.adjustOffset()
			if m.groupedByStatus {
				m.statusMessage = "Grouped status view"
			} else {
				m.statusMessage = "Ungrouped list view"
			}
			return m, nil
		case "i":
			return m, func() tea.Msg { return switchToSystemWatchMsg{} }
		case "?":
			m.showHelp = true
			return m, nil
		case "a":
			job := m.currentSelectedJob()
			if job == nil {
				m.statusMessage = "Select a job row to view attempts"
				return m, nil
			}
			return m, func() tea.Msg { return switchToAttemptsMsg{jobID: job.ID} }
		case "c":
			job := m.currentSelectedJob()
			if job == nil {
				m.statusMessage = "Select a job row for coding-assistant"
				return m, nil
			}
			cmd := m.beginAIAssist(job)
			return m, cmd
		case "A":
			if m.groupedByStatus {
				m.autoMode = !m.autoMode
				if m.autoMode {
					m.clearAutoPilotPersistentState()
					m.resumeAutoPilotNow()
					m.statusMessage = "Auto-pilot ON"
					return m, m.runAutoPilot()
				}
				m.autoInProgress = false
				m.autoBlockReasons = nil
				m.clearAutoPilotPersistentState()
				_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
				m.statusMessage = "Auto-pilot OFF"
				return m, nil
			}
		case "n":
			if !m.groupedByStatus {
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
		m.jobs = msg.jobs
		m.launchLiveByID = msg.launchLiveByID
		m.launchStatusByID = msg.launchStatusByID
		m.placingJobIDs = msg.placingJobIDs
		m.placementQueuedAtByJob = msg.placementQueuedAtByJob
		m.launchByID = msg.launchByID
		m.hostInfoByName = msg.hostInfoByName
		m.autoPassPhase = msg.autoPassPhase
		m.pruneAutoBlockReasons()
		if m.countUnplacedQueuedJobs() == 0 {
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
		return m, m.runAutoPilot()

	case listSyncFinishedMsg:
		if !msg.full {
			if len(msg.warnings) > 0 {
				if !m.quickLaunchStatusProtected() {
					m.statusMessage = strings.Join(msg.warnings, " | ")
				}
			} else {
				if !m.quickLaunchStatusProtected() {
					m.statusMessage = "Running full sync..."
				}
			}
			return m, tea.Batch(m.reloadJobs(), m.runBackgroundSync(true))
		}

		delete(m.pendingSyncHosts, backgroundSyncKey)
		if len(msg.warnings) > 0 {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = strings.Join(msg.warnings, " | ")
			}
		} else if len(m.jobs) == 0 {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "No jobs match this view."
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
		m.dbWatcher = msg.watcher
		m.dbWatcherTargets = msg.targets
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
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return listSyncWorkerResultMsg{result: r}
			}),
		)

	case listSyncTickMsg:
		m.nextSyncTickAt = time.Now().Add(throttledInterval(listTUISyncInterval, m.focused))
		cmds := []tea.Cmd{m.scheduleListSyncTick()}
		if m.syncWorker != nil {
			m.requestActiveSyncs()
			cmds = append(cmds, m.reloadJobs(), m.runBackgroundCloudSync(true))
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
		if !m.autoMode {
			return m, nil
		}
		m.autoBlockReasons = msg.blockedReasons
		if msg.err != nil {
			m.lastAutoPilotErrorRaw = msg.err.Error()
			m.showAutoPilotErrorDetails = false
			m.autoPersistentError = summarizeAutoPilotError(msg.err)
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot failed: " + summarizeAutoPilotError(msg.err)
			}
			m.autoNextPassAt = time.Now().Add(listAutoPilotCooldownError)
			m.rebuildGroupedRows()
			return m, nil
		}
		m.lastAutoPilotErrorRaw = ""
		m.showAutoPilotErrorDetails = false
		m.autoPersistentError = ""
		if msg.paused {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot: paused (resume with `weft autopilot resume`)"
			}
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
		if summary := autoPilotBlockSummary(msg.blockedReasons); summary != "" {
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
				m.statusMessage = fmt.Sprintf("Auto-pilot: rebalanced %d job(s) across existing instances.", msg.rebalanced)
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
			m.statusMessage = fmt.Sprintf("Mark processed failed: %v", msg.err)
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
			cursor:  0,
		}
		return m, nil

	case listMoveOptionsReadyMsg:
		if !m.moveLookupPending || msg.requestID != m.moveLookupRequestID {
			return m, nil
		}
		m.moveLookupPending = false
		m.moveLookupRequestID = 0

		selected := m.selectedGroupedJob()
		if selected == nil || selected.ID != msg.jobID {
			m.statusMessage = "Move lookup discarded (selection changed)"
			return m, nil
		}
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Move: %v", msg.err)
			return m, nil
		}
		m.movePicker = movePickerModel{
			active:  true,
			jobID:   msg.jobID,
			options: msg.options,
			cursor:  0,
		}
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
	}

	return m, nil
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
		return m.movePicker.View(m.width, m.height)
	}
	if m.aiAssist != nil {
		return m.renderAIAssistOverlay()
	}
	if m.showHelp {
		return m.renderListHelpView()
	}
	if m.groupedByStatus {
		return m.groupedView()
	}

	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	layout := m.layout
	var b strings.Builder
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width, m.autoRunRateTargetCents)
	selectedDetailLines := m.selectedJobDetailLines()

	mateRows, matesActive := hostMatesForFlatView(m.jobs, m.cursor)
	rowWidth := m.width

	title := fmt.Sprintf("%s (%d)", m.title, len(m.jobs))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")
	header := truncateDisplayWidth(formatJobListHeader(layout), rowWidth)
	b.WriteString(listTUIHeaderStyle.Render(truncateDisplayWidth(header, m.width)))
	b.WriteString("\n")

	footerBlockLines := len(sharedStatusLines) + len(selectedDetailLines) + 1
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
				row = applyHostMateMarker(row)
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

func (m listTUIModel) groupedView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	var b strings.Builder
	title := fmt.Sprintf("%s (%d)", m.title, len(m.jobs))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")

	groupedJobs := m.groupedJobsWithAutoReasons()
	rows := m.groupedRows
	if len(rows) == 0 {
		rows = []groupedStatusRow{{text: "None"}}
	}

	// Build footer lines first so we can reserve space for them.
	// Per-job ETA now lives on the "Job:" line in selectedDetailLines, so
	// no standalone campaign-wide ETA line is rendered.
	eta := computeGroupedETA(groupedJobs, m.launchLiveByID, time.Now())
	statusLine := m.groupedStatusText()
	visibleRunning := countVisibleRunningJobs(groupedJobs)
	sharedStatusLines := renderSharedTUIStatusLinesWithVisibleRunning(m.database, m.width, visibleRunning, m.autoRunRateTargetCents)
	autoPilotLine := m.groupedAutoPilotStatusText(visibleRunning)
	errorDetailsLines := m.groupedErrorDetailsLines()
	selectedDetailLines := m.selectedJobDetailLines()
	baseFooterLines := 2 // blank separator + controls
	if statusLine != "" {
		baseFooterLines++
	}
	baseFooterLines += len(sharedStatusLines)
	baseFooterLines += len(selectedDetailLines)
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
	controlsLine := m.groupedControlsText(eta.HasQueued)
	// Reserve: 1 title + 1 blank separator + footer lines.
	footerLines := baseFooterLines
	footerLines += len(errorDetailsLines)
	maxBodyLines := max(0, m.height-1-footerLines) // 1 for title
	selectedRow := m.selectedGroupedRow()
	mateJobs, matesActive := hostMatesForGroupedView(m.selectedGroupedJob(), m.jobs)
	rowWidth := m.width
	visibleRows := selectGroupedRowsForViewport(rows, maxBodyLines)
	bodyLinesWritten := 0
	for _, row := range visibleRows {
		line := truncateDisplayWidth(row.text, rowWidth)
		if matesActive && row.rowIdx >= 0 && row.rowIdx < len(m.groupedRows) {
			if rj := m.groupedRows[row.rowIdx].job; rj != nil && mateJobs[rj.ID] {
				line = applyHostMateMarker(line)
			}
		}
		if selectedRow >= 0 && row.rowIdx >= 0 && row.rowIdx == selectedRow {
			if matesActive {
				line = applyHostMateMarker(line)
			}
			line = renderSelectedRow(line, m.width)
		}
		b.WriteString(line)
		b.WriteString("\n")
		bodyLinesWritten++
	}
	for bodyLinesWritten < maxBodyLines {
		b.WriteString("\n")
		bodyLinesWritten++
	}

	// Visually separate grouped job rows from footer lines.
	b.WriteString("\n")
	for _, line := range errorDetailsLines {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	for _, line := range selectedDetailLines {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	if statusLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(statusLine, m.width)))
		b.WriteString("\n")
	}
	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if autoPilotLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(autoPilotLine, m.width)))
		b.WriteString("\n")
	}
	for _, line := range budgetPanelLines {
		b.WriteString(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)))
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(controlsLine, m.width)))
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
	status := normalizeStatusLineText(m.statusMessage)
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

func (m listTUIModel) countUnplacedQueuedJobs() int {
	n := 0
	for _, job := range m.jobs {
		if job.IsUnplacedAwaitingPlacement() {
			n++
		}
	}
	return n
}

func (m listTUIModel) groupedAutoPilotStatusText(visibleRunning int) string {
	if !m.autoMode {
		return ""
	}
	if m.autoRunRateInputActive {
		// Prompt is rendered in the controls line; keep this line empty
		// so the prompt does not appear twice in slightly different forms.
		return ""
	}
	if line := formatAgentBuildStatus(); line != "" {
		return line
	}
	unplaced := m.countUnplacedQueuedJobs()
	target := formatAutoRunRateTarget(m.autoRunRateTargetCents)
	if m.syncInProgress() {
		hosts := m.pendingHostList()
		if len(hosts) > 0 {
			return fmt.Sprintf("Auto-pilot: syncing %s... (target %s)", strings.Join(hosts, ", "), target)
		}
		return "Auto-pilot: syncing cloud state... (target " + target + ")"
	}
	if m.autoInProgress {
		elapsed := time.Since(m.autoPassStartedAt).Round(time.Second)
		phase := activePassPhase(m.autoPassPhase, m.autoPassStartedAt)
		return fmt.Sprintf("Auto-pilot: evaluating %s%s (%s)... (target %s)", pluralize(unplaced, "unplaced job", "unplaced jobs"), phase, elapsed, target)
	}
	if strings.TrimSpace(m.autoPersistentError) != "" {
		return "Auto-pilot: failed — " + m.autoPersistentError
	}
	if strings.TrimSpace(m.autoPersistentBlocked) != "" {
		return fmt.Sprintf("Auto-pilot: paused — %s (%d jobs)", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
	if !m.autoNextPassAt.IsZero() && time.Now().Before(m.autoNextPassAt) {
		return formatAutoPilotNextPass(m.autoNextPassAt, unplaced)
	}
	if !m.nextSyncTickAt.IsZero() && time.Now().Before(m.nextSyncTickAt) {
		return fmt.Sprintf("Auto-pilot: idle — next sync in %s · watching DB (%d unplaced, %d running, target %s)", waitUntil(m.nextSyncTickAt), unplaced, visibleRunning, target)
	}
	return fmt.Sprintf("Auto-pilot: monitoring (%d unplaced, %d running, target %s)", unplaced, visibleRunning, target)
}

func (m listTUIModel) groupedControlsText(hasQueued bool) string {
	if m.autoRunRateInputActive {
		// While the budget panel is open, the panel itself shows the
		// applicable keys; suppress the regular controls so they don't
		// muddle the picture.
		return ""
	}
	autoState := "OFF"
	if m.autoMode {
		autoState = "ON"
	}
	line := fmt.Sprintf("A:auto (%s)", autoState)
	line += "  r:refresh"
	if m.selectedGroupedJob() != nil {
		line += "  a:attempts  k:kill  u:unplace  p:processed"
		if selected := m.selectedGroupedJob(); selected != nil && selected.EffectiveStatus() == db.StatusQueued {
			line += "  m:move  N:new for selected"
		}
		line += "  P:priority"
	}
	if hasQueued {
		line += "  n:new instance"
	}
	line += "  R:rebalance"
	if strings.TrimSpace(m.lastAutoPilotErrorRaw) != "" {
		if m.showAutoPilotErrorDetails {
			line += "  e:hide error"
		} else {
			line += "  e:error details"
		}
	}
	line += "  v:ungroup  i:watch  ?:help"
	line += "  q:quit"
	return line
}

func (m listTUIModel) selectedJobDetailLines() []string {
	var job *db.Job
	if m.groupedByStatus {
		job = m.selectedGroupedJob()
	} else if m.cursor >= 0 && m.cursor < len(m.jobs) {
		job = m.jobs[m.cursor]
	}
	if job == nil {
		return nil
	}
	return renderSelectedJobDetail(job, selectedJobContext{
		launchLiveByID:  m.launchLiveByID,
		launchByID:      m.launchByID,
		hostInfoByName:  m.hostInfoByName,
		siblingJobs:     m.jobs,
		cloudConfigured: len(m.cloudClients) > 0,
	}, time.Now())
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
	if m.groupedByStatus {
		return m.selectedGroupedJob()
	}
	if m.cursor >= 0 && m.cursor < len(m.jobs) {
		return m.jobs[m.cursor]
	}
	return nil
}

func (m listTUIModel) selectedGroupedJob() *db.Job {
	row := m.selectedGroupedRow()
	if row < 0 || row >= len(m.groupedRows) {
		return nil
	}
	return m.groupedRows[row].job
}

func (m listTUIModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.rebalancePreview.active || m.movePicker.active || m.aiAssist != nil || m.showHelp || m.autoRunRateInputActive {
		return m, nil
	}
	if m.groupedByStatus {
		m.selectGroupedMouseRow(msg.Y)
		return m, nil
	}
	m.selectFlatMouseRow(msg.Y)
	return m, nil
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
	footerBlockLines := len(sharedStatusLines) + len(selectedDetailLines) + 1
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
	if row.job == nil || row.isHeader || row.isBlocked {
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
	if len(rows) == 0 {
		rows = []groupedStatusRow{{text: "None"}}
	}

	groupedJobs := m.groupedJobsWithAutoReasons()
	statusLine := m.groupedStatusText()
	visibleRunning := countVisibleRunningJobs(groupedJobs)
	sharedStatusLines := renderSharedTUIStatusLinesWithVisibleRunning(m.database, m.width, visibleRunning, m.autoRunRateTargetCents)
	autoPilotLine := m.groupedAutoPilotStatusText(visibleRunning)
	errorDetailsLines := m.groupedErrorDetailsLines()
	selectedDetailLines := m.selectedJobDetailLines()
	baseFooterLines := 2
	if statusLine != "" {
		baseFooterLines++
	}
	baseFooterLines += len(sharedStatusLines)
	baseFooterLines += len(selectedDetailLines)
	if autoPilotLine != "" {
		baseFooterLines++
	}
	if m.autoRunRateInputActive {
		baseFooterLines += len(renderAutoBudgetPanel(m.currentBudgetState()))
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
	maxBodyLines := max(0, m.height-1-footerLines)
	return selectGroupedRowsForViewport(rows, maxBodyLines)
}

func (m listTUIModel) handleMovePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.movePicker.reset()
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
		m.movePicker.reset()
		targetDesc := fmt.Sprintf("instance %s", ids.FormatInstanceID(selected.instanceID))
		if selected.isNew {
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
		m.statusMessage = "Applying rebalance moves..."
		return m, requestRebalanceApply(m.database)
	}
	return m, nil
}

func (m listTUIModel) handleGroupedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.autoRunRateInputActive {
		return m.handleAutoRunRateInputKey(msg)
	}
	switch msg.String() {
	case "esc":
		if m.moveLookupPending {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
			m.statusMessage = "Move lookup canceled"
			return m, nil
		}
		m.shutdown()
		return m, tea.Quit
	case "ctrl+c", "q":
		m.shutdown()
		return m, tea.Quit
	case "ctrl+z":
		return m, tea.Suspend
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.groupedSelectableRows)-1 {
			m.cursor++
		}
		return m, nil
	case "g", "home":
		m.cursor = 0
		return m, nil
	case "G", "end":
		if len(m.groupedSelectableRows) > 0 {
			m.cursor = len(m.groupedSelectableRows) - 1
		}
		return m, nil
	case "pgdown", "space":
		m.cursor += m.pageSize()
		if m.cursor >= len(m.groupedSelectableRows) {
			m.cursor = max(0, len(m.groupedSelectableRows)-1)
		}
		return m, nil
	case "pgup", "b":
		m.cursor -= m.pageSize()
		if m.cursor < 0 {
			m.cursor = 0
		}
		return m, nil
	case "r":
		return m.triggerManualRefresh()
	case "a":
		job := m.selectedGroupedJob()
		if job == nil {
			m.statusMessage = "Select a job row to view attempts"
			return m, nil
		}
		return m, func() tea.Msg { return switchToAttemptsMsg{jobID: job.ID} }
	case "c":
		job := m.selectedGroupedJob()
		if job == nil {
			m.statusMessage = "Select a job row for coding-assistant"
			return m, nil
		}
		cmd := m.beginAIAssist(job)
		return m, cmd
	case "A":
		m.autoMode = !m.autoMode
		if m.autoMode {
			m.clearAutoPilotPersistentState()
			m.resumeAutoPilotNow()
			m.statusMessage = "Auto-pilot ON"
			return m, m.runAutoPilot()
		}
		m.autoInProgress = false
		m.autoBlockReasons = nil
		m.clearAutoPilotPersistentState()
		_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
		m.statusMessage = "Auto-pilot OFF"
		return m, nil
	case "$":
		return m.beginAutoRunRateInput()
	case "v":
		if m.moveLookupPending {
			m.moveLookupPending = false
			m.moveLookupRequestID = 0
		}
		m.groupedByStatus = false
		m.rebuildLayout()
		m.rebuildGroupedRows()
		m.clampCursor()
		m.adjustOffset()
		m.statusMessage = "Ungrouped list view"
		return m, nil
	case "i":
		return m, func() tea.Msg { return switchToSystemWatchMsg{} }
	case "?":
		m.showHelp = true
		return m, nil
	case "e":
		if strings.TrimSpace(m.lastAutoPilotErrorRaw) == "" {
			m.statusMessage = "No auto-pilot error details."
			return m, nil
		}
		m.showAutoPilotErrorDetails = !m.showAutoPilotErrorDetails
		return m, nil
	case "n":
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
	case "N":
		return m.beginSelectedLaunchNew()
	case "P":
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
	case "R":
		m.clearAutoPilotPersistentState()
		_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
		m.rebalancePreview = rebalancePreviewModel{active: true, loading: true}
		m.statusMessage = "Planning rebalance moves..."
		return m, requestRebalancePreview(m.database)
	case "k":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
		m.clearAutoPilotPersistentState()
		m.statusMessage = fmt.Sprintf("Killing job #%d...", job.ID)
		return m, requestWatchJobKill(m.database, job.ID)
	case "u":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
		if job.EffectiveStatus() != db.StatusQueued {
			m.statusMessage = fmt.Sprintf("Job #%d is %s; only queued jobs can be unplaced", job.ID, job.EffectiveStatus())
			return m, nil
		}
		m.clearAutoPilotPersistentState()
		m.statusMessage = fmt.Sprintf("Unplacing job #%d...", job.ID)
		return m, requestWatchJobUnplace(m.database, job.ID)
	case "p":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
		m.clearAutoPilotPersistentState()
		m.statusMessage = fmt.Sprintf("Marking job #%d as processed...", job.ID)
		return m, requestWatchJobMarkProcessed(m.database, job.ID)
	case "m":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
		if job.EffectiveStatus() != db.StatusQueued {
			m.statusMessage = "Move is only available for queued jobs"
			return m, nil
		}
		m.clearAutoPilotPersistentState()
		m.moveLookupSeq++
		reqID := m.moveLookupSeq
		m.moveLookupPending = true
		m.moveLookupRequestID = reqID
		m.moveLookupJobID = job.ID
		m.statusMessage = fmt.Sprintf("Searching move destinations for job #%d... (Esc to cancel)", job.ID)
		return m, m.requestGroupedMoveOptions(reqID, job.ID)
	}
	return m, nil
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
	database := m.database
	appCfg := m.appConfig
	cachedCloudClients := append([]cloud.Client(nil), m.cloudClients...)
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
		cloudClients := cachedCloudClients
		if len(cloudClients) == 0 {
			var clientsErr error
			cloudClients, clientsErr = buildCloudClients(cfg)
			if clientsErr != nil {
				return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("build cloud clients: %w", clientsErr)}
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
		msg := requestMoveOptions(database, cfg, cloudClients, job, capacities, queuedCounts, sourceInstanceID)()
		ready, ok := msg.(moveOptionsReadyMsg)
		if !ok {
			return listMoveOptionsReadyMsg{requestID: requestID, jobID: jobID, err: fmt.Errorf("unexpected move-options response")}
		}
		return listMoveOptionsReadyMsg{
			requestID: requestID,
			jobID:     ready.jobID,
			options:   ready.options,
			err:       ready.err,
		}
	}
}

func requestRebalancePreview(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		result, err := orchestration.RebalanceQueuedJobsAcrossInstances(context.Background(), database, orchestration.QueueRebalanceOptions{
			Apply:               false,
			CostCeilingOverride: 1e6,
			Operation:           "tui.rebalance",
		})
		if err != nil {
			return rebalancePreviewLoadedMsg{err: err}
		}
		return rebalancePreviewLoadedMsg{moves: result.Moves}
	}
}

func requestRebalanceApply(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		result, err := orchestration.RebalanceQueuedJobsAcrossInstances(context.Background(), database, orchestration.QueueRebalanceOptions{
			Apply:               true,
			CostCeilingOverride: 1e6,
			Operation:           "tui.rebalance",
		})
		if err != nil {
			return rebalanceAppliedMsg{err: err}
		}
		return rebalanceAppliedMsg{
			count:      len(result.Moves),
			overBudget: countOverBudgetRebalanceMoves(result.Moves),
		}
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
	state += "  up/down move  space/b page  g/G top/bottom  P:priority  v:toggle  i:instances  ? help  q quit"
	return state
}

func (m listTUIModel) renderListHelpView() string {
	lines := []string{
		"Jobs List Keybindings",
		"",
		"Navigation:",
		"  up/down (or j/k) move selection",
		"  pgup/pgdown (or b/space) page up/down",
		"  g/G jump top/bottom",
		"",
		"Common:",
		"  a view attempts for selected job",
		"  c coding-assistant (progress / review / remediate, status-dependent)",
		"  r refresh",
		"  v toggle grouped/ungrouped view",
		"  i open instances watch",
		"  q quit",
	}
	if m.groupedByStatus {
		lines = append(lines,
			"",
			"Grouped-only actions:",
			"  A toggle auto-pilot",
			"  $ set run-rate + daily cap (H/D clear; r resets breaker)",
			"  n launch a new instance for queued jobs",
			"  N launch a new instance for selected queued job",
			"  P toggle selected job priority",
			"  R preview rebalance moves",
			"  k kill selected job",
			"  u unplace selected queued job",
			"  p mark selected job as processed",
			"  m move selected queued job",
			"  e toggle auto-pilot error details",
		)
	}
	lines = append(lines,
		"",
		"Help:",
		"  ? toggle this help",
		"  q or Esc close help",
	)

	var b strings.Builder
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(lines[0], m.width)))
	b.WriteString("\n")
	for i := 1; i < len(lines); i++ {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(lines[i], m.width)))
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth("? close help", m.width)))
	return b.String()
}

func (m listTUIModel) triggerManualRefresh() (listTUIModel, tea.Cmd) {
	cmds := []tea.Cmd{m.reloadJobs()}
	if !m.quickLaunchStatusProtected() {
		m.statusMessage = "Refreshing..."
	}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
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
	if m.syncInProgress() {
		return "No jobs in this view yet. Waiting for startup sync and DB updates..."
	}
	if m.statusMessage != "" {
		return "No jobs in this view. " + m.statusMessage
	}
	return "No jobs match this view."
}

func (m *listTUIModel) rebuildLayout() {
	m.layout = newJobListLayout(max(20, m.width-2), m.jobs, nil, false)
}

func (m *listTUIModel) rebuildGroupedRows() {
	if !m.groupedByStatus {
		m.groupedRows = nil
		m.groupedSelectableRows = nil
		return
	}
	groupedJobs := m.groupedJobsWithAutoReasons()
	m.groupedRows = buildGroupedStatusRowsAt(groupedJobs, m.width, m.launchLiveByID, m.launchStatusByID, m.placingJobIDs, m.placementQueuedAtByJob, time.Now())
	m.groupedSelectableRows = m.groupedSelectableRows[:0]
	for i, row := range m.groupedRows {
		if row.job != nil && !row.isHeader && !row.isBlocked {
			m.groupedSelectableRows = append(m.groupedSelectableRows, i)
		}
	}
	m.clampGroupedCursor()
}

func (m *listTUIModel) clampCursor() {
	if m.groupedByStatus {
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
	if m.groupedByStatus {
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
	return max(1, m.height-3)
}

func (m listTUIModel) reloadJobs() tea.Cmd {
	database := m.database
	args := append([]string(nil), m.args...)
	return func() tea.Msg {
		jobs, err := collectJobsForList(database, args)
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
		placementQueuedAtByJob, placementErr := db.PlacementDisplayQueuedAt(database, queuedRentalJobIDs(jobs))
		if placementErr != nil {
			placementQueuedAtByJob = map[int64]int64{}
		}
		hostInfoByName := loadInventoryHostInfo(database, jobs)
		return listJobsLoadedMsg{
			jobs:                   jobs,
			launchLiveByID:         launchLiveByID,
			launchStatusByID:       launchStatusByID,
			placingJobIDs:          loadPlacingJobIDs(database),
			placementQueuedAtByJob: placementQueuedAtByJob,
			launchByID:             launchByID,
			hostInfoByName:         hostInfoByName,
			autoPassPhase:          loadLatestAutoPilotPhase(database),
		}
	}
}

func queuedRentalJobIDs(jobs []*db.Job) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		if job.EffectiveStatus() == db.StatusQueued {
			ids = append(ids, job.ID)
		}
	}
	return ids
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
		m.syncWorker.Request(hostsync.Request{
			Host: host,
			Rate: hostsync.GetHostSyncRate(jobs),
		})
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

func (m *listTUIModel) runAutoPilot() tea.Cmd {
	if !m.groupedByStatus || !m.autoMode || m.autoInProgress || m.database == nil {
		return nil
	}
	if !m.autoNextPassAt.IsZero() && time.Now().Before(m.autoNextPassAt) {
		return nil
	}
	if m.countUnplacedQueuedJobs() == 0 {
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
		placed, rebalanced, launched, launchedClass, blockedReasons, runErr := runGatedAutoPilotPass(ctx, database, jobs, "list-tui")
		if runErr != nil {
			if errors.Is(runErr, orchestration.ErrAutopilotPaused) {
				return listAutoPilotDoneMsg{paused: true}
			}
			if errors.Is(runErr, orchestration.ErrAutopilotBusy) {
				return listAutoPilotDoneMsg{anotherHolding: true}
			}
		}
		return listAutoPilotDoneMsg{
			placed:         placed,
			rebalanced:     rebalanced,
			launched:       launched,
			launchedClass:  launchedClass,
			blockedReasons: blockedReasons,
			err:            runErr,
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
func runGatedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job, label string) (int, int, int, string, map[int64]string, error) {
	result, err := orchestration.RunGroupedAutoPilotPassGated(ctx, database, scopedJobs, label)
	if result != nil {
		return result.Placed, result.Rebalanced, result.Launched, result.LaunchedClass, result.BlockedReasons, err
	}
	if err != nil {
		return 0, 0, 0, "", nil, err
	}
	return 0, 0, 0, "", nil, nil
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
		instanceIDs:  res.InstanceIDs,
		movedJobs:    res.MovedJobs,
		warning:      res.Warning,
		runningJobID: res.RunningJobID,
	}
}

func (m *listTUIModel) pruneAutoBlockReasons() {
	if len(m.autoBlockReasons) == 0 {
		return
	}
	visibleUnplaced := make(map[int64]struct{}, len(m.jobs))
	for _, job := range m.jobs {
		if job.IsUnplacedAwaitingPlacement() {
			visibleUnplaced[job.ID] = struct{}{}
		}
	}
	for jobID := range m.autoBlockReasons {
		if _, ok := visibleUnplaced[jobID]; !ok {
			delete(m.autoBlockReasons, jobID)
		}
	}
}

func (m listTUIModel) groupedJobsWithAutoReasons() []*db.Job {
	baseJobs := m.jobs
	if m.groupedByStatus && m.groupedUnprocessedView {
		baseJobs = excludeJobsWithStatus(baseJobs, db.StatusCanceled)
	}

	if len(m.autoBlockReasons) == 0 {
		return baseJobs
	}
	decorated := make([]*db.Job, 0, len(baseJobs))
	for _, job := range baseJobs {
		if job == nil {
			decorated = append(decorated, nil)
			continue
		}
		reason, ok := m.autoBlockReasons[job.ID]
		if !ok || strings.TrimSpace(reason) == "" || !job.IsUnplacedAwaitingPlacement() {
			decorated = append(decorated, job)
			continue
		}
		copyJob := *job
		copyJob.QueueBlockedReason = reason
		decorated = append(decorated, &copyJob)
	}
	return decorated
}

func groupedStatusUnprocessedView(title string) bool {
	for _, part := range strings.Split(strings.ToLower(title), "•") {
		if strings.TrimSpace(part) == "unprocessed" {
			return true
		}
	}
	return false
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

func autoPilotBlockSummary(reasons map[int64]string) string {
	if len(reasons) == 0 {
		return ""
	}
	unique := map[string]int{}
	for _, reason := range reasons {
		r := strings.TrimSpace(reason)
		if r != "" {
			unique[r]++
		}
	}
	if len(unique) == 0 {
		return ""
	}
	if len(unique) == 1 {
		for reason, count := range unique {
			if count == 1 {
				return reason
			}
			return fmt.Sprintf("%s (%d jobs)", reason, count)
		}
	}
	total := 0
	for _, c := range unique {
		total += c
	}
	return fmt.Sprintf("%d jobs blocked", total)
}

func summarizeAutoPilotError(err error) string {
	if err == nil {
		return "unknown error"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled (will retry)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out (will retry)"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown error"
	}
	if strings.Contains(msg, "submit jobs to instance control plane:") {
		if matches := graceAckKeyPattern.FindStringSubmatch(msg); len(matches) == 2 {
			if instanceID, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
				return normalizeStatusLineText(fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", ids.FormatInstanceID(instanceID)))
			}
			return normalizeStatusLineText(fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", matches[1]))
		}
		return normalizeStatusLineText("instance control plane did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)")
	}
	return normalizeStatusLineText(msg)
}

func normalizeStatusLineText(msg string) string {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\r", "\n")
	parts := strings.Split(msg, "\n")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		collapsed := strings.Join(strings.Fields(part), " ")
		if collapsed != "" {
			clean = append(clean, collapsed)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return strings.Join(clean, " | ")
}

func (m listTUIModel) groupedErrorDetailsLines() []string {
	if !m.groupedByStatus || !m.showAutoPilotErrorDetails {
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
	jobCount       int
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
		if row.job != nil && !row.isBlocked {
			current.jobCount++
		}
	}
	if active {
		current.shownRows = len(current.rows)
		sections = append(sections, current)
	}
	return sections
}

// selectGroupedRowsForViewport keeps grouped sections in order and, when needed,
// abbreviates section tails with "..." or collapses a fully elided section to
// "Section (N)" (without a trailing colon).
func selectGroupedRowsForViewport(rows []groupedStatusRow, maxLines int) []groupedViewportLine {
	if maxLines <= 0 || len(rows) == 0 {
		return nil
	}
	type sectionState struct {
		groupedViewportSection
		rowIdxs []int
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
		for idx, section := range sections {
			currAbbreviated := section.abbreviated()
			if idx > 0 && !(prevAbbreviated && currAbbreviated) {
				out = append(out, groupedViewportLine{text: "", rowIdx: -1})
			}
			if section.summaryOnly {
				out = append(out, groupedViewportLine{text: fmt.Sprintf("%s (%d)", section.title, section.jobCount), rowIdx: -1})
			} else {
				out = append(out, groupedViewportLine{text: fmt.Sprintf("%s (%d):", section.title, section.jobCount), rowIdx: section.headerRowIndex})
				for i := 0; i < section.shownRows && i < len(section.rows) && i < len(section.rowIdxs); i++ {
					out = append(out, groupedViewportLine{text: section.rows[i].text, rowIdx: section.rowIdxs[i]})
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

	lines := render()
	for len(lines) > maxLines {
		changed := false
		for i := len(sections) - 1; i >= 0; i-- {
			s := &sections[i]
			total := len(s.rows)
			if s.summaryOnly {
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

func (m listTUIModel) startDBWatcher() tea.Cmd {
	return func() tea.Msg {
		watcher, targets, err := dbwatch.OpenJobsDBWatcher()
		if err != nil {
			return listDBWatcherReadyMsg{err: err}
		}
		if watcher == nil {
			return nil
		}
		return listDBWatcherReadyMsg{watcher: watcher, targets: targets}
	}
}

func (m listTUIModel) waitForDBEvent() tea.Cmd {
	if m.dbWatcher == nil || len(m.dbWatcherTargets) == 0 {
		return nil
	}
	watcher := m.dbWatcher
	targets := m.dbWatcherTargets

	return func() tea.Msg {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return listDBWatchEventMsg{err: fmt.Errorf("db watcher closed")}
				}
				if !dbwatch.IsWatchedFile(event.Name, targets) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				return listDBWatchEventMsg{}
			case err, ok := <-watcher.Errors:
				if !ok {
					return listDBWatchEventMsg{err: fmt.Errorf("db watcher error channel closed")}
				}
				return listDBWatchEventMsg{err: err}
			}
		}
	}
}
