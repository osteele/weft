package tui

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/scripts"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Default intervals for background operations
const (
	DefaultSyncInterval        = 15 * time.Second
	DefaultLogRefreshInterval  = 3 * time.Second
	DefaultHostRefreshInterval = 30 * time.Second
	DefaultHostCacheDuration   = 24 * time.Hour // How long cached host info is considered fresh
)

// ViewMode represents which view is currently active
type ViewMode int

const (
	ViewModeJobs ViewMode = iota
	ViewModeHosts
)

// jobFilterMode controls which subset of jobs is displayed in the Jobs view
type jobFilterMode int

const (
	jobFilterAll jobFilterMode = iota
	jobFilterActive
	jobFilterSucceeded
	jobFilterFailed
	jobFilterModeCount
)

// DetailTab represents which tab is active in the job detail panel
type DetailTab int

const (
	DetailTabDetails DetailTab = iota
	DetailTabLogs
)

// Key bindings
type keyMap struct {
	Up          key.Binding
	Down        key.Binding
	Enter       key.Binding
	Logs        key.Binding
	Filter      key.Binding
	Escape      key.Binding
	Kill        key.Binding
	Restart     key.Binding
	EditRestart key.Binding
	Remove      key.Binding
	NewJob      key.Binding
	Prune       key.Binding
	Suspend     key.Binding
	Quit        key.Binding
	HostsView   key.Binding
	JobsView    key.Binding
	Tab         key.Binding
	Sync        key.Binding
	Help        key.Binding
	StartQueue  key.Binding
	StartNow    key.Binding
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
	Filter: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "cycle filter"),
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
	EditRestart: key.NewBinding(
		key.WithKeys("R"),
		key.WithHelp("R", "edit & restart"),
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
		key.WithKeys("P"),
		key.WithHelp("P", "prune"),
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
	Sync: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "sync"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	StartQueue: key.NewBinding(
		key.WithKeys("S"),
		key.WithHelp("S", "start queue"),
	),
	StartNow: key.NewBinding(
		key.WithKeys("g"),
		key.WithHelp("g", "start now"),
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
	jobID     int64
	content   string
	err       error
	connError bool // true if this was a connection error (host unreachable)
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

type jobStartedNowMsg struct {
	jobID int64
	err   error
}

type pruneCompletedMsg struct {
	count int64
	err   error
}

type queueStartedMsg struct {
	host    string
	already bool // true if queue was already running
	err     error
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
type flashExpiredMsg struct{}

// Host-related messages
type hostsLoadedMsg struct {
	hostNames []string
	err       error
}

type hostInfoMsg struct {
	hostName string
	info     *Host
}

type queueStatusMsg struct {
	hostName string
	info     *QueueStatusInfo
}

type hostJobsGPUMsg struct {
	hostName    string
	runningJobs []HostRunningJob
}

type processStatsMsg struct {
	jobID int64
	stats *ssh.ProcessStats
}

// Input field indices for new job form
const (
	inputHost = iota
	inputDescription
	inputCommand
	inputWorkingDir
	inputEnvVars
)

// Model is the main TUI state
type Model struct {
	// View mode
	viewMode ViewMode

	// Jobs data
	allJobs       []*db.Job
	jobs          []*db.Job
	selectedIndex int
	selectedJob   *db.Job
	jobFilter     jobFilterMode

	// Hosts data
	hosts           []*Host
	selectedHostIdx int

	// UI State
	detailTab    DetailTab // Which tab is active in detail panel (Details or Logs)
	logContent   string
	logStale     bool             // true if showing cached content due to connection error
	logCache     map[int64]string // cache of last successful log content per job
	logLoading   bool
	logViewport  viewport.Model
	flashMessage string
	flashIsError bool
	flashExpiry  time.Time

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

	// Help overlay
	showHelp bool

	// Configurable intervals
	syncInterval        time.Duration
	logRefreshInterval  time.Duration
	hostRefreshInterval time.Duration
	hostCacheDuration   time.Duration

	// Host cache tracking - which hosts have been freshly queried this session
	hostsQueriedThisSession map[string]bool
}

// ModelOptions contains configuration for the TUI model
type ModelOptions struct {
	SyncInterval        time.Duration
	LogRefreshInterval  time.Duration
	HostRefreshInterval time.Duration
	HostCacheDuration   time.Duration // How long cached host info is considered fresh
}

// DefaultModelOptions returns the default TUI options
func DefaultModelOptions() ModelOptions {
	return ModelOptions{
		SyncInterval:        DefaultSyncInterval,
		LogRefreshInterval:  DefaultLogRefreshInterval,
		HostRefreshInterval: DefaultHostRefreshInterval,
		HostCacheDuration:   DefaultHostCacheDuration,
	}
}

// NewModel creates a new TUI model
func NewModel(database *sql.DB) Model {
	return NewModelWithOptions(database, DefaultModelOptions())
}

// NewModelWithOptions creates a new TUI model with custom options
func NewModelWithOptions(database *sql.DB, opts ModelOptions) Model {
	// Create text inputs for new job form
	inputs := make([]textinput.Model, 5)

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

	inputs[inputEnvVars] = textinput.New()
	inputs[inputEnvVars].Placeholder = "VAR=value, VAR2=value2 (optional)"
	inputs[inputEnvVars].Prompt = ""
	inputs[inputEnvVars].Width = 40
	inputs[inputEnvVars].CharLimit = 512

	return Model{
		database:                database,
		selectedIndex:           0,
		jobFilter:               jobFilterAll,
		inputs:                  inputs,
		syncInterval:            opts.SyncInterval,
		logRefreshInterval:      opts.LogRefreshInterval,
		hostRefreshInterval:     opts.HostRefreshInterval,
		hostCacheDuration:       opts.HostCacheDuration,
		hostsQueriedThisSession: make(map[string]bool),
		logCache:                make(map[int64]string),
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
		// Update viewport dimensions for log scrolling
		detailHeight := int(float64(m.height) * 0.35)
		m.logViewport.Width = m.width - 6
		m.logViewport.Height = detailHeight - 4
		return m, nil

	case tea.KeyMsg:
		if m.inputMode {
			return m.handleInputKeyPress(msg)
		}
		return m.handleKeyPress(msg)

	case tea.MouseMsg:
		return m.handleMouseClick(msg)

	case jobsRefreshedMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Error loading jobs: %v", msg.err), true)
		}
		m.allJobs = msg.jobs
		m.applyJobFilter()

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
		return m, nil

	case syncCompletedMsg:
		m.syncing = false
		m.lastSyncTime = time.Now()
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Sync error: %v", msg.err), true)
		} else if msg.updated > 0 {
			// Silently refresh jobs without flash message
			return m, m.refreshJobs()
		}
		return m, nil

	case logFetchedMsg:
		m.logLoading = false
		if msg.err != nil {
			m.logContent = fmt.Sprintf("Error: %v", msg.err)
			m.logStale = false
			m.logViewport.SetContent(m.logContent)
		} else if m.selectedJob != nil && msg.jobID == m.selectedJob.ID {
			if msg.connError {
				// Connection error - try to show cached content
				if cached, ok := m.logCache[msg.jobID]; ok {
					m.logContent = cached
					m.logStale = true
				} else {
					m.logContent = msg.content // Show "Host X unreachable" message
					m.logStale = false
				}
			} else {
				// Successful fetch - update cache and show content
				m.logCache[msg.jobID] = msg.content
				m.logContent = msg.content
				m.logStale = false
			}
			m.logViewport.SetContent(m.logContent)
			m.logViewport.GotoBottom()
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
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Kill failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash("Job killed", false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobRestartedMsg:
		m.restarting = false
		m.restartingJobName = ""
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Restart failed: %v", msg.err), true)
		}
		m.pendingSelectJobID = msg.newJobID
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job restarted (new ID: %d)", msg.newJobID), false), m.refreshJobs())

	case jobStartedNowMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Start failed: %v", msg.err), true)
		}
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %d started", msg.jobID), false), m.refreshJobs())

	case pruneCompletedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Prune failed: %v", msg.err), true)
		} else if msg.count > 0 {
			flashCmd = m.setFlash(fmt.Sprintf("Pruned %d job(s)", msg.count), false)
		} else {
			flashCmd = m.setFlash("No jobs to prune", false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case queueStartedMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Failed to start queue: %v", msg.err), true)
		} else if msg.already {
			return m, m.setFlash(fmt.Sprintf("Queue already running on %s", msg.host), false)
		}
		return m, m.setFlash(fmt.Sprintf("Queue started on %s", msg.host), false)

	case jobRemovedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Remove failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash("Job removed", false)
			m.selectedJob = nil
			m.logContent = ""
			m.logStale = false
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobCreateProgressMsg:
		m.createJobStep = msg.step
		return m, nil

	case jobCreatedMsg:
		m.creatingJob = false
		m.createJobStep = ""
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Create failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d started", msg.jobID), false)
			m.pendingSelectJobID = msg.jobID
			// Keep inputs for easy re-use (user can modify and submit again)
		}
		// Reload hosts in case this job was on a new host
		return m, tea.Batch(flashCmd, m.refreshJobs(), m.loadHosts())

	case tickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startSyncTicker())
		// Always refresh job list to pick up new jobs created elsewhere
		cmds = append(cmds, m.refreshJobs())
		if !m.syncing {
			m.syncing = true
			cmds = append(cmds, m.performBackgroundSync())
		}
		return m, tea.Batch(cmds...)

	case logTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startLogTicker())
		// Refresh logs if in Logs tab with a running job
		if m.detailTab == DetailTabLogs && m.selectedJob != nil && m.selectedJob.Status == db.StatusRunning {
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
			return m, m.setFlash(fmt.Sprintf("Error loading hosts: %v", msg.err), true)
		}
		// Initialize hosts with names, loading cached data where available
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
				// Try to load cached host info
				var host *Host
				cachedInfo, err := db.LoadCachedHostInfo(m.database, name)
				if err == nil && cachedInfo != nil {
					// Use cached info
					host = hostFromCachedInfo(cachedInfo)
					// Check if cache is stale (older than configured duration)
					cacheAge := time.Since(time.Unix(cachedInfo.LastUpdated, 0))
					if cacheAge > m.hostCacheDuration {
						// Cache is stale, mark as checking and fetch fresh
						host.Status = HostStatusChecking
						cmds = append(cmds, m.fetchHostInfo(name))
						cmds = append(cmds, m.fetchQueueStatus(name))
					}
					// If cache is fresh, we'll still show it but won't fetch unless user switches to hosts view
				} else {
					// No cached info, create empty host and fetch
					host = &Host{
						Name:   name,
						Status: HostStatusChecking,
					}
					cmds = append(cmds, m.fetchHostInfo(name))
					cmds = append(cmds, m.fetchQueueStatus(name))
				}
				m.hosts = append(m.hosts, host)
			}
		}
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case hostInfoMsg:
		// Update host info
		var cmd tea.Cmd
		for i, h := range m.hosts {
			if h.Name == msg.hostName {
				msg.info.Name = msg.hostName
				// Preserve queue status when updating host info
				msg.info.QueueStatus = h.QueueStatus
				msg.info.QueueRunnerActive = h.QueueRunnerActive
				msg.info.QueuedJobCount = h.QueuedJobCount
				msg.info.CurrentQueueJob = h.CurrentQueueJob
				msg.info.QueueStopPending = h.QueueStopPending
				// Preserve running jobs until new data arrives
				msg.info.RunningJobs = h.RunningJobs
				// Preserve LastCheck from previous state if new one is zero (offline)
				if msg.info.LastCheck.IsZero() && !h.LastCheck.IsZero() {
					msg.info.LastCheck = h.LastCheck
				}
				m.hosts[i] = msg.info
				break
			}
		}
		// Mark host as queried this session
		m.hostsQueriedThisSession[msg.hostName] = true
		return m, cmd

	case queueStatusMsg:
		// Update queue status for host
		for i, h := range m.hosts {
			if h.Name == msg.hostName {
				m.hosts[i].QueueStatus = QueueCheckChecked
				m.hosts[i].QueueRunnerActive = msg.info.RunnerActive
				m.hosts[i].QueuedJobCount = msg.info.QueuedJobCount
				m.hosts[i].CurrentQueueJob = msg.info.CurrentJob
				m.hosts[i].QueueStopPending = msg.info.StopPending
				break
			}
		}
		return m, nil

	case hostJobsGPUMsg:
		// Update running jobs and GPU mappings for host
		for i, h := range m.hosts {
			if h.Name == msg.hostName {
				m.hosts[i].RunningJobs = msg.runningJobs
				// Update GPU table with job info
				for j := range m.hosts[i].GPUs {
					m.hosts[i].GPUs[j].JobID = 0
					m.hosts[i].GPUs[j].JobLabel = ""
				}
				for _, job := range msg.runningJobs {
					for _, gpu := range job.GPUs {
						for j := range m.hosts[i].GPUs {
							if m.hosts[i].GPUs[j].Index == gpu.GPUIndex {
								m.hosts[i].GPUs[j].JobID = job.ID
								label := fmt.Sprintf("#%d", job.ID)
								if job.Description != "" {
									// Truncate description if too long
									desc := job.Description
									if len(desc) > 15 {
										desc = desc[:12] + "..."
									}
									label += " " + desc
								}
								m.hosts[i].GPUs[j].JobLabel = label
								break
							}
						}
					}
				}
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
				// Only refresh if:
				// 1. Host hasn't been queried this session yet, OR
				// 2. Host is online (to get updated dynamic info like load/memory)
				if !m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline {
					cmds = append(cmds, m.fetchHostInfo(host.Name))
					cmds = append(cmds, m.fetchQueueStatus(host.Name))
				}
			}
		}
		return m, tea.Batch(cmds...)

	case flashExpiredMsg:
		// Only clear if the flash has actually expired (not replaced by a newer one)
		if !m.flashExpiry.IsZero() && time.Now().After(m.flashExpiry) {
			m.flashMessage = ""
			m.flashIsError = false
			m.flashExpiry = time.Time{}
		}
		return m, nil
	}

	return m, nil
}

