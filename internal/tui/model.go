package tui

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Default intervals for background operations
const (
	DefaultSyncInterval        = 15 * time.Second
	DefaultLogRefreshInterval  = 3 * time.Second
	DefaultHostRefreshInterval = 30 * time.Second
)

// ViewMode represents which view is currently active
type ViewMode int

const (
	ViewModeJobs ViewMode = iota
	ViewModeHosts
)

// Key bindings
type keyMap struct {
	Up        key.Binding
	Down      key.Binding
	Enter     key.Binding
	Logs      key.Binding
	Escape    key.Binding
	Kill      key.Binding
	Restart   key.Binding
	Remove    key.Binding
	NewJob    key.Binding
	Prune     key.Binding
	Suspend   key.Binding
	Quit      key.Binding
	HostsView key.Binding
	JobsView  key.Binding
	Tab       key.Binding
	Refresh   key.Binding
	Sync      key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	Logs: key.NewBinding(
		key.WithKeys("l"),
		key.WithHelp("l", "logs"),
	),
	Escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "clear"),
	),
	Kill: key.NewBinding(
		key.WithKeys("k", "delete"),
		key.WithHelp("k", "kill"),
	),
	Restart: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart"),
	),
	Remove: key.NewBinding(
		key.WithKeys("x"),
		key.WithHelp("x", "remove"),
	),
	NewJob: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "new job"),
	),
	Prune: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "prune"),
	),
	Suspend: key.NewBinding(
		key.WithKeys("ctrl+z"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
	HostsView: key.NewBinding(
		key.WithKeys("h"),
		key.WithHelp("h", "hosts"),
	),
	JobsView: key.NewBinding(
		key.WithKeys("j"),
		key.WithHelp("j", "jobs"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch view"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("R"),
		key.WithHelp("R", "refresh"),
	),
	Sync: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "sync"),
	),
}

// Messages
type jobsRefreshedMsg struct {
	jobs []*db.Job
	err  error
}

type syncCompletedMsg struct {
	updated int
	err     error
}

type logFetchedMsg struct {
	jobID   int64
	content string
	err     error
}

type jobKilledMsg struct {
	jobID int64
	err   error
}

type jobRestartedMsg struct {
	oldJobID int64
	newJobID int64
	err      error
}

type pruneCompletedMsg struct {
	count int64
	err   error
}

type jobRemovedMsg struct {
	jobID int64
	err   error
}

type jobCreatedMsg struct {
	jobID int64
	err   error
}

type jobCreateProgressMsg struct {
	step string
}

type tickMsg time.Time
type logTickMsg time.Time
type createTickMsg time.Time
type hostRefreshTickMsg time.Time

// Host-related messages
type hostsLoadedMsg struct {
	hostNames []string
	err       error
}

type hostInfoMsg struct {
	hostName string
	info     *Host
}

type processStatsMsg struct {
	jobID int64
	stats *ssh.ProcessStats
}

// Input field indices for new job form
const (
	inputHost = iota
	inputCommand
	inputDescription
	inputWorkingDir
)

// Model is the main TUI state
type Model struct {
	// View mode
	viewMode ViewMode

	// Jobs data
	jobs          []*db.Job
	selectedIndex int
	selectedJob   *db.Job

	// Hosts data
	hosts           []*Host
	selectedHostIdx int

	// UI State
	logContent    string
	logLoading    bool
	statusMessage string
	errorMessage  string

	// Process stats for running jobs
	processStats      *ssh.ProcessStats
	prevProcessStats  *ssh.ProcessStats // Previous sample for CPU% calculation
	processStatsJobID int64

	// Operation state
	restarting         bool
	restartingJobName  string
	pendingSelectJobID int64

	// New job input mode
	inputMode      bool
	inputFocus     int
	inputs         []textinput.Model
	creatingJob    bool
	createJobStart time.Time
	createJobStep  string

	// Layout
	width  int
	height int

	// Database connection
	database *sql.DB

	// Background sync state
	syncing      bool
	lastSyncTime time.Time

	// Configurable intervals
	syncInterval        time.Duration
	logRefreshInterval  time.Duration
	hostRefreshInterval time.Duration
}

// ModelOptions contains configuration for the TUI model
type ModelOptions struct {
	SyncInterval        time.Duration
	LogRefreshInterval  time.Duration
	HostRefreshInterval time.Duration
}

// DefaultModelOptions returns the default TUI options
func DefaultModelOptions() ModelOptions {
	return ModelOptions{
		SyncInterval:        DefaultSyncInterval,
		LogRefreshInterval:  DefaultLogRefreshInterval,
		HostRefreshInterval: DefaultHostRefreshInterval,
	}
}

// NewModel creates a new TUI model
func NewModel(database *sql.DB) Model {
	return NewModelWithOptions(database, DefaultModelOptions())
}

