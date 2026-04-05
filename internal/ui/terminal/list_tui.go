package terminal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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
	"github.com/osteele/weft/internal/logging"
)

const listDBChangeDebounce = 200 * time.Millisecond
const listTUISyncInterval = TerminalSyncInterval
const listAutoLeaseTTL = 30 * time.Second

type listTUIModel struct {
	database         *sql.DB
	args             []string
	title            string
	jobs             []*db.Job
	layout           jobListLayout
	cursor           int
	offset           int
	width            int
	height           int
	syncEnabled      bool
	syncInProgress   bool
	statusMessage    string
	dbWatcher        *fsnotify.Watcher
	dbWatcherTargets map[string]struct{}
	debounceActive   bool
	syncWorker       *hostsync.Worker
	ctx              context.Context
	cancel           context.CancelFunc
	groupedByStatus  bool
	autoMode         bool
	autoInProgress   bool
	autoLeaseOwner   string
	autoLeaseScope   string
	autoBlockReasons map[int64]string
	launchLiveByID   map[int64]*db.LaunchLiveState
	quickLaunching   bool
	quickLaunchScope string
	focused          bool
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
	blockedReasons map[int64]string
	anotherHolding bool
	err            error
}
type listQuickLaunchDoneMsg struct {
	instanceIDs []int64
	err         error
}

var (
	listTUITitleStyle    = lipgloss.NewStyle().Bold(true)
	listTUIHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUISelectedStyle = lipgloss.NewStyle().Reverse(true)
	listTUIFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUIEmptyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, autoMode bool) error {
	ctx, cancel := context.WithCancel(context.Background())

	var sw *hostsync.Worker
	if syncEnabled {
		cfg, _ := config.Load()
		sw = hostsync.New(database, nil, nil, cfg)
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
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case tea.KeyMsg:
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
			return m, m.launchOneInstanceForQueue()
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
		m.clampCursor()
		m.adjustOffset()
		if len(m.jobs) > 0 && strings.HasPrefix(m.statusMessage, "No jobs") {
			m.statusMessage = ""
		}
		return m, m.runAutoPilot()

	case listSyncFinishedMsg:
		if !msg.full {
			if len(msg.warnings) > 0 {
				m.statusMessage = strings.Join(msg.warnings, " | ")
			} else {
				m.statusMessage = "Running full sync..."
			}
			return m, tea.Batch(m.reloadJobs(), m.runBackgroundSync(true))
		}

		m.syncInProgress = false
		if len(msg.warnings) > 0 {
			m.statusMessage = strings.Join(msg.warnings, " | ")
		} else if len(m.jobs) == 0 {
			m.statusMessage = "No jobs match this view."
		} else {
			m.statusMessage = "Synced."
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
			m.statusMessage = fmt.Sprintf("Sync error (%s): %v", msg.result.Host, msg.result.Error)
		} else if msg.result.Updated > 0 || m.statusMessage == "Refreshing..." {
			m.statusMessage = ""
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
			m.statusMessage = "Refreshing..."
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
			m.statusMessage = fmt.Sprintf("Auto-pilot failed: %v", msg.err)
			return m, nil
		}
		if msg.anotherHolding {
			m.statusMessage = "Auto-pilot: another TUI is active for this scope"
			return m, nil
		}
		switch {
		case msg.placed > 0 && msg.launched > 0:
			m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d, launched %d", msg.placed, msg.launched)
		case msg.placed > 0:
			m.statusMessage = fmt.Sprintf("Auto-pilot: placed %d", msg.placed)
		case msg.launched > 0:
			m.statusMessage = fmt.Sprintf("Auto-pilot: launched %d", msg.launched)
		default:
			m.statusMessage = "Auto-pilot: monitoring"
		}
		return m, m.reloadJobs()

	case listQuickLaunchDoneMsg:
		m.quickLaunching = false
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Launch failed: %v", msg.err)
			return m, nil
		}
		if len(msg.instanceIDs) == 0 {
			m.statusMessage = "No new instance launched."
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Instance #%d launched", msg.instanceIDs[0])
		return m, m.reloadJobs()
	}

	return m, nil
}

func (m listTUIModel) View() string {
	if m.groupedByStatus {
		return m.groupedView()
	}

	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	layout := m.layout
	var b strings.Builder

	title := fmt.Sprintf("%s (%d)", m.title, len(m.jobs))
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")
	b.WriteString(listTUIHeaderStyle.Render(truncateDisplayWidth(formatJobListHeader(layout), m.width)))
	b.WriteString("\n")

	rows := m.pageSize()
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
	body := renderJobListGroupedStatusPlainWithLiveState(groupedJobs, m.width, m.launchLiveByID)
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		body = "None"
	}
	bodyLines := strings.Split(body, "\n")
	for _, line := range bodyLines {
		b.WriteString(truncateDisplayWidth(line, m.width))
		b.WriteString("\n")
	}

	// Visually separate grouped job rows from ETA/actions footer lines.
	b.WriteString("\n")

	if etaLine := m.groupedETALine(groupedJobs); etaLine != "" {
		b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(etaLine, m.width)))
		b.WriteString("\n")
	}
	b.WriteString(listTUIFooterStyle.Render(truncateDisplayWidth(m.groupedFooterText(), m.width)))
	return b.String()
}

