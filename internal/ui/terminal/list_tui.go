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
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/ops"
)

const listDBChangeDebounce = 200 * time.Millisecond
const listTUISyncInterval = TerminalSyncInterval
const listAutoLeaseTTL = 30 * time.Second

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
	syncInProgress             bool
	statusMessage              string
	dbWatcher                  *fsnotify.Watcher
	dbWatcherTargets           map[string]struct{}
	debounceActive             bool
	syncWorker                 *hostsync.Worker
	ctx                        context.Context
	cancel                     context.CancelFunc
	groupedByStatus            bool
	groupedRows                []groupedStatusRow
	groupedSelectableRows      []int
	autoMode                   bool
	autoInProgress             bool
	autoLeaseOwner             string
	autoLeaseScope             string
	autoBlockReasons           map[int64]string
	launchLiveByID             map[int64]*db.LaunchLiveState
	quickLaunching             bool
	quickLaunchScope           string
	quickLaunchProgress        <-chan listQuickLaunchProgressMsg
	quickLaunchDone            <-chan listQuickLaunchDoneMsg
	quickLaunchStatusHoldUntil time.Time
	movePicker                 movePickerModel
	focused                    bool
}

type listJobsLoadedMsg struct {
	jobs           []*db.Job
	launchLiveByID map[int64]*db.LaunchLiveState
	err            error
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
	launched       int
	launchedClass  string
	blockedReasons map[int64]string
	anotherHolding bool
	err            error
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

var (
	listTUITitleStyle    = lipgloss.NewStyle().Bold(true)
	listTUIHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUISelectedStyle = lipgloss.NewStyle().Reverse(true)
	listTUIFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUIEmptyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
	graceAckKeyPattern   = regexp.MustCompile(`grace/(\d+)/acks/`)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool) error {
	ctx, cancel := context.WithCancel(context.Background())

	var sw *hostsync.Worker
	if syncEnabled {
		cfg, _ := config.Load()
		cloudClients, _ := buildCloudClients(cfg)
		r2Client, _ := buildR2Client(cfg)
		sw = hostsync.New(database, cloudClients, r2Client, cfg)
		sw.Start()
	}

	model := listTUIModel{
		database:         database,
		args:             append([]string(nil), args...),
		title:            title,
		jobs:             jobs,
		syncEnabled:      syncEnabled,
		syncInProgress:   syncEnabled,
		syncWorker:       sw,
		ctx:              ctx,
		cancel:           cancel,
		groupedByStatus:  groupedByStatus,
		autoMode:         groupedByStatus && autoMode,
		autoLeaseOwner:   fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		autoLeaseScope:   buildListAutoLeaseScope(title),
		quickLaunchScope: "list_quick_launch:" + buildListAutoLeaseScope(title),
		focused:          true,
	}
	model.rebuildGroupedRows()

	restore := logging.Suppress()
	defer restore()

	_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithReportFocus()).Run()
	cancel()
	if sw != nil {
		sw.Stop()
	}
	if err != nil {
		return fmt.Errorf("run list TUI: %w", err)
	}
	return nil
}