// NewModelWithOptions creates a new TUI model with custom options
func NewModelWithOptions(database *sql.DB, opts ModelOptions) Model {
	// Create text inputs for new job form
	inputs := make([]textinput.Model, 4)

	inputs[inputHost] = textinput.New()
	inputs[inputHost].Placeholder = "e.g., cool30"
	inputs[inputHost].Prompt = ""
	inputs[inputHost].Width = 40
	inputs[inputHost].CharLimit = 64

	inputs[inputCommand] = textinput.New()
	inputs[inputCommand].Placeholder = "e.g., python train.py"
	inputs[inputCommand].Prompt = ""
	inputs[inputCommand].Width = 40
	inputs[inputCommand].CharLimit = 512

	inputs[inputDescription] = textinput.New()
	inputs[inputDescription].Placeholder = "(optional)"
	inputs[inputDescription].Prompt = ""
	inputs[inputDescription].Width = 40
	inputs[inputDescription].CharLimit = 256

	inputs[inputWorkingDir] = textinput.New()
	inputs[inputWorkingDir].Placeholder = "(optional, defaults to ~)"
	inputs[inputWorkingDir].Prompt = ""
	inputs[inputWorkingDir].Width = 40
	inputs[inputWorkingDir].CharLimit = 256

	return Model{
		database:            database,
		selectedIndex:       0,
		inputs:              inputs,
		syncInterval:        opts.SyncInterval,
		logRefreshInterval:  opts.LogRefreshInterval,
		hostRefreshInterval: opts.HostRefreshInterval,
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshJobs(),
		m.loadHosts(),
		m.startSyncTicker(),
		m.startLogTicker(),
		m.startHostRefreshTicker(),
	)
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.inputMode {
			return m.handleInputKeyPress(msg)
		}
		return m.handleKeyPress(msg)

	case jobsRefreshedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Error loading jobs: %v", msg.err)
		} else {
			m.jobs = msg.jobs
			m.errorMessage = ""

			// If there's a pending job selection, find and select it
			if m.pendingSelectJobID > 0 {
				for i, job := range m.jobs {
					if job.ID == m.pendingSelectJobID {
						m.selectedIndex = i
						break
					}
				}
				m.pendingSelectJobID = 0
			}

			// Keep selection in bounds
			if m.selectedIndex >= len(m.jobs) {
				m.selectedIndex = len(m.jobs) - 1
			}
			if m.selectedIndex < 0 {
				m.selectedIndex = 0
			}
		}
		return m, nil

	case syncCompletedMsg:
		m.syncing = false
		m.lastSyncTime = time.Now()
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Sync error: %v", msg.err)
		} else if msg.updated > 0 {
			m.statusMessage = fmt.Sprintf("Synced %d job(s)", msg.updated)
			return m, m.refreshJobs()
		}
		return m, nil

	case logFetchedMsg:
		m.logLoading = false
		if msg.err != nil {
			m.logContent = fmt.Sprintf("Error: %v", msg.err)
		} else if m.selectedJob != nil && msg.jobID == m.selectedJob.ID {
			m.logContent = msg.content
		}
		return m, nil

	case processStatsMsg:
		// Accept stats for the currently highlighted job (whether in log mode or not)
		targetJob := m.getTargetJob()
		if targetJob != nil && msg.jobID == targetJob.ID {
			// Only update if we got valid stats (Running=true means process check succeeded)
			// Don't overwrite good stats with failed fetches
			if msg.stats.Running || m.processStats == nil || m.processStatsJobID != msg.jobID {
				// Calculate CPU% from delta if we have a previous sample
				if m.prevProcessStats != nil && m.processStatsJobID == msg.jobID &&
					msg.stats.Timestamp > m.prevProcessStats.Timestamp && msg.stats.Running {
					deltaTicks := (msg.stats.CPUUserTicks + msg.stats.CPUSysTicks) -
						(m.prevProcessStats.CPUUserTicks + m.prevProcessStats.CPUSysTicks)
					deltaTime := msg.stats.Timestamp - m.prevProcessStats.Timestamp
					// CPU% = (ticks / (time_seconds * CLK_TCK)) * 100
					// CLK_TCK is typically 100, so ticks/time gives rough %
					if deltaTime > 0 {
						msg.stats.CPUPct = float64(deltaTicks) / float64(deltaTime)
					}
				}
				if msg.stats.Running {
					m.prevProcessStats = m.processStats
				}
				m.processStats = msg.stats
				m.processStatsJobID = msg.jobID
			}
		}
		return m, nil

	case jobKilledMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Kill failed: %v", msg.err)
		} else {
			m.statusMessage = "Job killed"
		}
		return m, m.refreshJobs()

	case jobRestartedMsg:
		m.restarting = false
		m.restartingJobName = ""
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Restart failed: %v", msg.err)
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Job restarted (new ID: %d)", msg.newJobID)
		m.pendingSelectJobID = msg.newJobID
		return m, m.refreshJobs()

	case pruneCompletedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Prune failed: %v", msg.err)
		} else if msg.count > 0 {
			m.statusMessage = fmt.Sprintf("Pruned %d job(s)", msg.count)
		} else {
			m.statusMessage = "No jobs to prune"
		}
		return m, m.refreshJobs()

	case jobRemovedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Remove failed: %v", msg.err)
		} else {
			m.statusMessage = "Job removed"
			m.selectedJob = nil
			m.logContent = ""
		}
		return m, m.refreshJobs()

	case jobCreateProgressMsg:
		m.createJobStep = msg.step
		return m, nil

	case jobCreatedMsg:
		m.creatingJob = false
		m.createJobStep = ""
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Create failed: %v", msg.err)
		} else {
			m.statusMessage = fmt.Sprintf("Job %d started", msg.jobID)
			m.pendingSelectJobID = msg.jobID
			// Keep inputs for easy re-use (user can modify and submit again)
		}
		return m, m.refreshJobs()

	case tickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startSyncTicker())
		if !m.syncing {
			m.syncing = true
			cmds = append(cmds, m.performBackgroundSync())
		}
		return m, tea.Batch(cmds...)

	case logTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startLogTicker())
		// Refresh logs if in log mode
		if m.selectedJob != nil && m.selectedJob.Status == db.StatusRunning {
			cmds = append(cmds, m.fetchSelectedJobLog())
		}
		// Refresh process stats for highlighted running job (even if not in log mode)
		targetJob := m.getTargetJob()
		if targetJob != nil && targetJob.Status == db.StatusRunning {
			cmds = append(cmds, m.fetchProcessStats(targetJob))
		}
		return m, tea.Batch(cmds...)

	case createTickMsg:
		// Only continue ticking if still creating
		if m.creatingJob {
			return m, m.startCreateTicker()
		}
		return m, nil

	case hostsLoadedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Error loading hosts: %v", msg.err)
		} else {
			// Initialize hosts with names, set status to checking
			var cmds []tea.Cmd
			for _, name := range msg.hostNames {
				// Check if host already exists
				found := false
				for _, h := range m.hosts {
					if h.Name == name {
						found = true
						break
					}
				}
				if !found {
					host := &Host{
						Name:   name,
						Status: HostStatusChecking,
					}
					m.hosts = append(m.hosts, host)
					cmds = append(cmds, m.fetchHostInfo(name))
				}
			}
			if len(cmds) > 0 {
				return m, tea.Batch(cmds...)
			}
		}
		return m, nil

	case hostInfoMsg:
		// Update host info
		for i, h := range m.hosts {
			if h.Name == msg.hostName {
				msg.info.Name = msg.hostName
				m.hosts[i] = msg.info
				break
			}
		}
		return m, nil

	case hostRefreshTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startHostRefreshTicker())
		// Only refresh hosts if in hosts view
		if m.viewMode == ViewModeHosts {
			for _, host := range m.hosts {
				cmds = append(cmds, m.fetchHostInfo(host.Name))
			}
		}
		return m, tea.Batch(cmds...)
	}

	return m, nil
}

