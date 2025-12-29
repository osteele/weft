package tui

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/progress"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/queuejob"
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

// jobSortMode represents how jobs are sorted in the list
type jobSortMode int

const (
	jobSortNewest     jobSortMode = iota // Most recent first (default)
	jobSortOldest                        // Oldest first
	jobSortIDDesc                        // Job ID descending
	jobSortIDAsc                         // Job ID ascending
	jobSortQueueOrder                    // Queued jobs in execution order, then running, then completed
	jobSortModeCount
)

func (m jobSortMode) String() string {
	switch m {
	case jobSortNewest:
		return "Newest"
	case jobSortOldest:
		return "Oldest"
	case jobSortIDDesc:
		return "ID↓"
	case jobSortIDAsc:
		return "ID↑"
	case jobSortQueueOrder:
		return "Queue"
	default:
		return "Unknown"
	}
}

// DetailTab represents which tab is active in the job detail panel
type DetailTab int

const (
	DetailTabDetails DetailTab = iota
	DetailTabLogs
)

// HostDetailTab represents which tab is active in the host detail panel
type HostDetailTab int

const (
	HostDetailTabInfo       HostDetailTab = iota // Host Info
	HostDetailTabGPUSummary                      // GPU Summary (all GPUs)
	HostDetailTabGPUBase                         // Base for GPU detail tabs (position in GPU list, not actual GPU index)
)

// IsGPUDetailTab returns true if this tab shows details for a specific GPU
func (t HostDetailTab) IsGPUDetailTab() bool {
	return t >= HostDetailTabGPUBase
}

// GPUPosition returns the position in the GPU list for a GPU detail tab (-1 if not a GPU detail tab)
// This is the index into getHostGPUIndices(), not the actual GPU hardware index
func (t HostDetailTab) GPUPosition() int {
	if t >= HostDetailTabGPUBase {
		return int(t - HostDetailTabGPUBase)
	}
	return -1
}

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
	ShiftTab    key.Binding
	Sync        key.Binding
	Help        key.Binding
	StartQueue  key.Binding
	StartNow    key.Binding
	MoveToFront key.Binding
	Edit        key.Binding
	Sort        key.Binding
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
		key.WithHelp("k", "kill/cancel"),
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
		key.WithKeys("right", "h"),
		key.WithHelp("→/h", "hosts"),
	),
	JobsView: key.NewBinding(
		key.WithKeys("left", "j"),
		key.WithHelp("←/j", "jobs"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch view"),
	),
	ShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "switch view back"),
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
	MoveToFront: key.NewBinding(
		key.WithKeys("F"),
		key.WithHelp("F", "move to front"),
	),
	Edit: key.NewBinding(
		key.WithKeys("e"),
		key.WithHelp("e", "edit"),
	),
	Sort: key.NewBinding(
		key.WithKeys("o"),
		key.WithHelp("o", "cycle sort"),
	),
}

// Messages
type jobsRefreshedMsg struct {
	jobs             []*db.Job
	pendingOpsJobIDs map[int64]bool
	jobDependencies  map[int64]string // jobID -> dep_spec (e.g., "930" or "930+")
	err              error
}

type syncCompletedMsg struct {
	updated       int
	queuesStarted []string // hosts where queue runners were started
	err           error
}

type logFetchedMsg struct {
	jobID     int64
	content   string
	progress  *progress.Progress // extracted progress info (nil if none found)
	err       error
	connError bool // true if this was a connection error (host unreachable)
}

type jobKilledMsg struct {
	jobID     int64
	err       error
	deferred  bool // true if kill was queued for later (host offline)
	cancelled bool // true if this was a queued job that was cancelled
}

type jobRestartedMsg struct {
	oldJobID int64
	newJobID int64
	err      error
	deferred bool // true if restart was queued for later (host offline)
}

