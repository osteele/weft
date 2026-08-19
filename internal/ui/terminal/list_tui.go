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
	"github.com/osteele/weft/internal/app/flash"
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
	"github.com/osteele/weft/internal/processguard"
)

const listDBChangeDebounce = 200 * time.Millisecond
const listTUISyncInterval = TerminalSyncInterval
const listAutoLeaseTTL = 30 * time.Second

// listQuitTeardownCap bounds how long the TUI shows "Quitting…" while releasing
// auto-leases before exiting anyway. The release contends with the background
// cloud-sync write lock (SQLite busy_timeout), so cap it to keep quit fast; an
// abandoned lease self-expires via listAutoLeaseTTL.
const listQuitTeardownCap = 1 * time.Second

// backgroundSyncKey is the pendingSyncHosts sentinel for the non-worker
// startup / tick cloud sync, which is not tied to a specific host.
const backgroundSyncKey = ""

type listTUIModel struct {
	database         *sql.DB
	args             []string
	title            string
	unprocessedView  bool
	statusView       string
	jobs             []*db.Job
	layout           jobListLayout
	cursor           int
	offset           int
	width            int
	height           int
	syncEnabled      bool
	pendingSyncHosts map[string]struct{}
	nextSyncTickAt   time.Time
	// statusMessage is durable operation state, set when an async op starts
	// and overwritten when it completes ("Restarting daemon...", "Moving
	// job..."). flash is the transient result of a completed synchronous
	// action; it self-expires. Targets that START something use statusMessage;
	// targets that COMPLETE something use flash.
	statusMessage            string
	flash                    flash.State
	daemonRestartInProgress  bool
	dbWatcher                *dbwatch.Source
	debounceActive           bool
	debouncePending          bool
	reloadInProgress         bool
	reloadPending            bool
	syncWorker               *hostsync.Worker
	ctx                      context.Context
	cancel                   context.CancelFunc
	groupMode                listGroupMode
	groupedByStatus          bool
	groupedUnprocessedView   bool
	projectFilter            string
	projectInputActive       bool
	projectInputValue        string
	projectCandidates        []string
	projectCandidateCursor   int
	projectCandidatesLoading bool
	hideStatusArea           bool
	groupedRows              []groupedStatusRow
	groupedSelectableRows    []int
	autoInProgress           bool
	autoUpgradePending       bool
	autoPassStartedAt        time.Time
	autoPassPhase            autoPilotPhaseHint
	autoPersistentError      string
	autoPersistentBlocked    string
	autoPersistentBlockedN   int
	autoRunRateTargetCents   int
	autoRunRateInputActive   bool
	autoRunRateInputValue    string
	autoRunRateInputPhase    autoBudgetPhase
	autoDailyCapCents        int
	autoNextPassAt           time.Time
	autoTimerRunsPass        bool
	autoWakeSnapshot         orchestration.WakeSnapshot
	autoLastPassAt           time.Time
	autoLeaseOwner           string
	autoLeaseScope           string
	autoBlockReasons         map[int64]string
	autoBlockDetail          map[int64]*blockreason.Structured
	expandedBlocked          map[int64]bool
	// collapsedSections holds the session-scoped set of grouped-status section
	// keys the user has collapsed to their header row.
	collapsedSections          map[string]bool
	showInstanceFailures       bool
	instanceFailuresScroll     int
	showJobDiagnosis           bool
	jobDiagnosisJobID          int64
	jobDiagnosisLines          []string
	jobDiagnosisScroll         int
	jobDiagnosisLoading        bool
	lastAutoPilotErrorRaw      string
	showAutoPilotErrorDetails  bool
	autopilotPaused            bool
	autopilotPausedReason      string
	autopilotActive            bool
	autopilotPassStartedAt     time.Time
	launchLiveByID             map[int64]*db.LaunchLiveState
	launchStatusByID           map[int64]string
	placingJobIDs              map[int64]struct{}
	placementQueuedAtByJob     map[int64]int64
	placementStatusByJob       map[int64]jobview.PlacementStatus
	attemptOutcomeByJob        map[int64]db.AttemptOutcomeEvent
	launchByID                 map[int64]*db.Launch
	externalBindingByJobID     map[int64]*db.ExternalJobBinding
	launchBootstrapP50         time.Duration
	launchBootstrapDurations   db.BootstrapDurations
	launchBootstrapSamples     int
	launchStageETAByName       map[string]groupedStatusLaunchingStageETA
	launchStageEnteredAtByID   map[int64]int64
	launchSpinner              spinner.Model
	launchSpinnerRunning       bool
	recentFailedInstances      *recentFailedInstances
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
	quitting                   bool
	appConfig                  *config.Config
	cloudClients               []cloud.Client
	aiAssist                   *aiAssistState
	// selAnchor is the cursor position where a mouse-drag selection started.
	// selRangeActive marks the selection as a range (anchor…cursor) rather
	// than a single row; the bool keeps the zero value range-free.
	selAnchor      int
	selRangeActive bool
	// dragging is true between a left press on a selectable row and its
	// release; motion events in that window extend the range.
	dragging bool
	// mouseEnabled tracks whether mouse reporting is on, so the M toggle and
	// the title-line indicator agree. Initialized from config.EnableMouse.
	mouseEnabled bool
	// planCache holds the plan from the most recent View(). Pointer so it survives bubbletea's
	// value-receiver copies. Hit testing reads it so a click resolves against the frame the user
	// actually saw, not one rebuilt from a database that may have moved between render and click.
	planCache *screenPlan
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

type listJobDiagnosisLoadedMsg struct {
	jobID  int64
	output string
	err    error
}

type groupedViewLayout struct {
	visibleRows         []groupedViewportLine
	statusLine          string
	sharedStatus        sharedTUIStatusLinesView
	autoPilotLine       string
	autoPilotTarget     clickTarget
	errorDetailsLines   []string
	instanceHealthLines []string
	selectedDetails     []string
	budgetPanelLines    []string
	controlsLine        string
	maxBodyLines        int
}

type groupedSelectionKey struct {
	kind string
	id   int64
	name string
}

var listRestartDaemonFunc = restartDaemonForListTUI
var listOpenURLFunc = openURLForListTUI

const vastaiBillingURL = "https://cloud.vast.ai/billing/"
const runpodBillingURL = "https://console.runpod.io/user/billing"

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
	m.selRangeActive = false
	m.dragging = false
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
	attemptOutcomeByJob      map[int64]db.AttemptOutcomeEvent
	launchByID               map[int64]*db.Launch
	externalBindingByJobID   map[int64]*db.ExternalJobBinding
	launchBootstrapP50       time.Duration
	launchBootstrapDurations db.BootstrapDurations
	launchBootstrapSamples   int
	launchStageETAByName     map[string]groupedStatusLaunchingStageETA
	launchStageEnteredAtByID map[int64]int64
	recentFailedInstances    *recentFailedInstances
	placementDaemonStopped   bool
	hostInfoByName           map[string]*db.CachedHostInfo
	cordonedHostsByName      map[string]bool
	overloadedHostsByName    map[string]bool
	autoPassPhase            autoPilotPhaseHint
	autopilotPaused          bool
	autopilotPausedReason    string
	autopilotActive          bool
	autopilotPassStartedAt   time.Time
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

// listQuitNowMsg ends the program once quit teardown finishes or its cap fires.
type listQuitNowMsg struct{}
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
	upgradePending    bool
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

func renderSelectedGroupedRow(row string, job *db.Job, width int) string {
	selected := renderSelectedRow(row, width)
	if job != nil && job.DisplayMoveDim {
		return moveAttemptDimStyle.Render(selected)
	}
	return selected
}

var (
	listTUIFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUIPromptStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
	listTUIEmptyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, projectFilter string) error {
	cfg, _ := config.Load()
	router := newListWatchRouterModel(database, cfg, args, jobs, title, syncEnabled, groupedByStatus, projectFilter)

	stdio := InstallTUIStdioCapture()

	programOpts := []tea.ProgramOption{stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()}
	if cfg.EnableMouse {
		programOpts = append(programOpts, tea.WithMouseCellMotion())
	}
	finalModel, err := tea.NewProgram(router, programOpts...).Run()
	stdio.Restore()
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

	foregroundSyncEnabled := syncEnabled && !listTUIDelegatesBackgroundWorkToDaemon()
	var sw *hostsync.Worker
	if foregroundSyncEnabled {
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
		syncEnabled:            foregroundSyncEnabled,
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
		mouseEnabled:           cfg.EnableMouse,
		appConfig:              cfg,
		cloudClients:           cloudClients,
		launchSpinner:          s,
		planCache:              new(screenPlan),
	}
	model.rebuildGroupedRows()
	if foregroundSyncEnabled {
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

func listTUIDelegatesBackgroundWorkToDaemon() bool {
	status, err := daemonStatusProbe(daemonStatusPaths())
	return err == nil && status.Live
}

func (m *listTUIModel) shutdown() {
	m.shutdownImmediate()
	if m.syncWorker != nil {
		m.syncWorker.Stop()
	}
}

func (m *listTUIModel) shutdownImmediate() {
	// Cancel background work first so it stops contending for the DB write
	// lock, then release the auto-leases off the calling goroutine (this path
	// runs on a view switch) so a contended release can't freeze the UI. An
	// abandoned release self-expires via listAutoLeaseTTL.
	if m.cancel != nil {
		m.cancel()
	}
	if m.dbWatcher != nil {
		_ = m.dbWatcher.Close()
	}
	database, owner := m.database, m.autoLeaseOwner
	scope, quickScope := m.autoLeaseScope, m.quickLaunchScope
	go func() {
		if database == nil || owner == "" {
			return
		}
		if scope != "" {
			_ = db.ReleaseAutoLease(database, scope, owner)
		}
		if quickScope != "" {
			_ = db.ReleaseAutoLease(database, quickScope, owner)
		}
	}()
}

// beginQuit runs the non-blocking part of teardown on the update goroutine:
// cancel background work (so it stops contending for the DB write lock) and
// close the file watcher. The auto-lease release — which can block on the
// cloud-sync write lock for up to busy_timeout — is deferred to quitTeardownCmd
// so it never freezes rendering. See list_tui_keybindings.go's quit handler.
func (m listTUIModel) beginQuit() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.dbWatcher != nil {
		_ = m.dbWatcher.Close()
	}
	if m.syncWorker != nil {
		stopSyncWorkerAfterQuit(m.syncWorker)
	}
}

// quitTeardownCmd releases the auto-leases off the update goroutine, then
// signals quit. A competing cap timer guarantees quit happens within
// listQuitTeardownCap even if the release is stuck behind the cloud-sync write
// lock; an abandoned lease self-expires via its TTL.
func (m listTUIModel) quitTeardownCmd() tea.Cmd {
	database := m.database
	scope := m.autoLeaseScope
	quickScope := m.quickLaunchScope
	owner := m.autoLeaseOwner
	release := func() tea.Msg {
		if database != nil && owner != "" {
			if scope != "" {
				_ = db.ReleaseAutoLease(database, scope, owner)
			}
			if quickScope != "" {
				_ = db.ReleaseAutoLease(database, quickScope, owner)
			}
		}
		return listQuitNowMsg{}
	}
	capCmd := tea.Tick(listQuitTeardownCap, func(time.Time) tea.Msg { return listQuitNowMsg{} })
	return tea.Batch(release, capCmd)
}

// renderQuittingView is shown while quit teardown runs.
func (m listTUIModel) renderQuittingView() string {
	const msg = "Quitting…"
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	}
	return msg
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
	if m.quitting {
		// Teardown in progress: ignore input and other messages; only the quit
		// signal (release done, or cap elapsed) ends the program.
		if _, ok := msg.(listQuitNowMsg); ok {
			return m, tea.Quit
		}
		return m, nil
	}
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
		if m.showJobDiagnosis {
			page := max(1, m.height-3)
			switch msg.String() {
			case "z", "esc", "q", "enter":
				m.showJobDiagnosis = false
			case "up", "k":
				m.jobDiagnosisScroll = max(0, m.jobDiagnosisScroll-1)
			case "down", "j":
				m.jobDiagnosisScroll = min(m.jobDiagnosisMaxScroll(), m.jobDiagnosisScroll+1)
			case "pgup", "b":
				m.jobDiagnosisScroll = max(0, m.jobDiagnosisScroll-page)
			case "pgdown", " ":
				m.jobDiagnosisScroll = min(m.jobDiagnosisMaxScroll(), m.jobDiagnosisScroll+page)
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

	case flash.ExpiredMsg:
		m.flash.HandleExpired()
		return m, nil

	case clipboardCopiedMsg:
		text, isError := msg.flashText()
		return m, m.flash.Set(text, isError)

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
		m.reloadInProgress = false
		trailingReload := m.reloadPending
		m.reloadPending = false
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			if trailingReload {
				return m, m.requestReloadJobs()
			}
			return m, nil
		}
		m.jobs = m.visibleJobsForLoadedMsg(msg.jobs)
		m.launchLiveByID = msg.launchLiveByID
		m.launchStatusByID = msg.launchStatusByID
		m.placingJobIDs = msg.placingJobIDs
		m.placementQueuedAtByJob = msg.placementQueuedAtByJob
		m.placementStatusByJob = msg.placementStatusByJob
		m.attemptOutcomeByJob = msg.attemptOutcomeByJob
		m.launchByID = msg.launchByID
		m.externalBindingByJobID = msg.externalBindingByJobID
		m.launchBootstrapP50 = msg.launchBootstrapP50
		m.launchBootstrapDurations = msg.launchBootstrapDurations
		m.launchBootstrapSamples = msg.launchBootstrapSamples
		m.launchStageETAByName = msg.launchStageETAByName
		m.launchStageEnteredAtByID = msg.launchStageEnteredAtByID
		m.recentFailedInstances = msg.recentFailedInstances
		m.placementDaemonStopped = msg.placementDaemonStopped
		m.hostInfoByName = msg.hostInfoByName
		m.cordonedHostsByName = msg.cordonedHostsByName
		m.overloadedHostsByName = msg.overloadedHostsByName
		m.autoPassPhase = msg.autoPassPhase
		m.autopilotPaused = msg.autopilotPaused
		m.autopilotPausedReason = msg.autopilotPausedReason
		m.autopilotActive = msg.autopilotActive
		m.autopilotPassStartedAt = msg.autopilotPassStartedAt
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
		if trailingReload {
			cmds = append(cmds, m.requestReloadJobs())
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
			return m, tea.Batch(m.requestReloadJobs(), m.runBackgroundSync(true))
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
		return m, m.requestReloadJobs()

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
		cmds := []tea.Cmd{m.requestReloadJobs()}
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
			m.requestReloadJobs(),
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
			cmds = append(cmds, m.requestReloadJobs())
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
		if msg.upgradePending {
			m.replaceAutoBlockOverlay(nil, nil)
			m.lastAutoPilotErrorRaw = ""
			m.showAutoPilotErrorDetails = false
			m.clearAutoPilotPersistentState()
			m.autoUpgradePending = true
			m.autoNextPassAt = time.Time{}
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot handed off to upgraded Weft daemon"
			}
			m.rebuildGroupedRows()
			return m, nil
		}
		if msg.err != nil {
			m.replaceAutoBlockOverlay(nil, nil)
			m.lastAutoPilotErrorRaw = msg.err.Error()
			m.showAutoPilotErrorDetails = false
			m.autoPersistentError = orchestration.SummarizeAutoPilotError(msg.err)
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot failed: " + orchestration.SummarizeAutoPilotError(msg.err)
			}
			m.recordAutoPilotPassOutcome(nil, msg.err)
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
			m.recordAutoPilotPassOutcome(nil, orchestration.ErrAutopilotPaused)
			return m, nil
		}
		if msg.anotherHolding {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot: another runner is active"
			}
			m.recordAutoPilotPassOutcome(nil, orchestration.ErrAutopilotBusy)
			return m, nil
		}
		m.replaceAutoBlockOverlay(msg.blockedReasons, msg.structuredBlocked)
		m.autoPersistentBlocked = ""
		m.autoPersistentBlockedN = 0
		if summary, count := orchestration.AutoPilotBlockSummaryWithCount(msg.blockedReasons); summary != "" {
			m.autoPersistentBlocked = summary
			m.autoPersistentBlockedN = count
		}
		m.recordAutoPilotPassOutcome(&orchestration.GroupedAutoPilotResult{
			Placed:         msg.placed,
			Rebalanced:     msg.rebalanced,
			Launched:       msg.launched,
			BlockedReasons: msg.blockedReasons,
		}, nil)
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
		return m, m.requestReloadJobs()

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
		return m, m.requestReloadJobs()

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
		return m, m.requestReloadJobs()

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
		return m, m.requestReloadJobs()

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
		return m, m.requestReloadJobs()

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
			return m, m.requestReloadJobs()
		}
		m.statusMessage = fmt.Sprintf("Moved job #%d to %s", msg.jobID, msg.targetDesc)
		return m, m.requestReloadJobs()

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
		cmds := []tea.Cmd{m.requestReloadJobs()}
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
		return m, m.requestReloadJobs()

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
		m.statusMessage = fmt.Sprintf("Opened %s billing", providerNameForBillingURL(msg.url))
		return m, nil

	case listJobDiagnosisLoadedMsg:
		m.jobDiagnosisLoading = false
		if msg.err != nil && strings.TrimSpace(msg.output) == "" {
			m.statusMessage = fmt.Sprintf("Diagnosis for job #%d failed: %v", msg.jobID, msg.err)
			return m, nil
		}
		m.showJobDiagnosis = true
		m.jobDiagnosisJobID = msg.jobID
		m.jobDiagnosisLines = strings.Split(strings.TrimRight(msg.output, "\n"), "\n")
		m.jobDiagnosisScroll = 0
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
	if m.quitting {
		return m.renderQuittingView()
	}
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
	if m.showJobDiagnosis {
		return m.renderJobDiagnosisView()
	}
	if m.isGroupedView() {
		return m.groupedView()
	}

	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	return m.cachePlan(m.buildFlatScreenPlan()).Render()
}

