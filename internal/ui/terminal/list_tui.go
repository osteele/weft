package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
)

const listDBChangeDebounce = 200 * time.Millisecond
const listTUISyncInterval = 30 * time.Second

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
}

type listJobsLoadedMsg struct {
	jobs []*db.Job
	err  error
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

var (
	listTUITitleStyle    = lipgloss.NewStyle().Bold(true)
	listTUIHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUISelectedStyle = lipgloss.NewStyle().Reverse(true)
	listTUIFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	listTUIEmptyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
)

func runListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool) error {
	ctx, cancel := context.WithCancel(context.Background())

	var sw *hostsync.Worker
	if syncEnabled {
		cfg, _ := config.Load()
		sw = hostsync.New(database, nil, nil, cfg)
		sw.Start()
	}

	model := listTUIModel{
		database:        database,
		args:            append([]string(nil), args...),
		title:           title,
		jobs:            jobs,
		syncEnabled:     syncEnabled,
		syncInProgress:  syncEnabled,
		syncWorker:      sw,
		ctx:             ctx,
		cancel:          cancel,
		groupedByStatus: groupedByStatus,
	}

	restore := logging.Suppress()
	defer restore()

	_, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
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
	cmds := []tea.Cmd{m.startDBWatcher(), m.reloadJobs(), scheduleListSyncTick()}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
		cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
			return listSyncWorkerResultMsg{result: r}
		}))
	} else if m.syncEnabled {
		cmds = append(cmds, m.runBackgroundSync(false))
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
		}
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case listJobsLoadedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			return m, nil
		}
		m.jobs = msg.jobs
		m.rebuildLayout()
		m.clampCursor()
		m.adjustOffset()
		if len(m.jobs) > 0 && strings.HasPrefix(m.statusMessage, "No jobs") {
			m.statusMessage = ""
		}
		return m, nil

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
		cmds := []tea.Cmd{scheduleListSyncTick()}
		if m.syncWorker != nil {
			m.requestActiveSyncs()
			cmds = append(cmds, m.reloadJobs())
		} else if m.syncEnabled && !m.syncInProgress {
			m.syncInProgress = true
			m.statusMessage = "Refreshing..."
			cmds = append(cmds, m.runBackgroundSync(true))
		}
		return m, tea.Batch(cmds...)
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

	body := renderJobListGroupedStatusPlain(m.jobs, m.width)
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		body = "None"
	}
	bodyLines := strings.Split(body, "\n")
	for _, line := range bodyLines {
		b.WriteString(truncateDisplayWidth(line, m.width))
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
	return state + " q:quit"
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
		return listJobsLoadedMsg{jobs: jobs, err: err}
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
	return warnings
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

func scheduleListSyncTick() tea.Cmd {
	return tea.Tick(listTUISyncInterval, func(time.Time) tea.Msg {
		return listSyncTickMsg{}
	})
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