func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Allow cancelling job creation with Escape
	if m.creatingJob && key.Matches(msg, keys.Escape) {
		m.creatingJob = false
		m.createJobStep = ""
		m.statusMessage = "Job creation running in background..."
		return m, nil
	}

	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Suspend):
		return m, tea.Suspend

	case key.Matches(msg, keys.Tab):
		// Toggle between views
		if m.viewMode == ViewModeJobs {
			m.viewMode = ViewModeHosts
			// Refresh hosts when switching to hosts view
			var cmds []tea.Cmd
			for _, host := range m.hosts {
				cmds = append(cmds, m.fetchHostInfo(host.Name))
			}
			return m, tea.Batch(cmds...)
		}
		m.viewMode = ViewModeJobs
		return m, nil

	case key.Matches(msg, keys.HostsView):
		if m.viewMode != ViewModeHosts {
			m.viewMode = ViewModeHosts
			// Refresh hosts when switching to hosts view
			var cmds []tea.Cmd
			for _, host := range m.hosts {
				cmds = append(cmds, m.fetchHostInfo(host.Name))
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case key.Matches(msg, keys.JobsView):
		m.viewMode = ViewModeJobs
		return m, nil

	case key.Matches(msg, keys.Up):
		if m.viewMode == ViewModeHosts {
			if m.selectedHostIdx > 0 {
				m.selectedHostIdx--
			}
		} else {
			if m.selectedIndex > 0 {
				m.selectedIndex--
				// Clear cached process stats when changing jobs
				m.processStats = nil
				m.prevProcessStats = nil
				m.processStatsJobID = 0
				// If in logs mode, fetch logs for new job
				if m.selectedJob != nil && len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
					m.selectedJob = m.jobs[m.selectedIndex]
					m.logLoading = true
					var cmds []tea.Cmd
					cmds = append(cmds, m.fetchSelectedJobLog())
					// Fetch process stats for running jobs
					if m.selectedJob.Status == db.StatusRunning {
						cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
					}
					return m, tea.Batch(cmds...)
				}
				// Even if not in logs mode, fetch stats for running jobs
				if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
					job := m.jobs[m.selectedIndex]
					if job.Status == db.StatusRunning {
						return m, m.fetchProcessStats(job)
					}
				}
			}
		}
		return m, nil

	case key.Matches(msg, keys.Down):
		if m.viewMode == ViewModeHosts {
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts)-1 {
				m.selectedHostIdx++
			}
		} else {
			if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs)-1 {
				m.selectedIndex++
				// Clear cached process stats when changing jobs
				m.processStats = nil
				m.prevProcessStats = nil
				m.processStatsJobID = 0
				// If in logs mode, fetch logs for new job
				if m.selectedJob != nil && m.selectedIndex < len(m.jobs) {
					m.selectedJob = m.jobs[m.selectedIndex]
					m.logLoading = true
					var cmds []tea.Cmd
					cmds = append(cmds, m.fetchSelectedJobLog())
					// Fetch process stats for running jobs
					if m.selectedJob.Status == db.StatusRunning {
						cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
					}
					return m, tea.Batch(cmds...)
				}
				// Even if not in logs mode, fetch stats for running jobs
				if m.selectedIndex < len(m.jobs) {
					job := m.jobs[m.selectedIndex]
					if job.Status == db.StatusRunning {
						return m, m.fetchProcessStats(job)
					}
				}
			}
		}
		return m, nil

	case key.Matches(msg, keys.Refresh):
		if m.viewMode == ViewModeHosts && len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts) {
			host := m.hosts[m.selectedHostIdx]
			host.Status = HostStatusChecking
			return m, m.fetchHostInfo(host.Name)
		}
		return m, nil

	case key.Matches(msg, keys.Logs):
		if m.viewMode == ViewModeJobs {
			// Toggle logs mode
			if m.selectedJob != nil {
				// Already in logs mode - go back to details
				m.selectedJob = nil
				m.logContent = ""
			} else if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
				// Enter logs mode
				m.selectedJob = m.jobs[m.selectedIndex]
				m.logLoading = true
				var cmds []tea.Cmd
				cmds = append(cmds, m.fetchSelectedJobLog())
				// Fetch process stats for running jobs
				if m.selectedJob.Status == db.StatusRunning {
					cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
				}
				return m, tea.Batch(cmds...)
			}
		}
		return m, nil

	case key.Matches(msg, keys.Escape):
		m.selectedJob = nil
		m.logContent = ""
		m.statusMessage = ""
		m.errorMessage = ""
		return m, nil

	case key.Matches(msg, keys.Kill):
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusRunning {
			m.statusMessage = "Killing job..."
			return m, m.killJob(job)
		}
		return m, nil

	case key.Matches(msg, keys.Restart):
		job := m.getTargetJob()
		if job == nil {
			m.errorMessage = "No job selected"
			return m, nil
		}
		if m.restarting {
			m.statusMessage = "Restart already in progress..."
			return m, nil
		}
		m.restarting = true
		m.restartingJobName = fmt.Sprintf("job %d", job.ID)
		m.statusMessage = fmt.Sprintf("Restarting job %d...", job.ID)
		m.errorMessage = ""
		return m, m.restartJob(job)

	case key.Matches(msg, keys.Remove):
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		m.statusMessage = "Removing job..."
		return m, m.removeJob(job)

	case key.Matches(msg, keys.NewJob):
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.errorMessage = ""
		m.statusMessage = ""

		// Pre-populate from highlighted job if inputs are empty
		job := m.getTargetJob()
		if job != nil && m.inputs[inputHost].Value() == "" {
			m.inputs[inputHost].SetValue(job.Host)
			m.inputs[inputCommand].SetValue(job.Command)
			// Don't pre-populate description - it may contain error messages from failed jobs
			// and descriptions are usually different for each job anyway
			m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		}
		return m, nil

	case key.Matches(msg, keys.Prune):
		m.statusMessage = "Pruning completed/dead jobs..."
		return m, m.pruneJobs()

	case key.Matches(msg, keys.Sync):
		if m.viewMode == ViewModeJobs && !m.syncing {
			m.syncing = true
			m.statusMessage = "Syncing..."
			return m, m.performBackgroundSync()
		}
		return m, nil
	}

	return m, nil
}