// flatFrameParts is the flat frame's footer content plus the body-row budget that
// survives reserving it. One definition serves both the renderer and the scroller,
// so the rows the frame draws and the rows the scroller pages by cannot disagree.
type flatFrameParts struct {
	sharedStatus        sharedTUIStatusLinesView
	selectedDetailLines []string
	promptLines         []string
	bodyRows            int
}

func (m listTUIModel) flatFrameParts() flatFrameParts {
	parts := flatFrameParts{sharedStatus: emptySharedTUIStatusLinesView()}
	if !m.hideStatusArea {
		parts.sharedStatus = renderSharedTUIStatusLinesView(m.database, m.width, -1, m.autoRunRateTargetCents, false)
		parts.selectedDetailLines = m.selectedJobDetailLines()
	}
	if m.projectInputActive {
		parts.promptLines = m.projectFilterPromptLines()
	}
	// Footer block: the controls line plus everything drawn below the separator.
	footerBlock := 1 + len(parts.promptLines) + len(parts.selectedDetailLines) + len(parts.sharedStatus.lines)
	parts.bodyRows = max(0, m.height-2-1-footerBlock)
	return parts
}

// flatBodyHeight returns the flat body's row budget; the viewport scroller
// pages by it.
func (m listTUIModel) flatBodyHeight() int {
	return m.flatFrameParts().bodyRows
}