type jobStartedNowMsg struct {
	jobID    int64
	host     string
	deferred bool
	err      error
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

type jobMovedToFrontMsg struct {
	jobID int64
	moved bool // false if already at front
	err   error
}

type jobRemovedMsg struct {
	jobID int64
	err   error
}

type jobCreatedMsg struct {
	jobID    int64
	err      error
	deferred bool // true if job creation was queued for later (host offline)
}

type jobEditedMsg struct {
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

type hostDeletedMsg struct {
	hostName string
	err      error
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
	allJobs     []*db.Job
	jobs        []*db.Job
	jobList     list.Model // bubbles/list for job selection
	selectedJob *db.Job
	jobFilter   jobFilterMode
	jobSort     jobSortMode

	// Hosts data
	hosts           []*Host
	selectedHostIdx int
	hostDetailTab   HostDetailTab // Which tab is active in host detail panel (Info or GPU)

	// UI State
	detailTab    DetailTab // Which tab is active in detail panel (Details or Logs)
	logContent   string
	logStale     bool             // true if showing cached content due to connection error
	logCache     map[int64]string // cache of last successful log content per job
	logLoading   bool
	logViewport  viewport.Model
	spinner      spinner.Model // Loading spinner
	flashMessage string
	flashIsError bool
	flashExpiry  time.Time

	// Process stats for running jobs
	processStats      *ssh.ProcessStats
	prevProcessStats  *ssh.ProcessStats // Previous sample for CPU% calculation
	processStatsJobID int64

	// Progress tracking for running jobs
	progressTracker *progress.Tracker            // tracks file sizes for incremental reads
	jobProgress     map[int64]*progress.Progress // jobID -> latest progress

	// Operation state
	restarting         bool
	restartingJobName  string
	pendingSelectJobID int64

	// New job input mode
	inputMode      bool
	inputs         []textinput.Model
	inputFocus     int
	creatingJob    bool
	createJobStart time.Time
	createJobStep  string

	// Edit job mode (for queued jobs only)
	editMode     bool
	editingJobID int64

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

	// Jobs with pending deferred operations (for status display)
	pendingOpsJobIDs map[int64]bool

	// Job dependencies (jobID -> dep_spec like "930" or "930+")
	jobDependencies map[int64]string
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

	inputs[inputDescription] = textinput.New()
	inputs[inputDescription].Placeholder = "(optional)"
	inputs[inputDescription].Prompt = ""
	inputs[inputDescription].Width = 40
	inputs[inputDescription].CharLimit = 256

	inputs[inputCommand] = textinput.New()
	inputs[inputCommand].Placeholder = "e.g., python train.py"
	inputs[inputCommand].Prompt = ""
	inputs[inputCommand].Width = 40
	inputs[inputCommand].CharLimit = 512

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

	// Initialize the job list with custom delegate
	delegate := NewJobDelegate()
	jobList := list.New([]list.Item{}, delegate, 0, 0)
	jobList.SetShowTitle(false)
	jobList.SetShowStatusBar(false)
	jobList.SetShowFilter(false)
	jobList.SetShowHelp(false)
	jobList.SetFilteringEnabled(false)
	// Disable quit/filter keys - we handle those at the app level
	jobList.KeyMap.Filter = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.ClearFilter = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.CancelWhileFiltering = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.AcceptWhileFiltering = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.ShowFullHelp = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.CloseFullHelp = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.Quit = key.NewBinding(key.WithDisabled())
	jobList.KeyMap.ForceQuit = key.NewBinding(key.WithDisabled())
	// Keep navigation keys enabled - let the list handle up/down/pgup/pgdown

	// Initialize spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return Model{
		database:                database,
		jobList:                 jobList,
		jobFilter:               jobFilterAll,
		inputs:                  inputs,
		spinner:                 s,
		syncInterval:            opts.SyncInterval,
		logRefreshInterval:      opts.LogRefreshInterval,
		hostRefreshInterval:     opts.HostRefreshInterval,
		hostCacheDuration:       opts.HostCacheDuration,
		hostsQueriedThisSession: make(map[string]bool),
		logCache:                make(map[int64]string),
		pendingOpsJobIDs:        make(map[int64]bool),
		jobDependencies:         make(map[int64]string),
		progressTracker:         progress.NewTracker(),
		jobProgress:             make(map[int64]*progress.Progress),
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
		m.spinner.Tick,
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
		// Update job list dimensions (subtract 2 more for column header + filter row)
		listHeight := m.height - detailHeight - 5 // account for header/footer
		m.jobList.SetWidth(m.width - 2)
		m.jobList.SetHeight(listHeight - 2)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

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
		if msg.pendingOpsJobIDs != nil {
			m.pendingOpsJobIDs = msg.pendingOpsJobIDs
		}
		if msg.jobDependencies != nil {
			m.jobDependencies = msg.jobDependencies
		}
		m.applyJobFilter()

		// If there's a pending job selection, find and select it
		if m.pendingSelectJobID > 0 {
			for i, job := range m.jobs {
				if job.ID == m.pendingSelectJobID {
					m.jobList.Select(i)
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
		}
		// Build flash message
		var flashParts []string
		if msg.updated > 0 {
			flashParts = append(flashParts, fmt.Sprintf("Synced %d job(s)", msg.updated))
		}
		if len(msg.queuesStarted) > 0 {
			flashParts = append(flashParts, fmt.Sprintf("Started queue on %s", strings.Join(msg.queuesStarted, ", ")))
		}
		if len(flashParts) > 0 {
			return m, tea.Batch(
				m.setFlash(strings.Join(flashParts, "; "), false),
				m.refreshJobs(),
				m.loadHosts(), // Refresh hosts to update queue status
			)
		}
		return m, m.setFlash("Sync complete", false)

	case logFetchedMsg:
		m.logLoading = false

		// Store progress info if available (regardless of selection)
		if msg.progress != nil {
			m.jobProgress[msg.jobID] = msg.progress
		}

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
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Cancel failed: %v", msg.err), true)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Kill failed: %v", msg.err), true)
			}
		} else if msg.deferred {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d cancelled (removal queued for when host is online)", msg.jobID), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d marked dead (kill queued for when host is online)", msg.jobID), false)
			}
		} else {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d cancelled", msg.jobID), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d killed", msg.jobID), false)
			}
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobRestartedMsg:
		m.restarting = false
		m.restartingJobName = ""
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Restart failed: %v", msg.err), true)
		}
		m.pendingSelectJobID = msg.newJobID
		if msg.deferred {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %d created (will start when host is online)", msg.newJobID), false), m.refreshJobs())
		}
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job restarted (new ID: %d)", msg.newJobID), false), m.refreshJobs())

	case jobStartedNowMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Start failed: %v", msg.err), true)
		}
		if msg.deferred {
			return m, tea.Batch(
				m.setFlash(fmt.Sprintf("Host %s unreachable. Job %d will start on next sync.", msg.host, msg.jobID), false),
				m.refreshJobs(),
			)
		}
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %d started", msg.jobID), false), m.refreshJobs())

	case pruneCompletedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Prune failed: %v", msg.err), true)
		} else if msg.count > 0 {
			flashCmd = m.setFlash(fmt.Sprintf("Tombstoned %d job(s)", msg.count), false)
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

	case jobMovedToFrontMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Move to front failed: %v", msg.err), true)
		} else if !msg.moved {
			return m, m.setFlash(fmt.Sprintf("Job %d is already at the front", msg.jobID), false)
		}
		return m, m.setFlash(fmt.Sprintf("Job %d moved to front of queue", msg.jobID), false)

	case jobRemovedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Remove failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d removed", msg.jobID), false)
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
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d created (will start when host is online)", msg.jobID), false)
			m.pendingSelectJobID = msg.jobID
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d started", msg.jobID), false)
			m.pendingSelectJobID = msg.jobID
			// Keep inputs for easy re-use (user can modify and submit again)
		}
		// Reload hosts in case this job was on a new host
		return m, tea.Batch(flashCmd, m.refreshJobs(), m.loadHosts())

	case jobEditedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Edit failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d updated", msg.jobID), false)
			m.pendingSelectJobID = msg.jobID
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

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

	case hostDeletedMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Delete failed: %v", msg.err), true)
		}
		// Remove host from list
		for i, h := range m.hosts {
			if h.Name == msg.hostName {
				m.hosts = append(m.hosts[:i], m.hosts[i+1:]...)
				// Adjust selection if needed
				if m.selectedHostIdx >= len(m.hosts) && m.selectedHostIdx > 0 {
					m.selectedHostIdx--
				}
				break
			}
		}
		delete(m.hostsQueriedThisSession, msg.hostName)
		return m, m.setFlash(fmt.Sprintf("Host %s deleted", msg.hostName), false)

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

		// Build set of hosts with running jobs
		hostsWithRunningJobs := make(map[string]bool)
		for _, job := range m.jobs {
			if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
				hostsWithRunningJobs[job.Host] = true
			}
		}

		for _, host := range m.hosts {
			// Refresh host if:
			// 1. In hosts view and (not queried yet OR online), OR
			// 2. Host has running jobs (to update LastCheck for stale indicator)
			inHostsView := m.viewMode == ViewModeHosts
			needsRefresh := (!m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline)
			hasRunningJobs := hostsWithRunningJobs[host.Name]

			if (inHostsView && needsRefresh) || hasRunningJobs {
				cmds = append(cmds, m.fetchHostInfo(host.Name))
				if inHostsView {
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

// handleMouseClick handles mouse click and wheel events
func (m Model) handleMouseClick(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Ignore mouse events when in input mode or showing overlays
	if m.inputMode || m.showHelp || m.restarting || m.creatingJob {
		return m, nil
	}

	// Calculate list panel height (same as in View)
	listHeight := int(float64(m.height) * 0.55)

	// Handle mouse wheel
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		scrollUp := msg.Button == tea.MouseButtonWheelUp

		// Check if mouse is in the log panel area (bottom portion)
		if msg.Y >= listHeight && m.detailTab == DetailTabLogs {
			scrollDelta := 3
			if scrollUp {
				scrollDelta = -scrollDelta
			}
			m.logViewport.SetYOffset(m.logViewport.YOffset + scrollDelta)
			return m, nil
		}

		// Scroll job list by moving cursor (bubbles/list doesn't handle wheel events)
		if m.viewMode == ViewModeJobs && len(m.jobs) > 0 {
			prevIdx := m.jobList.Index()
			for i := 0; i < 3; i++ {
				if scrollUp {
					m.jobList.CursorUp()
				} else {
					m.jobList.CursorDown()
				}
			}
			if m.jobList.Index() != prevIdx {
				return m, m.handleSelectionChanged()
			}
		}
		return m, nil
	}

	// Handle mouse clicks manually (bubbles/list has limited mouse support)
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
		// Layout within panel: border(1) + header(1) + filter(1) + jobs start at Y=3
		if m.viewMode == ViewModeJobs && msg.Y >= 3 && msg.Y < listHeight-1 {
			clickedRow := msg.Y - 3
			// Get the visible range from the list's paginator
			start, _ := m.jobList.Paginator.GetSliceBounds(len(m.jobs))
			clickedIndex := start + clickedRow
			if clickedIndex >= 0 && clickedIndex < len(m.jobs) {
				prevIdx := m.jobList.Index()
				m.jobList.Select(clickedIndex)
				if m.jobList.Index() != prevIdx {
					return m, m.handleSelectionChanged()
				}
			}
			return m, nil
		}
	}

	// Handle host clicks manually since hosts don't use bubbles/list
	if m.viewMode == ViewModeHosts {
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			// Layout within panel: border(1) + header(1) + hosts start at Y=2 (no filter row)
			if msg.Y >= 2 && msg.Y < listHeight-1 {
				clickedRow := msg.Y - 2
				if clickedRow >= 0 && clickedRow < len(m.hosts) {
					m.selectedHostIdx = clickedRow
				}
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

	case key.Matches(msg, keys.Tab), key.Matches(msg, keys.ShiftTab):
		forward := key.Matches(msg, keys.Tab)
		return m.cycleTab(forward)

	case key.Matches(msg, keys.HostsView):
		// Toggle between hosts and jobs view
		if m.viewMode == ViewModeHosts {
			m.viewMode = ViewModeJobs
			return m, nil
		}
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
			return m, nil
		}
		// Forward to list and handle selection change
		prevIdx := m.jobList.Index()
		newList, cmd := m.jobList.Update(msg)
		m.jobList = newList
		if m.jobList.Index() != prevIdx {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		return m, cmd

	case key.Matches(msg, keys.Down):
		if m.viewMode == ViewModeHosts {
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts)-1 {
				m.selectedHostIdx++
			}
			return m, nil
		}
		// Forward to list and handle selection change
		prevIdx := m.jobList.Index()
		newList, cmd := m.jobList.Update(msg)
		m.jobList = newList
		if m.jobList.Index() != prevIdx {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		return m, cmd

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
			} else {
				idx := m.jobList.Index()
				if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
					// Enter logs mode
					m.detailTab = DetailTabLogs
					m.selectedJob = m.jobs[idx]
					m.logLoading = true
					// Show cached content immediately while fetching fresh logs
					if cached, ok := m.logCache[m.selectedJob.ID]; ok {
						m.logContent = cached
						m.logStale = true
						m.logViewport.SetContent(m.logContent)
					} else {
						m.logContent = ""
						m.logStale = false
					}
					var cmds []tea.Cmd
					cmds = append(cmds, m.fetchSelectedJobLog())
					// Fetch process stats for running jobs
					if m.selectedJob.Status == db.StatusRunning {
						cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
					}
					return m, tea.Batch(cmds...)
				}
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
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		switch job.Status {
		case db.StatusRunning, db.StatusStarting:
			return m, tea.Batch(m.setFlash("Killing job...", false), m.killJob(job))
		case db.StatusQueued:
			return m, tea.Batch(m.setFlash("Cancelling queued job...", false), m.cancelQueuedJob(job))
		case db.StatusCompleted:
			return m, m.setFlash(fmt.Sprintf("Job %d already completed", job.ID), true)
		case db.StatusDead:
			return m, m.setFlash(fmt.Sprintf("Job %d already dead", job.ID), true)
		case db.StatusFailed:
			return m, m.setFlash(fmt.Sprintf("Job %d already failed", job.ID), true)
		default:
			return m, m.setFlash(fmt.Sprintf("Can't kill job %d (status: %s)", job.ID, job.Status), true)
		}

	case key.Matches(msg, keys.Restart):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
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
		if m.viewMode == ViewModeHosts {
			// Delete host in hosts view
			if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
				return m, nil
			}
			host := m.hosts[m.selectedHostIdx]
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Deleting host %s...", host.Name), false), m.deleteHost(host.Name))
		}
		// Remove job in jobs view
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		// Refuse to remove active jobs - suggest killing first
		if job.Status == db.StatusRunning || job.Status == db.StatusQueued || job.Status == db.StatusStarting {
			return m, m.setFlash(fmt.Sprintf("Job %d is %s. Kill it first (k)", job.ID, job.Status), true)
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
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		m.jobFilter = jobFilterMode((int(m.jobFilter) + 1) % int(jobFilterModeCount))
		m.applyJobFilter()
		return m, m.setFlash(fmt.Sprintf("Filter: %s", jobFilterDescription(m.jobFilter)), false)

	case key.Matches(msg, keys.Prune):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		return m, tea.Batch(m.setFlash("Pruning completed/dead jobs...", false), m.pruneJobs())

	case key.Matches(msg, keys.StartQueue):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting queue on %s...", job.Host), false), m.startQueue(job.Host))
		}
		return m, nil

	case key.Matches(msg, keys.StartNow):
		if m.viewMode == ViewModeHosts {
			// 'g' in hosts view switches to GPU Summary tab
			m.hostDetailTab = HostDetailTabGPUSummary
			return m, nil
		}
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting job %d now...", job.ID), false), m.startQueuedJobNow(job))
		}
		return m, m.setFlash("Can only start queued jobs", true)

	case key.Matches(msg, keys.MoveToFront):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Moving job %d to front...", job.ID), false), m.moveJobToFront(job))
		}
		return m, m.setFlash("Can only move queued jobs to front", true)

	case key.Matches(msg, keys.Sort):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		m.jobSort = (m.jobSort + 1) % jobSortModeCount
		m.applyJobFilter() // Re-filter and sort
		return m, m.setFlash(fmt.Sprintf("Sort: %s", m.jobSort), false)

	case key.Matches(msg, keys.Edit):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		if job.Status != db.StatusQueued {
			return m, m.setFlash("Can only edit queued jobs", true)
		}
		// Enter edit mode with form pre-populated
		m.inputMode = true
		m.editMode = true
		m.editingJobID = job.ID
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.flashMessage = ""
		// Pre-populate all fields
		m.inputs[inputHost].SetValue(job.Host)
		m.inputs[inputCommand].SetValue(job.Command)
		m.inputs[inputDescription].SetValue(job.Description)
		m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		return m, nil

	case key.Matches(msg, keys.Sync):
		if m.viewMode == ViewModeJobs && !m.syncing {
			m.syncing = true
			return m, tea.Batch(m.setFlash("Syncing...", false), m.performBackgroundSync())
		}
		return m, nil
	}

	// Host view specific key bindings (handled after the switch)
	if m.viewMode == ViewModeHosts {
		switch msg.String() {
		case "i":
			// Switch to Info tab
			m.hostDetailTab = HostDetailTabInfo
			return m, nil
		case "S":
			// Start queue runner on selected host
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts) {
				host := m.hosts[m.selectedHostIdx]
				return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting queue on %s...", host.Name), false), m.startQueue(host.Name))
			}
			return m, nil
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
			// Switch to specific GPU tab by hardware index
			gpuIndex, _ := strconv.Atoi(msg.String())
			gpuIndices := m.getHostGPUIndices()
			for pos, idx := range gpuIndices {
				if idx == gpuIndex {
					m.hostDetailTab = HostDetailTabGPUBase + HostDetailTab(pos)
					return m, nil
				}
			}
			// GPU not found - show message
			return m, m.setFlash(fmt.Sprintf("No GPU %d on this host", gpuIndex), true)
		}
	}

	return m, nil
}

