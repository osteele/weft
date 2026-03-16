package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/tui"
	"github.com/spf13/cobra"
)

var projectWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch active and recent jobs grouped by project",
	Long: `Watch active and recent jobs grouped by project.

In an interactive terminal this defaults to a read-only TUI. Otherwise it
prints a grouped plain-text snapshot.`,
	RunE: runProjectWatch,
}

var (
	projectWatchTUI    bool
	projectWatchPlain  bool
	projectWatchSync   bool
	projectWatchNoSync bool
	projectWatchRecent time.Duration
)

const projectWatchSyncInterval = 30 * time.Second

type projectWatchModel struct {
	database         *sql.DB
	groups           []projectGroup
	cachedLines      []string
	recentWindow     time.Duration
	width            int
	height           int
	cursor           int
	offset           int
	syncEnabled      bool
	syncInProgress   bool
	statusMessage    string
	dbWatcher        *fsnotify.Watcher
	dbWatcherTargets map[string]struct{}
	debounceActive   bool
	syncWorker       *tui.SyncWorker
	ctx              context.Context
	cancel           context.CancelFunc
}

type projectWatchLoadedMsg struct {
	groups []projectGroup
	err    error
}

type projectWatchSyncFinishedMsg struct {
	warnings []string
	full     bool
}

type projectSyncWorkerResultMsg struct {
	result tui.SyncResult
}

type projectWatchDBWatcherReadyMsg struct {
	watcher *fsnotify.Watcher
	targets map[string]struct{}
	err     error
}

type projectWatchDBWatchEventMsg struct {
	err error
}

type projectWatchDBRefreshTriggeredMsg struct{}
type projectWatchSyncTickMsg struct{}

func init() {
	projectCmd.AddCommand(projectWatchCmd)
	projectWatchCmd.Flags().BoolVar(&projectWatchTUI, "tui", false, "Force interactive TUI mode")
	projectWatchCmd.Flags().BoolVar(&projectWatchPlain, "plain", false, "Force plain text output")
	projectWatchCmd.Flags().BoolVar(&projectWatchSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	projectWatchCmd.Flags().BoolVar(&projectWatchNoSync, "no-sync", false, "Skip syncing job statuses before displaying")
	projectWatchCmd.Flags().DurationVar(&projectWatchRecent, "recent", 24*time.Hour, "Window for recent terminal jobs")
	projectWatchCmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func runProjectWatch(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveCampaignTUI(projectWatchTUI, projectWatchPlain)
	if err != nil {
		return err
	}

	database, err := openJobsDB()
	if err != nil {
		return err
	}
	defer database.Close()

	if useTUI {
		return runProjectWatchTUI(database, projectWatchRecent, !projectWatchNoSync)
	}

	for _, warning := range syncProjectWatchData(database) {
		fmt.Fprintln(cmd.ErrOrStderr(), warning)
	}
	groups, err := loadProjectWatchGroups(database, projectWatchRecent)
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), renderProjectWatchPlain(groups, listOutputWidth(), time.Now(), projectWatchRecent))
	return err
}

func runProjectWatchTUI(database *sql.DB, recentWindow time.Duration, syncEnabled bool) error {
	ctx, cancel := context.WithCancel(context.Background())

	var sw *tui.SyncWorker
	if syncEnabled {
		cfg, _ := config.Load()
		sw = tui.NewSyncWorker(database, nil, nil, cfg)
		sw.Start()
	}

	model := projectWatchModel{
		database:       database,
		recentWindow:   recentWindow,
		syncEnabled:    syncEnabled,
		syncInProgress: syncEnabled,
		syncWorker:     sw,
		ctx:            ctx,
		cancel:         cancel,
	}

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

	_, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	cancel()
	if sw != nil {
		sw.Stop()
	}
	if err != nil {
		return fmt.Errorf("run project watch TUI: %w", err)
	}
	return nil
}