func (m Model) handleInputKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Cancel input mode
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		return m, nil

	case tea.KeyTab, tea.KeyShiftTab:
		// Cycle through inputs
		m.inputs[m.inputFocus].Blur()
		if msg.Type == tea.KeyShiftTab {
			m.inputFocus--
			if m.inputFocus < 0 {
				m.inputFocus = len(m.inputs) - 1
			}
		} else {
			m.inputFocus++
			if m.inputFocus >= len(m.inputs) {
				m.inputFocus = 0
			}
		}
		m.inputs[m.inputFocus].Focus()
		return m, nil

	case tea.KeyEnter:
		// Submit if we have required fields
		host := strings.TrimSpace(m.inputs[inputHost].Value())
		command := strings.TrimSpace(m.inputs[inputCommand].Value())

		if host == "" || command == "" {
			m.errorMessage = "Host and command are required"
			return m, nil
		}

		// Exit input mode and create job
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		m.creatingJob = true
		m.createJobStart = time.Now()
		m.createJobStep = "Connecting..."
		m.statusMessage = ""
		return m, tea.Batch(m.createJob(), m.startCreateTicker())
	}

	// Forward other keys to the focused input
	var cmd tea.Cmd
	m.inputs[m.inputFocus], cmd = m.inputs[m.inputFocus].Update(msg)
	return m, cmd
}

// View renders the UI
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	// Calculate panel heights
	listHeight := int(float64(m.height) * 0.55)
	detailHeight := int(float64(m.height) * 0.35)

	var mainView string

	if m.viewMode == ViewModeHosts {
		// Hosts view
		listView := m.renderHostList(listHeight)
		detailView := m.renderHostDetail(detailHeight)
		statusView := m.renderHostsStatusBar()

		mainView = lipgloss.JoinVertical(
			lipgloss.Left,
			listView,
			detailView,
			statusView,
		)
	} else {
		// Jobs view (default)
		listView := m.renderJobList(listHeight)
		logView := m.renderLogPanel(detailHeight)
		statusView := m.renderStatusBar()

		mainView = lipgloss.JoinVertical(
			lipgloss.Left,
			listView,
			logView,
			statusView,
		)
	}

	// Show modal overlay for long-running operations
	if m.restarting {
		return m.renderWithModal(mainView, fmt.Sprintf("Restarting %s...", m.restartingJobName))
	}

	if m.creatingJob {
		elapsed := time.Since(m.createJobStart).Truncate(time.Second)
		msg := fmt.Sprintf("Creating job... %s\n\n%s\n\nPress Esc to dismiss", elapsed, m.createJobStep)
		return m.renderWithModal(mainView, msg)
	}

	// Show input form
	if m.inputMode {
		return m.renderInputForm(mainView)
	}

	return mainView
}