func (m Model) handleInputKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Cancel input mode
		m.inputMode = false
		m.editMode = false
		m.editingJobID = 0
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

		// Exit input mode
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		m.flashMessage = ""

		if m.editMode {
			// Edit existing job
			m.editMode = false
			return m, m.editJob()
		}

		// Create new job
		m.creatingJob = true
		m.createJobStart = time.Now()
		m.createJobStep = "Connecting..."
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
		return m.spinner.View() + " Loading..."
	}

	// Calculate panel heights
	listHeight := int(float64(m.height) * 0.55)
	detailHeight := int(float64(m.height) * 0.35)

	var mainView string

	if m.viewMode == ViewModeHosts {
		// Hosts view
		listView := m.renderHostList(listHeight)
		detailView := m.renderHostDetailPanel(detailHeight)
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
			{"←/→", "Switch to hosts view"},
			{"l", "Toggle logs view"},
			{"s", "Sync job statuses"},
			{"n", "New job"},
			{"e", "Edit queued job"},
			{"r", "Restart job"},
			{"R", "Edit & restart job"},
			{"k", "Kill/cancel job"},
			{"g", "Start queued job now"},
			{"S", "Start queue runner"},
			{"x", "Remove job from list"},
			{"P", "Prune completed/dead jobs"},
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
			{"←/→", "Switch to jobs view"},
			{"x", "Delete host"},
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
	if m.editMode {
		b.WriteString(fmt.Sprintf("Edit Job %d\n\n", m.editingJobID))
	} else {
		b.WriteString("New Job\n\n")
	}

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
	var helpText string
	if m.editMode {
		helpText = "Tab: next field • Enter: save changes • Esc: cancel"
	} else {
		helpText = "Tab: next field • Enter: create job • Esc: cancel"
	}
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
	// DEBUG: Log at start of every render
	f, _ := os.OpenFile("/tmp/rj-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString(fmt.Sprintf("=== renderJobList called, jobs=%d hosts=%d ===\n", len(m.jobs), len(m.hosts)))
		for _, h := range m.hosts {
			f.WriteString(fmt.Sprintf("  host=%s status=%d lastCheck=%v\n", h.Name, h.Status, h.LastCheck))
		}
		f.Close()
	}

	var rows []string

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %-4s %s",
		"ID", "HOST", "STATUS", "TIME", "GPU", "COMMAND / DESCRIPTION")
	rows = append(rows, headerStyle.Render(header))
	filterLabel := fmt.Sprintf(" Filter: %s | Sort: %s", jobFilterDescription(m.jobFilter), m.jobSort)
	rows = append(rows, dimStyle.Render(filterLabel))

	if len(m.jobs) == 0 {
		rows = append(rows, dimStyle.Render(" No jobs match this filter"))
		content := strings.Join(rows, "\n")
		return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
	}

	// Render jobs manually using list's paginator for scroll offset
	contentHeight := height - 4 // Account for borders, header, filter
	start, end := m.jobList.Paginator.GetSliceBounds(len(m.jobs))
	if end > len(m.jobs) {
		end = len(m.jobs)
	}
	// Limit to visible height
	if end-start > contentHeight {
		end = start + contentHeight
	}

	selectedIdx := m.jobList.Index()
	for i := start; i < end; i++ {
		job := m.jobs[i]
		status := m.formatStatus(job)
		timeCol := formatJobTime(job)
		gpu := job.GetGPU()
		if gpu == "" {
			gpu = "—"
		}

		// Show description if available, otherwise truncated command
		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		display = truncate(display, 35)

		line := fmt.Sprintf(" %-4d %-10s %-12s %-12s %-4s %s",
			job.ID, truncate(job.Host, 10),
			status, timeCol, truncate(gpu, 4), display)

		// DEBUG: Log host state for disconnected host jobs
		if job.Host == "cool100" || job.Host == "studio" {
			var debugInfo string
			found := false
			for _, h := range m.hosts {
				if h.Name == job.Host {
					found = true
					debugInfo = fmt.Sprintf("job=%d host=%s status=%d lastCheck=%v ago=%v disconnected=%v\n",
						job.ID, job.Host, h.Status, h.LastCheck, time.Since(h.LastCheck), m.isHostDisconnectedLong(job))
					break
				}
			}
			if !found {
				debugInfo = fmt.Sprintf("job=%d host=%s NOT_FOUND_IN_HOSTS (len=%d)\n", job.ID, job.Host, len(m.hosts))
			}
			f, _ := os.OpenFile("/tmp/rj-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if f != nil {
				f.WriteString(debugInfo)
				f.Close()
			}
		}

		if i == selectedIdx {
			line = selectedStyle.Width(m.width - 4).Render(line)
		} else if m.isHostDisconnectedLong(job) {
			// Dim jobs on hosts disconnected for more than 30 minutes
			line = dimStyle.Render(line)
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

// renderTabHeader renders the "Details  Logs" tab header as visual tabs
func (m Model) renderTabHeader() string {
	detailsLabel := "Details"
	logsLabel := "Logs"

	var detailsTab, logsTab string
	if m.detailTab == DetailTabDetails {
		detailsTab = activeTabStyle.Render(detailsLabel)
		logsTab = inactiveTabStyle.Render(logsLabel)
	} else {
		detailsTab = inactiveTabStyle.Render(detailsLabel)
		logsTab = activeTabStyle.Render(logsLabel)
	}

	// Add hint about Tab key switching
	hint := dimStyle.Render(" (Tab to switch)")

	return detailsTab + " " + logsTab + hint
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
		content = dimStyle.Render(m.spinner.View() + " Loading logs...")
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
	var b strings.Builder

	// Styles for the details view
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Width(10)
	valueStyle := lipgloss.NewStyle()
	headerStyle := lipgloss.NewStyle().Bold(true)
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)

	highlightedJob := m.getTargetJob()

	if highlightedJob == nil {
		content = dimStyle.Render("No jobs to display")
	} else {
		job := highlightedJob

		// Header line with job ID, host, and status
		statusStr := m.formatStatus(job)
		b.WriteString(headerStyle.Render(fmt.Sprintf("Job %d", job.ID)))
		b.WriteString(dimStyle.Render(" on "))
		b.WriteString(headerStyle.Render(job.Host))
		b.WriteString(dimStyle.Render(" · "))
		b.WriteString(statusStr)

		// Show dependency info on same line if present
		if depSpec := m.jobDependencies[job.ID]; depSpec != "" {
			if strings.HasSuffix(depSpec, "+") {
				b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s)", strings.TrimSuffix(depSpec, "+"))))
			} else {
				b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s succeeds)", depSpec)))
			}
		} else if m.pendingOpsJobIDs[job.ID] && job.Status == db.StatusQueued {
			b.WriteString(dimStyle.Render(" (pending sync)"))
		}
		b.WriteString("\n\n")

		// Description (if any) - shown in italic without label
		if job.Description != "" {
			descStyle := lipgloss.NewStyle().Italic(true)
			b.WriteString(descStyle.Render(job.Description))
			b.WriteString("\n")
		}

		// Command (most important) - wrap and indent continuation lines, but limit height
		b.WriteString(labelStyle.Render("Command"))
		cmd := job.EffectiveCommand()
		labelWidth := 10
		// Panel content width is m.width - 6 (borders + padding), minus label
		availableWidth := m.width - 6 - labelWidth
		if availableWidth < 20 {
			availableWidth = 60 // fallback if window too narrow
		}
		wrappedCmd := wrapTextWithIndent(cmd, availableWidth, labelWidth)
		// Limit command display to 8 lines max to avoid overwhelming the details panel
		const maxCmdLines = 8
		cmdLines := strings.Split(wrappedCmd, "\n")
		if len(cmdLines) > maxCmdLines {
			cmdLines = cmdLines[:maxCmdLines]
			wrappedCmd = strings.Join(cmdLines, "\n") + "\n" + strings.Repeat(" ", labelWidth) + "..."
		}
		b.WriteString(valueStyle.Render(wrappedCmd))
		b.WriteString("\n")

		// Directory
		b.WriteString(labelStyle.Render("Directory"))
		b.WriteString(valueStyle.Render(job.EffectiveWorkingDir()))
		b.WriteString("\n")

		// Environment variables (if any)
		envVars := job.ParseExportVars()
		if len(envVars) > 0 {
			b.WriteString(labelStyle.Render("Env"))
			b.WriteString(valueStyle.Render(strings.Join(envVars, ", ")))
			b.WriteString("\n")
		}

		// Timing section
		if job.CreatedAt > 0 || job.StartTime > 0 || job.EndTime != nil {
			b.WriteString("\n")

			// Show created time if available and different from start time
			if job.CreatedAt > 0 && job.CreatedAt != job.StartTime {
				createdTime := time.Unix(job.CreatedAt, 0)
				label := "Created"
				if job.Status == db.StatusQueued {
					label = "Queued"
				}
				b.WriteString(labelStyle.Render(label))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%s (%s)", createdTime.Format("2006-01-02 15:04:05"), formatStartTime(job.CreatedAt))))
				b.WriteString("\n")
			}

			if job.StartTime > 0 {
				startTime := time.Unix(job.StartTime, 0)
				b.WriteString(labelStyle.Render("Started"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%s (%s)", startTime.Format("2006-01-02 15:04:05"), formatStartTime(job.StartTime))))
				b.WriteString("\n")

				// Show timing information based on job status
				if job.Status == db.StatusRunning {
					elapsed := time.Since(startTime)
					b.WriteString(labelStyle.Render("Elapsed"))
					b.WriteString(valueStyle.Render(formatDuration(elapsed)))
					b.WriteString("\n")
				} else if job.EndTime != nil {
					endTime := time.Unix(*job.EndTime, 0)
					duration := endTime.Sub(startTime)
					b.WriteString(labelStyle.Render("Ended"))
					b.WriteString(valueStyle.Render(fmt.Sprintf("%s (%s)", endTime.Format("2006-01-02 15:04:05"), formatStartTime(*job.EndTime))))
					b.WriteString("\n")
					b.WriteString(labelStyle.Render("Duration"))
					b.WriteString(valueStyle.Render(formatDuration(duration)))
					b.WriteString("\n")
				}
			} else if job.EndTime != nil {
				// Job ended without ever starting (failed/killed before start)
				endTime := time.Unix(*job.EndTime, 0)
				b.WriteString(labelStyle.Render("Ended"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%s (%s)", endTime.Format("2006-01-02 15:04:05"), formatStartTime(*job.EndTime))))
				b.WriteString("\n")
			}
		}

		// Exit status
		if job.Status == db.StatusCompleted && job.ExitCode != nil {
			b.WriteString(labelStyle.Render("Exit"))
			if *job.ExitCode == 0 {
				b.WriteString(completedStyle.Render("0 (success)"))
			} else {
				b.WriteString(failedStyle.Render(fmt.Sprintf("%d (failed)", *job.ExitCode)))
			}
			b.WriteString("\n")
		} else if job.Status == db.StatusDead {
			b.WriteString(labelStyle.Render("Exit"))
			b.WriteString(deadStyle.Render("killed/crashed"))
			b.WriteString("\n")
		} else if job.Status == db.StatusFailed {
			b.WriteString(labelStyle.Render("Exit"))
			b.WriteString(failedStyle.Render("failed to start"))
			b.WriteString("\n")
			if job.ErrorMessage != "" {
				b.WriteString(labelStyle.Render("Error"))
				b.WriteString(errorStyle.Render(job.ErrorMessage))
				b.WriteString("\n")
			}
		}

		// Process stats for running jobs
		if job.Status == db.StatusRunning && m.processStats != nil && m.processStatsJobID == job.ID {
			b.WriteString("\n")
			b.WriteString(sectionStyle.Render("Process Stats"))
			b.WriteString("\n")

			// CPU: show % if available, plus user/sys time
			if m.processStats.CPUUser != "" || m.processStats.CPUSys != "" {
				b.WriteString(labelStyle.Render("  CPU"))
				cpuVal := ""
				if m.processStats.CPUPct > 0 {
					cpuVal += fmt.Sprintf("%.0f%% ", m.processStats.CPUPct)
				}
				cpuVal += fmt.Sprintf("(%s user, %s sys)", m.processStats.CPUUser, m.processStats.CPUSys)
				b.WriteString(valueStyle.Render(cpuVal))
				b.WriteString("\n")
			}

			// Memory
			if m.processStats.MemoryRSS != "" {
				b.WriteString(labelStyle.Render("  Memory"))
				mem := m.processStats.MemoryRSS
				if m.processStats.MemoryPct != "" {
					mem += " (" + m.processStats.MemoryPct + ")"
				}
				b.WriteString(valueStyle.Render(mem))
				b.WriteString("\n")
			}

			// Threads
			if m.processStats.Threads > 0 {
				b.WriteString(labelStyle.Render("  Threads"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%d", m.processStats.Threads)))
				b.WriteString("\n")
			}

			// GPUs with utilization and memory
			if len(m.processStats.GPUs) > 0 {
				for _, gpu := range m.processStats.GPUs {
					b.WriteString(labelStyle.Render(fmt.Sprintf("  GPU %d", gpu.Index)))
					gpuVal := ""
					if gpu.Utilization > 0 {
						gpuVal += fmt.Sprintf("%d%% util, ", gpu.Utilization)
					}
					gpuVal += gpu.MemUsed
					b.WriteString(valueStyle.Render(gpuVal))
					b.WriteString("\n")
				}
			}
		}

		// Progress section for running jobs
		if job.Status == db.StatusRunning {
			if prog, ok := m.jobProgress[job.ID]; ok {
				b.WriteString("\n")
				b.WriteString(sectionStyle.Render("Progress"))
				b.WriteString("\n")

				// Render progress bar
				pct := prog.DisplayPercent()
				if pct >= 0 {
					b.WriteString("  ")
					b.WriteString(renderProgressBar(pct, 20))
					b.WriteString(fmt.Sprintf(" %d%%", pct))
					b.WriteString("\n")
				}

				// Show step info for N/M format
				if prog.Total > 0 {
					b.WriteString(labelStyle.Render("  Step"))
					b.WriteString(valueStyle.Render(fmt.Sprintf("%d of %d", prog.Current, prog.Total)))
					b.WriteString("\n")
				}
			}
		}
	}

	panelContent := m.renderTabHeader() + "\n"
	panelContent += b.String()
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

// renderProgressBar renders a progress bar with the given percentage and width
func renderProgressBar(percent int, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := (percent * width) / 100
	empty := width - filled

	bar := progressBarFilledStyle.Render(strings.Repeat("█", filled))
	bar += progressBarEmptyStyle.Render(strings.Repeat("░", empty))
	return bar
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

// wrapTextWithIndent wraps text to the given width, indenting continuation lines.
// It handles both explicit newlines in the text and wrapping at word boundaries.
// Long tokens that exceed the width are broken at the width boundary.
func wrapTextWithIndent(text string, width, indent int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	indentStr := strings.Repeat(" ", indent)

	// Split by explicit newlines first
	lines := strings.Split(text, "\n")
	for li, line := range lines {
		if li > 0 {
			result.WriteString("\n")
			result.WriteString(indentStr)
		}

		// Wrap this line
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}

		lineLen := 0
		for i, word := range words {
			wordLen := len(word)

			if i == 0 && li == 0 {
				// First word of first line (no indent needed)
				if wordLen > width {
					// Break long word
					for len(word) > 0 {
						take := width - lineLen
						if take <= 0 {
							result.WriteString("\n")
							result.WriteString(indentStr)
							lineLen = 0
							take = width
						}
						if take > len(word) {
							take = len(word)
						}
						result.WriteString(word[:take])
						word = word[take:]
						lineLen += take
					}
				} else {
					result.WriteString(word)
					lineLen = wordLen
				}
			} else if lineLen+1+wordLen <= width {
				// Word fits on current line
				if lineLen > 0 {
					result.WriteString(" ")
					lineLen++
				}
				result.WriteString(word)
				lineLen += wordLen
			} else if wordLen > width {
				// Word is longer than available width - break it
				if lineLen > 0 {
					result.WriteString("\n")
					result.WriteString(indentStr)
					lineLen = 0
				}
				for len(word) > 0 {
					take := width
					if take > len(word) {
						take = len(word)
					}
					result.WriteString(word[:take])
					word = word[take:]
					lineLen = take
					if len(word) > 0 {
						result.WriteString("\n")
						result.WriteString(indentStr)
						lineLen = 0
					}
				}
			} else {
				// Need to wrap - word fits on next line
				result.WriteString("\n")
				result.WriteString(indentStr)
				result.WriteString(word)
				lineLen = wordLen
			}
		}
	}

	return result.String()
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
	help := helpStyle.Render("?:help q:quit ↑/↓:nav ←/→:views l:logs f:filter o:sort s:sync n:new e:edit r:restart k:kill P:prune")

	if m.syncing {
		help = syncingStyle.Render(m.spinner.View()+" ") + help
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
	header := fmt.Sprintf(" %-12s %-10s %-16s %-6s %-5s %-5s",
		"HOST", "STATUS", "ARCH", "QUEUE", "CPU", "RAM")
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
			queue := m.queueSummaryForHost(host)
			arch := truncate(host.Arch, 16)
			if arch == "" {
				arch = "-"
			}
			cpu := host.CPUUtilization()
			ram := host.RAMUtilization()

			// Style CPU/RAM in red if >90%
			cpuPct := host.CPUUtilizationPct()
			ramPct := host.RAMUtilizationPct()
			if cpuPct > 90 {
				cpu = failedStyle.Render(cpu)
			}
			if ramPct > 90 {
				ram = failedStyle.Render(ram)
			}

			line := fmt.Sprintf(" %-12s %-10s %-16s %-6s %-5s %-5s",
				truncate(host.Name, 12), status, arch, queue, cpu, ram)

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

		hostLine := fmt.Sprintf("Host: %s (%s)", host.Name, host.StatusString())
		if host.Status == HostStatusOffline && !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck).Truncate(time.Second)
			hostLine += fmt.Sprintf(" for %s", formatDuration(elapsed))
		}
		if host.Error != "" {
			hostLine += fmt.Sprintf(" - %s", host.Error)
		}
		lines = append(lines, hostLine)

		// Show static info (cached) regardless of online status
		hasStaticInfo := host.Model != "" || host.Arch != "" || host.OS != "" || host.CPUModel != "" || host.CPUs > 0 || len(host.GPUs) > 0
		if hasStaticInfo {
			lines = append(lines, "───────────────────────────────────────────────────────────────")
			// Helper to format label:value with bold label (14 chars for "Architecture:" + space)
			fmtLine := func(label, value string) string {
				return labelStyle.Render(fmt.Sprintf("%-14s", label)) + value
			}
			if host.Model != "" {
				lines = append(lines, fmtLine("Model:", host.Model))
			}
			if host.Arch != "" {
				lines = append(lines, fmtLine("Architecture:", host.Arch))
			}
			if host.OS != "" {
				lines = append(lines, fmtLine("OS Version:", host.OS))
			}
			if host.CPUModel != "" {
				lines = append(lines, fmtLine("CPU:", host.CPUModel))
			}
			if host.CPUs > 0 {
				lines = append(lines, fmtLine("CPU Cores:", fmt.Sprintf("%d", host.CPUs)))
			}

			// Memory (before GPUs, with CPU info)
			if host.MemTotal != "" {
				memInfo := host.MemTotal
				if host.MemUsed != "" {
					// Calculate utilization percentage
					usedMiB := parseMiB(host.MemUsed)
					totalMiB := parseMiB(host.MemTotal)
					if totalMiB > 0 {
						pct := (usedMiB * 100) / totalMiB
						pctStr := fmt.Sprintf("(%d%%)", pct)
						if pct > 90 {
							pctStr = failedStyle.Render(pctStr)
						}
						memInfo = fmt.Sprintf("%s used / %s total %s", host.MemUsed, host.MemTotal, pctStr)
					} else {
						memInfo = fmt.Sprintf("%s used / %s total", host.MemUsed, host.MemTotal)
					}
				}
				lines = append(lines, fmtLine("Memory:", memInfo))
			}

			// Load average (1m, 5m, 15m values)
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
						pctStr := fmt.Sprintf("[%d%% of %d cores]", pct, host.CPUs)
						if pct > 90 {
							pctStr = failedStyle.Render(pctStr)
						}
						lines = append(lines, fmtLine("Load:", fmt.Sprintf("%s, %s, %s  %s", load1m, load5m, load15m, pctStr)))
					} else {
						lines = append(lines, fmtLine("Load:", fmt.Sprintf("%s, %s, %s", load1m, load5m, load15m)))
					}
				} else {
					lines = append(lines, fmtLine("Load:", host.LoadAvg))
				}
			}

			// GPUs (just summary, full stats are in GPUs tab)
			if len(host.GPUs) > 0 {
				gpuNames := make(map[string]int)
				for _, gpu := range host.GPUs {
					gpuNames[gpu.Name]++
				}
				if len(gpuNames) == 1 {
					for name, count := range gpuNames {
						lines = append(lines, fmtLine("GPUs:", fmt.Sprintf("%d× %s", count, name)))
					}
				} else {
					lines = append(lines, fmtLine("GPUs:", fmt.Sprintf("%d", len(host.GPUs))))
				}
			}

			// Job counts for this host
			var runningCount, queuedCount, recentFailedCount int
			oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
			for _, job := range m.allJobs {
				if job.Host != host.Name {
					continue
				}
				switch job.Status {
				case db.StatusRunning, db.StatusStarting:
					runningCount++
				case db.StatusQueued:
					queuedCount++
				case db.StatusDead, db.StatusFailed:
					// Count as recent if ended within the last hour
					if job.EndTime != nil && *job.EndTime > oneHourAgo {
						recentFailedCount++
					}
				case db.StatusCompleted:
					// Count failed completions (non-zero exit) as recent failures
					if job.ExitCode != nil && *job.ExitCode != 0 && job.EndTime != nil && *job.EndTime > oneHourAgo {
						recentFailedCount++
					}
				}
			}
			lines = append(lines, "")
			lines = append(lines, "Jobs")
			lines = append(lines, fmt.Sprintf("  Running: %d", runningCount))
			lines = append(lines, fmt.Sprintf("  Queued:  %d", queuedCount))
			if recentFailedCount > 0 {
				lines = append(lines, fmt.Sprintf("  Failed:  %d (last hour)", recentFailedCount))
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
				queuedCount, _ := db.CountQueuedByHost(m.database, host.Name)
				lines = append(lines, fmt.Sprintf("  Jobs waiting: %d", queuedCount))
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
	panelContent := m.renderHostTabHeader() + "\n" + content
	if footerText != "" {
		panelContent = panelContent + "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(footerText)
	}

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

// renderHostTabHeader renders the tab header for the host detail panel
func (m Model) renderHostTabHeader() string {
	var tabs []string
	gpuIndices := m.getHostGPUIndices()

	// Info tab
	infoLabel := "Info"
	if m.hostDetailTab == HostDetailTabInfo {
		tabs = append(tabs, activeTabStyle.Render(infoLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(infoLabel))
	}

	// GPU Summary tab
	summaryLabel := "GPUs"
	if m.hostDetailTab == HostDetailTabGPUSummary {
		tabs = append(tabs, activeTabStyle.Render(summaryLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(summaryLabel))
	}

	// Count jobs per GPU for styling
	gpuJobStatus := m.getGPUJobStatus()

	// Individual GPU tabs (position in array, not actual GPU index)
	for pos, gpuIdx := range gpuIndices {
		label := fmt.Sprintf("%d", gpuIdx)
		gpuTab := HostDetailTabGPUBase + HostDetailTab(pos)
		if m.hostDetailTab == gpuTab {
			tabs = append(tabs, activeTabStyle.Render(label))
		} else {
			// Style based on job status
			status := gpuJobStatus[gpuIdx]
			switch {
			case status.running > 0:
				tabs = append(tabs, gpuTabRunningStyle.Render(label))
			case status.queued > 0:
				tabs = append(tabs, gpuTabQueuedStyle.Render(label))
			default:
				tabs = append(tabs, gpuTabEmptyStyle.Render(label))
			}
		}
	}

	hint := dimStyle.Render(" (Tab)")
	return strings.Join(tabs, " ") + hint
}

// getHostGPUIndices returns the sorted list of GPU indices for the selected host
// This includes both hardware GPUs and GPUs referenced by jobs
func (m Model) getHostGPUIndices() []int {
	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return nil
	}
	host := m.hosts[m.selectedHostIdx]

	knownGPUs := make(map[int]bool)

	// Add hardware GPUs
	for _, gpu := range host.GPUs {
		knownGPUs[gpu.Index] = true
	}

	// Add GPUs from jobs
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusQueued {
			continue
		}
		for _, idx := range parseGPUIndices(job.GetGPU()) {
			knownGPUs[idx] = true
		}
	}

	var indices []int
	for idx := range knownGPUs {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}

// gpuJobCounts holds running and queued job counts for a GPU
type gpuJobCounts struct {
	running int
	queued  int
}

// getGPUJobStatus returns job counts per GPU for the selected host
func (m Model) getGPUJobStatus() map[int]gpuJobCounts {
	result := make(map[int]gpuJobCounts)

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return result
	}
	host := m.hosts[m.selectedHostIdx]

	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting
		for _, idx := range parseGPUIndices(job.GetGPU()) {
			counts := result[idx]
			if isRunning {
				counts.running++
			} else {
				counts.queued++
			}
			result[idx] = counts
		}
	}

	return result
}

// parseGPUIndices parses a CUDA_VISIBLE_DEVICES value into GPU indices
func parseGPUIndices(gpuStr string) []int {
	if gpuStr == "" {
		return nil
	}
	parts := strings.Split(gpuStr, ",")
	var indices []int
	for _, p := range parts {
		if idx, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			indices = append(indices, idx)
		}
	}
	return indices
}

// renderGPUSummary renders a compact summary of all GPUs with job counts
func (m Model) renderGPUSummary(height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]

	// Build job counts per GPU
	type gpuCounts struct {
		running int
		queued  int
	}
	gpuJobCounts := make(map[int]*gpuCounts)
	var noGPURunning, noGPUQueued int

	// Initialize with hardware GPUs
	for _, gpu := range host.GPUs {
		gpuJobCounts[gpu.Index] = &gpuCounts{}
	}

	// Count jobs per GPU
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting
		gpuIndices := parseGPUIndices(job.GetGPU())

		if len(gpuIndices) == 0 {
			if isRunning {
				noGPURunning++
			} else {
				noGPUQueued++
			}
		} else {
			for _, idx := range gpuIndices {
				if gpuJobCounts[idx] == nil {
					gpuJobCounts[idx] = &gpuCounts{}
				}
				if isRunning {
					gpuJobCounts[idx].running++
				} else {
					gpuJobCounts[idx].queued++
				}
			}
		}
	}

	// Get sorted GPU indices
	var gpuIndices []int
	for idx := range gpuJobCounts {
		gpuIndices = append(gpuIndices, idx)
	}
	sort.Ints(gpuIndices)

	// Check if we have stats to show
	hasStats := false
	if host.Status == HostStatusOnline {
		for _, gpu := range host.GPUs {
			if gpu.Temperature > 0 || gpu.Utilization > 0 || gpu.MemUsed != "" {
				hasStats = true
				break
			}
		}
	}

	lines = append(lines, fmt.Sprintf("Jobs on %s:", host.Name))
	lines = append(lines, "")

	if hasStats {
		// Combined table with job counts and GPU stats
		lines = append(lines, "GPU  Run  Queue  Temp  Util  Memory            Name")
		lines = append(lines, "───  ───  ─────  ────  ────  ────────────────  ────")

		for _, gpuIdx := range gpuIndices {
			counts := gpuJobCounts[gpuIdx]
			var gpu *GPUInfo
			for i := range host.GPUs {
				if host.GPUs[i].Index == gpuIdx {
					gpu = &host.GPUs[i]
					break
				}
			}

			temp := "  -"
			util := "  -"
			mem := "-"
			gpuName := ""

			if gpu != nil {
				gpuName = truncate(gpu.Name, 18)
				if gpu.Temperature > 0 {
					temp = fmt.Sprintf("%3d°C", gpu.Temperature)
				}
				if gpu.Utilization > 0 || gpu.MemUsed != "" {
					util = fmt.Sprintf("%3d%%", gpu.Utilization)
				}
				if gpu.MemUsed != "" && gpu.MemTotal != "" {
					usedMiB := parseMiB(gpu.MemUsed)
					totalMiB := parseMiB(gpu.MemTotal)
					if totalMiB > 0 {
						pct := (usedMiB * 100) / totalMiB
						mem = fmt.Sprintf("%s/%s(%d%%)", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal), pct)
					} else {
						mem = fmt.Sprintf("%s/%s", formatGPUMem(gpu.MemUsed), formatGPUMem(gpu.MemTotal))
					}
				}
			}
			lines = append(lines, fmt.Sprintf("%3d  %3d  %5d  %5s %5s  %-16s  %s",
				gpuIdx, counts.running, counts.queued, temp, util, mem, gpuName))
		}
	} else {
		// Simple table without stats (host offline or no stats available)
		lines = append(lines, "GPU  Running  Queued  Name")
		lines = append(lines, "───  ───────  ──────  ────")

		for _, gpuIdx := range gpuIndices {
			counts := gpuJobCounts[gpuIdx]
			gpuName := ""
			for _, gpu := range host.GPUs {
				if gpu.Index == gpuIdx {
					gpuName = truncate(gpu.Name, 30)
					break
				}
			}
			lines = append(lines, fmt.Sprintf("%3d  %7d  %6d  %s", gpuIdx, counts.running, counts.queued, gpuName))
		}
	}

	// No GPU section
	if noGPURunning > 0 || noGPUQueued > 0 {
		if hasStats {
			lines = append(lines, fmt.Sprintf("  —  %3d  %5d                              (no GPU specified)", noGPURunning, noGPUQueued))
		} else {
			lines = append(lines, fmt.Sprintf("  —  %7d  %6d  (no GPU specified)", noGPURunning, noGPUQueued))
		}
	}

	return strings.Join(lines, "\n")
}