// handleMouseClick handles mouse click events
func (m Model) handleMouseClick(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Only handle left button press
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}

	// Ignore clicks when in input mode or showing overlays
	if m.inputMode || m.showHelp || m.restarting || m.creatingJob {
		return m, nil
	}

	// Calculate list panel height (same as in View)
	listHeight := int(float64(m.height) * 0.55)

	// Check if click is within the list panel (top portion of screen)
	// Account for: top border (1), header row (1), then job rows
	// So first job row is at Y=2
	if msg.Y >= 2 && msg.Y < listHeight-1 {
		clickedIndex := msg.Y - 2 // Subtract border + header

		if m.viewMode == ViewModeJobs {
			if clickedIndex >= 0 && clickedIndex < len(m.jobs) {
				m.selectedIndex = clickedIndex
				// Clear cached process stats when changing jobs
				m.processStats = nil
				m.prevProcessStats = nil
				m.processStatsJobID = 0
				// If in Logs tab, fetch logs for new selection
				if m.detailTab == DetailTabLogs {
					m.selectedJob = m.jobs[m.selectedIndex]
					m.logLoading = true
					var cmds []tea.Cmd
					cmds = append(cmds, m.fetchSelectedJobLog())
					if m.selectedJob.Status == db.StatusRunning {
						cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
					}
					return m, tea.Batch(cmds...)
				}
				// Fetch stats for running jobs even if not in Logs tab
				job := m.jobs[m.selectedIndex]
				if job.Status == db.StatusRunning {
					return m, m.fetchProcessStats(job)
				}
			}
		} else if m.viewMode == ViewModeHosts {
			if clickedIndex >= 0 && clickedIndex < len(m.hosts) {
				m.selectedHostIdx = clickedIndex
			}
		}
	}

	return m, nil
}