func (m listTUIModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.startDBWatcher(), m.reloadJobs(), m.scheduleListSyncTick()}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
		cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
			return listSyncWorkerResultMsg{result: r}
		}))
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
		if m.movePicker.active {
			return m.handleMovePickerKey(msg)
		}
		if m.groupedByStatus {
			return m.handleGroupedKey(msg)
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
			_ = db.ReleaseAutoLease(m.database, m.quickLaunchScope, m.autoLeaseOwner)
			if m.dbWatcher != nil {
				_ = m.dbWatcher.Close()
			}
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
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
		case "a":
			if m.groupedByStatus {
				m.autoMode = !m.autoMode
				if m.autoMode {
					m.statusMessage = "Auto-pilot ON"
					return m, m.runAutoPilot()
				}
				m.autoInProgress = false
				m.autoBlockReasons = nil
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

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case listJobsLoadedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			return m, nil
		}
		m.jobs = msg.jobs
		m.launchLiveByID = msg.launchLiveByID
		m.pruneAutoBlockReasons()
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

		m.syncInProgress = false
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
				m.statusMessage = "Synced."
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
		} else if !m.debounceActive {
			m.debounceActive = true
			cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
				return listDBRefreshTriggeredMsg{}
			}))
		}
		if cmd := m.waitForDBEvent(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case listDBRefreshTriggeredMsg:
		m.debounceActive = false
		return m, m.reloadJobs()

	case listSyncWorkerResultMsg:
		m.syncInProgress = false
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
		cmds := []tea.Cmd{m.scheduleListSyncTick()}
		if m.syncWorker != nil {
			m.requestActiveSyncs()
			cmds = append(cmds, m.reloadJobs())
		} else if m.syncEnabled && !m.syncInProgress {
			m.syncInProgress = true
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
		m.autoInProgress = false
		if !m.autoMode {
			return m, nil
		}
		m.autoBlockReasons = msg.blockedReasons
		if msg.err != nil {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot failed: " + summarizeAutoPilotError(msg.err)
			}
			return m, nil
		}
		if msg.anotherHolding {
			if !m.quickLaunchStatusProtected() {
				m.statusMessage = "Auto-pilot: another TUI is active for this scope"
			}
			return m, nil
		}
		if !m.quickLaunchStatusProtected() {
			switch {
			case msg.placed > 0 && msg.launched > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d, %s", msg.placed, formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass))
			case msg.placed > 0:
				m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d", msg.placed)
			case msg.launched > 0:
				m.statusMessage = "Auto-pilot: " + formatAutoPilotLaunchedSummary(msg.launched, msg.launchedClass)
			default:
				if summary := autoPilotBlockSummary(msg.blockedReasons); summary != "" {
					m.statusMessage = "Auto-pilot: " + summary
				} else {
					m.statusMessage = "Auto-pilot: monitoring"
				}
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

	case moveOptionsReadyMsg:
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
			m.statusMessage = fmt.Sprintf("Move failed: %v", msg.err)
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Moved job #%d to %s", msg.jobID, msg.targetDesc)
		return m, m.reloadJobs()

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
	if m.movePicker.active {
		return m.movePicker.View(m.width, m.height)
	}
	if m.groupedByStatus {
		return m.groupedView()
	}

	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	layout := m.layout
	var b strings.Builder
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width)

	title := fmt.Sprintf("%s (%d)", m.title, len(m.jobs))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")
	b.WriteString(listTUIHeaderStyle.Render(truncateDisplayWidth(formatJobListHeader(layout), m.width)))
	b.WriteString("\n")

	rows := m.pageSize()
	if len(sharedStatusLines) > 0 && rows > len(sharedStatusLines) {
		rows -= len(sharedStatusLines)
	}
	if len(m.jobs) == 0 {
		b.WriteString(listTUIEmptyStyle.Render(truncateDisplayWidth(m.emptyStateText(), m.width)))
		b.WriteString("\n")
		for i := 1; i < rows; i++ {
			b.WriteString("\n")
		}
	} else {
		for i := 0; i < rows; i++ {
			idx := m.offset + i
			if idx >= len(m.jobs) {
				b.WriteString("\n")
				continue
			}
			row := truncateDisplayWidth(formatJobListRow(layout, m.jobs[idx]), m.width)
			if idx == m.cursor {
				row = listTUISelectedStyle.Render(row)
			}
			b.WriteString(row)
			b.WriteString("\n")
		}
	}

	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(m.footerText(rows), m.width)))
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

	// Build footer lines first so we can reserve space for them.
	eta := computeGroupedETA(groupedJobs, m.launchLiveByID, time.Now())
	etaLine := formatETALine(eta)
	statusLine := m.groupedStatusText()
	controlsLine := m.groupedControlsText(eta.HasQueued)
	sharedStatusLines := renderSharedTUIStatusLines(m.database, m.width)
	// Reserve: 1 title + 1 blank separator + footer lines.
	footerLines := 2 // blank separator + controls
	if etaLine != "" {
		footerLines++
	}
	if statusLine != "" {
		footerLines++
	}
	footerLines += len(sharedStatusLines)
	maxBodyLines := m.height - 1 - footerLines // 1 for title

	rows := m.groupedRows
	if len(rows) == 0 {
		rows = []groupedStatusRow{{text: "None"}}
	}
	bodyLines := make([]string, 0, len(rows))
	selectedVisualLine := -1
	selectedRow := m.selectedGroupedRow()
	for idx, row := range rows {
		line := row.text
		if selectedRow >= 0 && idx == selectedRow {
			line = listTUISelectedStyle.Render(line)
			selectedVisualLine = idx
		}
		bodyLines = append(bodyLines, line)
	}
	if maxBodyLines > 0 && len(bodyLines) > maxBodyLines {
		start := 0
		if selectedVisualLine >= 0 {
			start = watchScrollStart(len(bodyLines), selectedVisualLine, maxBodyLines)
		}
		end := min(len(bodyLines), start+maxBodyLines)
		bodyLines = bodyLines[start:end]
	}
	for _, line := range bodyLines {
		b.WriteString(truncateDisplayWidth(line, m.width))
		b.WriteString("\n")
	}

	// Visually separate grouped job rows from footer lines.
	b.WriteString("\n")
	if etaLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(etaLine, m.width)))
		b.WriteString("\n")
	}
	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if statusLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(statusLine, m.width)))
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(controlsLine, m.width)))
	return b.String()
}