// buildFlatScreenPlan composes the flat frame as an ordered list of screen
// lines: index in lines == screen Y.
func (m listTUIModel) buildFlatScreenPlan() screenPlan {
	layout := m.layout
	parts := m.flatFrameParts()
	bodyRows := parts.bodyRows
	mateRows, matesActive := hostMatesForFlatView(m.jobs, m.cursor)
	rangeLo, rangeHi, rangeActive := m.rangeSelectionSpan()
	rowWidth := m.width
	plan := newScreenPlan(m.width, m.height)

	title := fmt.Sprintf("%s (%d)", m.displayTitle(), len(m.jobs))
	plan.Add(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)), -1, clickTarget{})
	header := truncateDisplayWidth(formatJobListHeader(layout), rowWidth)
	plan.Add(listTUIHeaderStyle.Render(truncateDisplayWidth(header, m.width)), -1, clickTarget{})

	if len(m.jobs) == 0 {
		plan.Add(listTUIEmptyStyle.Render(truncateDisplayWidth(m.emptyStateText(), m.width)), -1, clickTarget{})
		for i := 1; i < bodyRows; i++ {
			plan.Add("", -1, clickTarget{})
		}
	} else {
		for i := 0; i < bodyRows; i++ {
			idx := m.offset + i
			if idx >= len(m.jobs) {
				plan.Add("", -1, clickTarget{})
				continue
			}
			row := truncateDisplayWidth(formatJobListRow(layout, m.jobs[idx]), rowWidth)
			if matesActive && (idx == m.cursor || mateRows[idx]) {
				row = applyHostMateMarkerForJob(row, m.jobs[idx])
			}
			if idx == m.cursor || (rangeActive && idx >= rangeLo && idx <= rangeHi) {
				row = renderSelectedRow(row, m.width)
			}
			plan.Add(row, idx, clickTarget{kind: targetSelectRow, rowIdx: idx})
		}
	}

	// Always keep a visible separator above the status/footer block.
	plan.Add("", -1, clickTarget{})
	for _, line := range parts.promptLines {
		plan.Add(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)), -1, clickTarget{})
	}
	for _, line := range parts.selectedDetailLines {
		text, target := m.selectedDetailPlanLine(line)
		plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(text, m.width)), -1, target)
	}
	for i := range parts.sharedStatus.lines {
		line, target := sharedStatusPlanLine(parts.sharedStatus, i, m.width)
		plan.Add(line, -1, target)
	}
	// The flat footer mixes a transient flash with the controls legend; only a
	// showing flash makes the whole line dismiss-on-click.
	footerTarget := clickTarget{}
	if m.flash.Render() != "" {
		footerTarget = clickTarget{kind: targetDismissStatus}
	}
	plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(m.statusLineText(m.footerText(bodyRows)), m.width)), -1, footerTarget)
	return plan
}

// sharedStatusLineTarget returns the click target for shared status line i, or an
// inert target when that line offers no action.
func sharedStatusLineTarget(status sharedTUIStatusLinesView, i int) clickTarget {
	switch {
	case status.daemonActionable && status.daemonLineIndex == i:
		return clickTarget{kind: targetRestartDaemon, label: "daemon"}
	case status.vastCreditWarningActionable && status.vastCreditWarningLineIndex == i:
		return clickTarget{kind: targetOpenURL, url: status.creditWarningBillingURL, label: status.creditWarningProviderName + " billing"}
	case status.systemLineIndex >= 0 && status.systemLineIndex == i:
		return clickTarget{kind: targetInstances, label: "instances"}
	case status.pausedBannerLineIndex >= 0 && status.pausedBannerLineIndex == i:
		// The banner warns about paused instances; the instances view is where
		// they are inspected. Resume/destroy stay keyboard-only: both are
		// mutations with money attached.
		return clickTarget{kind: targetInstances, label: "instances"}
	default:
		return clickTarget{}
	}
}

// sharedStatusPlanLine resolves shared status line i's click target and, when
// the target is one a first-time viewer could not guess, appends its hint.
func sharedStatusPlanLine(status sharedTUIStatusLinesView, i, width int) (string, clickTarget) {
	target := sharedStatusLineTarget(status, i)
	line := status.lines[i]
	if target.kind == targetInstances {
		line = truncateDisplayWidth(line+" ("+listKeyInstances.footerToken()+")", width)
	}
	return line, target
}

// selectedDetailPlanLine resolves the click target for one selected-job footer
// line ("Job:", "Host:") and appends the hint that advertises it. Lines that
// name nothing actionable — "Move:" above all, where the tempting verbs would
// mutate a job with a pending move — stay inert and unhinted.
func (m listTUIModel) selectedDetailPlanLine(line string) (string, clickTarget) {
	job := m.currentSelectedJob()
	if job == nil {
		return line, clickTarget{}
	}
	switch {
	case strings.HasPrefix(line, "Job: "):
		return line + " (" + listKeyDiagnose.footerToken() + ")", clickTarget{kind: targetDiagnose}
	case strings.HasPrefix(line, "Host: "):
		return m.hostDetailPlanLine(line, job)
	default:
		return line, clickTarget{}
	}
}

// hostDetailPlanLine resolves the "Host:" footer line's target from the
// selected job's placement kind: rental instances travel to the instances
// view (or the failures overlay when the launch failed), inventory hosts to
// the hosts view, and a SkyPilot external job opens its dashboard URL.
func (m listTUIModel) hostDetailPlanLine(line string, job *db.Job) (string, clickTarget) {
	switch job.TargetKind() {
	case db.JobTargetInventoryHost:
		return line + " (" + listKeyHosts.footerToken() + ")", clickTarget{kind: targetHosts}
	case db.JobTargetExternal:
		binding := m.externalBindingByJobID[job.ID]
		if binding == nil || strings.TrimSpace(binding.DashboardURL) == "" {
			return line, clickTarget{}
		}
		return line + " (click: dashboard)", clickTarget{kind: targetOpenURL, url: binding.DashboardURL, label: "SkyPilot dashboard"}
	case db.JobTargetRentalInstance:
		var launch *db.Launch
		if job.LaunchID != nil {
			launch = m.launchByID[*job.LaunchID]
		}
		if launch != nil && launch.Status == db.LaunchStatusFailed {
			// The f overlay only has content when recent failures were
			// summarized, so the hint and the target gate on it together: a
			// hint that lies about clickability is worse than none.
			if m.hasRecentInstanceFailures() {
				return line + " (f:diagnose)", clickTarget{kind: targetInstanceFailures}
			}
			return line, clickTarget{}
		}
		return line + " (" + listKeyInstances.footerToken() + ")", clickTarget{kind: targetInstances}
	default:
		return line, clickTarget{}
	}
}

// autoPilotLineAction resolves the Auto-pilot status line's click target and
// its inline hint. Toggling the autopilot is deliberately never one of the
// outcomes: pausing placement produces no visible failure, and the symptom
// surfaces minutes later looking like a weft bug rather than a misclick.
func (m listTUIModel) autoPilotLineAction() (clickTarget, string) {
	// "Reporting an error" is what the line renders (autoPersistentError); the
	// e toggle additionally needs the raw error, and the two are set and
	// cleared together on pass completion.
	if strings.TrimSpace(m.autoPersistentError) != "" && strings.TrimSpace(m.lastAutoPilotErrorRaw) != "" {
		return clickTarget{kind: targetAutoErrorToggle}, " (e:details)"
	}
	if m.hasExpandableAutoBlockers() {
		return clickTarget{kind: targetExpandAllBlockers}, " (O:expand)"
	}
	return clickTarget{}, ""
}

// controlsLineKeyExcludedFromClick reports whether a controls-line token stays
// keyboard-only. The controls line is the deliberate carve-out where a click
// may act on something other than selection or navigation — it is a legend,
// not a statement about the system — but even here the destructive tokens
// (kill/cancel fires immediately, with no confirmation) and the tokens that
// launch instances, spend money, or change a policy or budget are excluded.
// This carve-out is deliberate: do not generalize it into "clicks may mutate".
func controlsLineKeyExcludedFromClick(key string) bool {
	switch key {
	case listKeyKillCancel.keys, // x: kill/cancel
		listKeyUnplace.keys,         // u: unplace
		listKeyToggleProcessed.keys, // p: processed tag
		listKeyMove.keys,            // m: move
		listKeyLaunchSelected.keys,  // N: launch for selected
		listKeyLaunchQueued.keys,    // n: launch
		listKeyPriority.keys,        // P: priority
		listKeyRebalance.keys,       // R: rebalance
		listKeyGroupedAuto.keys,     // A: autopilot toggle
		listKeyToggleCordon.keys:    // C: cordon
		return true
	}
	return false
}

// controlsLineSpans resolves the clickable key:action tokens on the controls
// line. The line is already display-truncated, so a token cut by the ellipsis
// gets no span.
func controlsLineSpans(line string) []hitSpan {
	var spans []hitSpan
	col := 0
	for _, token := range strings.Split(line, "  ") {
		width := lipgloss.Width(token)
		if idx := strings.Index(token, ":"); idx > 0 && !strings.Contains(token, "…") {
			if key := token[:idx]; !controlsLineKeyExcludedFromClick(key) {
				spans = append(spans, hitSpan{StartCol: col, EndCol: col + width, Target: clickTarget{kind: targetControlsKey, key: key}})
			}
		}
		col += width + 2
	}
	return spans
}

// jobIDCopySpan locates the job ID in a rendered grouped-status row and
// returns the click span that copies it. The span is anchored at column 0
// with two cells of right padding: the ⌂ and ☁ placement glyphs are East
// Asian Ambiguous, so terminals disagree on their width by one cell, and a
// span computed from the assumed glyph width could sit one cell off from what
// was drawn — silently selecting when the user meant to copy. The decoration
// left of the ID has no competing target and the " — " after it is dead
// space, so glyph-width drift moves the ID within the span, never outside it.
func jobIDCopySpan(line string, job *db.Job, width int) (hitSpan, bool) {
	plain := ansi.Strip(line)
	id := ids.FormatJobID(job.ID)
	idx := strings.Index(plain, id)
	if idx < 0 {
		return hitSpan{}, false
	}
	endCol := lipgloss.Width(plain[:idx]) + lipgloss.Width(id) + 2
	if width > 0 && endCol > width {
		endCol = width
	}
	return hitSpan{StartCol: 0, EndCol: endCol, Target: clickTarget{kind: targetCopy, label: id, payload: id}}, true
}

// cachePlan records the plan a View() just composed so hit testing resolves clicks
// against the frame the user actually saw. Models built as bare struct literals in
// tests have no cache and simply skip the record.
func (m listTUIModel) cachePlan(plan screenPlan) screenPlan {
	if m.planCache != nil {
		*m.planCache = plan
	}
	return plan
}

// buildScreenPlan builds the plan for the frame hit testing resolves against.
func (m listTUIModel) buildScreenPlan() screenPlan {
	if m.isGroupedView() {
		return m.buildGroupedScreenPlan()
	}
	return m.buildFlatScreenPlan()
}

// currentPlan returns the plan from the most recent View() when it was composed for
// the current terminal size, and rebuilds otherwise. Models built as bare struct
// literals in tests have no cache and always rebuild.
func (m listTUIModel) currentPlan() screenPlan {
	if m.planCache != nil && m.planCache.Width == m.width && m.planCache.Height == m.height && len(m.planCache.Lines) > 0 {
		return *m.planCache
	}
	return m.buildScreenPlan()
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
		title = strings.TrimSpace(title + " • unprocessed")
	}
	if !m.mouseEnabled {
		title = strings.TrimSpace(title + " • mouse off (M to re-enable)")
	}
	return title
}