func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Help overlay - dismiss with ? or Esc
	if m.showHelp {
		if key.Matches(msg, keys.Help) || key.Matches(msg, keys.Escape) {
			m.showHelp = false
		}
		return m, nil
	}

	// When in log view, forward scroll keys to viewport
	if m.detailTab == DetailTabLogs {
		switch msg.String() {
		case "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.logViewport, cmd = m.logViewport.Update(msg)
			return m, cmd
		}
	}

	// Toggle help overlay
	if key.Matches(msg, keys.Help) {
		m.showHelp = true
		return m, nil
	}

	// Allow cancelling job creation with Escape
	if m.creatingJob && key.Matches(msg, keys.Escape) {
		m.creatingJob = false
		m.createJobStep = ""
		return m, m.setFlash("Job creation running in background...", false)
	}

	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Suspend):
		return m, tea.Suspend

	case key.Matches(msg, keys.Tab):
		// In Jobs view, toggle between Details and Logs tabs
		if m.viewMode == ViewModeJobs {
			if m.detailTab == DetailTabDetails {
				// Switch to Logs tab
				m.detailTab = DetailTabLogs
				if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
					m.selectedJob = m.jobs[m.selectedIndex]
					m.logLoading = true
					var cmds []tea.Cmd
					cmds = append(cmds, m.fetchSelectedJobLog())
					if m.selectedJob.Status == db.StatusRunning {
						cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
					}
					return m, tea.Batch(cmds...)
				}
			} else {
				// Switch to Details tab
				m.detailTab = DetailTabDetails
			}
			return m, nil
		}
		// In Hosts view, switch to Jobs view
		m.viewMode = ViewModeJobs
		return m, nil

	case key.Matches(msg, keys.HostsView):
		if m.viewMode != ViewModeHosts {
			m.viewMode = ViewModeHosts
			// Refresh hosts when switching to hosts view, but only if needed
			var cmds []tea.Cmd
			for _, host := range m.hosts {
				// Only refresh if not queried this session or if online (for dynamic data)
				if !m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline {
					cmds = append(cmds, m.fetchHostInfo(host.Name))
					cmds = append(cmds, m.fetchQueueStatus(host.Name))
				}
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case key.Matches(msg, keys.JobsView):
		// Toggle between jobs and hosts view
		if m.viewMode == ViewModeJobs {
			m.viewMode = ViewModeHosts
			// Refresh hosts when switching to hosts view, but only if needed
			var cmds []tea.Cmd
			for _, host := range m.hosts {
				// Only refresh if not queried this session or if online (for dynamic data)
				if !m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline {
					cmds = append(cmds, m.fetchHostInfo(host.Name))
					cmds = append(cmds, m.fetchQueueStatus(host.Name))
				}
			}
			return m, tea.Batch(cmds...)
		}
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
				// If in Logs tab, fetch logs for new job
				if m.detailTab == DetailTabLogs && len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
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
				// Even if not in Logs tab, fetch stats for running jobs
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
				// If in Logs tab, fetch logs for new job
				if m.detailTab == DetailTabLogs && m.selectedIndex < len(m.jobs) {
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
				// Even if not in Logs tab, fetch stats for running jobs
				if m.selectedIndex < len(m.jobs) {
					job := m.jobs[m.selectedIndex]
					if job.Status == db.StatusRunning {
						return m, m.fetchProcessStats(job)
					}
				}
			}
		}
		return m, nil

	case key.Matches(msg, keys.EditRestart):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		// Open new job form pre-populated with ALL fields from this job
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flashMessage = ""
		m.inputs[inputHost].SetValue(job.Host)
		m.inputs[inputCommand].SetValue(job.Command)
		m.inputs[inputDescription].SetValue(job.Description)
		m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		return m, nil

	case key.Matches(msg, keys.Logs):
		if m.viewMode == ViewModeJobs {
			// Toggle to logs tab (or toggle if already there)
			if m.detailTab == DetailTabLogs {
				// Already in logs mode - go back to details
				m.detailTab = DetailTabDetails
			} else if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
				// Enter logs mode
				m.detailTab = DetailTabLogs
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
		m.detailTab = DetailTabDetails
		m.selectedJob = nil
		m.logContent = ""
		m.logStale = false
		m.flashMessage = ""
		return m, nil

	case key.Matches(msg, keys.Kill):
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusRunning {
			return m, tea.Batch(m.setFlash("Killing job...", false), m.killJob(job))
		}
		return m, nil

	case key.Matches(msg, keys.Restart):
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if m.restarting {
			return m, m.setFlash("Restart already in progress...", false)
		}
		m.restarting = true
		m.restartingJobName = fmt.Sprintf("job %d", job.ID)
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Restarting job %d...", job.ID), false), m.restartJob(job))

	case key.Matches(msg, keys.Remove):
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		return m, tea.Batch(m.setFlash("Removing job...", false), m.removeJob(job))

	case key.Matches(msg, keys.NewJob):
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flashMessage = ""

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

	case key.Matches(msg, keys.Filter):
		m.jobFilter = jobFilterMode((int(m.jobFilter) + 1) % int(jobFilterModeCount))
		m.applyJobFilter()
		return m, m.setFlash(fmt.Sprintf("Filter: %s", jobFilterDescription(m.jobFilter)), false)

	case key.Matches(msg, keys.Prune):
		return m, tea.Batch(m.setFlash("Pruning completed/dead jobs...", false), m.pruneJobs())

	case key.Matches(msg, keys.StartQueue):
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting queue on %s...", job.Host), false), m.startQueue(job.Host))
		}
		return m, nil

	case key.Matches(msg, keys.StartNow):
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting job %d now...", job.ID), false), m.startQueuedJobNow(job))
		}
		return m, m.setFlash("Can only start queued jobs", true)

	case key.Matches(msg, keys.Sync):
		if m.viewMode == ViewModeJobs && !m.syncing {
			m.syncing = true
			return m, tea.Batch(m.setFlash("Syncing...", false), m.performBackgroundSync())
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
			return m, m.setFlash("Host and command are required", true)
		}

		// Exit input mode and create job
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		m.creatingJob = true
		m.createJobStart = time.Now()
		m.createJobStep = "Connecting..."
		m.flashMessage = ""
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
		flashView := m.renderFlash()
		statusView := m.renderHostsStatusBar()

		mainView = lipgloss.JoinVertical(
			lipgloss.Left,
			listView,
			detailView,
			flashView,
			statusView,
		)
	} else {
		// Jobs view (default)
		listView := m.renderJobList(listHeight)
		logView := m.renderLogPanel(detailHeight)
		flashView := m.renderFlash()
		statusView := m.renderStatusBar()

		mainView = lipgloss.JoinVertical(
			lipgloss.Left,
			listView,
			logView,
			flashView,
			statusView,
		)
	}

	// Show help overlay
	if m.showHelp {
		return m.renderHelpOverlay(mainView)
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

func (m Model) renderHelpOverlay(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(50)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true).Width(12) // Cyan, bold
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Bold(true)         // Medium gray, bold

	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\n\n")

	if m.viewMode == ViewModeJobs {
		b.WriteString(titleStyle.Render("Jobs View"))
		b.WriteString("\n")
		shortcuts := []struct{ key, desc string }{
			{"↑/↓", "Navigate job list"},
			{"l", "Toggle logs view"},
			{"s", "Sync job statuses"},
			{"n", "New job"},
			{"r", "Restart job"},
			{"R", "Edit & restart job"},
			{"k", "Kill running job"},
			{"S", "Start queue (for queued jobs)"},
			{"x", "Remove job from list"},
			{"P", "Prune completed/dead jobs"},
			{"h / Tab", "Switch to hosts view"},
			{"Esc", "Clear selection/messages"},
		}
		for _, s := range shortcuts {
			b.WriteString(keyStyle.Render(s.key))
			b.WriteString(descStyle.Render(s.desc))
			b.WriteString("\n")
		}
	} else {
		b.WriteString(titleStyle.Render("Hosts View"))
		b.WriteString("\n")
		shortcuts := []struct{ key, desc string }{
			{"↑/↓", "Navigate host list"},
			{"j / Tab", "Switch to jobs view"},
		}
		for _, s := range shortcuts {
			b.WriteString(keyStyle.Render(s.key))
			b.WriteString(descStyle.Render(s.desc))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(titleStyle.Render("General"))
	b.WriteString("\n")
	generalShortcuts := []struct{ key, desc string }{
		{"?", "Show/hide this help"},
		{"q", "Quit"},
		{"Ctrl+Z", "Suspend (fg to resume)"},
	}
	for _, s := range generalShortcuts {
		b.WriteString(keyStyle.Render(s.key))
		b.WriteString(descStyle.Render(s.desc))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Render("Press ? or Esc to close"))

	modal := modalStyle.Render(b.String())

	// Place modal centered
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
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

	labels := []string{"Host:", "Description:", "Command:", "Working Dir:", "Env Vars:"}
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
	if m.flashIsError && m.flashMessage != "" {
		helpText = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.flashMessage)
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
	filterLabel := fmt.Sprintf(" Filter: %s (press f to cycle)", jobFilterDescription(m.jobFilter))
	rows = append(rows, dimStyle.Render(filterLabel))

	if len(m.jobs) == 0 {
		rows = append(rows, dimStyle.Render(" No jobs match this filter"))
		content := strings.Join(rows, "\n")
		return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
	}

	// Jobs
	contentHeight := height - 5 // Account for borders, header, and filter line
	for i, job := range m.jobs {
		if i >= contentHeight {
			break
		}

		status := m.formatStatus(job)
		started := formatStartTime(job.StartTime)

		// Show description if available, otherwise truncated command
		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		display = truncate(display, 40)

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
	// Render based on active tab
	if m.detailTab == DetailTabLogs {
		return m.renderLogsOnly(height)
	}
	return m.renderJobDetails(height)
}

// renderTabHeader renders the "Details  Logs" tab header with active tab bolded
func (m Model) renderTabHeader() string {
	detailsLabel := "Details"
	logsLabel := "Logs"

	if m.detailTab == DetailTabDetails {
		detailsLabel = headerStyle.Render(detailsLabel)
		logsLabel = dimStyle.Render(logsLabel)
	} else {
		detailsLabel = dimStyle.Render(detailsLabel)
		logsLabel = headerStyle.Render(logsLabel)
	}

	return detailsLabel + "  " + logsLabel
}

func (m Model) renderLogsOnly(height int) string {
	job := m.selectedJob
	var content string
	var staleIndicator string

	// Handle case where no job is selected yet
	if job == nil {
		job = m.getTargetJob()
		if job == nil {
			panelContent := m.renderTabHeader() + "\n" + dimStyle.Render("No job selected")
			return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
		}
	}

	// Calculate viewport dimensions (account for borders, title, tab header, padding)
	viewportHeight := height - 5
	viewportWidth := m.width - 6 // Account for panel borders and padding
	if m.logStale {
		viewportHeight -= 1 // Make room for stale indicator
	}

	if m.logLoading {
		content = dimStyle.Render("Loading logs...")
	} else if m.logContent == "" {
		content = dimStyle.Render("No log content available")
	} else {
		// Create viewport with correct dimensions and content for rendering
		vp := m.logViewport
		vp.Width = viewportWidth
		vp.Height = viewportHeight
		vp.SetContent(m.logContent)

		// Use viewport for scrollable content
		if m.logStale {
			// Use slightly dimmer style for stale content
			staleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
			content = staleStyle.Render(vp.View())
		} else {
			content = vp.View()
		}
	}

	jobInfo := fmt.Sprintf("Job %d on %s", job.ID, job.Host)
	if m.logStale {
		staleIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render(" (cached - host offline)")
	}

	// Show scroll position if there's more content
	scrollInfo := ""
	totalLines := strings.Count(m.logContent, "\n") + 1
	if totalLines > viewportHeight && viewportHeight > 0 {
		scrollInfo = fmt.Sprintf(" [%d/%d]", m.logViewport.YOffset+viewportHeight, totalLines)
	}

	panelContent := m.renderTabHeader() + "\n" + dimStyle.Render(jobInfo) + staleIndicator + scrollInfo + "\n" + content
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
		header = fmt.Sprintf("Job %d on %s\n", job.ID, job.Host)

		// Show Cmd and Dir first (most useful info)
		header += fmt.Sprintf("Cmd:     %s\n", job.EffectiveCommand())
		header += fmt.Sprintf("Dir:     %s\n", job.EffectiveWorkingDir())

		// Show environment variables if any
		envVars := job.ParseExportVars()
		if len(envVars) > 0 {
			header += fmt.Sprintf("Env:     %s\n", strings.Join(envVars, ", "))
		}

		// Then timing information
		if job.StartTime > 0 {
			startTime := time.Unix(job.StartTime, 0)
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
		} else if job.EndTime != nil {
			// Job ended without ever starting (failed/killed before start)
			endTime := time.Unix(*job.EndTime, 0)
			header += fmt.Sprintf("Ended:   %s (%s)\n", endTime.Format("2006-01-02 15:04:05"), formatStartTime(*job.EndTime))
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

	panelContent := m.renderTabHeader() + "\n"
	if header != "" {
		panelContent += header
	}
	panelContent += content

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

// parseMiB extracts a MiB value from various memory string formats
// Handles: "123MiB", "80GiB", "16G", "128Gi", "58.5G", etc.
func parseMiB(mem string) int {
	mem = strings.TrimSpace(mem)

	// Try MiB suffix first
	if strings.HasSuffix(mem, "MiB") {
		numStr := strings.TrimSuffix(mem, "MiB")
		if mib, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
			return mib
		}
	}

	// Try GiB suffix (convert to MiB)
	if strings.HasSuffix(mem, "GiB") {
		numStr := strings.TrimSuffix(mem, "GiB")
		if gib, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gib * 1024)
		}
	}

	// Try Gi suffix (convert to MiB)
	if strings.HasSuffix(mem, "Gi") {
		numStr := strings.TrimSuffix(mem, "Gi")
		if gib, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gib * 1024)
		}
	}

	// Try G suffix (treat as GB, convert to MiB approximately)
	if strings.HasSuffix(mem, "G") {
		numStr := strings.TrimSuffix(mem, "G")
		if gb, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64); err == nil {
			return int(gb * 1024) // Approximate GB as GiB for simplicity
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

func (m Model) renderFlash() string {
	if m.flashMessage == "" {
		return ""
	}

	// Style for flash message box
	var style lipgloss.Style
	if m.flashIsError {
		style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).  // White text
			Background(lipgloss.Color("124")). // Dark red background
			Bold(true).
			Padding(0, 1)
	} else {
		style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).  // White text
			Background(lipgloss.Color("240")). // Dark gray background
			Padding(0, 1)
	}

	return " " + style.Render(m.flashMessage)
}