// renderGPUDetailTab renders detailed job list for a specific GPU
func (m Model) renderGPUDetailTab(gpuIdx int, height int) string {
	var lines []string

	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]

	// Get GPU name
	gpuName := ""
	for _, gpu := range host.GPUs {
		if gpu.Index == gpuIdx {
			gpuName = gpu.Name
			break
		}
	}

	if gpuName != "" {
		lines = append(lines, fmt.Sprintf("GPU %d: %s", gpuIdx, gpuName))
	} else {
		lines = append(lines, fmt.Sprintf("GPU %d", gpuIdx))
	}
	lines = append(lines, "")

	// Collect jobs for this GPU
	var running, queued []*db.Job
	for _, job := range m.allJobs {
		if job.Host != host.Name {
			continue
		}
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusQueued {
			continue
		}

		jobGPUs := parseGPUIndices(job.GetGPU())
		for _, idx := range jobGPUs {
			if idx == gpuIdx {
				if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
					running = append(running, job)
				} else {
					queued = append(queued, job)
				}
				break
			}
		}
	}

	// Running jobs
	lines = append(lines, fmt.Sprintf("Running (%d):", len(running)))
	if len(running) == 0 {
		lines = append(lines, "  (none)")
	} else {
		for _, job := range running {
			desc := job.Description
			if desc == "" {
				desc = truncate(job.EffectiveCommand(), 50)
			}
			lines = append(lines, fmt.Sprintf("  #%-4d %s", job.ID, desc))
		}
	}

	lines = append(lines, "")

	// Queued jobs
	lines = append(lines, fmt.Sprintf("Queued (%d):", len(queued)))
	if len(queued) == 0 {
		lines = append(lines, "  (none)")
	} else {
		for _, job := range queued {
			desc := job.Description
			if desc == "" {
				desc = truncate(job.EffectiveCommand(), 50)
			}
			lines = append(lines, fmt.Sprintf("  #%-4d %s", job.ID, desc))
		}
	}

	return strings.Join(lines, "\n")
}