func (m projectWatchModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.startDBWatcher(), m.reloadGroups(), scheduleProjectWatchSyncTick()}
	if m.syncWorker != nil {
		m.requestActiveSyncs()
		cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
			return projectSyncWorkerResultMsg{result: r}
		}))
	} else if m.syncEnabled {
		cmds = append(cmds, m.runBackgroundSync(false))
	}
	return tea.Batch(cmds...)
}

func (m projectWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.cachedLines = m.computeContentLines()
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
			m.cursor--
		case "down", "j":
			m.cursor++
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			if lines := m.contentLines(); len(lines) > 0 {
				m.cursor = len(lines) - 1
			}
		case "pgdown", "space":
			m.cursor += m.pageSize()
		case "pgup", "b":
			m.cursor -= m.pageSize()
		case "r":
			if !m.syncEnabled || m.syncInProgress {
				return m, nil
			}
			m.syncInProgress = true
			m.statusMessage = "Refreshing..."
			return m, m.runBackgroundSync(false)
		}
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case projectWatchLoadedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			return m, nil
		}
		m.groups = msg.groups
		m.cachedLines = m.computeContentLines()
		if len(m.groups) == 0 && m.statusMessage == "" {
			m.statusMessage = "No project activity."
		}
		m.clampCursor()
		m.adjustOffset()
		return m, nil

	case projectWatchSyncFinishedMsg:
		if !msg.full {
			if len(msg.warnings) > 0 {
				m.statusMessage = strings.Join(msg.warnings, " | ")
			} else {
				m.statusMessage = "Running full sync..."
			}
			return m, tea.Batch(m.reloadGroups(), m.runBackgroundSync(true))
		}

		m.syncInProgress = false
		if len(msg.warnings) > 0 {
			m.statusMessage = strings.Join(msg.warnings, " | ")
		} else {
			m.statusMessage = "Synced."
		}
		return m, m.reloadGroups()

	case projectWatchDBWatcherReadyMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
			return m, nil
		}
		m.dbWatcher = msg.watcher
		m.dbWatcherTargets = msg.targets
		return m, m.waitForDBEvent()

	case projectWatchDBWatchEventMsg:
		cmds := []tea.Cmd{}
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
		} else if !m.debounceActive {
			m.debounceActive = true
			cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
				return projectWatchDBRefreshTriggeredMsg{}
			}))
		}
		if cmd := m.waitForDBEvent(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case projectWatchDBRefreshTriggeredMsg:
		m.debounceActive = false
		return m, m.reloadGroups()

	case projectSyncWorkerResultMsg:
		m.syncInProgress = false
		if msg.result.Error != nil {
			m.statusMessage = fmt.Sprintf("Sync error (%s): %v", msg.result.Host, msg.result.Error)
		} else if msg.result.Updated > 0 || m.statusMessage == "Refreshing..." {
			m.statusMessage = ""
		}
		return m, tea.Batch(
			m.reloadGroups(),
			m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
				return projectSyncWorkerResultMsg{result: r}
			}),
		)

	case projectWatchSyncTickMsg:
		cmds := []tea.Cmd{scheduleProjectWatchSyncTick()}
		if m.syncWorker != nil {
			m.requestActiveSyncs()
			cmds = append(cmds, m.reloadGroups())
		} else if m.syncEnabled && !m.syncInProgress {
			m.syncInProgress = true
			m.statusMessage = "Refreshing..."
			cmds = append(cmds, m.runBackgroundSync(true))
		}
		return m, tea.Batch(cmds...)
	}

	return m, nil
}

var projectWatchTitleStyle = lipgloss.NewStyle().Bold(true)
var projectWatchFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
var projectWatchSelectedStyle = lipgloss.NewStyle().Reverse(true)
var projectWatchEmptyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)