func (m Model) renderStatusBar() string {
	help := helpStyle.Render("?:help q:quit ↑/↓:nav l:logs f:filter s:sync n:new r:restart k:kill P:prune h:hosts")

	if m.syncing {
		help = syncingStyle.Render("⟳ ") + help
	}

	// Right-align the help text
	gap := m.width - lipgloss.Width(help) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + strings.Repeat(" ", gap) + help
}

func (m Model) renderHostList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-12s %-10s %-6s %-16s %-5s %-5s",
		"HOST", "STATUS", "QUEUE", "ARCH", "CPU", "RAM")
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
			queue := host.QueueSummary()
			arch := truncate(host.Arch, 16)
			if arch == "" {
				arch = "-"
			}
			cpu := host.CPUUtilization()
			ram := host.RAMUtilization()

			line := fmt.Sprintf(" %-12s %-10s %-6s %-16s %-5s %-5s",
				truncate(host.Name, 12), status, queue, arch, cpu, ram)

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

		// Show static info (cached) regardless of online status
		hasStaticInfo := host.Model != "" || host.Arch != "" || host.OS != "" || host.CPUModel != "" || host.CPUs > 0 || len(host.GPUs) > 0
		if hasStaticInfo {
			lines = append(lines, "───────────────────────────────────────────────────────────────")
			if host.Model != "" {
				lines = append(lines, fmt.Sprintf("Model:        %s", host.Model))
			}
			if host.Arch != "" {
				lines = append(lines, fmt.Sprintf("Architecture: %s", host.Arch))
			}
			if host.OS != "" {
				lines = append(lines, fmt.Sprintf("OS Version:   %s", host.OS))
			}
			if host.CPUModel != "" {
				lines = append(lines, fmt.Sprintf("CPU:          %s", host.CPUModel))
			}
			if host.CPUs > 0 {
				lines = append(lines, fmt.Sprintf("CPU Cores:    %d", host.CPUs))
			}

			// GPUs (right after CPU info)
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
				// Show per-GPU stats as a table (only when online - these are dynamic)
				hasStats := false
				if host.Status == HostStatusOnline {
					for _, gpu := range host.GPUs {
						if gpu.Temperature > 0 || gpu.Utilization > 0 || gpu.MemUsed != "" {
							hasStats = true
							break
						}
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

			// Memory (after GPUs)
			if host.MemTotal != "" {
				memInfo := host.MemTotal
				if host.MemUsed != "" {
					// Calculate utilization percentage
					usedMiB := parseMiB(host.MemUsed)
					totalMiB := parseMiB(host.MemTotal)
					if totalMiB > 0 {
						pct := (usedMiB * 100) / totalMiB
						memInfo = fmt.Sprintf("%s used / %s total (%d%%)", host.MemUsed, host.MemTotal, pct)
					} else {
						memInfo = fmt.Sprintf("%s used / %s total", host.MemUsed, host.MemTotal)
					}
				}
				lines = append(lines, fmt.Sprintf("Memory:       %s", memInfo))
			}

			// Load average (labeled: 1m, 5m, 15m)
			if host.LoadAvg != "" {
				// Parse load values - handle both comma-separated (Linux) and space-separated (macOS)
				loadStr := strings.ReplaceAll(host.LoadAvg, ",", " ")
				loads := strings.Fields(loadStr)
				if len(loads) >= 3 && host.CPUs > 0 {
					load1m := loads[0]
					load5m := loads[1]
					load15m := loads[2]
					// Calculate utilization percentage from 1-minute load
					if loadVal, err := strconv.ParseFloat(load1m, 64); err == nil {
						pct := int((loadVal / float64(host.CPUs)) * 100)
						lines = append(lines, fmt.Sprintf("Load (1/5/15m): %s, %s, %s  [%d%% of %d cores]", load1m, load5m, load15m, pct, host.CPUs))
					} else {
						lines = append(lines, fmt.Sprintf("Load (1/5/15m): %s, %s, %s", load1m, load5m, load15m))
					}
				} else {
					lines = append(lines, fmt.Sprintf("Load:         %s", host.LoadAvg))
				}
			}
		}

		// Queue status section
		if host.QueueStatus == QueueCheckChecked {
			lines = append(lines, "")
			lines = append(lines, "Queue")
			if host.QueueRunnerActive {
				lines = append(lines, "  Runner:       Active")
				if host.CurrentQueueJob != "" {
					lines = append(lines, fmt.Sprintf("  Current job:  %s", host.CurrentQueueJob))
				} else {
					lines = append(lines, "  Current job:  None")
				}
				lines = append(lines, fmt.Sprintf("  Jobs waiting: %d", host.QueuedJobCount))
				if host.QueueStopPending {
					lines = append(lines, "  Stop pending: Yes")
				}
			} else {
				lines = append(lines, "  Runner:       Stopped")
			}
		}

	}

	// Build footer with last successful connection time
	footerText := ""
	if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts) {
		host := m.hosts[m.selectedHostIdx]
		if !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck).Truncate(time.Second)
			footerText = fmt.Sprintf("Last online: %s ago", elapsed)
		}
	}

	// Calculate available lines: height - borders(2) - title(1) - footer(1 if present)
	footerLines := 0
	if footerText != "" {
		footerLines = 1
	}
	availableLines := height - 4 - footerLines

	// Clip content if needed
	if len(lines) > availableLines && availableLines > 0 {
		lines = lines[:availableLines]
	}

	// Pad with empty lines to push footer to bottom
	for len(lines) < availableLines {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	panelContent := titleStyle.Render("Host Details") + "\n" + content
	if footerText != "" {
		panelContent = panelContent + "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(footerText)
	}

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderHostsStatusBar() string {
	help := helpStyle.Render("?:help q:quit ↑/↓:nav R:refresh j:jobs tab:switch")

	// Right-align the help text
	gap := m.width - lipgloss.Width(help) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + strings.Repeat(" ", gap) + help
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
	case db.StatusQueued:
		return "◆ queued"
	case db.StatusFailed:
		return "✗ failed"
	case db.StatusStarting:
		return "◐ starting"
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
	case db.StatusQueued:
		return queuedStyle
	case db.StatusFailed:
		return failedStyle
	case db.StatusStarting:
		return pendingStyle
	default:
		return lipgloss.NewStyle()
	}
}