// renderHostDetailPanel renders the host detail panel with tabs
func (m Model) renderHostDetailPanel(height int) string {
	header := m.renderHostTabHeader()

	var content string
	switch {
	case m.hostDetailTab == HostDetailTabInfo:
		// Use the existing renderHostDetail
		return m.renderHostDetail(height)
	case m.hostDetailTab == HostDetailTabGPUSummary:
		content = m.renderGPUSummary(height - 3)
	case m.hostDetailTab.IsGPUDetailTab():
		// Convert position to actual GPU index
		gpuIndices := m.getHostGPUIndices()
		pos := m.hostDetailTab.GPUPosition()
		if pos >= 0 && pos < len(gpuIndices) {
			content = m.renderGPUDetailTab(gpuIndices[pos], height-3)
		} else {
			content = dimStyle.Render("Invalid GPU tab")
		}
	default:
		return m.renderHostDetail(height)
	}

	panelContent := header + "\n" + content
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderHostsStatusBar() string {
	help := helpStyle.Render("?:help q:quit ↑/↓:nav ←/→:jobs x:delete R:refresh")

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

// queueSummaryForHost returns a brief queue status string using database count
func (m Model) queueSummaryForHost(host *Host) string {
	switch host.QueueStatus {
	case QueueCheckUnknown, QueueCheckChecking:
		return "-"
	case QueueCheckChecked:
		// Get count from database instead of remote file
		count, err := db.CountQueuedByHost(m.database, host.Name)
		if err != nil {
			count = 0
		}
		if !host.QueueRunnerActive {
			if count > 0 {
				return fmt.Sprintf("○ %d", count) // Queue stopped but has jobs
			}
			return "○" // Queue stopped, no jobs
		}
		if host.QueueStopPending {
			return fmt.Sprintf("■ %d", count)
		}
		return fmt.Sprintf("▶ %d", count)
	default:
		return "-"
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
	// Check if job has pending deferred operations
	hasPendingOps := m.pendingOpsJobIDs[job.ID]
	depSpec := m.jobDependencies[job.ID]

	switch job.Status {
	case db.StatusRunning:
		// Check if status is stale (host not checked in over 1 minute)
		if m.isJobStatusStale(job) {
			return "● running?"
		}
		// Show progress percentage if available
		if prog, ok := m.jobProgress[job.ID]; ok {
			pct := prog.DisplayPercent()
			if pct >= 0 {
				return fmt.Sprintf("● %3d%%", pct)
			}
		}
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
	case db.StatusDraft:
		return "○ draft"
	case db.StatusQueued:
		if hasPendingOps {
			if depSpec != "" {
				// Show dependency - "930" means after success, "930+" means after any
				if strings.HasSuffix(depSpec, "+") {
					return fmt.Sprintf("◇ after %s", strings.TrimSuffix(depSpec, "+"))
				}
				return fmt.Sprintf("◇ after %s", depSpec)
			}
			return "◇ waiting" // Hollow diamond = waiting for sync (no dependency)
		}
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
	case db.StatusDraft:
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

// isJobStatusStale returns true if the job's host hasn't been checked recently
// This helps identify running jobs whose status might be outdated
func (m Model) isJobStatusStale(job *db.Job) bool {
	// Hosts with running jobs are refreshed every 30s, so 2 minutes gives margin
	const staleThreshold = 2 * time.Minute

	// Find the host for this job
	for _, host := range m.hosts {
		if host.Name == job.Host {
			// If host is offline or last check was more than threshold ago
			if host.Status == HostStatusOffline {
				return true
			}
			if !host.LastCheck.IsZero() && time.Since(host.LastCheck) > staleThreshold {
				return true
			}
			return false
		}
	}
	// Host not found in our list - consider it stale
	return true
}

// isHostDisconnectedLong returns true if the job's host has been disconnected
// for more than 30 minutes. Used to dim jobs on unreachable hosts.
func (m Model) isHostDisconnectedLong(job *db.Job) bool {
	const disconnectedThreshold = 30 * time.Minute

	for _, host := range m.hosts {
		if host.Name == job.Host {
			// If host is online, it's not disconnected
			if host.Status == HostStatusOnline {
				return false
			}
			// For any non-online status (offline, unknown, checking),
			// check if LastCheck is older than threshold
			if !host.LastCheck.IsZero() {
				return time.Since(host.LastCheck) > disconnectedThreshold
			}
			return false
		}
	}
	return false
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
		if err != nil {
			return jobsRefreshedMsg{err: err}
		}
		pendingOps, _ := db.GetJobIDsWithPendingOperations(m.database)
		deps, _ := db.GetJobDependencyInfo(m.database)
		return jobsRefreshedMsg{jobs: jobs, pendingOpsJobIDs: pendingOps, jobDependencies: deps}
	}
}

func (m *Model) applyJobFilter() {
	prevSelectedID := int64(0)
	selectedIdx := m.jobList.Index()
	if len(m.jobs) > 0 && selectedIdx >= 0 && selectedIdx < len(m.jobs) {
		prevSelectedID = m.jobs[selectedIdx].ID
	}

	var filtered []*db.Job
	for _, job := range m.allJobs {
		if jobMatchesFilter(job, m.jobFilter) {
			filtered = append(filtered, job)
		}
	}
	m.jobs = filtered

	// Apply sorting
	m.sortJobs()

	// Update the list items
	m.jobList.SetItems(JobsToListItems(m.jobs))

	// Try to restore selection to the same job
	if prevSelectedID != 0 {
		for i, job := range m.jobs {
			if job.ID == prevSelectedID {
				m.jobList.Select(i)
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

// sortJobs sorts m.jobs in place according to m.jobSort
func (m *Model) sortJobs() {
	switch m.jobSort {
	case jobSortNewest:
		// Most recent first (by start time, or created time for queued jobs)
		sort.Slice(m.jobs, func(i, j int) bool {
			return getJobSortTime(m.jobs[i]) > getJobSortTime(m.jobs[j])
		})
	case jobSortOldest:
		// Oldest first
		sort.Slice(m.jobs, func(i, j int) bool {
			return getJobSortTime(m.jobs[i]) < getJobSortTime(m.jobs[j])
		})
	case jobSortIDDesc:
		// Job ID descending (largest first)
		sort.Slice(m.jobs, func(i, j int) bool {
			return m.jobs[i].ID > m.jobs[j].ID
		})
	case jobSortIDAsc:
		// Job ID ascending (smallest first)
		sort.Slice(m.jobs, func(i, j int) bool {
			return m.jobs[i].ID < m.jobs[j].ID
		})
	case jobSortQueueOrder:
		// Queued jobs first in queue order, then running, then completed
		sort.Slice(m.jobs, func(i, j int) bool {
			return jobQueueOrderLess(m.jobs[i], m.jobs[j])
		})
	}
}

// getJobSortTime returns the time to use for sorting (start time or queue time)
func getJobSortTime(job *db.Job) int64 {
	if job.StartTime > 0 {
		return job.StartTime
	}
	// For queued jobs, use ID as a proxy for queue time (higher ID = more recent)
	return job.ID
}

// jobQueueOrderLess returns true if job i should come before job j in queue order
func jobQueueOrderLess(i, j *db.Job) bool {
	// Status priority: queued first, then running/starting, then completed/dead/failed
	statusPriority := func(job *db.Job) int {
		switch job.Status {
		case db.StatusQueued:
			return 0
		case db.StatusRunning, db.StatusStarting:
			return 1
		default:
			return 2
		}
	}

	pi, pj := statusPriority(i), statusPriority(j)
	if pi != pj {
		return pi < pj
	}

	// Within same status group:
	// - Queued: by queue position (lower ID typically means queued earlier, but could be reordered)
	// - Running: by start time (oldest first = running longest)
	// - Completed: by end time (most recent first)
	if i.Status == db.StatusQueued {
		// For queued jobs, use ID as proxy for queue position (lower = earlier)
		return i.ID < j.ID
	} else if i.Status == db.StatusRunning || i.Status == db.StatusStarting {
		// Running jobs: show oldest (running longest) first
		return i.StartTime < j.StartTime
	} else {
		// Completed/failed/dead: most recently finished first
		ti, tj := int64(0), int64(0)
		if i.EndTime != nil {
			ti = *i.EndTime
		}
		if j.EndTime != nil {
			tj = *j.EndTime
		}
		return ti > tj
	}
}

// cycleTab handles Tab/Shift+Tab navigation for both Jobs and Hosts views
func (m *Model) cycleTab(forward bool) (Model, tea.Cmd) {
	if m.viewMode == ViewModeJobs {
		return m.cycleJobsTab(forward)
	}
	return m.cycleHostsTab(forward)
}

// cycleJobsTab toggles between Details and Logs tabs in Jobs view
func (m *Model) cycleJobsTab(forward bool) (Model, tea.Cmd) {
	// Toggle between Details and Logs (same in both directions for 2 tabs)
	if m.detailTab == DetailTabDetails {
		return m.switchToLogsTab()
	}
	m.detailTab = DetailTabDetails
	return *m, nil
}

// switchToLogsTab switches to the Logs tab and fetches log content
func (m *Model) switchToLogsTab() (Model, tea.Cmd) {
	m.detailTab = DetailTabLogs
	idx := m.jobList.Index()
	if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
		m.selectedJob = m.jobs[idx]
		m.logLoading = true
		// Show cached content immediately while fetching fresh logs
		if cached, ok := m.logCache[m.selectedJob.ID]; ok {
			m.logContent = cached
			m.logStale = true
			m.logViewport.SetContent(m.logContent)
		} else {
			m.logContent = ""
			m.logStale = false
		}
		var cmds []tea.Cmd
		cmds = append(cmds, m.fetchSelectedJobLog())
		if m.selectedJob.Status == db.StatusRunning {
			cmds = append(cmds, m.fetchProcessStats(m.selectedJob))
		}
		return *m, tea.Batch(cmds...)
	}
	return *m, nil
}

// cycleHostsTab cycles through host detail tabs (Info, GPUs, GPU 0, GPU 1, ...)
func (m *Model) cycleHostsTab(forward bool) (Model, tea.Cmd) {
	gpuIndices := m.getHostGPUIndices()
	maxTab := HostDetailTabGPUBase + HostDetailTab(len(gpuIndices)-1)
	if len(gpuIndices) == 0 {
		maxTab = HostDetailTabGPUSummary
	}

	if forward {
		if m.hostDetailTab < maxTab {
			m.hostDetailTab++
		} else {
			m.hostDetailTab = HostDetailTabInfo
		}
	} else {
		if m.hostDetailTab > HostDetailTabInfo {
			m.hostDetailTab--
		} else {
			m.hostDetailTab = maxTab
		}
	}
	return *m, nil
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

		// Convert to sorted slice using natural ordering
		var hosts []string
		for h := range hostSet {
			hosts = append(hosts, h)
		}
		naturalSortStrings(hosts)

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
				DeclaredGPU: job.GetGPU(),
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
	idx := m.jobList.Index()
	if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
		return m.jobs[idx]
	}
	return nil
}

// handleSelectionChanged is called when the job list selection changes.
// It clears cached process stats and fetches logs/stats for the new selection.
func (m *Model) handleSelectionChanged() tea.Cmd {
	// Clear cached process stats when changing jobs
	m.processStats = nil
	m.prevProcessStats = nil
	m.processStatsJobID = 0

	idx := m.jobList.Index()
	if len(m.jobs) == 0 || idx < 0 || idx >= len(m.jobs) {
		return nil
	}

	job := m.jobs[idx]

	// If in Logs tab, fetch logs for new job
	if m.detailTab == DetailTabLogs {
		m.selectedJob = job
		m.logLoading = true
		// Show cached content immediately while fetching fresh logs
		if cached, ok := m.logCache[job.ID]; ok {
			m.logContent = cached
			m.logStale = true
			m.logViewport.SetContent(m.logContent)
		} else {
			m.logContent = ""
			m.logStale = false
		}
		var cmds []tea.Cmd
		cmds = append(cmds, m.fetchSelectedJobLog())
		if job.Status == db.StatusRunning {
			cmds = append(cmds, m.fetchProcessStats(job))
		}
		return tea.Batch(cmds...)
	}

	// Even if not in Logs tab, fetch stats for running jobs
	if job.Status == db.StatusRunning {
		return m.fetchProcessStats(job)
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
		return "Active (waiting/queued/running)"
	case jobFilterSucceeded:
		return "Succeeded"
	case jobFilterFailed:
		return "Failed"
	default:
		return "All"
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

		// Extract progress information from log content
		prog := progress.FindLastProgress(stdout)

		return logFetchedMsg{
			jobID:    job.ID,
			content:  stdout,
			progress: prog,
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

			syncOpts := ops.DefaultSyncOptions()
			for _, job := range jobs {
				changed, err := ops.SyncJobQuick(m.database, job, syncOpts)
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
			syncOpts := ops.DefaultSyncOptions()
			for _, job := range queuedJobs {
				changed, err := ops.SyncJobQuick(m.database, job, syncOpts)
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

		// Kill tombstoned jobs that are still marked as running/queued on remote hosts
		tombstonedJobs, err := db.GetTombstonedActiveJobs(m.database)
		if err == nil {
			for _, job := range tombstonedJobs {
				killed := killTombstonedJob(m.database, job)
				if killed {
					updated++
				}
			}
		}

		// Start queue runners on hosts with queued jobs
		var queuesStarted []string
		hostsWithQueued, err := db.ListHostsWithQueuedJobs(m.database)
		if err == nil {
			for _, host := range hostsWithQueued {
				started, err := ensureQueueRunnerStartedTUI(host)
				if err == nil && started {
					queuesStarted = append(queuesStarted, host)
				}
			}
		}

		return syncCompletedMsg{updated: updated, queuesStarted: queuesStarted}
	}
}

func (m Model) killJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	database := m.database
	return func() tea.Msg {
		result, err := ops.KillJob(database, job, ops.DefaultOptions())
		if err != nil {
			return jobKilledMsg{jobID: job.ID, err: err}
		}
		if result.Deferred {
			// Job marked dead locally, kill will execute on next sync
			return jobKilledMsg{jobID: job.ID, err: nil, deferred: true}
		}
		return jobKilledMsg{jobID: job.ID, err: nil}
	}
}

func (m Model) cancelQueuedJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	database := m.database
	return func() tea.Msg {
		result, err := ops.CancelQueuedJob(database, job, ops.DefaultOptions())
		if err != nil {
			return jobKilledMsg{jobID: job.ID, err: err, cancelled: true}
		}
		if result.Deferred {
			// Job marked dead locally, removal will execute on next sync
			return jobKilledMsg{jobID: job.ID, err: nil, deferred: true, cancelled: true}
		}
		return jobKilledMsg{jobID: job.ID, err: nil, cancelled: true}
	}
}

func (m Model) restartJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		// Try to read metadata from remote for more accurate info (best effort)
		metadataFile := session.JobMetadataFile(job.ID, job.StartTime, job.SessionName)
		content, _ := ssh.ReadRemoteFile(job.Host, metadataFile)

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

		// Kill existing session if running (best effort, ignore errors)
		oldTmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		exists, _ := ssh.TmuxSessionExistsQuick(job.Host, oldTmuxSession)
		if exists {
			ssh.TmuxKillSession(job.Host, oldTmuxSession)
		}

		// Use ops package to restart job (queues if host offline)
		result, err := ops.RestartJob(database, ops.RestartJobParams{
			OriginalJob: job,
			WorkingDir:  workingDir,
			Command:     command,
			Description: description,
		}, ops.DefaultOptions())

		if err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: err}
		}

		return jobRestartedMsg{
			oldJobID: job.ID,
			newJobID: result.JobID,
			deferred: result.Deferred,
		}
	}
}

// startQueuedJobNow starts a queued job immediately, bypassing any dependencies
func (m Model) startQueuedJobNow(job *db.Job) tea.Cmd {
	if job == nil || job.Status != db.StatusQueued {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		deferred, err := queuejob.StartNow(database, job)
		if err != nil {
			return jobStartedNowMsg{jobID: job.ID, host: job.Host, err: err}
		}
		return jobStartedNowMsg{jobID: job.ID, host: job.Host, deferred: deferred}
	}
}

// moveJobToFront moves a queued job to the front of its queue
func (m Model) moveJobToFront(job *db.Job) tea.Cmd {
	if job == nil || job.Status != db.StatusQueued {
		return nil
	}
	return func() tea.Msg {
		moved, err := queuefile.MoveToFront(job.Host, job.QueueName, job.ID)
		return jobMovedToFrontMsg{jobID: job.ID, moved: moved, err: err}
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

// killTombstonedJob kills a job that was tombstoned locally but may still be running remotely
// Returns true if the job was killed, false if host unreachable or already dead
func killTombstonedJob(database *sql.DB, job *db.Job) bool {
	// For queued jobs, remove from queue file
	if job.Status == db.StatusQueued {
		queueName := job.QueueName
		if queueName == "" {
			queueName = "default"
		}
		queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
		removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", job.ID, queueFile)
		_, _, err := ssh.RunWithTimeout(job.Host, removeCmd, 5*time.Second)
		if err != nil {
			return false // Host unreachable
		}
		// Update status to dead
		db.MarkDeadByID(database, job.ID)
		return true
	}

	// For running/starting jobs, kill the process
	if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
		var killed bool

		if job.SessionName == "" {
			// Queue runner job - kill via PID
			pidPattern := session.PidFilePattern(job.ID)
			killCmd := fmt.Sprintf(`
				pid=$(cat %s 2>/dev/null | head -1)
				if [ -n "$pid" ] && kill -0 $pid 2>/dev/null; then
					kill $pid 2>/dev/null && echo "killed" || echo "failed"
				else
					echo "not_running"
				fi
			`, pidPattern)
			stdout, _, err := ssh.RunWithTimeout(job.Host, killCmd, 5*time.Second)
			if err != nil {
				return false // Host unreachable
			}
			killed = strings.TrimSpace(stdout) == "killed"
		} else {
			// Regular job - kill via tmux
			tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
			err := ssh.TmuxKillSession(job.Host, tmuxSession)
			killed = err == nil
		}

		if killed {
			db.MarkDeadByID(database, job.ID)
			return true
		}
	}

	return false
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
		runnerCmd := fmt.Sprintf("bash $HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", queueName)
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", runnerSession, ssh.EscapeForSingleQuotes(runnerCmd))

		if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
			return queueStartedMsg{host: host, err: fmt.Errorf("start queue runner: %s", stderr)}
		}

		return queueStartedMsg{host: host}
	}
}

// ensureQueueRunnerStartedTUI checks if queue runner is running and starts it if not.
// Returns (true, nil) if started, (false, nil) if already running, (false, err) on error.
func ensureQueueRunnerStartedTUI(host string) (bool, error) {
	queueName := "default"
	runnerSession := fmt.Sprintf("rj-queue-%s", queueName)

	// Check if queue runner is already running
	exists, err := ssh.TmuxSessionExists(host, runnerSession)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil // Already running
	}

	// Create directories on remote
	queueDir := "~/.cache/remote-jobs/queue"
	scriptsDir := "~/.cache/remote-jobs/scripts"
	mkdirCmd := fmt.Sprintf("mkdir -p %s %s", queueDir, scriptsDir)
	if _, _, err := ssh.Run(host, mkdirCmd); err != nil {
		return false, err
	}

	// Deploy queue runner script
	queueRunnerPath := "~/.cache/remote-jobs/scripts/queue-runner.sh"
	writeCmd := fmt.Sprintf("cat > %s << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", queueRunnerPath, string(scripts.QueueRunnerScript))
	if _, _, err := ssh.Run(host, writeCmd); err != nil {
		return false, err
	}

	// Make script executable
	chmodCmd := fmt.Sprintf("chmod +x %s", queueRunnerPath)
	if _, _, err := ssh.Run(host, chmodCmd); err != nil {
		return false, err
	}

	// Start queue runner in tmux
	runnerCmd := fmt.Sprintf("bash $HOME/.cache/remote-jobs/scripts/queue-runner.sh %s", queueName)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", runnerSession, ssh.EscapeForSingleQuotes(runnerCmd))

	if _, _, err := ssh.Run(host, tmuxCmd); err != nil {
		return false, err
	}

	return true, nil
}

func (m Model) removeJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		err := db.TombstoneJob(database, job.ID)
		return jobRemovedMsg{jobID: job.ID, err: err}
	}
}