func (m projectWatchModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading..."
	}

	lines := m.contentLines()
	rows := m.pageSize()
	var b strings.Builder
	title := fmt.Sprintf("Project Watch (%d projects)", len(m.groups))
	b.WriteString(projectWatchTitleStyle.Render(truncateDisplayWidth(title, m.width)))
	b.WriteString("\n")

	if len(lines) == 0 {
		b.WriteString(projectWatchEmptyStyle.Render(truncateDisplayWidth(m.emptyStateText(), m.width)))
		b.WriteString("\n")
		for i := 1; i < rows; i++ {
			b.WriteString("\n")
		}
	} else {
		for i := 0; i < rows; i++ {
			idx := m.offset + i
			if idx >= len(lines) {
				b.WriteString("\n")
				continue
			}
			line := truncateDisplayWidth(lines[idx], m.width)
			if idx == m.cursor {
				line = projectWatchSelectedStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString(projectWatchFooterStyle.Render(truncateDisplayWidth(m.footerText(rows, len(lines)), m.width)))
	return b.String()
}

func (m projectWatchModel) contentLines() []string {
	if m.cachedLines == nil && len(m.groups) > 0 {
		return m.computeContentLines()
	}
	return m.cachedLines
}

func (m projectWatchModel) computeContentLines() []string {
	rendered := strings.TrimRight(renderProjectWatchPlain(m.groups, max(20, m.width-2), time.Now(), m.recentWindow), "\n")
	if rendered == "" {
		return nil
	}
	return strings.Split(rendered, "\n")
}

func (m projectWatchModel) footerText(rows, total int) string {
	start := 0
	end := 0
	if total > 0 {
		start = m.offset + 1
		end = min(total, m.offset+rows)
	}

	state := fmt.Sprintf("[%d-%d/%d]", start, end, total)
	if m.syncInProgress {
		state += " syncing..."
	}
	if m.statusMessage != "" {
		state += "  " + m.statusMessage
	}
	state += "  up/down move  space/b page  g/G top/bottom  r refresh  q quit"
	return state
}

func (m projectWatchModel) emptyStateText() string {
	if m.syncInProgress {
		return "No project activity yet. Waiting for startup sync and DB updates..."
	}
	if m.statusMessage != "" {
		return "No project activity. " + m.statusMessage
	}
	return "No project activity."
}

func (m *projectWatchModel) clampCursor() {
	lines := m.contentLines()
	if len(lines) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(lines) {
		m.cursor = len(lines) - 1
	}
}

func (m *projectWatchModel) adjustOffset() {
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
	lines := m.contentLines()
	maxOffset := max(0, len(lines)-pageSize)
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m projectWatchModel) pageSize() int {
	if m.height <= 0 {
		return 10
	}
	return max(1, m.height-3)
}

func (m projectWatchModel) reloadGroups() tea.Cmd {
	database := m.database
	recentWindow := m.recentWindow
	return func() tea.Msg {
		groups, err := loadProjectWatchGroups(database, recentWindow)
		return projectWatchLoadedMsg{groups: groups, err: err}
	}
}

func (m projectWatchModel) runBackgroundSync(full bool) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		return projectWatchSyncFinishedMsg{warnings: syncProjectWatchTUIData(database, full), full: full}
	}
}

func syncProjectWatchTUIData(database *sql.DB, full bool) []string {
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

func (m projectWatchModel) requestActiveSyncs() {
	if m.syncWorker == nil {
		return
	}
	hosts := make(map[string][]*db.Job)
	for _, g := range m.groups {
		for _, job := range append(g.Running, g.Queued...) {
			if job != nil && job.Host != "" {
				hosts[job.Host] = append(hosts[job.Host], job)
			}
		}
	}
	for host, jobs := range hosts {
		m.syncWorker.Request(tui.SyncRequest{
			Host: host,
			Rate: tui.GetHostSyncRate(jobs),
		})
	}
}

func scheduleProjectWatchSyncTick() tea.Cmd {
	return tea.Tick(projectWatchSyncInterval, func(time.Time) tea.Msg {
		return projectWatchSyncTickMsg{}
	})
}

func (m projectWatchModel) startDBWatcher() tea.Cmd {
	dbFile := db.Path()
	if dbFile == "" {
		return nil
	}
	dir := filepath.Dir(dbFile)
	targets := map[string]struct{}{}
	addTarget := func(name string) {
		if name == "" {
			return
		}
		targets[filepath.Clean(filepath.Join(dir, name))] = struct{}{}
	}
	base := filepath.Base(dbFile)
	addTarget(base)
	addTarget(base + "-wal")
	addTarget(base + "-shm")

	return func() tea.Msg {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return projectWatchDBWatcherReadyMsg{err: err}
		}
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return projectWatchDBWatcherReadyMsg{err: err}
		}
		return projectWatchDBWatcherReadyMsg{watcher: watcher, targets: targets}
	}
}