func (m Model) renderWithModal(background, message string) string {
	// Create modal box
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 3).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("229"))

	modal := modalStyle.Render(message)

	// Place modal centered on screen
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) renderInputForm(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(60)

	labelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("245"))
	focusedLabelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("69")).Bold(true)

	var b strings.Builder
	b.WriteString("New Job\n\n")

	labels := []string{"Host:", "Command:", "Description:", "Working Dir:"}
	for i, input := range m.inputs {
		label := labelStyle
		if i == m.inputFocus {
			label = focusedLabelStyle
		}
		b.WriteString(label.Render(labels[i]))
		b.WriteString(input.View())
		b.WriteString("\n\n")
	}

	b.WriteString("\n")
	helpText := "Tab: next field • Enter: create job • Esc: cancel"
	if m.errorMessage != "" {
		helpText = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.errorMessage)
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(helpText))

	modal := modalStyle.Render(b.String())

	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) renderJobList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %s",
		"ID", "HOST", "STATUS", "STARTED", "COMMAND / DESCRIPTION")
	rows = append(rows, headerStyle.Render(header))

	// Jobs
	contentHeight := height - 4 // Account for borders and header
	for i, job := range m.jobs {
		if i >= contentHeight {
			break
		}

		status := m.formatStatus(job)
		started := formatStartTime(job.StartTime)

		// Show description if available, otherwise truncated command
		display := job.Description
		if display == "" {
			display = job.Command
		}
		if len(display) > 40 {
			display = display[:37] + "..."
		}

		line := fmt.Sprintf(" %-4d %-10s %-12s %-12s %s",
			job.ID, truncate(job.Host, 10),
			status, started, display)

		if i == m.selectedIndex {
			line = selectedStyle.Width(m.width - 4).Render(line)
		} else {
			line = m.styleForStatus(job.Status).Render(line)
		}

		rows = append(rows, line)
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

func (m Model) renderLogPanel(height int) string {
	// If a job is selected (l pressed), show only logs
	if m.selectedJob != nil {
		return m.renderLogsOnly(height)
	}

	// Otherwise show details for highlighted job
	return m.renderJobDetails(height)
}

func (m Model) renderLogsOnly(height int) string {
	job := m.selectedJob
	var content string

	if m.logLoading {
		content = dimStyle.Render("Loading logs...")
	} else if m.logContent == "" {
		content = dimStyle.Render("No log content available")
	} else {
		// Take last N lines that fit
		lines := strings.Split(m.logContent, "\n")
		maxLines := height - 4 // Account for borders, title, and padding
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		content = strings.Join(lines, "\n")
	}

	title := fmt.Sprintf("Logs: Job %d on %s", job.ID, job.Host)
	panelContent := titleStyle.Render(title) + "\n" + content
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderJobDetails(height int) string {
	var content string
	var header string

	highlightedJob := m.getTargetJob()

	if highlightedJob == nil {
		content = dimStyle.Render("No jobs to display")
	} else {
		job := highlightedJob
		startTime := time.Unix(job.StartTime, 0)
		header = fmt.Sprintf("Job %d on %s\n", job.ID, job.Host)

		// Show Cmd and Dir first (most useful info)
		header += fmt.Sprintf("Cmd:     %s\n", job.Command)
		header += fmt.Sprintf("Dir:     %s\n", job.WorkingDir)

		// Then timing information
		header += fmt.Sprintf("Started: %s (%s)\n", startTime.Format("2006-01-02 15:04:05"), formatStartTime(job.StartTime))

		// Show timing information based on job status
		if job.Status == db.StatusRunning {
			elapsed := time.Since(startTime)
			header += fmt.Sprintf("Elapsed: %s (running)\n", formatDuration(elapsed))
		} else if job.EndTime != nil {
			endTime := time.Unix(*job.EndTime, 0)
			duration := endTime.Sub(startTime)
			header += fmt.Sprintf("Ended:   %s (%s)\n", endTime.Format("2006-01-02 15:04:05"), formatStartTime(*job.EndTime))
			header += fmt.Sprintf("Duration: %s\n", formatDuration(duration))
		}

		// Show exit status if available
		if job.Status == db.StatusCompleted && job.ExitCode != nil {
			if *job.ExitCode == 0 {
				header += "Exit:    0 (success)\n"
			} else {
				header += fmt.Sprintf("Exit:    %d (failed)\n", *job.ExitCode)
			}
		} else if job.Status == db.StatusDead {
			header += "Exit:    killed/crashed\n"
		} else if job.Status == db.StatusFailed {
			header += "Exit:    failed to start\n"
			if job.ErrorMessage != "" {
				header += fmt.Sprintf("Error:   %s\n", job.ErrorMessage)
			}
		}

		// Show process stats for running jobs (show whatever stats we have for this job)
		if job.Status == db.StatusRunning && m.processStats != nil && m.processStatsJobID == job.ID {
			header += "\n"
			header += "Process Stats:\n"

			// CPU: show % if available, plus user/sys time
			if m.processStats.CPUUser != "" || m.processStats.CPUSys != "" {
				cpuLine := "  CPU:     "
				if m.processStats.CPUPct > 0 {
					cpuLine += fmt.Sprintf("%.0f%% ", m.processStats.CPUPct)
				}
				cpuLine += fmt.Sprintf("(%s user, %s sys)\n", m.processStats.CPUUser, m.processStats.CPUSys)
				header += cpuLine
			}

			// Memory
			if m.processStats.MemoryRSS != "" {
				mem := m.processStats.MemoryRSS
				if m.processStats.MemoryPct != "" {
					mem += " (" + m.processStats.MemoryPct + ")"
				}
				header += fmt.Sprintf("  Memory:  %s\n", mem)
			}

			// Threads
			if m.processStats.Threads > 0 {
				header += fmt.Sprintf("  Threads: %d\n", m.processStats.Threads)
			}

			// GPUs with utilization and memory
			if len(m.processStats.GPUs) > 0 {
				for _, gpu := range m.processStats.GPUs {
					gpuLine := fmt.Sprintf("  GPU %d:   ", gpu.Index)
					if gpu.Utilization > 0 {
						gpuLine += fmt.Sprintf("%d%% util, ", gpu.Utilization)
					}
					gpuLine += gpu.MemUsed + "\n"
					header += gpuLine
				}
			}
		}
	}

	panelContent := titleStyle.Render("Details") + "\n"
	if header != "" {
		panelContent += header
	}
	panelContent += content

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

// parseMiB extracts the MiB value from a memory string like "123MiB"
func parseMiB(mem string) int {
	mem = strings.TrimSpace(mem)
	if strings.HasSuffix(mem, "MiB") {
		numStr := strings.TrimSuffix(mem, "MiB")
		if mib, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
			return mib
		}
	}
	return 0
}

// formatGPUMem formats GPU memory, converting large MiB values to GiB
func formatGPUMem(mem string) string {
	mem = strings.TrimSpace(mem)
	// Try to parse as MiB
	if strings.HasSuffix(mem, "MiB") {
		numStr := strings.TrimSuffix(mem, "MiB")
		if mib, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
			if mib >= 1024 {
				gib := float64(mib) / 1024.0
				return fmt.Sprintf("%.1fGiB", gib)
			}
			return fmt.Sprintf("%dMiB", mib)
		}
	}
	return mem
}

// formatDuration formats a duration in a human-readable form
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func (m Model) renderStatusBar() string {
	left := ""
	if m.errorMessage != "" {
		left = errorStyle.Render(m.errorMessage)
	} else if m.statusMessage != "" {
		left = statusMsgStyle.Render(m.statusMessage)
	}

	right := helpStyle.Render("↑/↓:nav l:logs s:sync n:new r:restart k:kill p:prune h:hosts q:quit")

	if m.syncing {
		right = syncingStyle.Render("⟳ ") + right
	}

	// Calculate gap
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) renderHostList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-12s %-10s %-16s %-8s %s",
		"HOST", "STATUS", "ARCH", "LOAD", "GPU")
	rows = append(rows, headerStyle.Render(header))

	if len(m.hosts) == 0 {
		rows = append(rows, dimStyle.Render(" No hosts found. Run a job first."))
	} else {
		// Hosts
		contentHeight := height - 4 // Account for borders and header
		for i, host := range m.hosts {
			if i >= contentHeight {
				break
			}

			status := m.formatHostStatus(host)
			arch := truncate(host.Arch, 16)
			if arch == "" {
				arch = "-"
			}
			load := host.LoadAvgShort()
			gpu := host.GPUSummary()

			line := fmt.Sprintf(" %-12s %-10s %-16s %-8s %s",
				truncate(host.Name, 12), status, arch, load, gpu)

			if i == m.selectedHostIdx {
				line = selectedStyle.Width(m.width - 4).Render(line)
			} else {
				line = m.styleForHostStatus(host.Status).Render(line)
			}

			rows = append(rows, line)
		}
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

func (m Model) renderHostDetail(height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		lines = append(lines, dimStyle.Render("No host selected"))
	} else {
		host := m.hosts[m.selectedHostIdx]

		lines = append(lines, fmt.Sprintf("Host: %s", host.Name))
		statusLine := fmt.Sprintf("Status: %s", host.StatusString())
		if host.Error != "" {
			statusLine += fmt.Sprintf(" (%s)", host.Error)
		}
		lines = append(lines, statusLine)

		if host.Status == HostStatusOnline {
			lines = append(lines, "───────────────────────────────────────────────────────────────")
			if host.Arch != "" {
				lines = append(lines, fmt.Sprintf("Architecture: %s", host.Arch))
			}
			if host.OS != "" {
				lines = append(lines, fmt.Sprintf("OS Version:   %s", host.OS))
			}
			if host.CPUs > 0 {
				lines = append(lines, fmt.Sprintf("CPUs:         %d", host.CPUs))
			}
			if host.MemTotal != "" {
				memInfo := host.MemTotal
				if host.MemUsed != "" {
					memInfo = fmt.Sprintf("%s used / %s total", host.MemUsed, host.MemTotal)
				}
				lines = append(lines, fmt.Sprintf("Memory:       %s", memInfo))
			}
			if host.LoadAvg != "" {
				loads := strings.Split(host.LoadAvg, ",")
				if len(loads) >= 3 && host.CPUs > 0 {
					load1m := strings.TrimSpace(loads[0])
					load5m := strings.TrimSpace(loads[1])
					load15m := strings.TrimSpace(loads[2])
					// Calculate utilization percentage from 1-minute load
					if loadVal, err := strconv.ParseFloat(load1m, 64); err == nil {
						pct := int((loadVal / float64(host.CPUs)) * 100)
						lines = append(lines, fmt.Sprintf("Load:         %s (1m), %s (5m), %s (15m)  [%d%% utilized]", load1m, load5m, load15m, pct))
					} else {
						lines = append(lines, fmt.Sprintf("Load:         %s (1m), %s (5m), %s (15m)", load1m, load5m, load15m))
					}
				} else {
					lines = append(lines, fmt.Sprintf("Load:         %s", host.LoadAvg))
				}
			}

			if len(host.GPUs) > 0 {
				// Show GPU summary header
				gpuNames := make(map[string]int)
				for _, gpu := range host.GPUs {
					gpuNames[gpu.Name]++
				}
				if len(gpuNames) == 1 {
					for name, count := range gpuNames {
						lines = append(lines, fmt.Sprintf("GPUs:         %d× %s", count, name))
					}
				} else {
					lines = append(lines, fmt.Sprintf("GPUs:         %d", len(host.GPUs)))
				}
				// Show per-GPU stats as a table
				hasStats := false
				for _, gpu := range host.GPUs {
					if gpu.Temperature > 0 || gpu.Utilization > 0 || gpu.MemUsed != "" {
						hasStats = true
						break
					}
				}
				if hasStats {
					lines = append(lines, "")
					lines = append(lines, "ID    TEMP    UTIL   MEM USED / TOTAL")
					for _, gpu := range host.GPUs {
						temp := "-"
						if gpu.Temperature > 0 {
							temp = fmt.Sprintf("%d°C", gpu.Temperature)
						}
						util := "-"
						if gpu.Utilization > 0 || gpu.MemUsed != "" {
							util = fmt.Sprintf("%d%%", gpu.Utilization)
						}
						mem := "-"
						if gpu.MemUsed != "" && gpu.MemTotal != "" {
							usedMiB := parseMiB(gpu.MemUsed)
							totalMiB := parseMiB(gpu.MemTotal)
							if totalMiB > 0 {
								pct := (usedMiB * 100) / totalMiB
								mem = fmt.Sprintf("%s / %s (%d%%)", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal), pct)
							} else {
								mem = fmt.Sprintf("%s / %s", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal))
							}
						}
						lines = append(lines, fmt.Sprintf("%2d   %5s   %5s   %s", gpu.Index, temp, util, mem))
					}
				}
			}
		}

		if !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck).Truncate(time.Second)
			lines = append(lines, fmt.Sprintf("Last checked: %s ago", elapsed))
		}
	}

	// Clip content to fit panel height (account for title, borders, padding)
	maxLines := height - 4
	if len(lines) > maxLines && maxLines > 0 {
		lines = lines[:maxLines]
	}

	content := strings.Join(lines, "\n")
	panelContent := titleStyle.Render("Host Details") + "\n" + content
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderHostsStatusBar() string {
	left := ""
	if m.errorMessage != "" {
		left = errorStyle.Render(m.errorMessage)
	} else if m.statusMessage != "" {
		left = statusMsgStyle.Render(m.statusMessage)
	}

	right := helpStyle.Render("↑/↓:nav R:refresh j:jobs tab:switch q:quit")

	// Calculate gap
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) formatHostStatus(host *Host) string {
	switch host.Status {
	case HostStatusOnline:
		return "● online"
	case HostStatusOffline:
		return "○ offline"
	case HostStatusChecking:
		return "◐ checking"
	default:
		return "? unknown"
	}
}

