package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudproviders"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/core"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/llm"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ssh"
)

// Model is the main TUI state
type Model struct {
	// View mode
	viewMode ViewMode

	// Jobs data
	allJobs              []*db.Job
	jobs                 []*db.Job
	jobList              list.Model // bubbles/list for job selection
	selectedJob          *db.Job
	jobFilter            jobFilterMode
	jobHostFilterMode    hostFilterMode
	jobHostFilterHost    string
	jobSort              jobSortMode
	jobSelectionActive   bool // false when user has deselected the highlighted job
	jobListContentHeight int

	// Hosts data
	hosts           []*Host
	selectedHostIdx int
	hostDetailTab   HostDetailTab // Which tab is active in host detail panel (Info or GPU)

	// UI State
	detailTab               DetailTab // Which tab is active in detail panel (Details or Logs)
	logContent              string
	logStale                bool             // true if showing cached content due to connection error
	logCache                map[int64]string // cache of last successful log content per job
	logLoading              bool
	logViewport             viewport.Model
	detailViewport          *viewport.Model
	spinner                 spinner.Model // Loading spinner
	flash                   FlashState
	hostSummaryTickerOffset int
	daemonRestartInProgress bool

	// Process stats for running jobs
	processStats      *ssh.ProcessStats
	prevProcessStats  *ssh.ProcessStats // Previous sample for CPU% calculation
	processStatsJobID int64

	// Job CPU usage view (per-host snapshot)
	jobCPUTopEntries       []ssh.TopProcess
	jobCPUTopDataHost      string
	jobCPUTopRequestedHost string
	jobCPUTopLoading       bool
	jobCPUTopError         string
	jobCPUTopUpdated       time.Time

	// Host CPU usage view (hosts tab)
	hostCPUTopEntries       []ssh.TopProcess
	hostCPUTopDataHost      string
	hostCPUTopRequestedHost string
	hostCPUTopLoading       bool
	hostCPUTopError         string
	hostCPUTopUpdated       time.Time

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

	// Edit job mode (for queued or draft jobs)
	editMode          bool
	editingJobID      int64
	editingJobDepSpec string

	// Layout
	width  int
	height int

	// Database connection
	database                *sql.DB
	coreService             *core.Service
	monitor                 *monitor.Monitor
	monitorEvents           <-chan monitor.Event
	dbWatcher               *fsnotify.Watcher
	dbWatcherTargets        map[string]struct{}
	dbRefreshDebounceActive bool

	// Context for cancellation on quit
	ctx    context.Context
	cancel context.CancelFunc

	// Background sync worker
	syncWorker        *SyncWorker
	initialSyncNeeded bool // True until first priority sync after jobs load

	// Legacy sync state (being phased out)
	lastHostSyncTimes map[string]time.Time

	// Host sync tracking for job filtering
	hostSyncTimes map[string]time.Time

	// Help overlay
	showHelp bool

	// Cloud menu overlay
	showCloudMenu         bool
	cloudMenuJob          *db.Job
	cloudMenuOfferings    []placement.CloudOffering
	cloudMenuCursor       int
	cloudMenuLoading      bool
	cloudMenuConfirm      bool // true when showing cost confirmation
	cloudClients          []cloud.Client
	cloudClientErr        error
	cloudDiscoveryPending bool

	// Configurable intervals
	syncActiveInterval  time.Duration
	syncIdleInterval    time.Duration
	logRefreshInterval  time.Duration
	hostRefreshInterval time.Duration
	hostCacheDuration   time.Duration

	// Host cache tracking - which hosts have been freshly queried this session
	hostsQueriedThisSession map[string]bool

	// Host offline hysteresis: only mark offline after consecutive failures
	hostFailCount map[string]int
	focused       bool

	// Track hosts that have already shown low disk warning this session
	lowDiskWarnedHosts map[string]bool
	// Track hosts that have already shown queue runner stopped warning this session
	queueStoppedWarnedHosts map[string]bool

	// Job dependencies (jobID -> dep_spec like "930" or "930+")
	jobDependencies  map[int64]string
	externalBindings map[int64]*db.ExternalJobBinding

	// LLM description generator (nil if LLM backend not available or disabled)
	llmGenerator *llm.DescriptionGenerator

	// App configuration
	appConfig *config.Config

	// Host AI summaries
	showHostSummaries  bool                 // Toggle for showing AI summaries in hosts view
	hostSummaries      map[string]string    // host name -> AI-generated summary
	hostSummaryHashes  map[string]string    // host name -> hash of job states used for summary
	hostSummaryTimes   map[string]time.Time // host name -> last generation time
	hostSummaryPending map[string]bool      // host name -> currently generating

	cloudDiscoveryFn func(*config.Config) cloudproviders.Discovery
	llmInitFn        func(*sql.DB, *config.Config) *llm.DescriptionGenerator
}