func (m Model) deleteHost(hostName string) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		// Check for running or queued jobs on this host
		jobs, err := db.GetJobsByHost(database, hostName)
		if err != nil {
			return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("check jobs: %w", err)}
		}

		var activeJobs []int64
		var jobsToTombstone []int64
		for _, job := range jobs {
			if job.Status == db.StatusRunning || job.Status == db.StatusQueued || job.Status == db.StatusStarting {
				activeJobs = append(activeJobs, job.ID)
			} else {
				// Completed, failed, dead jobs can be tombstoned
				jobsToTombstone = append(jobsToTombstone, job.ID)
			}
		}

		if len(activeJobs) > 0 {
			return hostDeletedMsg{
				hostName: hostName,
				err:      fmt.Errorf("host has %d active jobs (IDs: %v)", len(activeJobs), activeJobs),
			}
		}

		// Tombstone (delete) inactive jobs for this host
		for _, jobID := range jobsToTombstone {
			if err := db.DeleteJob(database, jobID); err != nil {
				return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("tombstone job %d: %w", jobID, err)}
			}
		}

		// Delete the host from cache
		if err := db.DeleteCachedHost(database, hostName); err != nil {
			return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("delete host: %w", err)}
		}

		return hostDeletedMsg{hostName: hostName}
	}
}