func (m listTUIModel) groupedStatusText() string {
	var parts []string
	if m.syncInProgress {
		parts = append(parts, "syncing...")
	}
	if m.statusMessage != "" {
		parts = append(parts, m.statusMessage)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (m listTUIModel) groupedControlsText(hasQueued bool) string {
	autoState := "OFF"
	if m.autoMode {
		autoState = "ON"
	}
	line := fmt.Sprintf("a:auto (%s)", autoState)
	if m.selectedGroupedJob() != nil {
		line += "  k:kill  u:unplace"
		if selected := m.selectedGroupedJob(); selected != nil && selected.EffectiveStatus() == db.StatusQueued {
			line += "  m:move"
		}
	}
	if hasQueued {
		line += "  n:new instance"
	}
	line += "  q:quit"
	return line
}

func (m listTUIModel) selectedGroupedRow() int {
	if len(m.groupedSelectableRows) == 0 || m.cursor < 0 || m.cursor >= len(m.groupedSelectableRows) {
		return -1
	}
	return m.groupedSelectableRows[m.cursor]
}

func (m listTUIModel) selectedGroupedJob() *db.Job {
	row := m.selectedGroupedRow()
	if row < 0 || row >= len(m.groupedRows) {
		return nil
	}
	return m.groupedRows[row].job
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

func (m listTUIModel) handleGroupedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q", "esc":
		_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
		_ = db.ReleaseAutoLease(m.database, m.quickLaunchScope, m.autoLeaseOwner)
		if m.dbWatcher != nil {
			_ = m.dbWatcher.Close()
		}
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
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
	case "a":
		m.autoMode = !m.autoMode
		if m.autoMode {
			m.statusMessage = "Auto-pilot ON"
			return m, m.runAutoPilot()
		}
		m.autoInProgress = false
		m.autoBlockReasons = nil
		_ = db.ReleaseAutoLease(m.database, m.autoLeaseScope, m.autoLeaseOwner)
		m.statusMessage = "Auto-pilot OFF"
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
	case "k":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
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
		m.statusMessage = fmt.Sprintf("Unplacing job #%d...", job.ID)
		return m, requestWatchJobUnplace(m.database, job.ID)
	case "m":
		job := m.selectedGroupedJob()
		if job == nil {
			return m, nil
		}
		if job.EffectiveStatus() != db.StatusQueued {
			m.statusMessage = "Move is only available for queued jobs"
			return m, nil
		}
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			m.statusMessage = fmt.Sprintf("Move setup failed: %v", cfgErr)
			return m, nil
		}
		cloudClients, clientsErr := buildCloudClients(cfg)
		if clientsErr != nil {
			m.statusMessage = fmt.Sprintf("Move setup failed: %v", clientsErr)
			return m, nil
		}
		launches, err := db.ListRunningLaunches(m.database)
		if err != nil {
			m.statusMessage = fmt.Sprintf("Move setup failed: %v", err)
			return m, nil
		}
		capacities := make([]campaign.InstanceCapacity, 0, len(launches))
		queuedCounts := make(map[int64]int, len(launches))
		for _, ci := range launches {
			liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(m.database, ci.ID)
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
		m.statusMessage = "Searching for move destinations..."
		return m, requestMoveOptions(m.database, cfg, cloudClients, job, capacities, queuedCounts, sourceInstanceID)
	}
	return m, nil
}

func (m listTUIModel) footerText(rows int) string {
	start := 0
	end := 0
	if len(m.jobs) > 0 {
		start = m.offset + 1
		end = min(len(m.jobs), m.offset+rows)
	}

	state := fmt.Sprintf("[%d-%d/%d]", start, end, len(m.jobs))
	if m.syncInProgress {
		state += " syncing..."
	}
	if m.statusMessage != "" {
		state += "  " + m.statusMessage
	}
	state += "  up/down move  space/b page  g/G top/bottom  q quit"
	return state
}

func (m listTUIModel) emptyStateText() string {
	if m.syncInProgress {
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
	m.groupedRows = buildGroupedStatusRows(groupedJobs, m.width, m.launchLiveByID)
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
		hydrateRelaunchBlockedReasons(database, jobs)
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
		return listJobsLoadedMsg{jobs: jobs, launchLiveByID: launchLiveByID, err: nil}
	}
}

func (m listTUIModel) runBackgroundSync(full bool) tea.Cmd {
	database := m.database
	ctx := m.ctx
	return func() tea.Msg {
		done := make(chan listSyncFinishedMsg, 1)
		go func() {
			done <- listSyncFinishedMsg{warnings: syncListTUIData(database, full), full: full}
		}()
		select {
		case msg := <-done:
			return msg
		case <-ctx.Done():
			return listSyncFinishedMsg{full: true} // return terminal msg so no further syncs are triggered
		}
	}
}

func syncListTUIData(database *sql.DB, full bool) []string {
	timeout := FastSyncTimeout
	if full {
		timeout = NormalSyncTimeout
	}
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailedWithOptions(database, nil, timeout, false, full)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
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
	m.autoInProgress = true

	database := m.database
	ctx := m.ctx
	owner := m.autoLeaseOwner
	scope := m.autoLeaseScope
	jobs := append([]*db.Job(nil), m.jobs...)

	return func() tea.Msg {
		acquired, err := db.AcquireAutoLease(database, scope, owner, listAutoLeaseTTL)
		if err != nil {
			return listAutoPilotDoneMsg{err: err}
		}
		if !acquired {
			return listAutoPilotDoneMsg{anotherHolding: true}
		}
		defer db.ReleaseAutoLease(database, scope, owner)
		placed, launched, launchedClass, blockedReasons, runErr := runGroupedAutoPilotPass(ctx, database, jobs)
		return listAutoPilotDoneMsg{
			placed:         placed,
			launched:       launched,
			launchedClass:  launchedClass,
			blockedReasons: blockedReasons,
			err:            runErr,
		}
	}
}

func runGroupedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job) (int, int, string, map[int64]string, error) {
	if database == nil {
		return 0, 0, "", nil, nil
	}
	scoped := make(map[int64]struct{}, len(scopedJobs))
	for _, job := range scopedJobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return 0, 0, "", nil, err
	}
	unplaced := make([]*db.Job, 0, len(unplacedJobs))
	for _, job := range unplacedJobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		unplaced = append(unplaced, job)
	}
	if len(unplaced) == 0 {
		return 0, 0, "", nil, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return 0, 0, "", nil, err
	}
	r2Client, _ := buildR2Client(cfg)

	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return 0, 0, "", nil, err
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	for _, ci := range launches {
		jobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(jobs)); ok {
			capacities = append(capacities, cap)
		}
	}

	plan, err := buildAutoPlacementPlan(database, cfg, unplaced, capacities)
	if err != nil {
		return 0, 0, "", nil, err
	}
	blockedReasons := map[int64]string{}
	for jobID, reason := range plan.BlockedReasons {
		if strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}

	placed := 0
	for _, assignment := range plan.ReuseAssignments {
		if assignment.Job == nil || assignment.Instance.Instance == nil {
			continue
		}
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, assignment.Instance.Instance.ID, []*db.Job{assignment.Job}); err != nil {
			return placed, 0, "", blockedReasons, err
		}
		placed++
	}

	remaining, err := db.ListUnplacedJobs(database)
	if err != nil {
		return placed, 0, "", nil, err
	}
	rentalScope := make([]int64, 0, len(remaining))
	launchScope := make(map[int64]struct{}, len(plan.LaunchJobIDs))
	for _, jobID := range plan.LaunchJobIDs {
		launchScope[jobID] = struct{}{}
	}
	remainingByID := make(map[int64]*db.Job, len(remaining))
	for _, job := range remaining {
		if job != nil {
			remainingByID[job.ID] = job
		}
		if job == nil || job.EffectiveStatus() != db.StatusQueued || job.HasTag(db.TagInventory) {
			continue
		}
		if len(scoped) > 0 {
			if _, ok := scoped[job.ID]; !ok {
				continue
			}
		}
		if len(launchScope) == 0 {
			rentalScope = append(rentalScope, job.ID)
			continue
		}
		// Restrict auto-launch to planner-approved launch candidates.
		if _, ok := launchScope[job.ID]; ok {
			rentalScope = append(rentalScope, job.ID)
		}
	}
	if len(rentalScope) == 0 {
		return placed, 0, "", blockedReasons, nil
	}

	failedInstanceByJob := buildFailedInstanceByJob(database, rentalScope)
	passStartedAt := time.Now().Unix()
	result, err := attemptRelaunchOrphanedJobs(database, cfg, 0, nil, rentalScope, "", false, true)
	if err != nil {
		return placed, 0, "", nil, err
	}
	if result == nil {
		return placed, 0, "", blockedReasons, nil
	}
	if result.BlockedReason != "" {
		for _, jobID := range rentalScope {
			blockedReasons[jobID] = result.BlockedReason
		}
	}
	for jobID, failedID := range failedInstanceByJob {
		if failedID == 0 {
			continue
		}
		if reason, ok := result.NotReplacedReasons[failedID]; ok && strings.TrimSpace(reason) != "" {
			blockedReasons[jobID] = reason
		}
	}
	eventReasons := relaunchBlockedReasonsFromEvents(database, rentalScope, passStartedAt)
	for jobID, reason := range eventReasons {
		if strings.TrimSpace(reason) == "" {
			continue
		}
		if _, exists := blockedReasons[jobID]; !exists {
			blockedReasons[jobID] = reason
		}
	}
	for jobID := range blockedReasons {
		if _, exists := remainingByID[jobID]; !exists {
			delete(blockedReasons, jobID)
		}
	}
	// For rental-scope jobs with no specific reason when nothing launched,
	// provide a fallback so they don't appear silently stuck.
	if len(result.InstanceIDs) == 0 {
		for _, jobID := range rentalScope {
			if _, exists := blockedReasons[jobID]; !exists {
				blockedReasons[jobID] = "no offers available"
			}
		}
	}
	return placed, len(result.InstanceIDs), launchedClassFromResult(database, result.InstanceIDs), blockedReasons, nil
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