func (m Model) styleForHostStatus(status HostStatus) lipgloss.Style {
	switch status {
	case HostStatusOnline:
		return hostOnlineStyle
	case HostStatusOffline:
		return hostOfflineStyle
	case HostStatusChecking:
		return hostCheckingStyle
	default:
		return lipgloss.NewStyle()
	}
}

func (m Model) formatStatus(job *db.Job) string {
	switch job.Status {
	case db.StatusRunning:
		return "● running"
	case db.StatusCompleted:
		if job.ExitCode == nil {
			return "✓ done"
		}
		if *job.ExitCode == 0 {
			return "✓ done"
		}
		return fmt.Sprintf("✗ exit %d", *job.ExitCode)
	case db.StatusDead:
		return "✗ dead"
	case db.StatusPending:
		return "○ pending"
	default:
		return job.Status
	}
}

func (m Model) styleForStatus(status string) lipgloss.Style {
	switch status {
	case db.StatusRunning:
		return runningStyle
	case db.StatusCompleted:
		return completedStyle
	case db.StatusDead:
		return deadStyle
	case db.StatusPending:
		return pendingStyle
	default:
		return lipgloss.NewStyle()
	}
}

// Commands

func (m Model) startSyncTicker() tea.Cmd {
	return tea.Tick(m.syncInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) startLogTicker() tea.Cmd {
	return tea.Tick(m.logRefreshInterval, func(t time.Time) tea.Msg {
		return logTickMsg(t)
	})
}

func (m Model) startCreateTicker() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return createTickMsg(t)
	})
}