func (m Model) createJob() tea.Cmd {
	database := m.database
	host := strings.TrimSpace(m.inputs[inputHost].Value())
	command := strings.TrimSpace(m.inputs[inputCommand].Value())
	description := strings.TrimSpace(m.inputs[inputDescription].Value())
	workingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())
	envVarsStr := strings.TrimSpace(m.inputs[inputEnvVars].Value())

	// Normalize command: extract cd/env prefixes into proper fields
	// This allows users to paste commands like "cd /foo && env CUDA=0 python train.py"
	normalizedDir, normalizedCmd, normalizedEnv := db.NormalizeCommand(command)
	if normalizedDir != "" && workingDir == "" {
		workingDir = normalizedDir
		command = normalizedCmd
	}

	if workingDir == "" {
		workingDir = "~"
	}

	// Parse env vars (comma-separated VAR=value pairs from form field)
	var envVars []string
	if envVarsStr != "" {
		for _, ev := range strings.Split(envVarsStr, ",") {
			ev = strings.TrimSpace(ev)
			if ev != "" {
				envVars = append(envVars, ev)
			}
		}
	}

	// Append env vars extracted from command (if we didn't use them above)
	if normalizedDir != "" {
		// We extracted cd, so also use the normalized env vars
		envVars = append(envVars, normalizedEnv...)
	} else if len(normalizedEnv) > 0 {
		// No cd prefix, but we have env vars in the command - use them
		command = normalizedCmd
		envVars = append(envVars, normalizedEnv...)
	}

	return func() tea.Msg {
		// Use ops package to run job (queues if host offline)
		result, err := ops.RunJob(database, ops.RunJobParams{
			Host:        host,
			WorkingDir:  workingDir,
			Command:     command,
			Description: description,
			EnvVars:     envVars,
		}, ops.DefaultOptions())

		if err != nil {
			return jobCreatedMsg{err: err}
		}

		return jobCreatedMsg{
			jobID:    result.JobID,
			deferred: result.Deferred,
		}
	}
}