// Flash message duration
const flashDuration = 3 * time.Second

// setFlash sets a flash message and returns a timer command to clear it
func (m *Model) setFlash(msg string, isError bool) tea.Cmd {
	m.flashMessage = msg
	m.flashIsError = isError
	m.flashExpiry = time.Now().Add(flashDuration)
	return tea.Tick(flashDuration, func(t time.Time) tea.Msg {
		return flashExpiredMsg{}
	})
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

func (m *Model) applyJobFilter() {
	prevSelectedID := int64(0)
	if len(m.jobs) > 0 && m.selectedIndex >= 0 && m.selectedIndex < len(m.jobs) {
		prevSelectedID = m.jobs[m.selectedIndex].ID
	}

	var filtered []*db.Job
	for _, job := range m.allJobs {
		if jobMatchesFilter(job, m.jobFilter) {
			filtered = append(filtered, job)
		}
	}
	m.jobs = filtered

	if len(m.jobs) == 0 {
		m.selectedIndex = 0
	} else {
		if m.selectedIndex >= len(m.jobs) {
			m.selectedIndex = len(m.jobs) - 1
		}
		if m.selectedIndex < 0 {
			m.selectedIndex = 0
		}
	}

	if prevSelectedID != 0 {
		for i, job := range m.jobs {
			if job.ID == prevSelectedID {
				m.selectedIndex = i
				break
			}
		}
	}

	if m.selectedJob != nil && !jobMatchesFilter(m.selectedJob, m.jobFilter) {
		m.detailTab = DetailTabDetails
		m.selectedJob = nil
		m.logContent = ""
		m.logStale = false
	}
}

func (m Model) startHostRefreshTicker() tea.Cmd {
	return tea.Tick(m.hostRefreshInterval, func(t time.Time) tea.Msg {
		return hostRefreshTickMsg(t)
	})
}

func (m Model) loadHosts() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		// Get hosts from jobs
		jobHosts, err := db.ListUniqueHosts(database)
		if err != nil {
			return hostsLoadedMsg{err: err}
		}

		// Get hosts from cache
		cachedHosts, err := db.LoadAllCachedHosts(database)
		if err != nil {
			// If cache load fails, just use job hosts
			return hostsLoadedMsg{hostNames: jobHosts, err: nil}
		}

		// Merge into unique set
		hostSet := make(map[string]bool)
		for _, h := range jobHosts {
			hostSet[h] = true
		}
		for _, h := range cachedHosts {
			hostSet[h.Name] = true
		}

		// Convert to sorted slice
		var hosts []string
		for h := range hostSet {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)

		return hostsLoadedMsg{hostNames: hosts, err: nil}
	}
}