func buildFailedInstanceByJob(database *sql.DB, jobIDs []int64) map[int64]int64 {
	result := make(map[int64]int64, len(jobIDs))
	for _, jobID := range jobIDs {
		result[jobID] = latestAttemptLaunchID(database, jobID)
	}
	return result
}

func latestAttemptLaunchID(database *sql.DB, jobID int64) int64 {
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil || len(attempts) == 0 {
		return 0
	}
	return attempts[len(attempts)-1].LaunchID
}

func relaunchBlockedReasonsFromEvents(database *sql.DB, jobIDs []int64, sinceUnix int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(jobIDs) == 0 {
		return reasons
	}
	placeholders := make([]string, 0, len(jobIDs))
	for range jobIDs {
		placeholders = append(placeholders, "?")
	}
	query := fmt.Sprintf(`SELECT job_id, event_kind, detail, attempt_number, max_attempts
		FROM lifecycle_events
		WHERE event_kind LIKE 'relaunch.skipped.%%'
		  AND job_id IN (%s)`, strings.Join(placeholders, ","))
	if sinceUnix > 0 {
		query += "\n\t\t  AND occurred_at >= ?"
	}
	query += "\n\t\tORDER BY occurred_at DESC, id DESC"
	args := make([]any, 0, len(jobIDs)+1)
	for _, jobID := range jobIDs {
		args = append(args, jobID)
	}
	if sinceUnix > 0 {
		args = append(args, sinceUnix)
	}
	rows, err := database.Query(query, args...)
	if err != nil {
		return reasons
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jobID         int64
			eventKind     string
			detail        sql.NullString
			attemptNumber int
			maxAttempts   int
		)
		if err := rows.Scan(&jobID, &eventKind, &detail, &attemptNumber, &maxAttempts); err != nil {
			continue
		}
		if _, exists := reasons[jobID]; exists {
			continue
		}
		reasons[jobID] = summarizeRelaunchSkipEvent(eventKind, detail.String, attemptNumber, maxAttempts)
	}
	return reasons
}