func (m projectWatchModel) waitForDBEvent() tea.Cmd {
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
					return projectWatchDBWatchEventMsg{err: fmt.Errorf("db watcher closed")}
				}
				if !listTUIWatchedDBFile(event.Name, targets) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				return projectWatchDBWatchEventMsg{}
			case err, ok := <-watcher.Errors:
				if !ok {
					return projectWatchDBWatchEventMsg{err: fmt.Errorf("db watcher error channel closed")}
				}
				return projectWatchDBWatchEventMsg{err: err}
			}
		}
	}
}

func loadProjectWatchGroups(database *sql.DB, recentWindow time.Duration) ([]projectGroup, error) {
	activeJobs := make([]*db.Job, 0)
	running, err := db.ListAllRunning(database)
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	activeJobs = append(activeJobs, running...)

	for _, status := range []string{db.StatusStarting, db.StatusPaused, db.StatusQueued, db.StatusPendingPlacement} {
		jobs, err := db.ListJobs(database, status, "", 0, nil, "")
		if err != nil {
			return nil, fmt.Errorf("list %s jobs: %w", status, err)
		}
		activeJobs = append(activeJobs, jobs...)
	}

	recentJobs, err := db.ListRecentTerminalJobs(database, time.Now().Add(-recentWindow).Unix())
	if err != nil {
		return nil, fmt.Errorf("list recent terminal jobs: %w", err)
	}

	groups := groupProjectActivity(activeJobs, recentJobs)
	if err := attachProjectCloudInstances(database, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func attachProjectCloudInstances(database *sql.DB, groups []projectGroup) error {
	cache := make(map[int64]*db.CloudInstance)
	for i := range groups {
		seen := make(map[int64]struct{})
		appendJobInstance := func(job *db.Job) error {
			if job == nil || job.CloudInstanceID == nil || *job.CloudInstanceID <= 0 {
				return nil
			}
			instanceID := *job.CloudInstanceID
			if _, ok := seen[instanceID]; ok {
				return nil
			}
			seen[instanceID] = struct{}{}

			inst, ok := cache[instanceID]
			if !ok {
				var err error
				inst, err = db.GetCloudInstance(database, instanceID)
				if err != nil {
					return fmt.Errorf("get cloud instance %d: %w", instanceID, err)
				}
				cache[instanceID] = inst
			}
			if inst != nil {
				groups[i].CloudInsts = append(groups[i].CloudInsts, inst)
			}
			return nil
		}

		for _, bucket := range [][]*db.Job{groups[i].Running, groups[i].Queued, groups[i].Recent} {
			for _, job := range bucket {
				if err := appendJobInstance(job); err != nil {
					return err
				}
			}
		}
		sort.SliceStable(groups[i].CloudInsts, func(a, b int) bool {
			return groups[i].CloudInsts[a].ID < groups[i].CloudInsts[b].ID
		})
	}
	return nil
}

func syncProjectWatchData(database *sql.DB) []string {
	if projectWatchNoSync {
		return nil
	}

	var warnings []string
	if projectWatchSync {
		completed, unreachable, syncWarnings := performSyncWithTimeoutForHostsDetailed(database, nil, DefaultSyncTimeout, false)
		warnings = append(warnings, syncWarnings...)
		if !completed {
			if note := buildStaleDataNote(database, unreachable); note != "" {
				warnings = append(warnings, note)
			}
		}
		return warnings
	}

	completed, unreachable, syncWarnings := performSyncWithTimeoutForHostsDetailed(database, nil, FastSyncTimeout, false)
	warnings = append(warnings, syncWarnings...)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}