func (m Model) editJob() tea.Cmd {
	database := m.database
	jobID := m.editingJobID
	newHost := strings.TrimSpace(m.inputs[inputHost].Value())
	newCommand := strings.TrimSpace(m.inputs[inputCommand].Value())
	newDescription := strings.TrimSpace(m.inputs[inputDescription].Value())
	newWorkingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())

	return func() tea.Msg {
		// Get the current job to check status and get original host
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("get job: %w", err)}
		}
		if job == nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("job %d not found", jobID)}
		}
		if job.Status != db.StatusQueued {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("can only edit queued jobs")}
		}

		// Check if host is being changed - not allowed in edit, use job move instead
		if newHost != job.Host {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("cannot change host via edit; use 'job move' command")}
		}

		// Update fields in database
		if newDescription != job.Description {
			if err := db.UpdateJobDescription(database, jobID, newDescription); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update description: %w", err)}
			}
		}

		if newWorkingDir != job.WorkingDir {
			if err := db.UpdateJobWorkingDir(database, jobID, newWorkingDir); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update directory: %w", err)}
			}
		}

		if newCommand != job.Command {
			if err := db.UpdateJobCommand(database, jobID, newCommand); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update command: %w", err)}
			}
		}

		// Update the remote queue file
		queueName := job.QueueName
		if queueName == "" {
			queueName = "default"
		}
		updatedJob := &db.Job{
			ID:          jobID,
			Host:        job.Host,
			Command:     newCommand,
			WorkingDir:  newWorkingDir,
			Description: newDescription,
		}
		if err := updateRemoteQueueEntry(job.Host, queueName, updatedJob); err != nil {
			// Non-fatal - the local database was updated
			// Log warning but don't fail the edit
		}

		return jobEditedMsg{jobID: jobID}
	}
}

// updateRemoteQueueEntry updates a job's entry in the remote queue file
func updateRemoteQueueEntry(host, queueName string, job *db.Job) error {
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)

	// Remove old entry and add new one
	removeCmd := fmt.Sprintf("sed -i '/^%d\\t/d' %s 2>/dev/null || true", job.ID, queueFile)
	if _, stderr, err := ssh.Run(host, removeCmd); err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("remove old entry: %s", strings.TrimSpace(stderr))
	}

	// Add updated entry
	queueLine := fmt.Sprintf("%d\t%s\t%s\t%s", job.ID, job.WorkingDir, job.Command, job.Description)
	addCmd := fmt.Sprintf("echo '%s' >> %s", ssh.EscapeForSingleQuotes(queueLine), queueFile)
	if _, stderr, err := ssh.Run(host, addCmd); err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("add updated entry: %s", strings.TrimSpace(stderr))
	}

	return nil
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

// formatJobTime formats the time column for a job, showing either start time or queue time
func formatJobTime(job *db.Job) string {
	// If job has started, show start time
	if job.StartTime != 0 {
		return formatStartTime(job.StartTime)
	}
	// For unstarted jobs (pending/queued), show when they were queued
	if job.CreatedAt != 0 {
		return "Q:" + formatQueueTime(job.CreatedAt)
	}
	return "—"
}

// formatQueueTime formats a queue time in a compact form
func formatQueueTime(createdAt int64) string {
	t := time.Unix(createdAt, 0)
	elapsed := time.Since(t)

	if elapsed < time.Minute {
		return "<1m"
	} else if elapsed < time.Hour {
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	} else if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
	days := int(elapsed.Hours() / 24)
	return fmt.Sprintf("%dd", days)
}

// naturalSortStrings sorts strings using macOS Finder-style natural ordering
// where numeric segments are compared as numbers (e.g., "cool30" < "cool100")
func naturalSortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool {
		return naturalLess(s[i], s[j])
	})
}

// naturalLess compares two strings using natural ordering
func naturalLess(a, b string) bool {
	aParts := splitIntoSegments(a)
	bParts := splitIntoSegments(b)

	// Compare segment by segment
	minLen := len(aParts)
	if len(bParts) < minLen {
		minLen = len(bParts)
	}

	for i := 0; i < minLen; i++ {
		aSeg := aParts[i]
		bSeg := bParts[i]

		// If both are numeric, compare as numbers
		aNum, aIsNum := parseNumber(aSeg)
		bNum, bIsNum := parseNumber(bSeg)

		if aIsNum && bIsNum {
			if aNum != bNum {
				return aNum < bNum
			}
			// Numbers are equal, continue to next segment
		} else {
			// Compare as strings (case-insensitive)
			aLower := strings.ToLower(aSeg)
			bLower := strings.ToLower(bSeg)
			if aLower != bLower {
				return aLower < bLower
			}
			// Case-insensitive equal, continue to next segment
		}
	}

	// If all compared segments are equal, shorter one comes first
	// If same length, use case-sensitive comparison as final tiebreaker
	if len(aParts) != len(bParts) {
		return len(aParts) < len(bParts)
	}
	return a < b
}

// splitIntoSegments splits a string into alternating alphabetic and numeric segments
func splitIntoSegments(s string) []string {
	var segments []string
	var current strings.Builder

	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')

		if current.Len() == 0 {
			current.WriteRune(r)
		} else {
			lastRune := []rune(current.String())[current.Len()-1]
			lastIsDigit := lastRune >= '0' && lastRune <= '9'
			lastIsAlpha := (lastRune >= 'a' && lastRune <= 'z') || (lastRune >= 'A' && lastRune <= 'Z')

			// Check if we're in the same segment type
			sameType := (isDigit && lastIsDigit) || (isAlpha && lastIsAlpha) || (!isDigit && !isAlpha && !lastIsDigit && !lastIsAlpha)

			if sameType {
				current.WriteRune(r)
			} else {
				segments = append(segments, current.String())
				current.Reset()
				current.WriteRune(r)
			}
		}
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}

// parseNumber attempts to parse a string as an integer
func parseNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}