func (m Model) fetchHostInfo(hostName string) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		host := &Host{
			Name:   hostName,
			Status: HostStatusChecking,
		}

		// Use short timeout to avoid blocking UI
		stdout, stderr, err := ssh.RunWithTimeout(hostName, HostInfoCommand, 10*time.Second)
		if err != nil {
			host.Status = HostStatusOffline
			host.Error = strings.TrimSpace(stderr)
			if host.Error == "" {
				host.Error = err.Error()
			}
			// Load cached info to preserve static data and LastCheck when offline
			if cachedInfo, loadErr := db.LoadCachedHostInfo(database, hostName); loadErr == nil && cachedInfo != nil {
				cachedHost := hostFromCachedInfo(cachedInfo)
				// Preserve static info from cache
				host.Arch = cachedHost.Arch
				host.OS = cachedHost.OS
				host.Model = cachedHost.Model
				host.CPUs = cachedHost.CPUs
				host.CPUModel = cachedHost.CPUModel
				host.CPUFreq = cachedHost.CPUFreq
				host.MemTotal = cachedHost.MemTotal
				host.GPUs = cachedHost.GPUs
				// Preserve LastCheck from cache (last successful connection)
				host.LastCheck = cachedHost.LastCheck
			}
			return hostInfoMsg{hostName: hostName, info: host}
		}

		// Parse the output
		host = ParseHostInfo(stdout)
		host.Name = hostName

		// Save to cache (ignore errors - caching is best effort)
		cachedInfo := cachedInfoFromHost(host)
		db.SaveCachedHostInfo(database, cachedInfo)

		return hostInfoMsg{hostName: hostName, info: host}
	}
}

func (m Model) fetchQueueStatus(hostName string) tea.Cmd {
	return func() tea.Msg {
		// Use short timeout to avoid blocking UI
		stdout, _, err := ssh.RunWithTimeout(hostName, QueueStatusCommand("default"), 5*time.Second)
		if err != nil {
			// On error, return empty status (will show as "-")
			return queueStatusMsg{hostName: hostName, info: &QueueStatusInfo{}}
		}

		// Parse the output
		info := ParseQueueStatus(stdout)
		return queueStatusMsg{hostName: hostName, info: info}
	}
}

func (m Model) fetchHostJobsGPU(hostName string) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		// Get running jobs on this host from database
		jobs, err := db.GetRunningJobsByHost(database, hostName)
		if err != nil || len(jobs) == 0 {
			return hostJobsGPUMsg{hostName: hostName, runningJobs: nil}
		}

		// Build list of job PID info
		var jobPIDInfos []ssh.JobPIDInfo
		for _, job := range jobs {
			pidFile := session.JobPidFile(job.ID, job.StartTime)
			jobPIDInfos = append(jobPIDInfos, ssh.JobPIDInfo{
				JobID:   job.ID,
				PIDFile: pidFile,
			})
		}

		// Get GPU mappings via SSH script
		mappings, err := ssh.GetJobGPUMappings(hostName, scripts.GPUJobMappingScript, jobPIDInfos)
		if err != nil {
			// Don't fail - just return jobs without GPU info
			mappings = nil
		}

		// Build mapping from job ID to GPU usage
		gpuByJob := make(map[int64][]JobGPUUsage)
		for _, m := range mappings {
			gpuByJob[m.JobID] = append(gpuByJob[m.JobID], JobGPUUsage{
				GPUIndex: m.GPUIndex,
				MemUsed:  fmt.Sprintf("%d", m.MemMiB),
			})
		}

		// Build running jobs list with GPU info
		var runningJobs []HostRunningJob
		for _, job := range jobs {
			runningJobs = append(runningJobs, HostRunningJob{
				ID:          job.ID,
				Description: job.Description,
				Command:     job.Command,
				GPUs:        gpuByJob[job.ID],
			})
		}

		return hostJobsGPUMsg{hostName: hostName, runningJobs: runningJobs}
	}
}

// getTargetJob returns the job to act on - either the selected job or the highlighted job
func (m Model) getTargetJob() *db.Job {
	if m.detailTab == DetailTabLogs && m.selectedJob != nil {
		return m.selectedJob
	}
	if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
		return m.jobs[m.selectedIndex]
	}
	return nil
}

func jobMatchesFilter(job *db.Job, mode jobFilterMode) bool {
	switch mode {
	case jobFilterActive:
		return job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusQueued
	case jobFilterSucceeded:
		return job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0
	case jobFilterFailed:
		if job.Status == db.StatusFailed || job.Status == db.StatusDead {
			return true
		}
		return job.Status == db.StatusCompleted && (job.ExitCode == nil || *job.ExitCode != 0)
	default:
		return true
	}
}

func jobFilterDescription(mode jobFilterMode) string {
	switch mode {
	case jobFilterActive:
		return "Queued/Running"
	case jobFilterSucceeded:
		return "Completed (success)"
	case jobFilterFailed:
		return "Completed (failure)"
	default:
		return "All jobs"
	}
}

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	return func() tea.Msg {
		var logFile string

		// For jobs without a session name (queued jobs, or jobs started by queue runner),
		// we need to find the log file by pattern since the timestamp may differ
		if job.SessionName == "" {
			// Try to find log file by pattern
			pattern := session.LogFilePattern(job.ID)
			findCmd := fmt.Sprintf("ls -t %s 2>/dev/null | head -1", pattern)
			stdout, _, err := ssh.Run(job.Host, findCmd)
			if err == nil && strings.TrimSpace(stdout) != "" {
				logFile = strings.TrimSpace(stdout)
			} else {
				// Fall back to the expected path (may not exist)
				logFile = session.LogFile(job.ID, job.StartTime)
			}
		} else {
			logFile = session.JobLogFile(job.ID, job.StartTime, job.SessionName)
		}

		// Fetch the log content
		// Don't quote path - it contains ~ which needs shell expansion
		stdout, stderr, err := ssh.Run(job.Host, fmt.Sprintf("tail -500 %s 2>&1", logFile))
		if err != nil {
			// Check if it's a connection error
			combined := stdout + stderr
			if ssh.IsConnectionError(combined) {
				return logFetchedMsg{
					jobID:     job.ID,
					content:   fmt.Sprintf("Host %s unreachable", job.Host),
					connError: true,
				}
			}
			// Check if log file doesn't exist
			if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
				msg := "No log file yet"
				if job.Status == db.StatusCompleted || job.Status == db.StatusFailed || job.Status == db.StatusDead {
					msg = "Log file not found (may have been cleaned up)"
				}
				return logFetchedMsg{
					jobID:   job.ID,
					content: msg,
				}
			}
			// Other SSH error
			return logFetchedMsg{
				jobID:   job.ID,
				content: fmt.Sprintf("Error: %s", strings.TrimSpace(combined)),
			}
		}
		// Check if output indicates file not found (for cases where tail doesn't error)
		if strings.Contains(stdout, "No such file") || strings.Contains(stdout, "cannot open") {
			msg := "No log file yet"
			if job.Status == db.StatusCompleted || job.Status == db.StatusFailed || job.Status == db.StatusDead {
				msg = "Log file not found (may have been cleaned up)"
			}
			return logFetchedMsg{
				jobID:   job.ID,
				content: msg,
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
		var updated int

		// Sync running jobs
		hosts, err := db.ListUniqueRunningHosts(m.database)
		if err != nil {
			return syncCompletedMsg{err: err}
		}

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

		// Sync queued jobs (check if they've started or completed)
		queuedJobs, err := db.ListAllQueued(m.database)
		if err == nil {
			for _, job := range queuedJobs {
				changed, err := syncQueuedJob(m.database, job)
				if err != nil {
					continue
				}
				if changed {
					updated++
				}
			}
		}

		// Re-check recently-dead queue runner jobs (may have been incorrectly marked)
		// Look at jobs marked dead in the last hour
		oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
		deadJobs, err := db.ListRecentDeadQueueJobs(m.database, oneHourAgo)
		if err == nil {
			for _, job := range deadJobs {
				revived, err := checkAndReviveDeadJob(m.database, job)
				if err != nil {
					continue
				}
				if revived {
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

// startQueuedJobNow starts a queued job immediately, bypassing any dependencies
func (m Model) startQueuedJobNow(job *db.Job) tea.Cmd {
	if job == nil || job.Status != db.StatusQueued {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		// Remove job from remote queue file
		queueName := job.QueueName
		if queueName == "" {
			queueName = "default"
		}
		queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
		removeCmd := fmt.Sprintf("grep -v '^%d\\t' %s > %s.tmp 2>/dev/null && mv %s.tmp %s || true",
			job.ID, queueFile, queueFile, queueFile, queueFile)
		ssh.Run(job.Host, removeCmd)

		// Update database: transition from queued to running and set start time
		if err := db.UpdateQueuedToRunning(database, job.ID); err != nil {
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("update db: %w", err)}
		}

		// Get updated job to access new start time
		updatedJob, err := db.GetJobByID(database, job.ID)
		if err != nil || updatedJob == nil {
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("get job: %w", err)}
		}

		// Generate file paths from job ID
		tmuxSession := session.TmuxSessionName(job.ID)
		logFile := session.LogFile(job.ID, updatedJob.StartTime)
		statusFile := session.StatusFile(job.ID, updatedJob.StartTime)
		metadataFile := session.MetadataFile(job.ID, updatedJob.StartTime)
		pidFile := session.PidFile(job.ID, updatedJob.StartTime)

		// Create log directory on remote
		mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
		if _, stderr, err := ssh.Run(job.Host, mkdirCmd); err != nil {
			errMsg := ssh.FriendlyError(job.Host, stderr, err)
			db.UpdateJobFailed(database, job.ID, errMsg)
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("%s", errMsg)}
		}

		// Save metadata
		metadata := session.FormatMetadata(job.ID, job.WorkingDir, job.Command, job.Host, job.Description, updatedJob.StartTime)
		metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
		ssh.Run(job.Host, metadataCmd)

		// Create the wrapped command
		wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
			JobID:      job.ID,
			WorkingDir: job.WorkingDir,
			Command:    job.Command,
			LogFile:    logFile,
			StatusFile: statusFile,
			PidFile:    pidFile,
		})

		// Start tmux session
		escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
		if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
			errMsg := ssh.FriendlyError(job.Host, stderr, err)
			db.UpdateJobFailed(database, job.ID, errMsg)
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("%s", errMsg)}
		}

		return jobStartedNowMsg{jobID: job.ID}
	}
}