func summarizeRelaunchSkipEvent(kind, detail string, attemptNumber, maxAttempts int) string {
	detail = strings.TrimSpace(detail)
	if detail != "" {
		return detail
	}
	switch kind {
	case db.EventRelaunchSkippedNoOffers:
		return "no offers available"
	case db.EventRelaunchSkippedOfferError:
		return "offer query failed"
	case db.EventRelaunchSkippedMaxAttempts:
		if attemptNumber > 0 && maxAttempts > 0 {
			return "attempt " + strconv.Itoa(attemptNumber) + "/" + strconv.Itoa(maxAttempts)
		}
		return "max cloud attempts reached"
	default:
		return kind
	}
}

func hydrateRelaunchBlockedReasons(database *sql.DB, jobs []*db.Job) {
	if database == nil || len(jobs) == 0 {
		return
	}
	jobIDs := make([]int64, 0, len(jobs))
	queueFloorByJob := make(map[int64]int64, len(jobs))
	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued {
			continue
		}
		if strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if job.TargetKind() != db.JobTargetUnplaced {
			continue
		}
		jobIDs = append(jobIDs, job.ID)
		floor := job.QueuedAt
		if floor <= 0 {
			floor = job.CreatedAt
		}
		queueFloorByJob[job.ID] = floor
	}
	if len(jobIDs) == 0 {
		return
	}
	reasons := relaunchBlockedReasonsFromEventsWithFloor(database, queueFloorByJob)
	if len(reasons) == 0 {
		return
	}
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.QueueBlockedReason) != "" {
			continue
		}
		if reason := strings.TrimSpace(reasons[job.ID]); reason != "" {
			job.QueueBlockedReason = reason
		}
	}
}