func (m listTUIModel) groupedFooterText() string {
	state := fmt.Sprintf("[%d jobs]", len(m.jobs))
	if m.syncInProgress {
		state += " syncing..."
	}
	if m.statusMessage != "" {
		state += " " + m.statusMessage
	}
	autoHint := "auto:OFF"
	if m.autoMode {
		autoHint = "auto:ON"
	}
	return state + " a:toggle-auto " + autoHint + " q:quit"
}

func (m listTUIModel) groupedETALine(groupedJobs []*db.Job) string {
	eta := computeGroupedETA(groupedJobs, m.launchLiveByID, time.Now())
	line := formatETALine(eta)
	if line == "" {
		return ""
	}
	if eta.HasQueued {
		line += "  n:new instance"
	}
	return line
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

func (m *listTUIModel) clampCursor() {
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

func (m *listTUIModel) adjustOffset() {
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
		placed, launched, blockedReasons, runErr := runGroupedAutoPilotPass(ctx, database, jobs)
		return listAutoPilotDoneMsg{placed: placed, launched: launched, blockedReasons: blockedReasons, err: runErr}
	}
}

func runGroupedAutoPilotPass(ctx context.Context, database *sql.DB, scopedJobs []*db.Job) (int, int, map[int64]string, error) {
	if database == nil {
		return 0, 0, nil, nil
	}
	scoped := make(map[int64]struct{}, len(scopedJobs))
	for _, job := range scopedJobs {
		if job != nil {
			scoped[job.ID] = struct{}{}
		}
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return 0, 0, nil, err
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
		return 0, 0, nil, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return 0, 0, nil, err
	}
	r2Client, _ := buildR2Client(cfg)

	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return 0, 0, nil, err
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
		return 0, 0, nil, err
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
			return placed, 0, blockedReasons, err
		}
		placed++
	}

	remaining, err := db.ListUnplacedJobs(database)
	if err != nil {
		return placed, 0, nil, err
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
		return placed, 0, blockedReasons, nil
	}

	failedInstanceByJob := buildFailedInstanceByJob(database, rentalScope)
	passStartedAt := time.Now().Unix()
	result, err := attemptRelaunchOrphanedJobs(database, cfg, 0, nil, rentalScope, "", false, true)
	if err != nil {
		return placed, 0, nil, err
	}
	if result == nil {
		return placed, 0, blockedReasons, nil
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
	return placed, len(result.InstanceIDs), blockedReasons, nil
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
	args := make([]any, 0, len(jobIDs)+1)
	args = append(args, sinceUnix)
	for _, jobID := range jobIDs {
		placeholders = append(placeholders, "?")
		args = append(args, jobID)
	}
	query := fmt.Sprintf(`SELECT job_id, event_kind, detail, attempt_number, max_attempts
		FROM lifecycle_events
		WHERE occurred_at >= ?
		  AND event_kind LIKE 'relaunch.skipped.%%'
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

func (m listTUIModel) launchOneInstanceForQueue() tea.Cmd {
	database := m.database
	scopeOwner := m.autoLeaseOwner
	scope := m.quickLaunchScope
	jobs := append([]*db.Job(nil), m.jobs...)

	return func() tea.Msg {
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
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return listQuickLaunchDoneMsg{err: err}
		}
		unplaced := make([]*db.Job, 0, len(unplacedJobs))
		for _, job := range unplacedJobs {
			if job == nil || (job.EffectiveStatus() != db.StatusQueued && job.EffectiveStatus() != db.StatusPendingPlacement) {
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
			return listQuickLaunchDoneMsg{err: fmt.Errorf("no queued jobs in this view")}
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

		plan, err := buildAutoPlacementPlan(database, cfg, unplaced, capacities)
		if err != nil {
			return listQuickLaunchDoneMsg{err: err}
		}
		if len(plan.LaunchJobIDs) == 0 {
			return listQuickLaunchDoneMsg{err: fmt.Errorf("queued jobs can be placed on existing instances")}
		}
		launchJobID := plan.LaunchJobIDs[0]
		for _, jobID := range plan.LaunchJobIDs {
			if jobID < launchJobID {
				launchJobID = jobID
			}
		}

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

		// Keep one-key behavior deterministic: launch only one additional instance.
		return listQuickLaunchDoneMsg{instanceIDs: []int64{result.InstanceIDs[0]}}
	}
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