// updateStartTimeFromMetadataTUI reads the metadata file for a queued job and updates its start_time if not already set
func updateStartTimeFromMetadataTUI(database *sql.DB, job *db.Job) {
	// Only update if start_time is not set
	if job.StartTime > 0 {
		return
	}

	metadataPattern := session.MetadataFilePattern(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null", metadataPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, 5*time.Second)
	if err != nil || strings.TrimSpace(stdout) == "" {
		return // No metadata file or couldn't read it
	}

	// Parse metadata
	metadata := session.ParseMetadata(stdout)
	if startTimeStr, ok := metadata["start_time"]; ok {
		if startTime, err := strconv.ParseInt(startTimeStr, 10, 64); err == nil && startTime > 0 {
			// Update database with actual start time from metadata
			db.UpdateStartTime(database, job.ID, startTime)
			// Update in-memory job struct too for current sync cycle
			job.StartTime = startTime
		}
	}
}

// syncQueuedJob checks if a queued job has started or completed
func syncQueuedJob(database *sql.DB, job *db.Job) (bool, error) {
	// Look for status files matching this job ID
	// Pattern: ~/.cache/remote-jobs/logs/{jobID}-*.status
	statusPattern := session.StatusFilePattern(job.ID)

	// Check if any status file exists (job completed)
	cmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, 5*time.Second)
	if err == nil && strings.TrimSpace(stdout) != "" {
		// Job completed - read exit code and update start time from metadata
		exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
		endTime := time.Now().Unix()

		// Update start time from metadata if not already set
		updateStartTimeFromMetadataTUI(database, job)

		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Check if log file exists (job is running)
	logPattern := fmt.Sprintf("~/.cache/remote-jobs/logs/%d-*.log", job.ID)
	checkCmd := fmt.Sprintf("ls %s 2>/dev/null | head -1", logPattern)
	stdout, _, err = ssh.RunWithTimeout(job.Host, checkCmd, 5*time.Second)
	if err == nil && strings.TrimSpace(stdout) != "" {
		// Job has started running - update start time from metadata
		updateStartTimeFromMetadataTUI(database, job)

		// Update status to running only if not already set
		if job.Status == db.StatusQueued {
			if err := db.UpdateQueuedToRunning(database, job.ID); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Job still queued
	return false, nil
}

// syncJobQuick checks and updates a single job's status (no retry for TUI responsiveness)
func syncJobQuick(database *sql.DB, job *db.Job) (bool, error) {
	// Jobs without a session name were started by the queue runner
	// Use optimized quick sync that combines checks into one SSH command
	if job.SessionName == "" {
		return syncQueueRunnerJobQuick(database, job)
	}

	// Regular jobs have tmux sessions
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		// Can't reach host - don't change job status
		return false, nil
	}

	if exists {
		return false, nil
	}

	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		// Can't reach host - don't change job status
		return false, nil
	}

	if content != "" {
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Session doesn't exist and no status file - mark as dead
	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// syncQueueRunnerJobQuick is an optimized version for queue runner jobs that combines
// all status checks into a single SSH command to reduce latency
func syncQueueRunnerJobQuick(database *sql.DB, job *db.Job) (bool, error) {
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}

	// Combine all checks into ONE SSH command for fast sync
	// This checks: status file, .current file, .queue file, and PID file
	// Returns: exit code (if completed), RUNNING, QUEUED, or DEAD
	statusPattern := session.StatusFilePattern(job.ID)
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	pidPattern := session.PidFilePattern(job.ID)

	combinedCmd := fmt.Sprintf(`
		# Check status file (completed?)
		if [ -f %s ]; then
			cat %s 2>/dev/null | head -1
		# Check if currently running in queue
		elif [ -f %s ] && [ "$(cat %s 2>/dev/null)" = "%d" ]; then
			echo RUNNING
		# Check if waiting in queue
		elif grep -q '^%d	' %s 2>/dev/null; then
			echo QUEUED
		# Check if process still running via PID
		elif pid=$(cat %s 2>/dev/null) && [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1; then
			echo RUNNING
		else
			echo DEAD
		fi
	`, statusPattern, statusPattern,
		currentFile, currentFile, job.ID,
		job.ID, queueFile,
		pidPattern)

	stdout, _, err := ssh.RunWithTimeout(job.Host, combinedCmd, 5*time.Second)
	if err != nil {
		// Connection error - don't update status
		return false, nil
	}

	result := strings.TrimSpace(stdout)

	// Parse result and update database
	switch result {
	case "RUNNING":
		// Job is currently running - update start time from metadata if not set
		updateStartTimeFromMetadataTUI(database, job)
		return false, nil
	case "QUEUED":
		// Job is still waiting in queue, no change needed
		return false, nil
	case "DEAD":
		// Job has died unexpectedly
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return false, err
		}
		return true, nil
	case "":
		// Empty result (shouldn't happen with our logic, but handle gracefully)
		return false, nil
	default:
		// Numeric exit code - job completed
		exitCode, parseErr := strconv.Atoi(result)
		if parseErr != nil {
			// Unexpected output - don't change status
			return false, nil
		}
		endTime := time.Now().Unix()

		// Update start time from metadata if not already set
		updateStartTimeFromMetadataTUI(database, job)

		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}
}

// checkAndReviveDeadJob checks if a dead job is actually still running and revives it
func checkAndReviveDeadJob(database *sql.DB, job *db.Job) (bool, error) {
	// Check if log file exists but no status file (job still running)
	logPattern := session.LogFilePattern(job.ID)
	checkCmd := fmt.Sprintf("ls %s 2>/dev/null | head -1", logPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, checkCmd, 5*time.Second)
	if err != nil {
		return false, nil // Can't reach host, don't change status
	}

	if strings.TrimSpace(stdout) == "" {
		// No log file, check if job is in queue's .current file
		currentFile := "~/.cache/remote-jobs/queue/default.current"
		currentCmd := fmt.Sprintf("cat %s 2>/dev/null", currentFile)
		stdout, _, err = ssh.RunWithTimeout(job.Host, currentCmd, 5*time.Second)
		if err != nil || strings.TrimSpace(stdout) != fmt.Sprintf("%d", job.ID) {
			return false, nil // Job is not current, stay dead
		}
	}

	// Check if status file exists (job completed, not running)
	statusPattern := session.StatusFilePattern(job.ID)
	statusCmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err = ssh.RunWithTimeout(job.Host, statusCmd, 5*time.Second)
	if err == nil && strings.TrimSpace(stdout) != "" {
		// Job has completed, update to completed instead of reviving
		exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Job is running (has log file or is current, but no status file) - revive it
	if err := db.ReviveDeadJob(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

// syncQueueRunnerJob checks status for jobs started by the queue runner
// These jobs don't have tmux sessions, so we check for status/log files by pattern
func syncQueueRunnerJob(database *sql.DB, job *db.Job) (bool, error) {
	// Check if status file exists (job completed)
	statusPattern := session.StatusFilePattern(job.ID)
	cmd := fmt.Sprintf("cat %s 2>/dev/null | head -1", statusPattern)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, 5*time.Second)
	if err != nil {
		// Can't reach host - don't change job status
		return false, nil
	}
	if strings.TrimSpace(stdout) != "" {
		// Job completed - read exit code
		exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// Check if job is in queue's .current file (actively running right now)
	queueName := job.QueueName
	if queueName == "" {
		queueName = "default"
	}
	currentFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.current", queueName)
	currentCmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, currentCmd, 5*time.Second)
	if err != nil {
		// Can't reach host - don't change job status
		return false, nil
	}
	currentJobID := strings.TrimSpace(stdout)
	if currentJobID == fmt.Sprintf("%d", job.ID) {
		// Job is currently running
		return false, nil
	}

	// Check if job is still in the queue file (waiting to run)
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	grepCmd := fmt.Sprintf("grep -q '^%d	' %s 2>/dev/null && echo yes || echo no", job.ID, queueFile)
	stdout, _, err = ssh.RunWithTimeout(job.Host, grepCmd, 5*time.Second)
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(stdout) == "yes" {
		// Job is still in queue, waiting to run
		return false, nil
	}

	// Check if the job's process is still running (via PID file)
	pidPattern := session.PidFilePattern(job.ID)
	pidCmd := fmt.Sprintf("pid=$(cat %s 2>/dev/null); [ -n \"$pid\" ] && ps -p $pid > /dev/null 2>&1 && echo running || echo not_running", pidPattern)
	stdout, _, err = ssh.RunWithTimeout(job.Host, pidCmd, 5*time.Second)
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(stdout) == "running" {
		// Process is still running, don't mark as dead
		return false, nil
	}

	// Job is not current, not in queue, process not running, and has no status file - it's dead
	// (Either it died mid-execution, or was removed from queue)
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

func (m Model) startQueue(host string) tea.Cmd {
	return func() tea.Msg {
		queueName := "default"
		runnerSession := fmt.Sprintf("rj-queue-%s", queueName)

		// Check if queue runner is already running
		exists, err := ssh.TmuxSessionExists(host, runnerSession)
		if err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("check session: %w", err)}
		}

		if exists {
			return queueStartedMsg{host: host, already: true}
		}

		// Create directories on remote
		queueDir := "~/.cache/remote-jobs/queue"
		scriptsDir := "~/.cache/remote-jobs/scripts"
		mkdirCmd := fmt.Sprintf("mkdir -p %s %s", queueDir, scriptsDir)
		if _, stderr, err := ssh.Run(host, mkdirCmd); err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("create directories: %s", stderr)}
		}

		// Deploy queue runner script (embedded in binary)
		queueRunnerPath := "~/.cache/remote-jobs/scripts/queue-runner.sh"
		writeCmd := fmt.Sprintf("cat > %s << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", queueRunnerPath, string(scripts.QueueRunnerScript))
		if _, stderr, err := ssh.Run(host, writeCmd); err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("write queue runner: %s", stderr)}
		}

		// Make script executable
		chmodCmd := fmt.Sprintf("chmod +x %s", queueRunnerPath)
		if _, stderr, err := ssh.Run(host, chmodCmd); err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("chmod: %s", stderr)}
		}

		// Start queue runner in tmux
		runnerCmd := fmt.Sprintf("$HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", queueName)
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", runnerSession, ssh.EscapeForSingleQuotes(runnerCmd))

		if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("start queue runner: %s", stderr)}
		}

		return queueStartedMsg{host: host}
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
	envVarsStr := strings.TrimSpace(m.inputs[inputEnvVars].Value())

	if workingDir == "" {
		workingDir = "~"
	}

	// Parse env vars (comma-separated VAR=value pairs)
	var envVars []string
	if envVarsStr != "" {
		for _, ev := range strings.Split(envVarsStr, ",") {
			ev = strings.TrimSpace(ev)
			if ev != "" {
				envVars = append(envVars, ev)
			}
		}
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
			EnvVars:    envVars,
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

// hostFromCachedInfo creates a Host from cached database info
func hostFromCachedInfo(cached *db.CachedHostInfo) *Host {
	host := &Host{
		Name:      cached.Name,
		Status:    HostStatusUnknown, // Will be updated when we query
		Arch:      cached.Arch,
		OS:        cached.OSVersion,
		Model:     cached.Model,
		CPUs:      cached.CPUCount,
		CPUModel:  cached.CPUModel,
		CPUFreq:   cached.CPUFreq,
		MemTotal:  cached.MemTotal,
		LastCheck: time.Unix(cached.LastUpdated, 0),
	}

	// Parse GPUs from JSON
	if cached.GPUsJSON != "" {
		var gpus []GPUInfo
		if err := json.Unmarshal([]byte(cached.GPUsJSON), &gpus); err == nil {
			host.GPUs = gpus
		}
	}

	return host
}

// cachedInfoFromHost creates a CachedHostInfo from a Host
func cachedInfoFromHost(host *Host) *db.CachedHostInfo {
	cached := &db.CachedHostInfo{
		Name:        host.Name,
		Arch:        host.Arch,
		OSVersion:   host.OS,
		Model:       host.Model,
		CPUCount:    host.CPUs,
		CPUModel:    host.CPUModel,
		CPUFreq:     host.CPUFreq,
		MemTotal:    host.MemTotal,
		LastUpdated: time.Now().Unix(),
	}

	// Encode GPUs to JSON
	if len(host.GPUs) > 0 {
		if data, err := json.Marshal(host.GPUs); err == nil {
			cached.GPUsJSON = string(data)
		}
	}

	return cached
}

// updateHostWithCachedStatic updates a host's dynamic fields while preserving static cached data
func updateHostWithCachedStatic(host *Host, cached *Host) {
	// Copy static fields from cached host if current host doesn't have them
	// This preserves cached static info when we get a partial update
	if host.Arch == "" {
		host.Arch = cached.Arch
	}
	if host.OS == "" {
		host.OS = cached.OS
	}
	if host.Model == "" {
		host.Model = cached.Model
	}
	if host.CPUs == 0 {
		host.CPUs = cached.CPUs
	}
	if host.CPUModel == "" {
		host.CPUModel = cached.CPUModel
	}
	if host.CPUFreq == "" {
		host.CPUFreq = cached.CPUFreq
	}
	if host.MemTotal == "" {
		host.MemTotal = cached.MemTotal
	}
	// GPUs are static info about what GPUs exist (not utilization)
	// We always get fresh GPU data when online, so don't merge
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
	// Handle queued jobs that haven't started yet
	if startTime == 0 {
		return "—"
	}

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