func (m Model) refreshJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := db.ListJobs(m.database, "", "", 100)
		return jobsRefreshedMsg{jobs: jobs, err: err}
	}
}

func (m Model) startHostRefreshTicker() tea.Cmd {
	return tea.Tick(m.hostRefreshInterval, func(t time.Time) tea.Msg {
		return hostRefreshTickMsg(t)
	})
}

func (m Model) loadHosts() tea.Cmd {
	return func() tea.Msg {
		hosts, err := db.ListUniqueHosts(m.database)
		return hostsLoadedMsg{hostNames: hosts, err: err}
	}
}

func (m Model) fetchHostInfo(hostName string) tea.Cmd {
	return func() tea.Msg {
		host := &Host{
			Name:      hostName,
			Status:    HostStatusChecking,
			LastCheck: time.Now(),
		}

		// Use short timeout to avoid blocking UI
		stdout, stderr, err := ssh.RunWithTimeout(hostName, HostInfoCommand, 10*time.Second)
		if err != nil {
			host.Status = HostStatusOffline
			host.Error = strings.TrimSpace(stderr)
			if host.Error == "" {
				host.Error = err.Error()
			}
			return hostInfoMsg{hostName: hostName, info: host}
		}

		// Parse the output
		host = ParseHostInfo(stdout)
		host.Name = hostName
		return hostInfoMsg{hostName: hostName, info: host}
	}
}

// getTargetJob returns the job to act on - either the selected job or the highlighted job
func (m Model) getTargetJob() *db.Job {
	if m.selectedJob != nil {
		return m.selectedJob
	}
	if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
		return m.jobs[m.selectedIndex]
	}
	return nil
}

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	return func() tea.Msg {
		logFile := session.JobLogFile(job.ID, job.StartTime, job.SessionName)
		// First check if we can connect to the host
		// Don't quote path - it contains ~ which needs shell expansion
		stdout, stderr, err := ssh.Run(job.Host, fmt.Sprintf("tail -50 %s 2>&1", logFile))
		if err != nil {
			// Check if it's a connection error
			combined := stdout + stderr
			if ssh.IsConnectionError(combined) {
				return logFetchedMsg{
					jobID:   job.ID,
					content: fmt.Sprintf("Host %s unreachable", job.Host),
				}
			}
			// Other SSH error
			return logFetchedMsg{
				jobID:   job.ID,
				content: fmt.Sprintf("Error: %s", strings.TrimSpace(combined)),
			}
		}
		// Check if output indicates file not found
		if strings.Contains(stdout, "No such file") || strings.Contains(stdout, "cannot open") {
			return logFetchedMsg{
				jobID:   job.ID,
				content: "Log file not found (may have been cleaned up)",
			}
		}
		return logFetchedMsg{
			jobID:   job.ID,
			content: stdout,
		}
	}
}

func (m Model) fetchProcessStats(job *db.Job) tea.Cmd {
	if job == nil || job.Status != db.StatusRunning {
		return nil
	}

	return func() tea.Msg {
		pidFile := session.JobPidFile(job.ID, job.StartTime)
		stats, _ := ssh.GetProcessStats(job.Host, pidFile)
		return processStatsMsg{
			jobID: job.ID,
			stats: stats,
		}
	}
}

func (m Model) performBackgroundSync() tea.Cmd {
	return func() tea.Msg {
		hosts, err := db.ListUniqueRunningHosts(m.database)
		if err != nil {
			return syncCompletedMsg{err: err}
		}

		var updated int
		for _, host := range hosts {
			jobs, err := db.ListRunning(m.database, host)
			if err != nil {
				continue
			}

			for _, job := range jobs {
				changed, err := syncJobQuick(m.database, job)
				if err != nil {
					continue
				}
				if changed {
					updated++
				}
			}
		}

		return syncCompletedMsg{updated: updated}
	}
}

func (m Model) killJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	database := m.database
	return func() tea.Msg {
		tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		err := ssh.TmuxKillSession(job.Host, tmuxSession)
		if err == nil {
			db.MarkDeadByID(database, job.ID)
		}
		return jobKilledMsg{jobID: job.ID, err: err}
	}
}