func relaunchBlockedReasonsFromEventsWithFloor(database *sql.DB, floorByJob map[int64]int64) map[int64]string {
	reasons := make(map[int64]string)
	if database == nil || len(floorByJob) == 0 {
		return reasons
	}
	jobIDs := make([]int64, 0, len(floorByJob))
	for jobID := range floorByJob {
		jobIDs = append(jobIDs, jobID)
	}
	placeholders := make([]string, 0, len(jobIDs))
	args := make([]any, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		placeholders = append(placeholders, "?")
		args = append(args, jobID)
	}
	query := fmt.Sprintf(`SELECT job_id, occurred_at, event_kind, detail, attempt_number, max_attempts
		FROM lifecycle_events
		WHERE event_kind LIKE 'relaunch.skipped.%%'
		  AND job_id IN (%s)
		ORDER BY occurred_at DESC, id DESC`, strings.Join(placeholders, ","))
	rows, err := database.Query(query, args...)
	if err != nil {
		return reasons
	}
	defer rows.Close()
	for rows.Next() {
		var (
			jobID         int64
			occurredAt    int64
			eventKind     string
			detail        sql.NullString
			attemptNumber int
			maxAttempts   int
		)
		if err := rows.Scan(&jobID, &occurredAt, &eventKind, &detail, &attemptNumber, &maxAttempts); err != nil {
			continue
		}
		if _, exists := reasons[jobID]; exists {
			continue
		}
		if floor, ok := floorByJob[jobID]; ok && floor > 0 && occurredAt < floor {
			continue
		}
		reasons[jobID] = summarizeRelaunchSkipEvent(eventKind, detail.String, attemptNumber, maxAttempts)
	}
	return reasons
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
	emit := func(message string) {
		if strings.TrimSpace(message) == "" {
			return
		}
		select {
		case progress <- listQuickLaunchProgressMsg{message: message}:
		default:
		}
	}

	emit("Acquiring launch lease...")
	acquired, err := db.AcquireAutoLease(database, scope, scopeOwner, listAutoLeaseTTL)
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	if !acquired {
		return listQuickLaunchDoneMsg{err: fmt.Errorf("another TUI is launching for this scope")}
	}
	defer db.ReleaseAutoLease(database, scope, scopeOwner)

	scoped := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	emit("Planning placement...")
	scopedLaunchable, err := listScopedLaunchableJobs(database, scoped)
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	if len(scopedLaunchable) == 0 {
		return listQuickLaunchDoneMsg{err: fmt.Errorf("no launchable queued jobs in this view")}
	}

	cfg, err := config.Load()
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	for _, ci := range launches {
		liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobs(liveJobs)); ok {
			capacities = append(capacities, cap)
		}
	}

	plan, err := buildAutoPlacementPlan(database, cfg, scopedLaunchable, capacities)
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	if len(plan.LaunchJobIDs) == 0 {
		return listQuickLaunchDoneMsg{err: fmt.Errorf("queued jobs can be placed on existing instances")}
	}

	byID := make(map[int64]*db.Job, len(scopedLaunchable))
	for _, job := range scopedLaunchable {
		if job != nil {
			byID[job.ID] = job
		}
	}
	launchJobID := minLaunchJobID(plan.LaunchJobIDs)
	launchJob := byID[launchJobID]
	if launchJob == nil {
		return listQuickLaunchDoneMsg{err: fmt.Errorf("launch job %d not found in current scope", launchJobID)}
	}

	emit(fmt.Sprintf("Preparing job #%d for new instance...", launchJobID))
	// Relaunch helper only operates on unplaced jobs, so for a placed job
	// (rental queue item or inventory host) we first reset it back to the
	// unplaced pool.
	if launchJob.TargetKind() != db.JobTargetUnplaced {
		if _, err := ops.UnplaceQueuedJob(database, launchJob, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
			return listQuickLaunchDoneMsg{err: fmt.Errorf("prepare launch anchor job %d: %w", launchJob.ID, err)}
		}
	}

	emit(fmt.Sprintf("Launching new instance for job #%d...", launchJobID))
	result, err := attemptRelaunchOrphanedJobs(database, cfg, 0, nil, []int64{launchJobID}, "", false, true)
	if err != nil {
		return listQuickLaunchDoneMsg{err: err}
	}
	if result != nil && result.BlockedReason != "" {
		return listQuickLaunchDoneMsg{err: errors.New(result.BlockedReason)}
	}
	if result == nil || len(result.InstanceIDs) == 0 {
		return listQuickLaunchDoneMsg{err: fmt.Errorf("no compatible offer found")}
	}

	newInstanceID := result.InstanceIDs[0]
	emit(fmt.Sprintf("Instance %s created; rebalancing queued jobs...", ids.FormatInstanceID(newInstanceID)))
	movedJobs, warning, err := rebalanceQueuedJobsToLaunchedInstance(
		ctx,
		database,
		cfg,
		newInstanceID,
		launchJobID,
		scoped,
	)
	if err != nil {
		return listQuickLaunchDoneMsg{
			instanceIDs: []int64{newInstanceID},
			movedJobs:   movedJobs,
			err:         err,
		}
	}

	runningJobID, waitWarning, err := waitForQuickLaunchRunningState(ctx, database, newInstanceID, 90*time.Second, progress)
	if err != nil {
		return listQuickLaunchDoneMsg{
			instanceIDs: []int64{newInstanceID},
			movedJobs:   movedJobs,
			warning:     warning,
			err:         err,
		}
	}
	if strings.TrimSpace(waitWarning) != "" {
		if strings.TrimSpace(warning) != "" {
			warning = warning + "; " + waitWarning
		} else {
			warning = waitWarning
		}
	}

	// Keep one-key behavior deterministic: launch only one additional instance.
	return listQuickLaunchDoneMsg{
		instanceIDs:  []int64{newInstanceID},
		movedJobs:    movedJobs,
		warning:      warning,
		runningJobID: runningJobID,
	}
}