func (m listTUIModel) groupedView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	return m.cachePlan(m.buildGroupedScreenPlan()).Render()
}

// groupedRowClickable reports whether a click on the row's line should move the
// cursor to it. It is the single definition shared by row selection and the
// groupedSelectableRows index.
func groupedRowClickable(row groupedStatusRow) bool {
	if row.expandToggle != "" {
		return true
	}
	// Collapsible section headers are cursor stops so enter can toggle them.
	if row.isHeader {
		return row.collapsible
	}
	return (row.job != nil || row.launch != nil) && !row.isBlocked
}

// groupedRowClickTarget resolves the click target for a grouped body row.
func groupedRowClickTarget(row groupedStatusRow, rowIdx int) clickTarget {
	if row.isHeader && row.collapsible {
		return clickTarget{kind: targetToggleSection, toggle: row.section}
	}
	// A disclosure detail row collapses the per-avenue breakdown it belongs
	// to, duplicating left on the parent job row.
	if row.style == groupedStatusRowStyleDisclosure && row.parentJobID != 0 {
		return clickTarget{kind: targetCollapseBlocked, jobID: row.parentJobID}
	}
	// An incident rollup jumps to its sample job and opens that job's blocker
	// detail rather than copying: the rollup summarizes, it is not the thing
	// to paste into a bug report.
	if row.incidentFingerprint != "" {
		return clickTarget{kind: targetIncidentJump, rowIdx: rowIdx}
	}
	if groupedRowClickable(row) {
		return clickTarget{kind: targetSelectRow, rowIdx: rowIdx}
	}
	// Blocked/waiting reason bucket headers and the shared launch-blocker
	// hoist are refused by row selection (row.isBlocked), so a click would
	// otherwise be a silent no-op; the copy target gives it a meaning.
	if len(row.jobIDs) > 0 {
		return clickTarget{kind: targetCopy, rowIdx: rowIdx, label: "blocking status"}
	}
	return clickTarget{}
}

// buildGroupedScreenPlan composes the grouped frame as an ordered list of
// screen lines: index in lines == screen Y.
func (m listTUIModel) buildGroupedScreenPlan() screenPlan {
	rows := m.groupedRows
	layout := m.buildGroupedViewLayout(rows, m.groupedJobsWithAutoReasons())
	selectedRow := m.selectedGroupedRow()
	rangeRows := m.rangeSelectedRowIdxs()
	mateRows, matesActive := hostMatesForGroupedRows(rows, selectedRow)
	rowWidth := m.width
	plan := newScreenPlan(m.width, m.height)

	title := fmt.Sprintf("%s (%d) • group:%s", m.displayTitle(), len(m.jobs), listGroupModeLabel(m.effectiveGroupMode()))
	plan.Add(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)), -1, clickTarget{})

	bodyLinesWritten := 0
	if len(rows) == 0 {
		// Empty view: the message goes in the body — under the title, separated
		// by a blank line — where jobs would otherwise be listed.
		plan.Add("", -1, clickTarget{})
		plan.Add(listTUIEmptyStyle.Render(truncateDisplayWidth(m.emptyStateBaseText(), m.width)), -1, clickTarget{})
		bodyLinesWritten = 2
	}
	for _, row := range layout.visibleRows {
		line := truncateDisplayWidth(row.text, rowWidth)
		var sourceRow *groupedStatusRow
		if row.rowIdx >= 0 && row.rowIdx < len(m.groupedRows) {
			sourceRow = &m.groupedRows[row.rowIdx]
		}
		if matesActive && row.rowIdx >= 0 && row.rowIdx < len(m.groupedRows) {
			if rj := m.groupedRows[row.rowIdx].job; rj != nil && mateRows[row.rowIdx] {
				line = applyHostMateMarkerForJob(line, rj)
			}
		}
		if selectedRow >= 0 && row.rowIdx >= 0 && (row.rowIdx == selectedRow || rangeRows[row.rowIdx]) {
			if matesActive {
				line = applyHostMateMarkerForJob(line, m.groupedRows[row.rowIdx].job)
			}
			line = renderSelectedGroupedRow(line, m.groupedRows[row.rowIdx].job, m.width)
		} else if sourceRow != nil {
			styled := *sourceRow
			styled.text = line
			line = styled.renderText()
		}
		target := clickTarget{}
		if sourceRow != nil {
			target = groupedRowClickTarget(*sourceRow, row.rowIdx)
		}
		plan.Add(line, row.rowIdx, target)
		if sourceRow != nil && sourceRow.job != nil && m.isStatusGroupedView() {
			if span, ok := jobIDCopySpan(line, sourceRow.job, m.width); ok {
				plan.AddSpan(span)
			}
		}
		bodyLinesWritten++
	}
	for ; bodyLinesWritten < layout.maxBodyLines; bodyLinesWritten++ {
		plan.Add("", -1, clickTarget{})
	}

	// Visually separate grouped job rows from footer lines.
	plan.Add("", -1, clickTarget{})
	if m.projectInputActive {
		for _, line := range m.projectFilterPromptLines() {
			plan.Add(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)), -1, clickTarget{})
		}
	}
	for _, line := range layout.errorDetailsLines {
		// The disclosed error-details block hides on click, duplicating e.
		plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(line, m.width)), -1, clickTarget{kind: targetAutoErrorToggle})
	}
	for _, line := range layout.selectedDetails {
		text, target := m.selectedDetailPlanLine(line)
		plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(text, m.width)), -1, target)
	}
	if layout.statusLine != "" {
		plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(layout.statusLine, m.width)), -1, clickTarget{kind: targetDismissStatus})
	}
	healthTarget := clickTarget{}
	if m.hasRecentInstanceFailures() {
		healthTarget = clickTarget{kind: targetInstanceFailures}
	}
	for _, line := range layout.instanceHealthLines {
		plan.Add(line, -1, healthTarget)
	}
	for i := range layout.sharedStatus.lines {
		line, target := sharedStatusPlanLine(layout.sharedStatus, i, m.width)
		plan.Add(line, -1, target)
	}
	if layout.autoPilotLine != "" {
		plan.Add(listTUIFooterStyle.Render(truncateDisplayWidth(layout.autoPilotLine, m.width)), -1, layout.autoPilotTarget)
	}
	for _, line := range layout.budgetPanelLines {
		plan.Add(listTUIPromptStyle.Render(truncateDisplayWidth(line, m.width)), -1, clickTarget{})
	}
	controlsText := truncateDisplayWidth(layout.controlsLine, m.width)
	plan.Add(listTUIFooterStyle.Render(controlsText), -1, clickTarget{})
	for _, span := range controlsLineSpans(controlsText) {
		plan.AddSpan(span)
	}
	return plan
}