func (m Model) restartJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		// Read metadata from remote (for old jobs)
		metadataFile := session.JobMetadataFile(job.ID, job.StartTime, job.SessionName)
		content, err := ssh.ReadRemoteFile(job.Host, metadataFile)
		if err != nil || content == "" {
			// Fall back to database info
			content = ""
		}

		var workingDir, command, description string
		if content != "" {
			metadata := session.ParseMetadata(content)
			workingDir = metadata["working_dir"]
			command = metadata["command"]
			description = metadata["description"]
		}

		// Fall back to job info if metadata missing
		if workingDir == "" {
			workingDir = job.WorkingDir
		}
		if command == "" {
			command = job.Command
		}
		if description == "" {
			description = job.Description
		}

		if workingDir == "" || command == "" {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("missing working directory or command")}
		}

		// Kill existing session if running
		oldTmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		exists, _ := ssh.TmuxSessionExistsQuick(job.Host, oldTmuxSession)
		if exists {
			ssh.TmuxKillSession(job.Host, oldTmuxSession)
		}

		// Create new job record to get ID
		newJobID, err := db.RecordJobStarting(database, job.Host, workingDir, command, description)
		if err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("create job record: %w", err)}
		}

		// Get the new job to access start time
		newJob, err := db.GetJobByID(database, newJobID)
		if err != nil || newJob == nil {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("get new job: %w", err)}
		}

		// Generate new file paths from job ID
		newTmuxSession := session.TmuxSessionName(newJobID)
		logFile := session.LogFile(newJobID, newJob.StartTime)
		statusFile := session.StatusFile(newJobID, newJob.StartTime)
		newMetadataFile := session.MetadataFile(newJobID, newJob.StartTime)

		// Create log directory on remote
		mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
		if _, stderr, err := ssh.Run(job.Host, mkdirCmd); err != nil {
			errMsg := ssh.FriendlyError(job.Host, stderr, err)
			db.UpdateJobFailed(database, newJobID, errMsg)
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("%s", errMsg)}
		}

		// Save metadata
		newMetadata := session.FormatMetadata(newJobID, workingDir, command, job.Host, description, newJob.StartTime)
		// Don't quote path - it contains ~ which needs shell expansion
		metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", newMetadataFile, newMetadata)
		ssh.Run(job.Host, metadataCmd)

		// Generate pid file path
		pidFile := session.PidFile(newJobID, newJob.StartTime)

		// Create the wrapped command using the common builder (tested for tilde expansion)
		wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
			JobID:      newJobID,
			WorkingDir: workingDir,
			Command:    command,
			LogFile:    logFile,
			StatusFile: statusFile,
			PidFile:    pidFile,
		})

		// Escape single quotes for embedding in single-quoted string
		escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

		// Start tmux session - use single quotes to prevent shell expansion
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", newTmuxSession, escapedCommand)
		if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
			errMsg := ssh.FriendlyError(job.Host, stderr, err)
			db.UpdateJobFailed(database, newJobID, errMsg)
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("%s", errMsg)}
		}

		// Mark job as running
		if err := db.UpdateJobRunning(database, newJobID); err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: err}
		}

		return jobRestartedMsg{oldJobID: job.ID, newJobID: newJobID}
	}
}

// syncJobQuick checks and updates a single job's status (no retry for TUI responsiveness)
func syncJobQuick(database *sql.DB, job *db.Job) (bool, error) {
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

func (m Model) pruneJobs() tea.Cmd {
	return func() tea.Msg {
		count, err := db.PruneJobs(m.database, false, nil)
		return pruneCompletedMsg{count: count, err: err}
	}
}

func (m Model) removeJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		err := db.DeleteJob(database, job.ID)
		return jobRemovedMsg{jobID: job.ID, err: err}
	}
}

func (m Model) createJob() tea.Cmd {
	database := m.database
	host := strings.TrimSpace(m.inputs[inputHost].Value())
	command := strings.TrimSpace(m.inputs[inputCommand].Value())
	description := strings.TrimSpace(m.inputs[inputDescription].Value())
	workingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())

	if workingDir == "" {
		workingDir = "~"
	}

	return func() tea.Msg {
		timeout := 30 * time.Second

		// Create job record to get ID
		jobID, err := db.RecordJobStarting(database, host, workingDir, command, description)
		if err != nil {
			return jobCreatedMsg{err: fmt.Errorf("create job record: %w", err)}
		}

		// Get the new job to access start time
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			return jobCreatedMsg{err: fmt.Errorf("get new job: %w", err)}
		}

		// Generate file paths from job ID
		tmuxSession := session.TmuxSessionName(jobID)
		logFile := session.LogFile(jobID, job.StartTime)
		statusFile := session.StatusFile(jobID, job.StartTime)
		metadataFile := session.MetadataFile(jobID, job.StartTime)
		pidFile := session.PidFile(jobID, job.StartTime)

		// Create log directory on remote
		mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
		if _, stderr, err := ssh.RunWithTimeout(host, mkdirCmd, timeout); err != nil {
			errMsg := ssh.FriendlyError(host, stderr, err)
			db.UpdateJobFailed(database, jobID, errMsg)
			return jobCreatedMsg{err: fmt.Errorf("%s", errMsg)}
		}

		// Save metadata
		metadata := session.FormatMetadata(jobID, workingDir, command, host, description, job.StartTime)
		// Don't quote path - it contains ~ which needs shell expansion
		metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
		ssh.RunWithTimeout(host, metadataCmd, timeout)

		// Create the wrapped command using the common builder (tested for tilde expansion)
		wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
			JobID:      jobID,
			WorkingDir: workingDir,
			Command:    command,
			LogFile:    logFile,
			StatusFile: statusFile,
			PidFile:    pidFile,
		})

		// Escape single quotes for embedding in single-quoted string
		escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

		// Start tmux session - use single quotes to prevent shell expansion
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
		if _, stderr, err := ssh.RunWithTimeout(host, tmuxCmd, timeout); err != nil {
			errMsg := ssh.FriendlyError(host, stderr, err)
			db.UpdateJobFailed(database, jobID, errMsg)
			return jobCreatedMsg{err: fmt.Errorf("%s", errMsg)}
		}

		// Mark job as running
		if err := db.UpdateJobRunning(database, jobID); err != nil {
			return jobCreatedMsg{err: err}
		}

		return jobCreatedMsg{jobID: jobID}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// formatStartTime formats a start time as relative ("2h ago") for recent jobs
// or as absolute ("01/02 15:04") for older jobs
func formatStartTime(startTime int64) string {
	t := time.Unix(startTime, 0)
	elapsed := time.Since(t)

	if elapsed < 12*time.Hour {
		if elapsed < time.Minute {
			return "just now"
		} else if elapsed < time.Hour {
			return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
		}
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	}
	return t.Format("01/02 15:04")
}