// ModelOptions contains configuration for the TUI model
type ModelOptions struct {
	SyncActiveInterval  time.Duration
	SyncIdleInterval    time.Duration
	LogRefreshInterval  time.Duration
	HostRefreshInterval time.Duration
	HostCacheDuration   time.Duration // How long cached host info is considered fresh
	Monitor             *monitor.Monitor
	InitialSnapshot     *InitialSnapshot
	CloudDiscoveryFn    func(*config.Config) cloudproviders.Discovery
	LLMInitFn           func(*sql.DB, *config.Config) *llm.DescriptionGenerator
}

// DefaultModelOptions returns the default TUI options
func DefaultModelOptions() ModelOptions {
	return ModelOptions{
		SyncActiveInterval:  DefaultSyncActiveInterval,
		SyncIdleInterval:    DefaultSyncIdleInterval,
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
	inputs := make([]textinput.Model, 7)

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
	inputs[inputCommand].CharLimit = 1024

	inputs[inputWorkingDir] = textinput.New()
	inputs[inputWorkingDir].Placeholder = "(optional)"
	inputs[inputWorkingDir].Prompt = ""
	inputs[inputWorkingDir].Width = 40
	inputs[inputWorkingDir].CharLimit = 256

	inputs[inputGPU] = textinput.New()
	inputs[inputGPU].Placeholder = "(optional, e.g., 0 or 0,1)"
	inputs[inputGPU].Prompt = ""
	inputs[inputGPU].Width = 40
	inputs[inputGPU].CharLimit = 64

	inputs[inputCPUAllotment] = textinput.New()
	inputs[inputCPUAllotment].Placeholder = "default (1-100)"
	inputs[inputCPUAllotment].Prompt = ""
	inputs[inputCPUAllotment].Width = 40
	inputs[inputCPUAllotment].CharLimit = 8

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

	// Load app config
	appCfg, _ := config.Load()

	detailVP := viewport.New(0, 0)

	// Create context for cancellation on quit
	ctx, cancel := context.WithCancel(context.Background())
	cloudDiscoveryFn := opts.CloudDiscoveryFn
	if cloudDiscoveryFn == nil {
		cloudDiscoveryFn = cloudproviders.Discover
	}
	llmInitFn := opts.LLMInitFn
	if llmInitFn == nil {
		llmInitFn = defaultLLMInit
	}

	model := Model{
		database:    database,
		coreService: core.NewServiceWithDB(database),
		monitor:     opts.Monitor,
		ctx:         ctx,
		cancel:      cancel,
		monitorEvents: func() <-chan monitor.Event {
			if opts.Monitor != nil {
				return opts.Monitor.Events()
			}
			return nil
		}(),
		jobList:                 jobList,
		jobSelectionActive:      true,
		jobFilter:               jobFilterRecent,
		jobHostFilterMode:       hostFilterRecent,
		logViewport:             viewport.New(0, 0),
		detailViewport:          &detailVP,
		inputs:                  inputs,
		spinner:                 s,
		syncActiveInterval:      opts.SyncActiveInterval,
		syncIdleInterval:        opts.SyncIdleInterval,
		logRefreshInterval:      opts.LogRefreshInterval,
		hostRefreshInterval:     opts.HostRefreshInterval,
		hostCacheDuration:       opts.HostCacheDuration,
		hostsQueriedThisSession: make(map[string]bool),
		hostFailCount:           make(map[string]int),
		lowDiskWarnedHosts:      make(map[string]bool),
		queueStoppedWarnedHosts: make(map[string]bool),
		focused:                 true,
		logCache:                make(map[int64]string),
		jobDependencies:         make(map[int64]string),
		externalBindings:        make(map[int64]*db.ExternalJobBinding),
		progressTracker:         progress.NewTracker(),
		jobProgress:             make(map[int64]*progress.Progress),
		lastHostSyncTimes:       make(map[string]time.Time),
		hostSyncTimes:           make(map[string]time.Time),
		appConfig:               appCfg,
		showHostSummaries:       true, // Default to showing AI summaries
		hostSummaries:           make(map[string]string),
		hostSummaryHashes:       make(map[string]string),
		hostSummaryTimes:        make(map[string]time.Time),
		hostSummaryPending:      make(map[string]bool),
		initialSyncNeeded:       true, // Trigger priority sync after jobs load
		cloudDiscoveryPending:   cloudDiscoveryFn != nil,
		cloudDiscoveryFn:        cloudDiscoveryFn,
		llmInitFn:               llmInitFn,
	}

	if opts.InitialSnapshot != nil {
		model.allJobs = opts.InitialSnapshot.Jobs
		if opts.InitialSnapshot.JobDependencies != nil {
			model.jobDependencies = opts.InitialSnapshot.JobDependencies
		}
		model.hosts = opts.InitialSnapshot.Hosts
		if opts.InitialSnapshot.HostSyncTimes != nil {
			model.hostSyncTimes = opts.InitialSnapshot.HostSyncTimes
		}
		model.applyJobFilter()
	} else if opts.Monitor != nil {
		model.allJobs = opts.Monitor.Jobs()
		model.jobDependencies = opts.Monitor.JobDependencies()
		model.hosts = opts.Monitor.Hosts()
		model.hostSyncTimes = opts.Monitor.HostSyncTimes()
		model.applyJobFilter()
	}

	// Restore saved host filter if the host has jobs
	state := LoadState()
	if state.HostFilter != "" {
		hasJobs := false
		for _, job := range model.allJobs {
			if job.Host == state.HostFilter {
				hasJobs = true
				break
			}
		}
		if hasJobs {
			model.jobHostFilterMode = hostFilterSpecific
			model.jobHostFilterHost = state.HostFilter
			model.applyJobFilter()
		}
	}

	// Create and start sync worker (with cloud clients for full reconciliation)
	var r2Client *r2.Client
	if appCfg != nil {
		r2Cfg := appCfg.Vastai.R2
		if r2Cfg.Bucket != "" && r2Cfg.AccessKeyID != "" {
			var err error
			r2Client, err = r2.New(r2.Config{
				AccountID:       r2Cfg.AccountID,
				AccessKeyID:     r2Cfg.AccessKeyID,
				SecretAccessKey: r2Cfg.SecretAccessKey,
				Bucket:          r2Cfg.Bucket,
			})
			if err != nil {
				slog.Warn("R2 client init failed", "component", "tui", "error", err)
			}
		}
	}
	model.syncWorker = NewSyncWorker(database, model.cloudClients, r2Client, model.appConfig)
	model.syncWorker.Start()

	return model
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	if m.monitor != nil {
		return tea.Batch(
			m.startMonitor(),
			m.waitForMonitorEvent(),
			m.refreshJobs(),
			m.startCloudDiscovery(),
			m.startLLMInit(),
			m.startSyncTicker(),
			m.startLogTicker(),
			m.startHostRefreshTicker(),
			m.startHostSummaryTicker(),
			m.waitForSyncResult(),
			m.spinner.Tick,
		)
	}
	return tea.Batch(
		m.refreshJobs(),
		m.loadHosts(),
		m.loadHostSyncTimes(),
		m.startCloudDiscovery(),
		m.startLLMInit(),
		m.startSyncTicker(),
		m.startLogTicker(),
		m.startHostRefreshTicker(),
		m.startHostSummaryTicker(),
		m.startDBWatcher(),
		m.waitForSyncResult(),
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
		detailHeight := int(float64(m.height) * detailPanelHeightRatio)
		m.logViewport.Width = m.width - 6
		m.logViewport.Height = detailHeight - 4
		m.detailViewport.Width = m.width - 6
		m.detailViewport.Height = detailHeight - 4
		// Update job list dimensions (subtract 2 more for column header + filter row)
		listHeight := int(float64(m.height) * jobListHeightRatio)
		m.jobList.SetWidth(m.width - 2)
		contentHeight := listHeight - 4
		if contentHeight < 0 {
			contentHeight = 0
		}
		m.jobList.SetHeight(contentHeight)
		m.jobListContentHeight = contentHeight
		return m, nil

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
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
		return m.handleJobsRefreshed(msg)

	case dbWatcherReadyMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("DB watch error: %v", msg.err), true)
		}
		m.dbWatcher = msg.watcher
		m.dbWatcherTargets = msg.targets
		return m, m.waitForDBEvent()

	case dbWatchEventMsg:
		var cmds []tea.Cmd
		if msg.err != nil {
			cmds = append(cmds, m.setFlash(fmt.Sprintf("DB watch error: %v", msg.err), true))
		} else if !m.dbRefreshDebounceActive {
			m.dbRefreshDebounceActive = true
			cmds = append(cmds, tea.Tick(dbChangeDebounceInterval, func(time.Time) tea.Msg {
				return dbRefreshTriggeredMsg{}
			}))
		}
		// Request syncs for hosts with active jobs
		m.requestSyncsForActiveHosts()
		if cmd := m.waitForDBEvent(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case dbRefreshTriggeredMsg:
		m.dbRefreshDebounceActive = false
		return m, m.refreshJobs()

	case syncResultMsg:
		// Handle sync result from worker
		return m.handleSyncResult(msg)

	case syncCompletedMsg:
		// Legacy handler - should be removed once migration complete
		return m.handleSyncCompleted(msg)

	case monitorEventMsg:
		return m.handleMonitorEvent(msg)

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
				if msg.fromCache {
					m.logContent = msg.content
					m.logStale = true
					m.logCache[msg.jobID] = msg.content
				} else if cached, ok := m.logCache[msg.jobID]; ok {
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
				m.logStale = msg.connError
			}
			m.logViewport.SetContent(m.logContent)
			m.logViewport.GotoBottom()
		}
		return m, nil

	case quickProgressMsg:
		// Store quick progress fetch result (always update to keep progress current)
		if msg.progress != nil {
			m.jobProgress[msg.jobID] = msg.progress
		}
		return m, nil

	case cpuTopMsg:
		if msg.jobView {
			if m.jobCPUTopRequestedHost != "" && msg.host != m.jobCPUTopRequestedHost {
				return m, nil
			}
			m.jobCPUTopRequestedHost = ""
			m.jobCPUTopLoading = false
			if msg.err != nil {
				m.jobCPUTopError = msg.err.Error()
				m.jobCPUTopEntries = nil
				m.jobCPUTopDataHost = msg.host
				return m, nil
			}
			m.jobCPUTopError = ""
			m.jobCPUTopEntries = msg.processes
			m.jobCPUTopDataHost = msg.host
			m.jobCPUTopUpdated = time.Now()
			return m, nil
		}
		if m.hostCPUTopRequestedHost != "" && msg.host != m.hostCPUTopRequestedHost {
			return m, nil
		}
		m.hostCPUTopRequestedHost = ""
		m.hostCPUTopLoading = false
		if msg.err != nil {
			m.hostCPUTopError = msg.err.Error()
			m.hostCPUTopEntries = nil
			m.hostCPUTopDataHost = msg.host
			return m, nil
		}
		m.hostCPUTopError = ""
		m.hostCPUTopEntries = msg.processes
		m.hostCPUTopDataHost = msg.host
		m.hostCPUTopUpdated = time.Now()
		return m, nil

	case jobKilledMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Cancel failed: %v", msg.err), true)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Kill failed: %v", msg.err), true)
			}
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Job %s canceled (removal queued for when host is online)", ids.FormatJobID(msg.jobID)), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %s kill pending (host offline)", ids.FormatJobID(msg.jobID)), false)
			}
		} else {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Job %s canceled", ids.FormatJobID(msg.jobID)), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %s killed", ids.FormatJobID(msg.jobID)), false)
			}
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobPausedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Pause failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s pause pending (host offline)", ids.FormatJobID(msg.jobID)), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s paused", ids.FormatJobID(msg.jobID)), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobResumedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Resume failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s resume pending (host offline)", ids.FormatJobID(msg.jobID)), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s resumed", ids.FormatJobID(msg.jobID)), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobDraftedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Draft failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s draft pending (sync when host online)", ids.FormatJobID(msg.jobID)), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s marked draft", ids.FormatJobID(msg.jobID)), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobQueuedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Queue failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s queued (pending sync)", ids.FormatJobID(msg.jobID)), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s queued", ids.FormatJobID(msg.jobID)), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobRestartedMsg:
		m.restarting = false
		m.restartingJobName = ""
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Restart failed: %v", msg.err), true)
		}
		if msg.deferred {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %s restarted (will start when host is online)", ids.FormatJobID(msg.jobID)), false), m.refreshJobs())
		}
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %s restarted", ids.FormatJobID(msg.jobID)), false), m.refreshJobs())

	case jobRetriedMsg:
		if msg.err != nil {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Retry failed: %v", msg.err), true), m.refreshJobs())
		}
		var flashCmd tea.Cmd
		if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s retried (pending sync)", ids.FormatJobID(msg.jobID)), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s retried", ids.FormatJobID(msg.jobID)), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobStartedNowMsg:
		if msg.err != nil {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Start failed: %v", msg.err), true), m.refreshJobs())
		}
		if msg.deferred {
			return m, tea.Batch(
				m.setFlash(fmt.Sprintf("Host %s unreachable. Job %s will start on next sync.", msg.host, ids.FormatJobID(msg.jobID)), false),
				m.refreshJobs(),
			)
		}
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Job %s started", ids.FormatJobID(msg.jobID)), false), m.refreshJobs())

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
		} else if msg.deferred {
			m.requestHostSyncPriority(msg.host)
			return m, m.setFlash(fmt.Sprintf("Job %s move queued (will sync when host is online)", ids.FormatJobID(msg.jobID)), false)
		} else if !msg.moved {
			m.requestHostSyncPriority(msg.host)
			return m, m.setFlash(fmt.Sprintf("Job %s is already at the front", ids.FormatJobID(msg.jobID)), false)
		}
		m.requestHostSyncPriority(msg.host)
		return m, m.setFlash(fmt.Sprintf("Job %s moved to front of queue", ids.FormatJobID(msg.jobID)), false)

	case descriptionGeneratedMsg:
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Generate description failed: %v", msg.err), true)
		}
		// Refresh jobs to show the new description
		return m, tea.Batch(
			m.setFlash(fmt.Sprintf("Generated: %s", msg.description), false),
			m.refreshJobs(),
		)

	case hostSummaryGeneratedMsg:
		delete(m.hostSummaryPending, msg.host)
		if msg.err != nil {
			// Silently ignore errors - just don't show summary
			// Still try to generate next summary
			if m.showHostSummaries {
				return m, m.generateAllHostSummaries()
			}
			return m, nil
		}
		m.hostSummaries[msg.host] = msg.summary
		m.hostSummaryHashes[msg.host] = msg.hash
		m.hostSummaryTimes[msg.host] = time.Now()
		// Trigger next summary if summaries are enabled
		if m.showHostSummaries {
			return m, m.generateAllHostSummaries()
		}
		return m, nil

	case jobRemovedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Remove failed: %v", msg.err), true)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s removed", ids.FormatJobID(msg.jobID)), false)
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
			flashCmd = m.setFlash(fmt.Sprintf("Job %s queued (will append when host is online)", ids.FormatJobID(msg.jobID)), false)
			m.pendingSelectJobID = msg.jobID
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %s queued", ids.FormatJobID(msg.jobID)), false)
			m.pendingSelectJobID = msg.jobID
			// Keep inputs for easy re-use (user can modify and submit again)
		}
		// Reload hosts in case this job was on a new host
		return m, tea.Batch(flashCmd, m.refreshJobs(), m.loadHosts())

	case jobEnvLoadedMsg:
		if !m.editMode || msg.jobID != m.editingJobID || msg.err != nil {
			return m, nil
		}
		gpu, rest := splitGPUEnvVars(msg.envVars)
		m.inputs[inputGPU].SetValue(gpu)
		m.inputs[inputEnvVars].SetValue(formatEnvInput(rest))
		m.editingJobDepSpec = msg.depSpec
		return m, nil

	case jobEditedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Edit failed: %v", msg.err), true)
		} else if msg.deferred {
			m.requestHostSyncPriority(msg.host)
			flashCmd = m.setFlash(fmt.Sprintf("Job %s updated (will sync when host is online)", ids.FormatJobID(msg.jobID)), false)
		} else {
			m.requestHostSyncPriority(msg.host)
			flashCmd = m.setFlash(fmt.Sprintf("Job %s updated", ids.FormatJobID(msg.jobID)), false)
			m.pendingSelectJobID = msg.jobID
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case tickMsg:
		cmds := []tea.Cmd{m.startSyncTicker()}
		if m.monitor == nil {
			if cmd := m.ensureCurrentDaemonForTick(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// Monitor mode already refreshes jobs and inventory hosts. The
			// legacy ticker owns those reads only when no monitor is installed.
			cmds = append(cmds, m.refreshJobs())
			m.requestSyncsForActiveHosts()
		}
		if cmd := m.refreshExternalJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case logTickMsg:
		cmds := []tea.Cmd{m.startLogTicker()}
		// Update the monitor's watched jobs based on current UI state.
		// The monitor handles the actual SSH polling and emits events.
		if m.monitor != nil {
			if m.detailTab == DetailTabLogs && m.selectedJob != nil && m.selectedJob.Backend != db.BackendSkyPilot && m.selectedJob.EffectiveStatus() == db.StatusRunning {
				m.monitor.WatchJobLog(m.selectedJob)
			} else {
				m.monitor.WatchJobLog(nil)
			}
			targetJob := m.getTargetJob()
			if targetJob != nil && targetJob.Backend != db.BackendSkyPilot && targetJob.EffectiveStatus() == db.StatusRunning {
				m.monitor.WatchJobStats(targetJob)
			} else {
				m.monitor.WatchJobStats(nil)
			}
		}
		if m.detailTab == DetailTabLogs && m.selectedJob != nil && m.selectedJob.Backend == db.BackendSkyPilot {
			cmds = append(cmds, m.fetchSelectedJobLog())
		}
		return m, tea.Batch(cmds...)

	case createTickMsg:
		// Only continue ticking if still creating
		if m.creatingJob {
			return m, m.startCreateTicker()
		}
		return m, nil

	case hostsLoadedMsg:
		return m.handleHostsLoaded(msg)

	case hostSyncTimesLoadedMsg:
		return m.handleHostSyncTimesLoaded(msg)

	case cloudDiscoveryLoadedMsg:
		m.cloudDiscoveryPending = false
		m.cloudClients = msg.clients
		m.cloudClientErr = msg.err
		if m.syncWorker != nil {
			m.syncWorker.SetCloudClients(msg.clients)
		}
		return m, nil

	case llmGeneratorLoadedMsg:
		m.llmGenerator = msg.generator
		if m.showHostSummaries && m.llmGenerator != nil {
			return m, m.generateAllHostSummaries()
		}
		return m, nil

	case hostInfoMsg:
		return m.handleHostInfo(msg)

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

	case hostRefreshTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startHostRefreshTicker())

		if m.monitor != nil {
			if m.viewMode == ViewModeHosts {
				for _, host := range m.hosts {
					if host != nil {
						m.requestHostInfoRefresh(host.Name, false)
					}
				}
			}
			if len(cmds) == 0 {
				return m, nil
			}
			return m, tea.Batch(cmds...)
		}

		// Build set of hosts with running jobs
		hostsWithRunningJobs := make(map[string]bool)
		for _, job := range m.jobs {
			switch job.EffectiveStatus() {
			case db.StatusRunning, db.StatusStarting, db.StatusPaused:
				hostsWithRunningJobs[job.Host] = true
			}
		}

		for _, host := range m.hosts {
			// Refresh host if:
			// 1. In hosts view and (not queried yet OR online), OR
			// 2. Host has running jobs (to update LastCheck for stale indicator), OR
			// 3. Host is marked online (to detect when it goes offline)
			inHostsView := m.viewMode == ViewModeHosts
			needsRefresh := (!m.hostsQueriedThisSession[host.Name] || host.Status == HostStatusOnline)
			hasRunningJobs := hostsWithRunningJobs[host.Name]
			isOnline := host.Status == HostStatusOnline

			if (inHostsView && needsRefresh) || hasRunningJobs || isOnline {
				m.requestHostInfoRefresh(host.Name, false)
			}
		}
		return m, tea.Batch(cmds...)

	case hostSummaryTickMsg:
		if m.viewMode == ViewModeJobs && len(m.hosts) > 0 {
			m.hostSummaryTickerOffset++
			if m.hostSummaryTickerOffset > 1000000 {
				m.hostSummaryTickerOffset = 0
			}
		}
		return m, m.startHostSummaryTicker()

	case daemonRestartedMsg:
		m.daemonRestartInProgress = false
		if msg.err != nil {
			return m, m.setFlash(fmt.Sprintf("Daemon restart failed: %v", msg.err), true)
		}
		if msg.action == daemoncontrol.EnsureNoop {
			return m, nil
		}
		return m, m.setFlash(fmt.Sprintf("Daemon restarted (PID %d)", msg.pid), false)

	case FlashExpiredMsg:
		m.flash.HandleExpired()
		return m, nil

	case cloudOffersLoadedMsg:
		m.cloudMenuLoading = false
		if msg.err != nil {
			m.showCloudMenu = false
			return m, m.setFlash(fmt.Sprintf("Rental GPU: %v", msg.err), true)
		}
		offerings := msg.offerings
		// For unplaced jobs, no local host can run this job — omit local option
		if m.cloudMenuJob != nil && m.cloudMenuJob.Host == "" {
			filtered := offerings[:0]
			for _, o := range offerings {
				if o.Source != "local" {
					filtered = append(filtered, o)
				}
			}
			offerings = filtered
		}
		m.cloudMenuOfferings = offerings
		return m, nil

	case cloudJobLaunchedMsg:
		if msg.err != nil {
			return m, tea.Batch(
				m.setFlash(fmt.Sprintf("Rental launch failed: %v", msg.err), true),
				m.refreshJobs(),
			)
		}
		return m, tea.Batch(
			m.setFlash(fmt.Sprintf("Rental job launched (instance %d) — results via R2", msg.instanceID), false),
			m.refreshJobs(),
		)

	case cloudJobProgressMsg:
		return m, m.setFlash(fmt.Sprintf("Rental job %s: %s", ids.FormatJobID(msg.jobID), msg.phase), false)

	case cloudJobCompletedMsg:
		if msg.err != nil {
			return m, tea.Batch(
				m.setFlash(fmt.Sprintf("Rental job failed: %v", msg.err), true),
				m.refreshJobs(),
			)
		}
		costStr := ""
		if msg.cost > 0 {
			costStr = fmt.Sprintf(" (cost: $%.2f)", msg.cost)
		}
		return m, tea.Batch(
			m.setFlash(fmt.Sprintf("Rental job completed (exit %d)%s", msg.exitCode, costStr), false),
			m.refreshJobs(),
		)
	}

	return m, nil
}