func (m listTUIModel) buildGroupedViewLayout(rows []groupedStatusRow, groupedJobs []*db.Job) groupedViewLayout {
	// Build footer lines first so we can reserve space for them.
	// Per-job ETA now lives on the "Job:" line in selectedDetailLines, so
	// no standalone campaign-wide ETA line is rendered.
	eta := computeGroupedETA(groupedJobs, m.launchLiveByID, time.Now())
	statusLine := m.statusLineText(m.groupedStatusText())
	visibleRunning := countVisibleRunningJobs(groupedJobs)
	sharedStatus := renderSharedTUIStatusLinesView(m.database, m.width, visibleRunning, m.autoRunRateTargetCents, m.isUJGroupedView())
	autoPilotLine := m.groupedAutoPilotStatusText(visibleRunning)
	autoPilotTarget := clickTarget{}
	if autoPilotLine != "" {
		var hint string
		autoPilotTarget, hint = m.autoPilotLineAction()
		autoPilotLine += hint
	}
	errorDetailsLines := m.groupedErrorDetailsLines()
	selectedDetailLines := m.selectedJobDetailLines()
	instanceHealthLines := buildInstanceHealthFooter(m.recentFailedInstances, m.width, time.Now()).lines
	if m.hideStatusArea {
		statusLine = ""
		sharedStatus = emptySharedTUIStatusLinesView()
		autoPilotLine = ""
		autoPilotTarget = clickTarget{}
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
	return groupedViewLayout{
		visibleRows:         selectGroupedRowsForViewport(rows, maxBodyLines, m.selectedGroupedRow()),
		statusLine:          statusLine,
		sharedStatus:        sharedStatus,
		autoPilotLine:       autoPilotLine,
		autoPilotTarget:     autoPilotTarget,
		errorDetailsLines:   errorDetailsLines,
		instanceHealthLines: instanceHealthLines,
		selectedDetails:     selectedDetailLines,
		budgetPanelLines:    budgetPanelLines,
		controlsLine:        m.groupedControlsText(eta.HasQueued),
		maxBodyLines:        maxBodyLines,
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

func (m listTUIModel) jobDiagnosisLinesRendered() []string {
	out := make([]string, 0, len(m.jobDiagnosisLines))
	for _, ln := range m.jobDiagnosisLines {
		out = append(out, truncateDisplayWidth(ln, m.width))
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (m listTUIModel) jobDiagnosisBodyHeight() int {
	return max(1, m.height-2) // title + footer
}

func (m listTUIModel) jobDiagnosisMaxScroll() int {
	return max(0, len(m.jobDiagnosisLinesRendered())-m.jobDiagnosisBodyHeight())
}

// renderJobDiagnosisView is the read-only `z` diagnose overlay: a scrollable
// view of the selected job's placement / blocker diagnosis, with esc/q/z to
// return to the list.
func (m listTUIModel) renderJobDiagnosisView() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}
	lines := m.jobDiagnosisLinesRendered()
	bodyHeight := m.jobDiagnosisBodyHeight()
	maxScroll := max(0, len(lines)-bodyHeight)
	scroll := min(m.jobDiagnosisScroll, maxScroll)
	end := min(len(lines), scroll+bodyHeight)

	var b strings.Builder
	title := fmt.Sprintf("Diagnosis for %s", ids.FormatJobID(m.jobDiagnosisJobID))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")
	if m.jobDiagnosisLoading {
		b.WriteString("Loading...")
		b.WriteString("\n")
		shown := 1
		for ; shown < bodyHeight; shown++ {
			b.WriteString("\n")
		}
	} else {
		shown := 0
		for _, ln := range lines[scroll:end] {
			b.WriteString(ln)
			b.WriteString("\n")
			shown++
		}
		for ; shown < bodyHeight; shown++ {
			b.WriteString("\n")
		}
	}
	foot := "esc/q/z back · ↑/↓ scroll"
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

// statusLineText returns the flash while one is showing, else the derived
// status text. The flash covers the status line without touching
// statusMessage, so a pinned quick-launch status reappears intact when the
// flash expires.
func (m listTUIModel) statusLineText(derived string) string {
	if rendered := m.flash.Render(); rendered != "" {
		return rendered
	}
	return derived
}

func (m *listTUIModel) clearAutoPilotPersistentState() {
	m.autoPersistentError = ""
	m.autoPersistentBlocked = ""
	m.autoPersistentBlockedN = 0
}

func (m *listTUIModel) resumeAutoPilotNow() {
	m.autoNextPassAt = time.Time{}
	// A resume follows a user action that changed autopilot inputs (budget,
	// breaker), so the next tick must replan even if the database has not
	// moved since the last pass.
	m.autoTimerRunsPass = true
}

// recordAutoPilotPassOutcome applies the shared dispatch policy to a finished
// pass: it sets the cooldown and records whether the cooldown expiring is by
// itself a reason to run the next one.
func (m *listTUIModel) recordAutoPilotPassOutcome(result *orchestration.GroupedAutoPilotResult, passErr error) {
	outcome, cooldown := orchestration.ClassifyPass(result, passErr, orchestration.AutopilotCooldownContend)
	if outcome == orchestration.OutcomeProgress {
		m.clearAutoPilotPersistentState()
	}
	var blockedReasons map[int64]string
	if result != nil {
		blockedReasons = result.BlockedReasons
	}
	m.autoNextPassAt = time.Now().Add(cooldown)
	m.autoTimerRunsPass = orchestration.TimerRunsPass(outcome, blockedReasons)
	if snapshot, err := orchestration.ReadWakeSnapshot(m.database); err == nil {
		m.autoWakeSnapshot = snapshot
	}
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
	autopilotActive, autopilotPassStartedAt := m.pendingPlacementPassState(time.Now())
	// The list TUI has two blocked-reason sources: the persistent summary
	// from the last pass, and reasons hydrated onto the visible unplaced
	// jobs. Prefer the former (it carries a job count); fall back to the
	// latter. The shared formatter renders whichever the caller supplies.
	blockedSummary := strings.TrimSpace(m.autoPersistentBlocked)
	blockedJobs := m.autoPersistentBlockedN
	if blockedSummary == "" {
		if summary, count := orchestration.AutoPilotBlockSummaryWithCount(m.visibleUnplacedBlockedReasons()); summary != "" {
			blockedSummary = summary
			blockedJobs = count
		}
	}
	return autopilotStatusLine(autopilotDisplayInput{
		inputActive:     m.autoRunRateInputActive,
		paused:          m.autopilotPaused,
		pausedReason:    m.autopilotPausedReason,
		syncing:         m.syncInProgress(),
		syncHosts:       m.pendingHostList(),
		inFlight:        autopilotActive,
		passPhase:       m.autoPassPhase,
		passStartedAt:   autopilotPassStartedAt,
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
		line += "  " + listKeyDiagnose.footerToken()
		line += "  " + listKeyAttempts.footerToken()
		line += "  " + listKeyKillCancel.footerToken()
		if selected := m.selectedGroupedJob(); selected != nil && selected.TargetKind() != db.JobTargetExternal {
			line += "  " + listKeyUnplace.footerToken()
		}
		line += "  " + listKeyToggleProcessed.footerToken()
		if selected := m.selectedGroupedJob(); m.isStatusGroupedView() && selected != nil && selected.TargetKind() != db.JobTargetExternal && selected.EffectiveStatus() == db.StatusQueued {
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
	line += "  " + listKeyCopyID.footerToken()
	line += "  " + listKeyCopyDetails.footerToken()
	line += "  " + listKeyToggleMouse.footerToken()
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
		launchLiveByID:         m.launchLiveByID,
		launchByID:             m.launchByID,
		hostMetricsByName:      m.hostMetricsByName,
		hostInfoByName:         m.hostInfoByName,
		cordonedHostsByName:    m.cordonedHostsByName,
		overloadedHostsByName:  m.overloadedHostsByName,
		siblingJobs:            m.jobs,
		moveByJob:              moveDisplayMap(m.placementStatusByJob),
		openIntentJobIDs:       openIntentJobIDMap(m.placementStatusByJob),
		externalBindingByJobID: m.externalBindingByJobID,
		cloudConfigured:        len(m.cloudClients) > 0,
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

func (m listTUIModel) selectedGroupedSelectionKey() groupedSelectionKey {
	rowIdx := m.selectedGroupedRow()
	if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
		return groupedSelectionKey{}
	}
	return groupedRowSelectionKey(m.groupedRows[rowIdx])
}

func groupedRowSelectionKey(row groupedStatusRow) groupedSelectionKey {
	if row.job != nil {
		return groupedSelectionKey{kind: "job", id: row.job.ID}
	}
	if row.launch != nil {
		return groupedSelectionKey{kind: "launch", id: row.launch.ID}
	}
	if row.expandToggle != "" {
		return groupedSelectionKey{kind: "expand", name: row.expandToggle}
	}
	if row.isHeader && row.collapsible {
		return groupedSelectionKey{kind: "section", name: row.section}
	}
	return groupedSelectionKey{}
}

func (k groupedSelectionKey) empty() bool {
	return k.kind == "" && k.id == 0 && k.name == ""
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
	if m.rebalancePreview.active || m.movePicker.active || m.aiAssist != nil || m.showHelp || m.showInstanceFailures || m.showJobDiagnosis || m.autoRunRateInputActive {
		m.dragging = false
		return m, nil
	}
	switch {
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		target, _ := m.currentPlan().Hit(msg.X, msg.Y)
		m.selRangeActive = false
		m.dragging = false
		if pos := m.selectablePositionForRowIdx(target.rowIdx); target.kind == targetSelectRow && pos >= 0 {
			m.selAnchor = pos
			m.dragging = true
		}
		return m, m.dispatchTarget(target)
	case m.dragging && msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionMotion:
		// A drag extends the selection to the row under the cursor. The row
		// comes from the plan's rowIdx (a model row), not the screen row, so a
		// repaint mid-drag cannot corrupt the range.
		if pos := m.selectablePositionForRowIdx(m.currentPlan().RowIdxAt(msg.Y)); pos >= 0 {
			m.cursor = pos
			m.selRangeActive = pos != m.selAnchor
			m.clampCursor()
			m.adjustOffset()
		}
		return m, nil
	case m.dragging && msg.Action == tea.MouseActionRelease:
		m.dragging = false
		return m, nil
	}
	return m, nil
}

// selectablePositionForRowIdx maps a plan row index to a cursor position in
// the current selection domain, or -1 when the row is not selectable.
func (m listTUIModel) selectablePositionForRowIdx(rowIdx int) int {
	if rowIdx < 0 {
		return -1
	}
	if !m.isGroupedView() {
		if rowIdx < len(m.jobs) {
			return rowIdx
		}
		return -1
	}
	for pos, ri := range m.groupedSelectableRows {
		if ri == rowIdx {
			return pos
		}
	}
	return -1
}

// rangeSelectionSpan returns the inclusive cursor-position span of the active
// drag selection, or false when the selection is a single row.
func (m listTUIModel) rangeSelectionSpan() (int, int, bool) {
	if !m.selRangeActive || m.selAnchor == m.cursor {
		return 0, 0, false
	}
	lo, hi := m.selAnchor, m.cursor
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi, true
}

// rangeSelectedRowIdxs returns the groupedRows indices the drag selection
// highlights: the body rows (jobs and launches) inside the span. Section
// headers and other non-body rows the drag crosses are never highlighted.
func (m listTUIModel) rangeSelectedRowIdxs() map[int]bool {
	lo, hi, ok := m.rangeSelectionSpan()
	if !ok || !m.isGroupedView() {
		return nil
	}
	rows := map[int]bool{}
	for pos := lo; pos <= hi && pos < len(m.groupedSelectableRows); pos++ {
		rowIdx := m.groupedSelectableRows[pos]
		if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
			continue
		}
		if row := m.groupedRows[rowIdx]; row.job != nil || row.launch != nil {
			rows[rowIdx] = true
		}
	}
	return rows
}

// rangeSelectedJobIDs returns the job IDs the drag selection covers, in
// display order, or nil when the selection is a single row.
func (m listTUIModel) rangeSelectedJobIDs() []int64 {
	lo, hi, ok := m.rangeSelectionSpan()
	if !ok {
		return nil
	}
	var jobIDs []int64
	if !m.isGroupedView() {
		for i := lo; i <= hi && i < len(m.jobs); i++ {
			if m.jobs[i] != nil {
				jobIDs = append(jobIDs, m.jobs[i].ID)
			}
		}
		return jobIDs
	}
	for pos := lo; pos <= hi && pos < len(m.groupedSelectableRows); pos++ {
		rowIdx := m.groupedSelectableRows[pos]
		if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
			continue
		}
		if job := m.groupedRows[rowIdx].job; job != nil {
			jobIDs = append(jobIDs, job.ID)
		}
	}
	return jobIDs
}

// copySelectionID is the y keybinding's action: copy the selected object's
// identifier — job ID, instance ID, or a blocked bucket's reason plus job IDs
// — or the compact job-ID list when a drag range is active.
func (m listTUIModel) copySelectionID() (listTUIModel, tea.Cmd) {
	if jobIDs := m.rangeSelectedJobIDs(); len(jobIDs) > 0 {
		return m, copyToClipboardCmd(pluralize(len(jobIDs), "job", "jobs"), ids.FormatJobIDListCompact(jobIDs))
	}
	if m.isGroupedView() {
		rowIdx := m.selectedGroupedRow()
		if rowIdx >= 0 && rowIdx < len(m.groupedRows) {
			row := m.groupedRows[rowIdx]
			switch {
			case row.job != nil:
				id := ids.FormatJobID(row.job.ID)
				return m, copyToClipboardCmd(id, id)
			case row.launch != nil:
				id := ids.FormatInstanceID(row.launch.ID)
				return m, copyToClipboardCmd(id, id)
			case len(row.jobIDs) > 0:
				return m, copyToClipboardCmd("blocking status", blockedBucketCopyPayload(row))
			}
		}
		m.statusMessage = "Select a job or instance row to copy"
		return m, nil
	}
	if job := m.currentSelectedJob(); job != nil {
		id := ids.FormatJobID(job.ID)
		return m, copyToClipboardCmd(id, id)
	}
	m.statusMessage = "Select a job row to copy"
	return m, nil
}

// copySelectionDetails is the Y keybinding's action: copy the selected
// object's footer detail block (the "Job:"/"Host:"/blocker lines) as plain
// text for pasting into a bug report.
func (m listTUIModel) copySelectionDetails() (listTUIModel, tea.Cmd) {
	lines := m.selectedJobDetailLines()
	if len(lines) == 0 {
		m.statusMessage = "No details to copy"
		return m, nil
	}
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansi.Strip(line)
	}
	return m, copyToClipboardCmd(pluralize(len(plain), "line", "lines"), strings.Join(plain, "\n"))
}

// dispatchTarget performs the action a resolved click target names. Clicks
// never destroy, never change a policy or budget, and never spend money: the
// mutating cases below only navigate, reveal or hide detail, copy an
// identifier, or re-invoke a keybinding from the controls line (which applies
// its own exclusion list).
func (m *listTUIModel) dispatchTarget(t clickTarget) tea.Cmd {
	switch t.kind {
	case targetSelectRow:
		if m.isGroupedView() {
			m.selectGroupedRowByIndex(t.rowIdx)
			return nil
		}
		if t.rowIdx >= 0 && t.rowIdx < len(m.jobs) {
			m.cursor = t.rowIdx
			m.clampCursor()
			m.adjustOffset()
		}
		return nil
	case targetToggleSection:
		m.toggleSectionCollapse(t.toggle)
		return nil
	case targetCollapseBlocked:
		if m.expandedBlocked[t.jobID] {
			delete(m.expandedBlocked, t.jobID)
			m.rebuildGroupedRows()
		}
		return nil
	case targetIncidentJump:
		if t.rowIdx < 0 || t.rowIdx >= len(m.groupedRows) {
			return nil
		}
		row := m.groupedRows[t.rowIdx]
		jobID := row.sampleJobID
		if jobID == 0 && len(row.jobIDs) > 0 {
			jobID = row.jobIDs[0]
		}
		if jobID == 0 {
			return nil
		}
		if m.expandedBlocked == nil {
			m.expandedBlocked = map[int64]bool{}
		}
		m.expandedBlocked[jobID] = true
		m.rebuildGroupedRows()
		// Selecting the row is enough to scroll it into view: the grouped
		// viewport always reveals the selected row.
		for i, r := range m.groupedRows {
			if r.job != nil && r.job.ID == jobID {
				m.selectGroupedRowByIndex(i)
				break
			}
		}
		return nil
	case targetRestartDaemon:
		if m.daemonRestartInProgress {
			return nil
		}
		m.daemonRestartInProgress = true
		m.statusMessage = "Restarting daemon..."
		return restartDaemonListCmd()
	case targetOpenURL:
		m.statusMessage = fmt.Sprintf("Opening %s...", t.label)
		return openURLListCmd(t.url)
	case targetCopy:
		if t.payload != "" {
			return copyToClipboardCmd(t.label, t.payload)
		}
		if t.rowIdx < 0 || t.rowIdx >= len(m.groupedRows) {
			return nil
		}
		return copyToClipboardCmd(t.label, blockedBucketCopyPayload(m.groupedRows[t.rowIdx]))
	case targetAutoErrorToggle:
		next, cmd := m.toggleAutoErrorDetails()
		*m = next
		return cmd
	case targetExpandAllBlockers:
		next, cmd := m.toggleExpandAllBlockers()
		*m = next
		return cmd
	case targetDiagnose:
		next, cmd := m.beginSelectedJobDiagnosis()
		*m = next
		return cmd
	case targetInstanceFailures:
		next, cmd := m.openInstanceFailuresOverlay()
		*m = next
		return cmd
	case targetInstances:
		return func() tea.Msg { return switchToSystemWatchMsg{} }
	case targetHosts:
		return func() tea.Msg { return switchToHostsMsg{} }
	case targetDismissStatus:
		m.flash.Clear()
		m.statusMessage = ""
		return nil
	case targetControlsKey:
		if controlsLineKeyExcludedFromClick(t.key) {
			return nil
		}
		next, cmd, handled := handleListKeyBinding(*m, t.key, listGroupedKeyBindings())
		if !handled {
			return nil
		}
		if nextList, ok := next.(listTUIModel); ok {
			*m = nextList
		}
		return cmd
	default:
		return nil
	}
}

// toggleSectionCollapse flips one grouped-status section between its full row
// list and its header-only collapsed form.
func (m *listTUIModel) toggleSectionCollapse(section string) {
	if section == "" {
		return
	}
	if m.collapsedSections == nil {
		m.collapsedSections = map[string]bool{}
	}
	if m.collapsedSections[section] {
		delete(m.collapsedSections, section)
	} else {
		m.collapsedSections[section] = true
	}
	m.rebuildGroupedRows()
}

// selectedCollapsibleSection returns the section key of the collapsible
// section header under the cursor, or "" when the cursor is on any other row.
func (m listTUIModel) selectedCollapsibleSection() string {
	rowIdx := m.selectedGroupedRow()
	if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
		return ""
	}
	row := m.groupedRows[rowIdx]
	if row.isHeader && row.collapsible {
		return row.section
	}
	return ""
}

// blockedBucketCopyPayload renders a blocked/waiting bucket header row as the
// two-line text to paste into a bug report: the reason, then the compact job
// ID list. The header's display count (" (N)") is redundant with the ID list,
// so it is stripped, as is the disclosure triangle the interactive render
// prefixes the header with.
func blockedBucketCopyPayload(row groupedStatusRow) string {
	text := strings.TrimSpace(row.text)
	text = strings.TrimPrefix(text, "▾ ")
	text = strings.TrimPrefix(text, "▸ ")
	if n := len(row.jobIDs); n > 1 {
		text = strings.TrimSuffix(text, fmt.Sprintf(" (%d)", n))
	}
	return text + "\n" + ids.FormatJobIDListCompact(row.jobIDs)
}

func (m *listTUIModel) selectGroupedRowByIndex(rowIdx int) {
	if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
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

func (m listTUIModel) isUJGroupedView() bool {
	return m.groupedByStatus && m.groupedUnprocessedView
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
		return m, m.requestReloadJobs()
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
	if job.TargetKind() == db.JobTargetExternal {
		m.statusMessage = fmt.Sprintf("Launch not available for externally managed job #%d", job.ID)
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
	return m, m.requestReloadJobs()
}

func (m listTUIModel) beginGroupedMove() (tea.Model, tea.Cmd) {
	job := m.selectedGroupedJob()
	if job == nil {
		return m, nil
	}
	if job.TargetKind() == db.JobTargetExternal {
		m.statusMessage = fmt.Sprintf("Move not available for externally managed job #%d", job.ID)
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
		listKeyDiagnose,
		listKeyToggleQueuedDraft,
		listKeyToggleUnprocessed,
		listKeyToggleProcessed,
		listKeyKillCancel,
		listKeyToggleCordon,
		listKeyPriority,
		listKeyCopyID,
		listKeyCopyDetails,
		listKeyToggleMouse,
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
		"  a attempts  z diagnose placement/job state",
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
	cmds := []tea.Cmd{m.requestReloadJobs()}
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
	selected := m.selectedGroupedSelectionKey()
	if m.isStatusGroupedView() {
		groupedJobs := m.groupedJobsWithAutoReasons()
		autopilotActive, autopilotPassStartedAt := m.pendingPlacementPassState(time.Now())
		m.groupedRows = buildGroupedStatusRowsWithOptions(groupedJobs, m.width, groupedStatusRenderOptions{
			launchLiveByID:         m.launchLiveByID,
			launchStatusByID:       m.launchStatusByID,
			placingJobIDs:          m.placingJobIDs,
			placementQueuedAtByJob: m.placementQueuedAtByJob,
			placementStatusByJob:   m.placementStatusByJob,
			attemptOutcomeByJob:    m.attemptOutcomeByJob,
			overloadedHostsByName:  m.overloadedHostsByName,
			failedInstances:        m.recentFailedInstances,
			launchByID:             m.launchByID,
			now:                    time.Now(),
			launchSpinner:          m.launchSpinner.View(),
			launchingETA: groupedStatusLaunchingETA{
				totalP50:               m.launchBootstrapP50,
				totalDurations:         m.launchBootstrapDurations,
				totalSamples:           m.launchBootstrapSamples,
				stageByName:            m.launchStageETAByName,
				stageEnteredAtByLaunch: m.launchStageEnteredAtByID,
			},
			blockedDetail:              m.effectiveBlockedDetail(),
			expandedBlocked:            m.expandedBlocked,
			collapsedSections:          m.collapsedSections,
			interactive:                true,
			daemonStopped:              m.placementDaemonStopped,
			autopilotPaused:            m.autopilotPaused,
			autopilotActive:            autopilotActive,
			autopilotPassStartedAtUnix: unixIfNonZero(autopilotPassStartedAt),
		})
	} else {
		m.groupedRows = buildListGroupedRows(m.jobs, m.effectiveGroupMode(), m.width, m.layout)
	}
	m.groupedSelectableRows = m.groupedSelectableRows[:0]
	for i, row := range m.groupedRows {
		if groupedRowClickable(row) {
			m.groupedSelectableRows = append(m.groupedSelectableRows, i)
		}
	}
	if !selected.empty() && m.restoreGroupedSelection(selected) {
		return
	}
	m.clampGroupedCursor()
	m.preferJobRowOverHeader()
}

// preferJobRowOverHeader nudges a selection that landed on a section header
// to the first job or launch row: a header is a control, and the initial or
// fallback selection should name a job so the footer detail lines have a
// subject. A deliberate header selection carries a section selection key and
// is restored before this runs, so only keyless fallback selections move.
func (m *listTUIModel) preferJobRowOverHeader() {
	rowIdx := m.selectedGroupedRow()
	if rowIdx < 0 || rowIdx >= len(m.groupedRows) || !m.groupedRows[rowIdx].isHeader {
		return
	}
	for i, ri := range m.groupedSelectableRows {
		if ri >= 0 && ri < len(m.groupedRows) && !m.groupedRows[ri].isHeader {
			m.cursor = i
			return
		}
	}
}

func (m *listTUIModel) restoreGroupedSelection(selected groupedSelectionKey) bool {
	for cursorIdx, rowIdx := range m.groupedSelectableRows {
		if rowIdx < 0 || rowIdx >= len(m.groupedRows) {
			continue
		}
		if groupedRowSelectionKey(m.groupedRows[rowIdx]) == selected {
			m.cursor = cursorIdx
			m.clampGroupedCursor()
			return true
		}
	}
	return false
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
	return max(1, m.flatBodyHeight())
}

func (m *listTUIModel) requestReloadJobs() tea.Cmd {
	if m.reloadInProgress {
		m.reloadPending = true
		return nil
	}
	m.reloadInProgress = true
	return m.reloadJobs()
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
		placementStatusByJob, placementErr := jobview.PlacementStatusForJobs(database, jobs, time.Now())
		if placementErr != nil {
			placementStatusByJob = map[int64]jobview.PlacementStatus{}
		}
		displayJobs := jobview.ExpandJobsForOpenMoves(jobs, placementStatusByJob)
		attemptOutcomeByJob, outcomeErr := db.LatestAttemptOutcomeEvents(database, jobIDsForOutcomeEvents(jobs))
		if outcomeErr != nil {
			attemptOutcomeByJob = map[int64]db.AttemptOutcomeEvent{}
		}
		launchIDs := collectLaunchIDsForList(displayJobs, placementStatusByJob)
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
		bootstrapDurations, bootstrapErr := db.TotalBootstrapDurations(database)
		bootstrapP50, bootstrapSamples := time.Duration(0), 0
		if bootstrapErr != nil {
			bootstrapDurations = nil
		} else {
			bootstrapSamples = len(bootstrapDurations)
			bootstrapP50, _ = bootstrapDurations.Percentile(0.5)
		}
		if bootstrapP50 <= 0 {
			bootstrapP50 = 0
			bootstrapSamples = 0
		}
		stageETAByName, stageEnteredAtByID := loadLaunchingStageETA(database, displayJobs, launchLiveByID)
		placingJobIDs, placementQueuedAtByJob := placementDisplayMaps(placementStatusByJob)
		hostInfoByName := loadInventoryHostInfo(database, jobs)
		externalBindingByJobID := loadExternalBindingsForJobs(database, jobs)
		cordonedHostsByName := loadCordonedHostsByName(database, jobs)
		overloadedHostsByName := loadOverloadedHostsByName(database, jobs)
		autopilotPaused, autopilotPausedReason, autopilotActive, autopilotPassStartedAt := autopilotPlacementWaitState(database)
		return listJobsLoadedMsg{
			jobs:                     jobs,
			launchLiveByID:           launchLiveByID,
			launchStatusByID:         launchStatusByID,
			placingJobIDs:            placingJobIDs,
			placementQueuedAtByJob:   placementQueuedAtByJob,
			placementStatusByJob:     placementStatusByJob,
			attemptOutcomeByJob:      attemptOutcomeByJob,
			launchByID:               launchByID,
			externalBindingByJobID:   externalBindingByJobID,
			launchBootstrapP50:       bootstrapP50,
			launchBootstrapDurations: bootstrapDurations,
			launchBootstrapSamples:   bootstrapSamples,
			launchStageETAByName:     stageETAByName,
			launchStageEnteredAtByID: stageEnteredAtByID,
			recentFailedInstances:    loadRecentFailedInstances(database, recentFailedInstanceWindow, time.Now()),
			placementDaemonStopped:   placementDaemonStopped(),
			hostInfoByName:           hostInfoByName,
			cordonedHostsByName:      cordonedHostsByName,
			overloadedHostsByName:    overloadedHostsByName,
			autoPassPhase:            loadLatestAutoPilotPhase(database),
			autopilotPaused:          autopilotPaused,
			autopilotPausedReason:    autopilotPausedReason,
			autopilotActive:          autopilotActive,
			autopilotPassStartedAt:   autopilotPassStartedAt,
		}
	}
}

func loadExternalBindingsForJobs(database *sql.DB, jobs []*db.Job) map[int64]*db.ExternalJobBinding {
	wanted := make(map[int64]struct{})
	for _, job := range jobs {
		if job != nil && job.Backend == db.BackendSkyPilot {
			wanted[job.ID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	bindings, err := db.ListExternalJobBindings(database, db.ExternalExecutorSkyPilot, "")
	if err != nil {
		return nil
	}
	result := make(map[int64]*db.ExternalJobBinding, len(wanted))
	for _, binding := range bindings {
		if _, ok := wanted[binding.JobID]; ok {
			result[binding.JobID] = binding
		}
	}
	return result
}

func autopilotPlacementWaitState(database *sql.DB) (paused bool, pausedReason string, active bool, passStartedAt time.Time) {
	if database == nil {
		return false, "", false, time.Time{}
	}
	state, err := db.LoadAutopilotState(database)
	if err != nil || state == nil {
		return false, "", false, time.Time{}
	}
	return state.Paused, strings.TrimSpace(state.PausedReason), state.IsActive(time.Now(), orchestration.AutopilotPassStaleAfter), state.PassStartedAt
}

const recentFailedInstanceWindow = 24 * time.Hour

func jobIDsForOutcomeEvents(jobs []*db.Job) []int64 {
	ids := make([]int64, 0, len(jobs))
	seen := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		ids = append(ids, job.ID)
	}
	return ids
}

func collectLaunchIDsForList(jobs []*db.Job, placementStatusByJob map[int64]jobview.PlacementStatus) []int64 {
	seen := make(map[int64]struct{}, len(jobs))
	launchIDs := make([]int64, 0, len(jobs))
	add := func(id *int64) {
		if id == nil || *id <= 0 {
			return
		}
		if _, ok := seen[*id]; ok {
			return
		}
		seen[*id] = struct{}{}
		launchIDs = append(launchIDs, *id)
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		add(job.LaunchID)
	}
	for _, status := range placementStatusByJob {
		move := status.Move
		if move == nil {
			continue
		}
		for _, attempt := range move.AttemptsByID {
			add(attempt.LaunchID)
		}
	}
	return launchIDs
}

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
			durations, oldest, err := db.BootstrapStageDurations(database, stage)
			if err == nil {
				p50, _ := durations.Percentile(0.5)
				stageByName[stage] = groupedStatusLaunchingStageETA{
					p50:       p50,
					durations: durations,
					samples:   len(durations),
					oldest:    oldest,
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

// runBackgroundCloudSync runs non-host refreshes. Used when a hostsync.Worker
// owns host syncing, so cloud reconcile and external-executor observations
// still run on every list-TUI tick.
func (m listTUIModel) runBackgroundCloudSync(full bool) tea.Cmd {
	return m.runBackgroundSyncFn(full, syncNonHostStateForTUI)
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

func (m listTUIModel) runBackgroundSyncFn(full bool, fn func(context.Context, *sql.DB, bool) []string) tea.Cmd {
	database := m.database
	ctx := m.ctx
	return func() tea.Msg {
		done := make(chan listSyncFinishedMsg, 1)
		go func() {
			done <- listSyncFinishedMsg{warnings: fn(ctx, database, full), full: full}
		}()
		select {
		case msg := <-done:
			return msg
		case <-ctx.Done():
			return listSyncFinishedMsg{full: true} // terminal msg so no further syncs are triggered
		}
	}
}

func syncListTUIData(ctx context.Context, database *sql.DB, full bool) []string {
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
	warnings = append(warnings, syncNonHostStateForTUI(ctx, database, full)...)
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
		return m, tea.Batch(m.runAutoPilot(), m.requestReloadJobs())
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
	return m, m.requestReloadJobs()
}

// shouldRunAutoPilotPass applies the shared dispatch policy to the TUI's
// tick-driven loop: once the cooldown expires, a pass runs only if the timer
// alone justifies one (see orchestration.TimerRunsPass) or if state the
// autopilot reads has actually moved since the last pass. Without this the TUI
// replanned every cooldown for as long as any job stayed blocked, re-deriving
// the same answer from the same inputs.
//
// The snapshot read is a handful of counting queries against the local SQLite
// file, and it happens at most once per cooldown window rather than per tick.
func (m *listTUIModel) shouldRunAutoPilotPass() bool {
	if m.autoTimerRunsPass || m.autoLastPassAt.IsZero() {
		return true
	}
	// Change detection is not provably complete, so never stay quiet forever.
	if time.Since(m.autoLastPassAt) >= orchestration.AutopilotQuietBackstop {
		return true
	}
	snapshot, err := orchestration.ReadWakeSnapshot(m.database)
	if err != nil {
		// A failed read cannot show that nothing moved.
		return true
	}
	if snapshot == m.autoWakeSnapshot {
		return false
	}
	m.autoWakeSnapshot = snapshot
	return true
}

func (m *listTUIModel) runAutoPilot() tea.Cmd {
	if !m.isStatusGroupedView() || m.autopilotPaused || m.autoInProgress || m.autoUpgradePending || m.database == nil {
		return nil
	}
	if listTUIDelegatesBackgroundWorkToDaemon() {
		return nil
	}
	if !m.autoNextPassAt.IsZero() && time.Now().Before(m.autoNextPassAt) {
		return nil
	}
	if m.countAutoPilotActionableQueuedJobs() == 0 {
		// Nothing to evaluate; skip the pass so the status line doesn't flicker.
		return nil
	}
	if !m.shouldRunAutoPilotPass() {
		return nil
	}
	m.autoInProgress = true
	m.autoPassStartedAt = time.Now()
	m.autoLastPassAt = m.autoPassStartedAt

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
			if errors.Is(runErr, processguard.ErrBinaryChanged) {
				return listAutoPilotDoneMsg{upgradePending: true}
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

// runGatedAutoPilotPass wraps the orchestration pass with the singleton and
// binary-freshness gates. It returns without running a pass when autopilot is
// paused, another runner owns the slot, or this process has been superseded.
func runGatedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job, label string) (int, int, int, string, map[int64]string, map[int64]*blockreason.Structured, error) {
	result, err := orchestration.RunGroupedAutoPilotPassGated(ctx, database, scopedJobs, label)
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
	status, err := daemonStatusProbe(daemonStatusPaths())
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

func runListJobDiagnosis(jobID int64) tea.Cmd {
	return func() tea.Msg {
		executable, err := os.Executable()
		if err != nil {
			return listJobDiagnosisLoadedMsg{jobID: jobID, err: err}
		}
		command := exec.Command(executable, "diagnose", "job", ids.FormatJobID(jobID))
		output, err := command.CombinedOutput()
		return listJobDiagnosisLoadedMsg{jobID: jobID, output: string(output), err: err}
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
		if summary, count := orchestration.AutoPilotBlockSummaryWithCount(m.autoBlockReasons); summary != "" {
			m.autoPersistentBlocked = summary
			m.autoPersistentBlockedN = count
		}
	}
}

func (m *listTUIModel) replaceAutoBlockOverlay(blockedReasons map[int64]string, structuredBlocked map[int64]*blockreason.Structured) {
	if len(blockedReasons) == 0 {
		m.autoBlockReasons = nil
	} else {
		m.autoBlockReasons = make(map[int64]string, len(blockedReasons))
		for jobID, reason := range blockedReasons {
			if strings.TrimSpace(reason) != "" {
				m.autoBlockReasons[jobID] = reason
			}
		}
	}
	if len(structuredBlocked) == 0 {
		m.autoBlockDetail = nil
	} else {
		m.autoBlockDetail = make(map[int64]*blockreason.Structured, len(structuredBlocked))
		for jobID, detail := range structuredBlocked {
			if detail != nil {
				m.autoBlockDetail[jobID] = detail
			}
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
			AutoPilotReason:        m.autoBlockReasons[job.ID],
			Compact:                true,
			PendingPlacementReason: m.pendingPlacementReasonForJob(job),
		})
		if result.Blocked {
			reasons[job.ID] = result.Reason
		}
	}
	return reasons
}

func (m listTUIModel) pendingPlacementReasonForJob(job *db.Job) string {
	active, passStartedAt := m.pendingPlacementPassState(time.Now())
	return groupedStatusPendingPlacementReason(job, groupedStatusRenderOptions{
		daemonStopped:              m.placementDaemonStopped,
		autopilotPaused:            m.autopilotPaused,
		autopilotActive:            active,
		autopilotPassStartedAtUnix: unixIfNonZero(passStartedAt),
	})
}

func (m listTUIModel) pendingPlacementPassStartedAtUnix() int64 {
	_, startedAt := m.pendingPlacementPassState(time.Now())
	return unixIfNonZero(startedAt)
}

func (m listTUIModel) pendingPlacementPassState(now time.Time) (bool, time.Time) {
	if m.autoInProgress && !m.autoPassStartedAt.IsZero() {
		return true, m.autoPassStartedAt
	}
	if !m.autopilotActive || m.autopilotPassStartedAt.IsZero() {
		return false, time.Time{}
	}
	return true, m.autopilotPassStartedAt
}

func unixIfNonZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
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
	rowIdxs        []int
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
				rowIdxs:        make([]int, 0, 8),
			}
			active = true
			continue
		}
		if !active || strings.TrimSpace(row.text) == "" {
			continue
		}
		current.rows = append(current.rows, row)
		current.rowIdxs = append(current.rowIdxs, i)
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

// sectionWeight is the cursor-distance falloff used to prioritise which section
// to reveal next: the cursor's own section has weight 1 (distance 0) and nearer
// sections outrank farther ones via 1/(1+d^sectionFalloffPow).
func sectionWeight(i, cursorIdx int) float64 {
	w := 1.0
	for p := 0; p < sectionFalloffPow; p++ {
		w *= float64(absDistance(i, cursorIdx))
	}
	return 1.0 / (1.0 + w)
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
// for rowIdx on screen. Reached only when the skeleton already overflows — a
// single section taller than the viewport, or more section headers than the
// viewport has lines.
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
// them to fit maxLines. The cursor-aware abbreviation contract (which rows and
// sections survive, and in what priority) is specified in specs/status-sync.allium
// under "TUI rendering contract for grouped status list"; this function is the
// implementation of that contract, not its definition.
//
// Mechanism: when cursorRowIdx is a valid groupedRows index, the fit grows from
// an all-collapsed (headers-only) skeleton, revealing one row at a time and
// keeping a reveal only if a line-count probe still fits maxLines. The reveal
// order encodes the spec's priorities (see the betterCandidate ranking below).
// When cursorRowIdx < 0 the legacy positional abbreviation (collapse the last
// section first) is used.
func selectGroupedRowsForViewport(rows []groupedStatusRow, maxLines, cursorRowIdx int) []groupedViewportLine {
	if maxLines <= 0 || len(rows) == 0 {
		return nil
	}
	type sectionState struct {
		groupedViewportSection
		windowStart     int
		leadingEllipsis bool
	}
	parsed := parseGroupedViewportSections(rows)
	sections := make([]sectionState, 0, len(parsed))
	for _, section := range parsed {
		sections = append(sections, sectionState{
			groupedViewportSection: section,
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
	// countLines returns render()'s line count without materialising the slice.
	// The fill loop probes thousands of candidate states; allocating a full line
	// slice for each (this is on the per-render TUI path) dominated the cost, so
	// the trial oracle counts and only the committed layout is rendered once.
	countLines := func() int {
		count := 0
		prevAbbreviated := false
		for idx := range sections {
			s := sections[idx]
			currAbbreviated := s.abbreviated() || s.leadingEllipsis
			if idx > 0 && !(prevAbbreviated && currAbbreviated) && !s.summaryOnly {
				count++
			}
			if s.summaryOnly {
				count++
			} else {
				count++ // header
				if s.leadingEllipsis {
					count++
				}
				for i := 0; i < s.shownRows; i++ {
					ri := s.windowStart + i
					if ri < 0 || ri >= len(s.rows) || ri >= len(s.rowIdxs) {
						break
					}
					count++
				}
				if s.ellipsis {
					count++
				}
			}
			prevAbbreviated = currAbbreviated
		}
		return count
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

	// Cursor-aware fill. The line cost of a section is not 1+shownRows: it varies
	// with blank separators, leading/trailing "..." markers, and the summaryOnly
	// collapse, all of which couple adjacent sections. Rather than predict that
	// cost and correct it (the source of the prior multi-pass churn), grow from a
	// skeleton that is guaranteed to fit and use countLines — the exact line count
	// render() would emit — as the oracle: a reveal is kept only if it still fits.

	// anchorCursorWindow positions the cursor section's visible window so it
	// contains the cursor, centering it (via visibleWindow) and setting the
	// leading/trailing ellipsis flags. Safe to re-call after changing shownRows.
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
	// headAnchor pins a section's window to its head: the first shownRows rows,
	// with a trailing "..." when rows remain below.
	headAnchor := func(s *sectionState) {
		s.summaryOnly = false
		s.windowStart = 0
		s.leadingEllipsis = false
		s.ellipsis = s.shownRows < len(s.rowIdxs)
		normalizeSectionWindow(s)
	}
	// anchorCursorSection windows the cursor section on the cursor: a centered
	// window with a leading "..." while the window cannot reach the cursor from
	// the section head, then head-anchored once it can. This realises the spec's
	// "runs off the top" / head-anchored selected-section rule.
	anchorCursorSection := func(s *sectionState) {
		if len(s.rowIdxs) == 0 || s.shownRows <= cursorOffset {
			anchorCursorWindow(s)
			return
		}
		headAnchor(s)
	}

	reanchor := func(i int) {
		s := &sections[i]
		if i == cursorSection {
			anchorCursorSection(s)
			return
		}
		headAnchor(s)
	}

	// Skeleton: every section collapsed to its header (header-only sections with
	// no rows render verbatim, never as a "(0)" summary). This all-headers floor
	// is the guaranteed-fitting starting point; revealing the cursor's own row is
	// the fill's first move, not baked in, so when even one row would push a header
	// off-screen the skeleton survives with every section header intact.
	for i := range sections {
		s := &sections[i]
		s.windowStart = 0
		s.leadingEllipsis = false
		s.ellipsis = false
		s.shownRows = 0
		s.summaryOnly = len(s.rowIdxs) > 0
	}

	// grow advances section i by the smallest meaningful reveal: uncollapse a
	// summaryOnly section to its minimum, else widen its window by one row.
	// Returns false when the section is already full.
	grow := func(i int) bool {
		s := &sections[i]
		total := len(s.rowIdxs)
		if total == 0 {
			return false
		}
		switch {
		case s.summaryOnly:
			s.summaryOnly = false
			s.shownRows = min(sectionMinVisible(total), total)
			reanchor(i)
			return true
		case s.shownRows < total:
			s.shownRows++
			reanchor(i)
			return true
		default:
			return false
		}
	}

	fits := func() bool { return countLines() <= maxLines }
	// rank orders candidate reveals to realise the spec's cursor-aware priorities
	// (status-sync.allium, "cursor-aware" clause): class 2 = the selected section
	// (priority 2, revealed before others); class 1 = a not-yet-opened section
	// earning its foothold (priority 3); class 0 = extending an already-open
	// section. Within a class the nearer section wins (priority 4), then the
	// smaller (so a large neighbour cannot starve a small one). Priority 1 — the
	// selected row is always present — is guaranteed structurally: the selected
	// section is class 2, so it is the first thing revealed.
	rank := func(i int) (int, float64, int) {
		switch {
		case i == cursorSection:
			return 2, sectionWeight(i, cursorSection), -len(sections[i].rowIdxs)
		case sections[i].summaryOnly:
			return 1, sectionWeight(i, cursorSection), -len(sections[i].rowIdxs)
		default:
			return 0, sectionWeight(i, cursorSection), -len(sections[i].rowIdxs)
		}
	}
	better := func(a, b int) bool {
		ca, wa, ta := rank(a)
		cb, wb, tb := rank(b)
		if ca != cb {
			return ca > cb
		}
		if wa != wb {
			return wa > wb
		}
		return ta > tb
	}
	// Each iteration first spends every "free" reveal — one that trades a trailing
	// "..." for the row it hid at no net line cost — then takes the single best
	// costed reveal that still fits. Terminates because every reveal strictly
	// increases shown rows, bounded by the section totals.
	for {
		progressed := false
		for i := range sections {
			snap := sections[i]
			before := countLines()
			if !grow(i) {
				sections[i] = snap
				continue
			}
			// A free reveal never adds a line (it trades a trailing "..." for the
			// row it hid); since the prior state already fit, countLines() <= before
			// implies it still fits maxLines.
			if countLines() <= before {
				progressed = true
			} else {
				sections[i] = snap
			}
		}
		if progressed {
			continue
		}
		best := -1
		for i := range sections {
			snap := sections[i]
			ok := grow(i) && fits()
			sections[i] = snap
			if ok && (best < 0 || better(i, best)) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		grow(best)
	}
	lines := render()
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