func waitForQuickLaunchRunningState(
	ctx context.Context,
	database *sql.DB,
	instanceID int64,
	maxWait time.Duration,
	progress chan<- listQuickLaunchProgressMsg,
) (int64, string, error) {
	if instanceID <= 0 || database == nil {
		return 0, "", nil
	}
	emit := func(message string) {
		select {
		case progress <- listQuickLaunchProgressMsg{message: message}:
		default:
		}
	}

	emit(fmt.Sprintf("Waiting for first running job on instance %s...", ids.FormatInstanceID(instanceID)))
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastProgress := time.Time{}
	for {
		if launchJobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID); err == nil {
			for _, job := range launchJobs {
				if job == nil {
					continue
				}
				status := job.EffectiveStatus()
				if status == db.StatusRunning || status == db.StatusStarting {
					return job.ID, "", nil
				}
			}
		}
		if launch, err := db.GetLaunch(database, instanceID); err == nil && launch != nil && campaign.IsInstanceTerminal(launch.Status) {
			return 0, "", fmt.Errorf("instance %s ended before any job started (%s)", ids.FormatInstanceID(instanceID), launch.Status)
		}
		if time.Now().After(deadline) {
			return 0, "waiting for scheduler to start first job", nil
		}
		if time.Since(lastProgress) >= 5*time.Second {
			emit(fmt.Sprintf("Waiting for first running job on instance %s... (%ds)", ids.FormatInstanceID(instanceID), int(time.Since(deadline.Add(-maxWait)).Seconds())))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// listScopedLaunchableJobs returns queued jobs in the scoped set that are
// eligible for cloud launch: rental, unplaced, or inventory-host jobs (unless
// explicitly tagged inventory-only).
func listScopedLaunchableJobs(database *sql.DB, scoped map[int64]struct{}) ([]*db.Job, error) {
	if database == nil {
		return nil, nil
	}
	ids := make([]int64, 0, len(scoped))
	for id := range scoped {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*db.Job, 0, len(ids))
	for _, id := range ids {
		job, err := db.GetJobByID(database, id)
		if err != nil || job == nil {
			continue
		}
		status := job.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			continue
		}
		if job.HasTag(db.TagInventory) {
			continue
		}
		out = append(out, job)
	}
	return out, nil
}

func minLaunchJobID(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	minID := ids[0]
	for _, id := range ids[1:] {
		if id < minID {
			minID = id
		}
	}
	return minID
}

type queueETAState struct {
	AvailByInstance  map[int64][]estimate.Estimate
	QueuedByInstance map[int64]int
}

func rebalanceQueuedJobsToLaunchedInstance(
	ctx context.Context,
	database *sql.DB,
	cfg *config.Config,
	targetInstanceID int64,
	anchorJobID int64,
	scoped map[int64]struct{},
) (moved int, warning string, err error) {
	if database == nil || targetInstanceID == 0 {
		return 0, "", nil
	}
	r2Client, _ := buildR2Client(cfg)
	scopedQueued, err := listScopedLaunchableJobs(database, scoped)
	if err != nil {
		return 0, "", err
	}

	// Refresh live state for scoped launch IDs before evaluating queue ETA.
	launchIDs := make([]int64, 0, len(scopedQueued))
	seenLaunch := make(map[int64]struct{})
	for _, job := range scopedQueued {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		if _, ok := seenLaunch[*job.LaunchID]; ok {
			continue
		}
		seenLaunch[*job.LaunchID] = struct{}{}
		launchIDs = append(launchIDs, *job.LaunchID)
	}
	launchLiveByID, _ := db.GetLaunchLiveStates(database, launchIDs)

	targetLaunch, err := db.GetLaunch(database, targetInstanceID)
	if err != nil || targetLaunch == nil {
		return 0, "", fmt.Errorf("load launched instance %s: %w", ids.FormatInstanceID(targetInstanceID), err)
	}
	targetLaunchJobs, _ := db.GetLaunchJobsIncludingAttempts(database, targetInstanceID)
	targetCap, ok := campaign.NewInstanceCapacity(targetLaunch, countRunningJobs(targetLaunchJobs))
	if !ok {
		return 0, "", fmt.Errorf("launched instance %s is not reusable", ids.FormatInstanceID(targetInstanceID))
	}

	candidates := make([]*db.Job, 0)
	for _, job := range scopedQueued {
		if job == nil || job.ID == anchorJobID {
			continue
		}
		if job.LaunchID == nil || *job.LaunchID <= 0 || *job.LaunchID == targetInstanceID {
			continue
		}
		if ok, _ := campaign.MatchJobToInstance(job, targetCap); !ok {
			continue
		}
		candidates = append(candidates, job)
	}
	if len(candidates) == 0 {
		return 0, "", nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	state := buildQueueETAState(scopedQueued, launchLiveByID, time.Now())
	currentMean := meanQueuedCompletionETA(state)
	if currentMean <= 0 {
		return 0, "", nil
	}

	for {
		bestGain := time.Duration(0)
		bestIdx := -1
		for i, job := range candidates {
			if job == nil || job.LaunchID == nil {
				continue
			}
			src := *job.LaunchID
			if src == 0 || src == targetInstanceID {
				continue
			}
			if state.QueuedByInstance[src] <= 0 {
				continue
			}
			next := cloneQueueETAState(state)
			next.QueuedByInstance[src]--
			next.QueuedByInstance[targetInstanceID]++
			nextMean := meanQueuedCompletionETA(next)
			gain := currentMean - nextMean
			if gain > bestGain {
				bestGain = gain
				bestIdx = i
			}
		}
		if bestIdx < 0 || bestGain <= 0 {
			break
		}
		job := candidates[bestIdx]
		candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)
		if job == nil || job.LaunchID == nil {
			continue
		}
		src := *job.LaunchID
		if _, unplaceErr := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); unplaceErr != nil {
			warning = "some queued jobs could not be moved"
			continue
		}
		refreshed, getErr := db.GetJobByID(database, job.ID)
		if getErr != nil || refreshed == nil {
			return moved, warning, fmt.Errorf("reload moved job %d: %w", job.ID, getErr)
		}
		if submitErr := campaign.SubmitJobsToInstance(ctx, database, r2Client, targetInstanceID, []*db.Job{refreshed}); submitErr != nil {
			return moved, warning, fmt.Errorf("submit moved job %d to instance %s: %w", job.ID, ids.FormatInstanceID(targetInstanceID), submitErr)
		}
		moved++
		state.QueuedByInstance[src]--
		state.QueuedByInstance[targetInstanceID]++
		currentMean -= bestGain
	}
	return moved, warning, nil
}