func defaultLLMInit(database *sql.DB, appCfg *config.Config) *llm.DescriptionGenerator {
	if appCfg == nil || !appCfg.IsAIEnabled() {
		return nil
	}

	genOpts := []llm.GeneratorOption{llm.WithClient(llm.NewConfiguredClient(appCfg))}
	if strings.TrimSpace(appCfg.LLM.Provider) == "" {
		model := appCfg.AIModel()
		genOpts = append(genOpts, llm.WithModel(model))
	}

	gen := llm.NewGenerator(database, genOpts...)
	if !gen.Start() {
		return nil
	}
	return gen
}

// View renders the UI
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return m.spinner.View() + " Loading..."
	}

	// Calculate panel heights
	listHeight := int(float64(m.height) * jobListHeightRatio)
	detailHeight := int(float64(m.height) * detailPanelHeightRatio)

	var mainView string

	if m.viewMode == ViewModeHosts {
		// Hosts view
		listView := m.renderHostList(listHeight)
		detailView := m.renderHostDetailPanel(detailHeight)
		flashView := m.flash.Render()
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
		flashView := m.flash.Render()
		statusView := m.renderStatusBar()

		mainView = lipgloss.JoinVertical(
			lipgloss.Left,
			listView,
			logView,
			flashView,
			statusView,
		)
	}

	// Show cloud menu overlay
	if m.showCloudMenu {
		return m.renderCloudMenu(mainView)
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

func (m *Model) setFlash(msg string, isError bool) tea.Cmd {
	return m.flash.Set(msg, isError)
}