func buildQueueETAState(jobs []*db.Job, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) queueETAState {
	state := queueETAState{
		AvailByInstance:  make(map[int64][]estimate.Estimate),
		QueuedByInstance: make(map[int64]int),
	}
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		instanceID := *job.LaunchID
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			state.AvailByInstance[instanceID] = append(
				state.AvailByInstance[instanceID],
				runningJobRemaining(job, launchLiveByID, now),
			)
		case db.StatusQueued, db.StatusPendingPlacement:
			state.QueuedByInstance[instanceID]++
		}
	}
	return state
}

func cloneQueueETAState(state queueETAState) queueETAState {
	copyState := queueETAState{
		AvailByInstance:  make(map[int64][]estimate.Estimate, len(state.AvailByInstance)),
		QueuedByInstance: make(map[int64]int, len(state.QueuedByInstance)),
	}
	for id, avail := range state.AvailByInstance {
		copyState.AvailByInstance[id] = append([]estimate.Estimate(nil), avail...)
	}
	for id, queued := range state.QueuedByInstance {
		copyState.QueuedByInstance[id] = queued
	}
	return copyState
}

func meanQueuedCompletionETA(state queueETAState) time.Duration {
	totalQueued := 0
	var total time.Duration
	for instanceID, queued := range state.QueuedByInstance {
		if queued <= 0 {
			continue
		}
		totalQueued += queued
		// Extract mean durations for the scheduling simulation.
		estimates := state.AvailByInstance[instanceID]
		avail := make([]time.Duration, len(estimates))
		for i, e := range estimates {
			avail[i] = e.Mean
		}
		if len(avail) == 0 {
			avail = []time.Duration{0}
		}
		for i := 0; i < queued; i++ {
			slot := 0
			for idx := 1; idx < len(avail); idx++ {
				if avail[idx] < avail[slot] {
					slot = idx
				}
			}
			completion := avail[slot] + estimate.DefaultJobDuration.Mean
			avail[slot] = completion
			total += completion
		}
	}
	if totalQueued == 0 {
		return 0
	}
	return total / time.Duration(totalQueued)
}

func (m *listTUIModel) pruneAutoBlockReasons() {
	if len(m.autoBlockReasons) == 0 {
		return
	}
	visible := make(map[int64]struct{}, len(m.jobs))
	for _, job := range m.jobs {
		if job != nil {
			visible[job.ID] = struct{}{}
		}
	}
	for jobID := range m.autoBlockReasons {
		if _, ok := visible[jobID]; !ok {
			delete(m.autoBlockReasons, jobID)
		}
	}
}

func (m listTUIModel) groupedJobsWithAutoReasons() []*db.Job {
	if len(m.autoBlockReasons) == 0 {
		return m.jobs
	}
	decorated := make([]*db.Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		if job == nil {
			decorated = append(decorated, nil)
			continue
		}
		reason, ok := m.autoBlockReasons[job.ID]
		if !ok || strings.TrimSpace(reason) == "" {
			decorated = append(decorated, job)
			continue
		}
		copyJob := *job
		copyJob.QueueBlockedReason = reason
		decorated = append(decorated, &copyJob)
	}
	return decorated
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
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown error"
	}
	if strings.Contains(msg, "submit jobs to instance control plane:") {
		if matches := graceAckKeyPattern.FindStringSubmatch(msg); len(matches) == 2 {
			if instanceID, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
				return fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", ids.FormatInstanceID(instanceID))
			}
			return fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", matches[1])
		}
		return "instance control plane did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)"
	}
	return msg
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
