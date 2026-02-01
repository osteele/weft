package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/core"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/llm"
	"github.com/osteele/remote-jobs/internal/logcache"
	"github.com/osteele/remote-jobs/internal/logfiles"
	"github.com/osteele/remote-jobs/internal/monitor"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/progress"
	"github.com/osteele/remote-jobs/internal/queuefile"
	"github.com/osteele/remote-jobs/internal/queuejob"
	"github.com/osteele/remote-jobs/internal/queuerunner"
	"github.com/osteele/remote-jobs/internal/remote"
	"github.com/osteele/remote-jobs/internal/slack"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Default intervals for background operations
const (
	DefaultSyncActiveInterval  = 15 * time.Second
	DefaultSyncIdleInterval    = 60 * time.Second
	DefaultLogRefreshInterval  = 3 * time.Second
	DefaultHostRefreshInterval = 30 * time.Second
	DefaultHostCacheDuration   = 24 * time.Hour // How long cached host info is considered fresh
	dbChangeDebounceInterval   = 200 * time.Millisecond
	topProcessLimit            = 15
	hostSummaryTickerGap       = "   "
	hostSummaryTickerInterval  = 200 * time.Millisecond
)

const (
	jobListHeightRatio     = 0.6
	detailPanelHeightRatio = 0.3
)

const hostRecentSyncWindow = 48 * time.Hour

// ViewMode represents which view is currently active
type ViewMode int

const (
	ViewModeJobs ViewMode = iota
	ViewModeHosts
)

// jobFilterMode controls which subset of jobs is displayed in the Jobs view
type jobFilterMode int

const (
	jobFilterAll    jobFilterMode = iota
	jobFilterRecent               // Active jobs + completed within 24h
	jobFilterActive
	jobFilterSucceeded
	jobFilterFailed
	jobFilterModeCount
)

// hostFilterMode controls host scoping in the Jobs view
type hostFilterMode int

const (
	hostFilterRecent hostFilterMode = iota // Hosts synced recently
	hostFilterAll
	hostFilterSpecific
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
	DetailTabCPU
)

// HostDetailTab represents which tab is active in the host detail panel
type HostDetailTab int

const (
	HostDetailTabInfo       HostDetailTab = iota // Host Info
	HostDetailTabCPU                             // Host-wide CPU processes
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
	Up              key.Binding
	Down            key.Binding
	PageDownList    key.Binding
	PageUpList      key.Binding
	TopList         key.Binding
	Enter           key.Binding
	Logs            key.Binding
	Filter          key.Binding
	HostFilter      key.Binding
	Escape          key.Binding
	Kill            key.Binding
	Pause           key.Binding
	Draft           key.Binding
	Restart         key.Binding
	Retry           key.Binding
	EditRestart     key.Binding
	Remove          key.Binding
	NewJob          key.Binding
	Prune           key.Binding
	Suspend         key.Binding
	Quit            key.Binding
	HostsView       key.Binding
	JobsView        key.Binding
	Tab             key.Binding
	ShiftTab        key.Binding
	Sync            key.Binding
	Help            key.Binding
	StartQueue      key.Binding
	StartNow        key.Binding
	MoveToFront     key.Binding
	Edit            key.Binding
	Sort            key.Binding
	RegenerateDesc  key.Binding
	ToggleSummaries key.Binding
}

var (
	keys = keyMap{
		Up: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("↑", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("↓", "down"),
		),
		PageDownList: key.NewBinding(
			key.WithKeys(" "),
			key.WithHelp("space", "jobs page down"),
		),
		PageUpList: key.NewBinding(
			key.WithKeys("b"),
			key.WithHelp("b", "jobs page up"),
		),
		TopList: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "jobs to top"),
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
			key.WithHelp("f", "cycle view"),
		),
		HostFilter: key.NewBinding(
			key.WithKeys("H"),
			key.WithHelp("H", "host filter"),
		),
		Escape: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "clear"),
		),
		Kill: key.NewBinding(
			key.WithKeys("k", "delete"),
			key.WithHelp("k", "kill/cancel"),
		),
		Pause: key.NewBinding(
			key.WithKeys("p"),
			key.WithHelp("p", "pause"),
		),
		Draft: key.NewBinding(
			key.WithKeys("d"),
			key.WithHelp("d", "toggle draft/queue"),
		),
		Restart: key.NewBinding(
			key.WithKeys("R"),
			key.WithHelp("R", "restart"),
		),
		Retry: key.NewBinding(
			key.WithKeys("y"),
			key.WithHelp("y", "retry"),
		),
		EditRestart: key.NewBinding(
			key.WithKeys("E"),
			key.WithHelp("E", "edit & restart"),
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
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
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
			key.WithHelp("g", "go/start/resume"),
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
		RegenerateDesc: key.NewBinding(
			key.WithKeys("G"),
			key.WithHelp("G", "generate AI description"),
		),
		ToggleSummaries: key.NewBinding(
			key.WithKeys("D"),
			key.WithHelp("D", "toggle AI host summaries"),
		),
	}
	localUserName = detectLocalUsername()
)

func detectLocalUsername() string {
	u, err := user.Current()
	if err != nil || u == nil {
		return ""
	}
	name := u.Username
	if idx := strings.LastIndex(name, `\`); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// Messages
type jobsRefreshedMsg struct {
	jobs            []*db.Job
	jobDependencies map[int64]string // jobID -> dep_spec (e.g., "930" or "930+")
	err             error
}

type dbWatcherReadyMsg struct {
	watcher *fsnotify.Watcher
	targets map[string]struct{}
	err     error
}

type dbWatchEventMsg struct {
	err error
}

type dbRefreshTriggeredMsg struct{}

type monitorEventMsg struct {
	event monitor.Event
}

type syncCompletedMsg struct {
	updated           int
	queuesStarted     []string // hosts where queue runners were started
	hostsSynced       []string
	queueRunnerErrors []string
	err               error
}

type logFetchedMsg struct {
	jobID     int64
	content   string
	progress  *progress.Progress // extracted progress info (nil if none found)
	err       error
	connError bool // true if this was a connection error (host unreachable)
	fromCache bool // true if this log was served from local cache
}

type quickProgressMsg struct {
	jobID    int64
	progress *progress.Progress
}

type jobKilledMsg struct {
	jobID     int64
	err       error
	deferred  bool // true if kill was queued for later (host offline)
	cancelled bool // true if this was a queued job that was cancelled
	message   string
}

type jobPausedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobResumedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobDraftedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobQueuedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobRestartedMsg struct {
	oldJobID int64
	newJobID int64
	err      error
	deferred bool // true if restart was queued for later (host offline)
}

type jobRetriedMsg struct {
	oldJobID int64
	newJobID int64
	err      error
	deferred bool
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
type hostSummaryTickMsg time.Time
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

type cpuTopMsg struct {
	host      string
	processes []remote.TopProcess
	err       error
	jobView   bool
}

type descriptionGeneratedMsg struct {
	jobID       int64
	description string
	err         error
}

type hostSummaryGeneratedMsg struct {
	host    string
	summary string
	hash    string
	err     error
}

type jobEnvLoadedMsg struct {
	jobID   int64
	envVars []string
	depSpec string
	err     error
}

type hostSyncTimesLoadedMsg struct {
	times map[string]time.Time
	err   error
}

// Input field indices for new job form
const (
	inputHost = iota
	inputDescription
	inputCommand
	inputWorkingDir
	inputGPU
	inputCPUAllotment
	inputEnvVars
)

// Baseline default: ~60% of a 12-core M2 Max host.
const defaultAllotmentCores = 7

var cpuAllotmentPresets = []int{20, 40, 60, 80}

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
	flashMessage            string
	flashIsError            bool
	flashExpiry             time.Time
	hostSummaryTickerOffset int

	// Process stats for running jobs
	processStats      *remote.ProcessStats
	prevProcessStats  *remote.ProcessStats // Previous sample for CPU% calculation
	processStatsJobID int64

	// Job CPU usage view (per-host snapshot)
	jobCPUTopEntries       []remote.TopProcess
	jobCPUTopDataHost      string
	jobCPUTopRequestedHost string
	jobCPUTopLoading       bool
	jobCPUTopError         string
	jobCPUTopUpdated       time.Time

	// Host CPU usage view (hosts tab)
	hostCPUTopEntries       []remote.TopProcess
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

	// Edit job mode (for queued jobs only)
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

	// Track hosts that have already shown low disk warning this session
	lowDiskWarnedHosts map[string]bool
	// Track hosts that have already shown jq missing warning this session
	jqMissingWarnedHosts map[string]bool
	// Track hosts that have already shown queue runner stopped warning this session
	queueStoppedWarnedHosts map[string]bool

	// Job dependencies (jobID -> dep_spec like "930" or "930+")
	jobDependencies map[int64]string

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
}

// ModelOptions contains configuration for the TUI model
type ModelOptions struct {
	SyncActiveInterval  time.Duration
	SyncIdleInterval    time.Duration
	LogRefreshInterval  time.Duration
	HostRefreshInterval time.Duration
	HostCacheDuration   time.Duration // How long cached host info is considered fresh
	Monitor             *monitor.Monitor
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
	inputs[inputCommand].CharLimit = 512

	inputs[inputWorkingDir] = textinput.New()
	inputs[inputWorkingDir].Placeholder = "(optional, defaults to ~)"
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

	// Create and start LLM generator if enabled and LLM backend is available
	var llmGen *llm.DescriptionGenerator
	if appCfg.IsAIEnabled() {
		genOpts := []llm.GeneratorOption{}
		if model := appCfg.AIModel(); model != "" {
			genOpts = append(genOpts, llm.WithModel(model))
		}
		gen := llm.NewGenerator(database, genOpts...)
		if gen.Start() {
			llmGen = gen
		}
	}

	detailVP := viewport.New(0, 0)

	// Create context for cancellation on quit
	ctx, cancel := context.WithCancel(context.Background())

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
		jqMissingWarnedHosts:    make(map[string]bool),
		queueStoppedWarnedHosts: make(map[string]bool),
		logCache:                make(map[int64]string),
		jobDependencies:         make(map[int64]string),
		progressTracker:         progress.NewTracker(),
		jobProgress:             make(map[int64]*progress.Progress),
		lastHostSyncTimes:       make(map[string]time.Time),
		hostSyncTimes:           make(map[string]time.Time),
		llmGenerator:            llmGen,
		appConfig:               appCfg,
		showHostSummaries:       true, // Default to showing AI summaries
		hostSummaries:           make(map[string]string),
		hostSummaryHashes:       make(map[string]string),
		hostSummaryTimes:        make(map[string]time.Time),
		hostSummaryPending:      make(map[string]bool),
		initialSyncNeeded:       true, // Trigger priority sync after jobs load
	}

	if opts.Monitor != nil {
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

	// Create and start sync worker
	model.syncWorker = NewSyncWorker(database)
	model.syncWorker.Start()

	return model
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	if m.monitor != nil {
		return tea.Batch(
			m.waitForMonitorEvent(),
			func() tea.Msg {
				m.monitor.RefreshJobs()
				m.monitor.RefreshHosts()
				m.monitor.RefreshHostSyncTimes()
				return nil
			},
			m.startLogTicker(),
			m.startHostRefreshTicker(),
			m.startHostSummaryTicker(),
			m.spinner.Tick,
		)
	}
	return tea.Batch(
		m.refreshJobs(),
		m.loadHosts(),
		m.loadHostSyncTimes(),
		m.startSyncTicker(),
		m.startLogTicker(),
		m.startHostRefreshTicker(),
		m.startHostSummaryTicker(),
		m.startDBWatcher(),
		m.checkSyncResults(),
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
				flashCmd = m.setFlash(fmt.Sprintf("Job %d canceled (removal queued for when host is online)", msg.jobID), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d kill pending (host offline)", msg.jobID), false)
			}
		} else {
			if msg.cancelled {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d canceled", msg.jobID), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job %d killed", msg.jobID), false)
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
			flashCmd = m.setFlash(fmt.Sprintf("Job %d pause pending (host offline)", msg.jobID), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d paused", msg.jobID), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobResumedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Resume failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d resume pending (host offline)", msg.jobID), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d resumed", msg.jobID), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobDraftedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Draft failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d draft pending (sync when host online)", msg.jobID), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d marked draft", msg.jobID), false)
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobQueuedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			flashCmd = m.setFlash(fmt.Sprintf("Queue failed: %v", msg.err), true)
		} else if msg.message != "" {
			flashCmd = m.setFlash(msg.message, false)
		} else if msg.deferred {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d queued (pending sync)", msg.jobID), false)
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d queued", msg.jobID), false)
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

	case jobRetriedMsg:
		var flashCmd tea.Cmd
		if msg.err != nil {
			if msg.newJobID > 0 {
				m.pendingSelectJobID = msg.newJobID
			}
			if msg.newJobID > 0 {
				flashCmd = m.setFlash(fmt.Sprintf("Retry created job %d but dependency update failed: %v", msg.newJobID, msg.err), true)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Retry failed: %v", msg.err), true)
			}
		} else {
			m.pendingSelectJobID = msg.newJobID
			if msg.deferred {
				flashCmd = m.setFlash(fmt.Sprintf("Job retried as %d (pending sync)", msg.newJobID), false)
			} else {
				flashCmd = m.setFlash(fmt.Sprintf("Job retried (new ID: %d)", msg.newJobID), false)
			}
		}
		return m, tea.Batch(flashCmd, m.refreshJobs())

	case jobStartedNowMsg:
		if msg.err != nil {
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Start failed: %v", msg.err), true), m.refreshJobs())
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
			flashCmd = m.setFlash(fmt.Sprintf("Job %d queued (will append when host is online)", msg.jobID), false)
			m.pendingSelectJobID = msg.jobID
		} else {
			flashCmd = m.setFlash(fmt.Sprintf("Job %d queued", msg.jobID), false)
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
		// Request syncs for hosts based on their job activity
		m.requestSyncsForActiveHosts()
		// Check for sync results
		cmds = append(cmds, m.checkSyncResults())
		return m, tea.Batch(cmds...)

	case logTickMsg:
		// Update the monitor's watched jobs based on current UI state.
		// The monitor handles the actual SSH polling and emits events.
		if m.monitor != nil {
			if m.detailTab == DetailTabLogs && m.selectedJob != nil && m.selectedJob.Status == db.StatusRunning {
				m.monitor.WatchJobLog(m.selectedJob)
			} else {
				m.monitor.WatchJobLog(nil)
			}
			targetJob := m.getTargetJob()
			if targetJob != nil && targetJob.Status == db.StatusRunning {
				m.monitor.WatchJobStats(targetJob)
			} else {
				m.monitor.WatchJobStats(nil)
			}
		}
		return m, m.startLogTicker()

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
			if job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
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
	listHeight := int(float64(m.height) * jobListHeightRatio)

	// Handle mouse wheel
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		scrollUp := msg.Button == tea.MouseButtonWheelUp

		// Check if mouse is in the log/detail panel area (bottom portion)
		if msg.Y >= listHeight {
			scrollDelta := 3
			if scrollUp {
				scrollDelta = -scrollDelta
			}
			switch m.detailTab {
			case DetailTabLogs:
				m.logViewport.SetYOffset(m.logViewport.YOffset + scrollDelta)
				return m, nil
			case DetailTabDetails:
				if m.jobSelectionActive {
					m.detailViewport.SetYOffset(m.detailViewport.YOffset + scrollDelta)
					return m, nil
				}
			}
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
				if clickedIndex == prevIdx {
					if m.jobSelectionActive {
						m.clearJobSelection()
						return m, nil
					}
					m.jobSelectionActive = true
					return m, m.handleSelectionChanged()
				}
				m.jobList.Select(clickedIndex)
				m.jobSelectionActive = true
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
				if idx := m.hostIndexAtRow(clickedRow, listHeight); idx >= 0 {
					m.selectedHostIdx = idx
					if cmd := m.handleHostSelectionChanged(); cmd != nil {
						return m, cmd
					}
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
	if m.detailTab == DetailTabDetails && m.jobSelectionActive {
		switch msg.String() {
		case "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
			updated, cmd := m.detailViewport.Update(msg)
			*m.detailViewport = updated
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
		// Cancel all background SSH operations
		if m.cancel != nil {
			m.cancel()
		}
		// Stop the sync worker
		if m.syncWorker != nil {
			m.syncWorker.Stop()
		}
		// Close SSH session pool
		ssh.ClosePool()
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
				m.requestHostInfoRefresh(host.Name, false)
			}
		}
		if m.hostDetailTab == HostDetailTabCPU {
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				cmds = append(cmds, cmd)
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
					m.requestHostInfoRefresh(host.Name, false)
				}
			}
			if m.hostDetailTab == HostDetailTabCPU {
				if cmd := m.handleHostSelectionChanged(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			return m, tea.Batch(cmds...)
		}
		m.viewMode = ViewModeJobs
		return m, nil

	case key.Matches(msg, keys.ToggleSummaries):
		// Toggle AI host summaries (only in hosts view)
		if m.viewMode == ViewModeHosts {
			m.showHostSummaries = !m.showHostSummaries
			if m.showHostSummaries {
				// Trigger summary generation for all hosts
				return m, tea.Batch(m.setFlash("AI summaries enabled", false), m.generateAllHostSummaries())
			}
			return m, m.setFlash("AI summaries disabled", false)
		}
		return m, nil

	case key.Matches(msg, keys.PageDownList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.pageJobList(1); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.PageUpList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.pageJobList(-1); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.TopList):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if cmd := m.jumpJobListToTop(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case key.Matches(msg, keys.Up):
		if m.viewMode == ViewModeHosts {
			if m.selectedHostIdx > 0 {
				m.selectedHostIdx--
			}
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				return m, cmd
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
		if !m.jobSelectionActive {
			return m, tea.Batch(cmd, m.handleSelectionChanged())
		}
		return m, cmd

	case key.Matches(msg, keys.Down):
		if m.viewMode == ViewModeHosts {
			if len(m.hosts) > 0 && m.selectedHostIdx < len(m.hosts)-1 {
				m.selectedHostIdx++
			}
			if cmd := m.handleHostSelectionChanged(); cmd != nil {
				return m, cmd
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
		if !m.jobSelectionActive {
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
		m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		return m, nil

	case key.Matches(msg, keys.Logs):
		if m.viewMode == ViewModeJobs {
			// Toggle to logs tab (or toggle if already there)
			if m.detailTab == DetailTabLogs {
				// Already in logs mode - go back to details
				m.detailTab = DetailTabDetails
				m.detailViewport.GotoTop()
			} else {
				idx := m.jobList.Index()
				if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
					// Enter logs mode
					m.detailTab = DetailTabLogs
					m.jobSelectionActive = true
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
					// For terminal jobs, try local cache first (no SSH needed)
					if m.selectedJob.Status == db.StatusCompleted || m.selectedJob.Status == db.StatusDead || m.selectedJob.Status == db.StatusFailed || m.selectedJob.Status == db.StatusKilled || m.selectedJob.Status == db.StatusCanceled {
						if cached, err := logcache.Read(m.selectedJob.ID); err == nil {
							lines := strings.Split(cached, "\n")
							if len(lines) > 500 {
								lines = lines[len(lines)-500:]
							}
							content := strings.Join(lines, "\n")
							m.logContent = content
							m.logLoading = false
							m.logStale = false
							m.logViewport.SetContent(content)
							prog := progress.FindLastProgress(content)
							if prog != nil {
								m.jobProgress[m.selectedJob.ID] = prog
							}
						}
					}
					// Tell the monitor to start polling for this job's log and stats
					if m.monitor != nil {
						m.monitor.WatchJobLog(m.selectedJob)
						if m.selectedJob.Status == db.StatusRunning {
							m.monitor.WatchJobStats(m.selectedJob)
						}
					} else {
						// Fallback for non-monitor mode
						return m, m.fetchSelectedJobLog()
					}
					return m, nil
				}
			}
		}
		return m, nil

	case key.Matches(msg, keys.Escape):
		m.clearJobSelection()
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
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=k action=kill"))
			return m, tea.Batch(m.setFlash("Killing job...", false), m.killJob(job))
		case db.StatusQueued:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=k action=cancel"))
			return m, tea.Batch(m.setFlash("Cancelling queued job...", false), m.cancelQueuedJob(job))
		case db.StatusCompleted:
			return m, m.setFlash(fmt.Sprintf("Job %d already completed", job.ID), true)
		case db.StatusDead:
			return m, m.setFlash(fmt.Sprintf("Job %d already failed to start", job.ID), true)
		case db.StatusFailed:
			return m, m.setFlash(fmt.Sprintf("Job %d already crashed", job.ID), true)
		case db.StatusKilled:
			return m, m.setFlash(fmt.Sprintf("Job %d already killed", job.ID), true)
		case db.StatusCanceled:
			return m, m.setFlash(fmt.Sprintf("Job %d already canceled", job.ID), true)
		default:
			return m, m.setFlash(fmt.Sprintf("Can't kill job %d (status: %s)", job.ID, job.Status), true)
		}

	case key.Matches(msg, keys.Pause):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		switch job.Status {
		case db.StatusRunning, db.StatusStarting:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=p action=pause"))
			return m, tea.Batch(m.setFlash("Pausing job...", false), m.pauseJob(job))
		case db.StatusPaused:
			return m, m.setFlash(fmt.Sprintf("Job %d already paused (press g to resume)", job.ID), false)
		default:
			return m, m.setFlash(fmt.Sprintf("Can't pause job %d (status: %s)", job.ID, job.Status), true)
		}

	case key.Matches(msg, keys.Draft):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		// Toggle behavior based on effective (displayed) status: draft→queued, anything else→draft
		if job.EffectiveStatus() == db.StatusDraft {
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=d action=queue_draft"))
			return m, tea.Batch(m.setFlash("Queueing draft job...", false), m.queueDraftJob(job))
		}
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=d action=draft"))
		return m, tea.Batch(m.setFlash("Marking job draft...", false), m.draftJob(job))

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
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=r action=restart"))
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Restarting job %d...", job.ID), false), m.restartJob(job))

	case key.Matches(msg, keys.Retry):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=y action=retry"))
		return m, tea.Batch(m.setFlash(fmt.Sprintf("Retrying job %d...", job.ID), false), m.retryJob(job))

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
		if job.Status == db.StatusRunning || job.Status == db.StatusQueued || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
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
			m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		}
		return m, nil

	case key.Matches(msg, keys.Filter):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		m.jobFilter = jobFilterMode((int(m.jobFilter) + 1) % int(jobFilterModeCount))
		m.applyJobFilter()
		var cmds []tea.Cmd
		cmds = append(cmds, m.setFlash(fmt.Sprintf("View: %s", jobFilterDescription(m.jobFilter)), false))
		if len(m.jobs) == 0 {
			m.clearJobSelection()
		} else if cmd := m.jumpJobListToTop(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case key.Matches(msg, keys.HostFilter):
		if m.viewMode == ViewModeHosts && m.selectedHostIdx >= 0 && m.selectedHostIdx < len(m.hosts) {
			host := m.hosts[m.selectedHostIdx].Name
			if host != "" {
				m.jobHostFilterMode = hostFilterSpecific
				m.jobHostFilterHost = host
			}
		} else {
			m.cycleHostFilter()
		}
		m.saveHostFilter()
		m.applyJobFilter()
		var cmds []tea.Cmd
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Host: %s", hostFilterDescription(m)), false))
		if len(m.jobs) == 0 {
			m.clearJobSelection()
		} else if cmd := m.jumpJobListToTop(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case key.Matches(msg, keys.Prune):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		return m, tea.Batch(m.setFlash("Pruning completed/dead jobs...", false), m.pruneJobs())

	case key.Matches(msg, keys.StartNow):
		if m.viewMode == ViewModeHosts {
			// 'g' in hosts view switches to GPU Summary tab
			m.hostDetailTab = HostDetailTabGPUSummary
			return m, nil
		}
		job := m.getTargetJob()
		if job == nil {
			return m, m.setFlash("No job selected", true)
		}
		// Check for pending start first (idempotent behavior)
		if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning {
			return m, m.setFlash(fmt.Sprintf("Job %d is already starting (pending)", job.ID), false)
		}
		// Use effective status so pending state is respected
		switch job.EffectiveStatus() {
		case db.StatusPaused:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=resume"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Resuming job %d...", job.ID), false), m.resumeJob(job))
		case db.StatusQueued:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=start_now"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Starting job %d now...", job.ID), false), m.startQueuedJobNow(job))
		case db.StatusDraft:
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=g action=run_draft"))
			return m, tea.Batch(m.setFlash(fmt.Sprintf("Running draft job %d...", job.ID), false), m.runDraftJob(job))
		case db.StatusRunning:
			return m, m.setFlash(fmt.Sprintf("Job %d is already running", job.ID), false)
		default:
			return m, m.setFlash(fmt.Sprintf("Can only start queued or draft jobs (job %d is %s)", job.ID, job.EffectiveStatus()), true)
		}

	case key.Matches(msg, keys.MoveToFront):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusQueued {
			oplog.LogJob(oplog.OpTUIAction, job.ID, job.Host, oplog.WithDetail("key=G action=move_to_front"))
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

	case key.Matches(msg, keys.RegenerateDesc):
		if m.viewMode != ViewModeJobs {
			return m, nil
		}
		if m.llmGenerator == nil {
			return m, m.setFlash("AI description generation not available", true)
		}
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		return m, tea.Batch(
			m.setFlash(fmt.Sprintf("Generating description for job %d...", job.ID), false),
			m.regenerateDescription(job),
		)

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
		m.inputs[inputGPU].SetValue("")
		m.inputs[inputCPUAllotment].SetValue(formatCPUAllotmentInput(job.CPUAllotment))
		m.inputs[inputEnvVars].SetValue("")
		m.editingJobDepSpec = ""
		return m, m.fetchQueuedJobEnv(job)

	case key.Matches(msg, keys.Sync):
		if m.viewMode == ViewModeJobs && m.syncWorker != nil {
			// Request high-priority sync for all hosts
			m.requestSyncAllHosts(true)
			return m, m.setFlash("Sync requested", false)
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
		m.editingJobDepSpec = ""
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
			m.editingJobDepSpec = ""
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
	listHeight := int(float64(m.height) * jobListHeightRatio)
	detailHeight := int(float64(m.height) * detailPanelHeightRatio)

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
			{"space", "Page down job list"},
			{"b", "Page up job list"},
			{"t", "Jump to top of jobs"},
			{"←/→", "Switch to hosts view"},
			{"l", "Toggle logs view"},
			{"f", "Cycle job filters"},
			{"H", "Cycle host filter"},
			{"r", "Refresh job statuses"},
			{"n", "New job"},
			{"e", "Edit queued job"},
			{"R", "Restart job"},
			{"E", "Edit & restart job"},
			{"k", "Kill/cancel job"},
			{"p", "Pause running job"},
			{"d", "Toggle draft/queue status"},
			{"g", "Start queued/draft job now or resume paused"},
			{"G", "Generate AI description"},
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
			{"d", "Toggle AI summaries"},
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

	labels := []string{"Host:", "Description:", "Command:", "Working Dir:", "GPU:", "CPU:", "Env Vars:"}
	for i, input := range m.inputs {
		label := labelStyle
		if i == m.inputFocus {
			label = focusedLabelStyle
		}
		b.WriteString(label.Render(labels[i]))
		b.WriteString(input.View())
		if i == inputCPUAllotment {
			if hint := m.cpuAllotmentHint(); hint != "" {
				b.WriteString(dimStyle.Render(" " + hint))
			}
		}
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

func (m Model) cpuAllotmentHint() string {
	hostName := strings.TrimSpace(m.inputs[inputHost].Value())
	if hostName == "" {
		return ""
	}
	host := m.findHostByName(hostName)
	if host == nil || host.CPUs == 0 {
		return ""
	}
	allotment, err := parseCPUAllotmentInput(m.inputs[inputCPUAllotment].Value())
	if err != nil {
		return "use 1-100 or default"
	}
	percent, ok := defaultAllotmentPercent(host)
	if !ok {
		return ""
	}
	if allotment != nil {
		percent = *allotment
	}
	cores := float64(host.CPUs) * float64(percent) / 100.0
	coreText := fmt.Sprintf("%.1f", cores)
	if rounded := math.Round(cores); math.Abs(cores-rounded) < 0.05 {
		coreText = fmt.Sprintf("%.0f", rounded)
	}
	return fmt.Sprintf("%d%% ≈ %s cores on %d-core host", percent, coreText, host.CPUs)
}

func (m Model) formatCPUAllotmentDisplay(job *db.Job) string {
	source := "default"
	if job.CPUAllotment != nil {
		source = "requested"
	}
	host := m.findHostByName(job.Host)
	if host == nil || host.CPUs == 0 {
		if source == "default" {
			return "default"
		}
		return fmt.Sprintf("%d%%", *job.CPUAllotment)
	}
	allotment := 0
	if job.CPUAllotment != nil {
		allotment = *job.CPUAllotment
	}
	if source == "default" {
		percent, ok := defaultAllotmentPercent(host)
		if !ok {
			return "default"
		}
		allotment = percent
	}
	cores := float64(host.CPUs) * float64(allotment) / 100.0
	coreText := fmt.Sprintf("%.1f", cores)
	if rounded := math.Round(cores); math.Abs(cores-rounded) < 0.05 {
		coreText = fmt.Sprintf("%.0f", rounded)
	}
	if source == "default" {
		return fmt.Sprintf("%d%% (%s cores on %d-core host, %s)", allotment, coreText, host.CPUs, source)
	}
	return fmt.Sprintf("%d%% (%s cores on %d-core host)", allotment, coreText, host.CPUs)
}

func defaultAllotmentPercent(host *Host) (int, bool) {
	if host == nil || host.CPUs == 0 {
		return 0, false
	}
	percent := int(math.Round(float64(defaultAllotmentCores) * 100.0 / float64(host.CPUs)))
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}
	return percent, true
}

func formatCPUPercent(value float64) string {
	rounded := math.Round(value)
	if math.Abs(value-rounded) < 0.05 {
		return fmt.Sprintf("%.0f%%", rounded)
	}
	return fmt.Sprintf("%.1f%%", value)
}

func (m Model) formatCPUUsageDisplay(job *db.Job) string {
	if job == nil || job.Metadata == nil || job.Metadata.CPU == nil {
		return ""
	}
	stats := job.Metadata.CPU
	parts := []string{}
	if job.Status == db.StatusRunning && stats.Latest != nil {
		parts = append(parts, fmt.Sprintf("latest %s", formatCPUPercent(*stats.Latest)))
	}
	if stats.Max != nil {
		parts = append(parts, fmt.Sprintf("max %s", formatCPUPercent(*stats.Max)))
	}
	if stats.Mean != nil {
		parts = append(parts, fmt.Sprintf("mean %s", formatCPUPercent(*stats.Mean)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func (m Model) findHostByName(name string) *Host {
	for _, host := range m.hosts {
		if host.Name == name {
			return host
		}
	}
	return nil
}

func (m Model) renderJobList(height int) string {
	panelWidth := m.width - 2
	if panelWidth < 0 {
		panelWidth = 0
	}
	frameWidth, frameHeight := listPanelStyle.GetFrameSize()
	contentWidth := panelWidth - frameWidth
	if contentWidth < 20 {
		contentWidth = 20
	}

	var rows []string

	hostSummary := m.renderInlineHostSummary(contentWidth)
	if hostSummary != "" {
		rows = append(rows, hostSummary)
	}

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %-4s %s",
		"ID", "HOST", "STATUS", "TIME", "GPU", "DESCRIPTION")
	headerIndex := len(rows)
	rows = append(rows, headerStyle.Render(header))
	filterLabel := fmt.Sprintf(" View: %s | Host: %s | Sort: %s", jobFilterDescription(m.jobFilter), hostFilterDescription(m), m.jobSort)
	filterLine := dimStyle.Render(filterLabel)

	if len(m.jobs) == 0 {
		rows = append(rows, dimStyle.Render(" No jobs match this view"))
		rows = append(rows, filterLine)
		content := strings.Join(rows, "\n")
		return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
	}

	// Render jobs manually using list's paginator for scroll offset
	contentTarget := height - frameHeight
	if contentTarget < 0 {
		contentTarget = 0
	}
	contentHeight := contentTarget - 2 // header + filter
	if hostSummary != "" {
		contentHeight--
	}
	if contentHeight < 0 {
		contentHeight = 0
	}
	start, end := m.jobList.Paginator.GetSliceBounds(len(m.jobs))
	if end > len(m.jobs) {
		end = len(m.jobs)
	}
	// Limit to visible height
	if end-start > contentHeight {
		end = start + contentHeight
	}

	// Check if any visible jobs have GPU specified
	showGPU := false
	for i := start; i < end; i++ {
		if m.jobs[i].GetGPU() != "" {
			showGPU = true
			break
		}
	}

	// Update header based on GPU column visibility
	if showGPU {
		header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %-4s %s",
			"ID", "HOST", "STATUS", "TIME", "GPU", "DESCRIPTION")
		rows[headerIndex] = headerStyle.Render(header)
	} else {
		header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %s",
			"ID", "HOST", "STATUS", "TIME", "DESCRIPTION")
		rows[headerIndex] = headerStyle.Render(header)
	}

	selectedIdx := m.jobList.Index()
	jobLines := 0
	for i := start; i < end; i++ {
		job := m.jobs[i]
		status := m.formatStatus(job)
		timeCol := formatJobTime(job)
		hostCol := fmt.Sprintf("%-10s", truncate(job.Host, 10))
		statusCol := status + strings.Repeat(" ", max(0, 12-lipgloss.Width(status)))
		timeColFormatted := fmt.Sprintf("%-12s", timeCol)

		prefixPlain := fmt.Sprintf(" %-4d %s %s %s ", job.ID, hostCol, statusCol, timeColFormatted)
		gpuField := ""
		if showGPU {
			gpu := job.GetGPU()
			if gpu == "" {
				gpu = "—"
			}
			gpuField = fmt.Sprintf("%-4s ", truncate(gpu, 4))
			prefixPlain += gpuField
		}

		descWidth := contentWidth - lipgloss.Width(prefixPlain)
		if descWidth < 5 {
			descWidth = 5
		}
		display := truncate(job.EffectiveDescription(), descWidth)
		generatedDesc := job.Description == "" && job.GeneratedDescription != ""

		rowStyle := m.styleForJob(job)
		selected := i == selectedIdx && m.jobSelectionActive
		if selected {
			rowStyle = rowStyle.Copy().Background(selectedBg)
		}

		idSegment := rowStyle.Render(fmt.Sprintf(" %-4d ", job.ID))
		hostSegment := rowStyle.Render(fmt.Sprintf("%s ", hostCol))
		statusSegment := rowStyle.Render(fmt.Sprintf("%s ", statusCol))
		timeSegment := rowStyle.Render(fmt.Sprintf("%s ", timeColFormatted))

		segments := []string{idSegment, hostSegment, statusSegment, timeSegment}
		if showGPU {
			segments = append(segments, rowStyle.Render(gpuField))
		}

		descStyle := rowStyle
		if generatedDesc {
			descStyle = descStyle.Copy().Italic(true)
		}
		segments = append(segments, descStyle.Render(display))

		line := lipgloss.JoinHorizontal(lipgloss.Left, segments...)
		lineWidth := lipgloss.Width(line)
		if lineWidth < contentWidth {
			line += rowStyle.Render(strings.Repeat(" ", contentWidth-lineWidth))
		}
		rows = append(rows, line)
		jobLines++
	}

	if jobLines < contentHeight {
		blank := strings.Repeat(" ", contentWidth)
		for i := jobLines; i < contentHeight; i++ {
			rows = append(rows, blank)
		}
	}

	rows = append(rows, filterLine)

	if len(rows) < contentTarget {
		pad := strings.Repeat(" ", contentWidth)
		for len(rows) < contentTarget {
			rows = append(rows, pad)
		}
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

func (m Model) renderInlineHostSummary(maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	hostCount := 0
	for _, host := range m.hosts {
		if host != nil {
			hostCount++
		}
	}

	if hostCount == 0 {
		if len(m.hosts) == 0 {
			return dimStyle.Render(" Hosts: no hosts configured")
		}
		return dimStyle.Render(" Hosts: no host data")
	}

	label := labelStyle.Render("Hosts")
	labelWidth := lipgloss.Width(label)
	separatorWidth := 2 * (hostCount - 1)
	segmentWidthAvailable := maxWidth - labelWidth - 1
	if segmentWidthAvailable <= 0 {
		return truncate(label, maxWidth)
	}

	// Select format based on available width
	format := hostSummaryFull
	widthPerHost := (segmentWidthAvailable - separatorWidth) / hostCount
	if widthPerHost < hostSummaryFullWidth {
		format = hostSummaryAbbrev
	}

	segments := make([]string, 0, hostCount)
	for _, host := range m.hosts {
		if host == nil {
			continue
		}
		segments = append(segments, m.renderHostSummarySegment(host, format))
	}

	segmentsText := strings.Join(segments, "  ")
	content := lipgloss.JoinHorizontal(
		lipgloss.Left,
		label,
		" ",
		segmentsText,
	)

	if lipgloss.Width(content) <= maxWidth {
		return content
	}

	tickerBase := segmentsText + hostSummaryTickerGap
	tickerWidth := lipgloss.Width(tickerBase)
	if tickerWidth == 0 {
		return truncate(content, maxWidth)
	}

	offset := m.hostSummaryTickerOffset % tickerWidth
	end := offset + segmentWidthAvailable
	segmentView := ""
	if end <= tickerWidth {
		segmentView = ansi.Cut(tickerBase, offset, end)
	} else {
		segmentView = ansi.Cut(tickerBase, offset, tickerWidth) + ansi.Cut(tickerBase, 0, end-tickerWidth)
	}

	viewWidth := lipgloss.Width(segmentView)
	if viewWidth < segmentWidthAvailable {
		segmentView += strings.Repeat(" ", segmentWidthAvailable-viewWidth)
	}

	return lipgloss.JoinHorizontal(
		lipgloss.Left,
		label,
		" ",
		segmentView,
	)
}

func (m Model) renderHostSummarySegment(host *Host, format hostSummaryFormat) string {
	statusSymbol, statusStyle := hostStatusIndicator(host)

	// Consider data stale if LastCheck is more than 5 minutes old
	const staleThreshold = 5 * time.Minute
	isStale := !host.LastCheck.IsZero() && time.Since(host.LastCheck) > staleThreshold

	nameStyle := hostSummaryNameStyle
	if host.Status != HostStatusOnline || isStale {
		nameStyle = hostSummaryOfflineStyle.Copy()
	}

	// Highlight host name in red if low disk space
	if host.HasLowDiskSpace() {
		nameStyle = hostSummaryCriticalStyle
	}

	name := nameStyle.Render(truncate(host.Name, 12))

	// Determine whether we have metrics to show.
	// Online hosts always show metrics. Offline hosts show stale metrics
	// (preserved from the last successful check) until the stale threshold.
	hasMetrics := host.Status == HostStatusOnline
	if !hasMetrics && !isStale && host.LoadAvg != "" {
		// Recently went offline but we still have metrics from last check
		hasMetrics = true
	}

	if !hasMetrics {
		return statusStyle.Render(statusSymbol) + " " + name
	}

	// Show stats (fresh or stale)
	cpuPct, cpuOK := hostCPULoadPercent(host)
	memPct, memOK := hostMemUsagePercent(host)
	gpuPct, gpuOK := hostGPULoadPercent(host)

	var cpuText, memText, gpuText, diskText string
	if format == hostSummaryAbbrev {
		cpuText = formatHostSummaryMetricAbbrev("C", cpuPct, cpuOK)
		memText = formatHostSummaryMetricAbbrev("R", memPct, memOK)
		gpuText = formatHostSummaryMetricAbbrev("G", gpuPct, gpuOK)
		diskText = formatDiskSummaryAbbrev(host)
	} else {
		cpuText = formatHostSummaryMetric("CPU", cpuPct, cpuOK)
		memText = formatHostSummaryMetric("RAM", memPct, memOK)
		gpuText = formatHostSummaryMetric("GPU", gpuPct, gpuOK)
		diskText = formatDiskSummary(host)
	}

	// Use italic style for stale metrics to indicate they're not fresh
	applyStyle := func(base lipgloss.Style, text string) string {
		if host.Status != HostStatusOnline {
			return base.Italic(true).Render(text)
		}
		return base.Render(text)
	}

	cpuStyle := hostSummaryStyleForMetric(cpuPct, cpuOK)
	memStyle := hostSummaryStyleForMetric(memPct, memOK)
	gpuStyle := hostSummaryStyleForMetric(gpuPct, gpuOK)
	diskStyle := hostSummaryNormalStyle
	if host.HasLowDiskSpace() {
		diskStyle = hostSummaryCriticalStyle
	}

	fields := []string{
		statusStyle.Render(statusSymbol),
		name,
		applyStyle(cpuStyle, cpuText),
		applyStyle(memStyle, memText),
		applyStyle(gpuStyle, gpuText),
	}

	// Only add disk if we have disk info
	if diskText != "" {
		fields = append(fields, applyStyle(diskStyle, diskText))
	}

	return strings.Join(fields, " ")
}

func hostStatusIndicator(host *Host) (string, lipgloss.Style) {
	switch host.Status {
	case HostStatusOnline:
		return "●", hostSummaryNormalStyle
	case HostStatusChecking:
		return "◐", hostSummaryWarningStyle
	case HostStatusOffline:
		return "○", hostSummaryOfflineStyle
	default:
		return "?", hostSummaryWarningStyle
	}
}

func hostSummaryStyleForMetric(pct int, ok bool) lipgloss.Style {
	if !ok {
		return dimStyle
	}
	switch {
	case pct >= 90:
		return hostSummaryCriticalStyle
	case pct >= 70:
		return hostSummaryWarningStyle
	default:
		return hostSummaryNormalStyle
	}
}

func formatHostSummaryMetric(label string, pct int, ok bool) string {
	if !ok {
		return label + "--"
	}
	return fmt.Sprintf("%s%3d%%", label, pct)
}

// formatHostSummaryMetricAbbrev formats a metric with abbreviated label (e.g., "C45%")
func formatHostSummaryMetricAbbrev(label string, pct int, ok bool) string {
	if !ok {
		return label + "--"
	}
	return fmt.Sprintf("%s%d%%", label, pct)
}

// formatDiskSummary formats disk free space for the host summary (e.g., "Disk:45G")
func formatDiskSummary(host *Host) string {
	if host.DiskFree == 0 {
		return ""
	}
	return "Disk:" + host.DiskFreeSummary()
}

// formatDiskSummaryAbbrev formats disk free space abbreviated (e.g., "D45G")
func formatDiskSummaryAbbrev(host *Host) string {
	if host.DiskFree == 0 {
		return ""
	}
	return "D" + host.DiskFreeSummary()
}

func (m Model) renderLogPanel(height int) string {
	// Render based on active tab
	switch m.detailTab {
	case DetailTabLogs:
		return m.renderLogsOnly(height)
	case DetailTabCPU:
		return m.renderJobCPUTop(height)
	default:
		return m.renderJobDetails(height)
	}
}

// renderTabHeader renders the "Details  Logs" tab header as visual tabs
func (m Model) renderTabHeader() string {
	tabs := []struct {
		label string
		tab   DetailTab
	}{
		{"Details", DetailTabDetails},
		{"Logs", DetailTabLogs},
		{"CPU", DetailTabCPU},
	}

	var rendered []string
	for _, t := range tabs {
		if m.detailTab == t.tab {
			rendered = append(rendered, activeTabStyle.Render(t.label))
		} else {
			rendered = append(rendered, inactiveTabStyle.Render(t.label))
		}
	}

	// Add hint about Tab key switching
	hint := dimStyle.Render(" (Tab to switch)")

	return strings.Join(rendered, " ") + hint
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

func (m Model) renderJobCPUTop(height int) string {
	job := m.getTargetJob()
	panelHeader := m.renderTabHeader() + "\n"
	if job == nil {
		panelContent := panelHeader + dimStyle.Render("No job selected")
		return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
	}

	var b strings.Builder
	host := job.Host
	b.WriteString(fmt.Sprintf("Top CPU processes on %s\n\n", host))

	switch {
	case m.jobCPUTopLoading && (m.jobCPUTopRequestedHost == host):
		b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		b.WriteString("\n")
	case m.jobCPUTopError != "" && m.jobCPUTopDataHost == host:
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed to fetch: %s", m.jobCPUTopError)))
		b.WriteString("\n")
	case m.jobCPUTopDataHost == host && len(m.jobCPUTopEntries) > 0:
		userWidth := 10
		cpuWidth := 6
		panelWidth := m.width - 6
		if panelWidth < 40 {
			panelWidth = 40
		}
		processWidth := panelWidth - (userWidth + cpuWidth + 4)
		if processWidth < 20 {
			processWidth = 20
		}

		header := fmt.Sprintf(" %-*s %*s %s", userWidth, "User", cpuWidth, "%CPU", "Process")
		b.WriteString(lipgloss.NewStyle().Bold(true).Render(header))
		b.WriteString("\n")

		availableLines := height - 4
		maxRows := len(m.jobCPUTopEntries)
		if availableLines > 0 && maxRows > availableLines-2 {
			maxRows = availableLines - 2
		}
		if maxRows < 0 {
			maxRows = 0
		}

		totalCPU := 0.0
		for i, proc := range m.jobCPUTopEntries {
			if i >= maxRows {
				break
			}
			process := proc.Command
			if process == "" {
				process = fmt.Sprintf("PID %d", proc.PID)
			}
			process = truncate(shortenCommandPath(process), processWidth)
			line := fmt.Sprintf(" %-*s %*.1f %s",
				userWidth, truncate(proc.User, userWidth),
				cpuWidth, proc.CPU,
				process,
			)
			b.WriteString(line)
			b.WriteString("\n")
			totalCPU += proc.CPU
		}

		coreEstimate := ""
		if totalCPU > 0 {
			coreEstimate = fmt.Sprintf("(~%.0f cores)", totalCPU/100.0)
		}
		totalLine := fmt.Sprintf(" %-*s %*.1f %s",
			userWidth, "TOTAL",
			cpuWidth, totalCPU,
			dimStyle.Render(coreEstimate),
		)
		b.WriteString(totalLine)
		b.WriteString("\n")

		remaining := len(m.jobCPUTopEntries) - maxRows
		if remaining > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			b.WriteString("\n")
		}

		if !m.jobCPUTopUpdated.IsZero() {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("Updated %s ago", time.Since(m.jobCPUTopUpdated).Truncate(time.Second))))
		}
	case m.jobCPUTopDataHost == host && len(m.jobCPUTopEntries) == 0:
		b.WriteString(dimStyle.Render("No active processes reported"))
		b.WriteString("\n")
	default:
		if m.jobCPUTopRequestedHost != "" {
			b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		} else {
			b.WriteString(dimStyle.Render("Select a job to load CPU data"))
		}
		b.WriteString("\n")
	}

	panelContent := panelHeader + b.String()
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderJobDetails(height int) string {
	if !m.jobSelectionActive {
		if m.detailViewport != nil {
			m.detailViewport.SetContent("")
		}
		return m.renderJobSummaryPanel(height)
	}

	panelHeader := m.renderTabHeader()
	job := m.getTargetJob()

	if job == nil {
		emptyContent := dimStyle.Render("No jobs to display")
		if m.detailViewport != nil {
			m.detailViewport.SetContent(emptyContent)
		}
		panelContent := panelHeader + "\n" + emptyContent
		return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
	}

	viewportWidth := m.width - 6
	if viewportWidth < 1 {
		viewportWidth = 1
	}
	viewportHeight := height - 4
	if viewportHeight < 1 {
		viewportHeight = 1
	}

	detailBody := m.jobDetailContent(job)
	m.detailViewport.SetContent(detailBody)

	vp := *m.detailViewport
	vp.Width = viewportWidth
	vp.Height = viewportHeight

	body := vp.View()
	if body == "" {
		body = dimStyle.Render("No jobs to display")
	}

	panelContent := panelHeader + "\n" + body
	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) jobDetailContent(job *db.Job) string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Width(11)
	valueStyle := lipgloss.NewStyle()
	headerStyle := lipgloss.NewStyle().Bold(true)
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Underline(true)

	// Header line with job ID and host (progress shown in Progress section)
	b.WriteString(headerStyle.Render(fmt.Sprintf("Job %d", job.ID)))
	b.WriteString(dimStyle.Render(" on "))
	b.WriteString(headerStyle.Render(job.Host))

	// Show dependency info on same line if present
	if depSpec := m.jobDependencies[job.ID]; depSpec != "" {
		if strings.HasSuffix(depSpec, "+") {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s)", strings.TrimSuffix(depSpec, "+"))))
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" (runs after job %s succeeds)", depSpec)))
		}
	}
	b.WriteString("\n\n")

	// Description (if any)
	if job.Description != "" {
		b.WriteString(labelStyle.Render("Desc"))
		descStyle := lipgloss.NewStyle().Italic(true)
		b.WriteString(descStyle.Render(job.Description))
		b.WriteString("\n")
	}
	if len(job.Tags) > 0 {
		b.WriteString(labelStyle.Render("Tags"))
		b.WriteString(valueStyle.Render(strings.Join(job.Tags, ", ")))
		b.WriteString("\n")
	}

	// Directory (show remote home when unset)
	b.WriteString(labelStyle.Render("Directory"))
	b.WriteString(valueStyle.Render(job.DisplayWorkingDir()))
	b.WriteString("\n")

	// Command (most important) - wrap and indent continuation lines, but limit height
	b.WriteString(labelStyle.Render("Command"))
	cmd := job.EffectiveCommand()
	labelWidth := 11
	// Panel content width is m.width - 6 (borders + padding), minus label
	availableWidth := m.width - 6 - labelWidth
	if availableWidth < 20 {
		availableWidth = 60 // fallback if window too narrow
	}
	// Wrap first (on plain text), then apply syntax highlighting to each line
	wrappedCmd := wrapTextWithIndent(cmd, availableWidth, labelWidth)
	// Limit command display to 4 lines max to avoid overwhelming the details panel
	const maxCmdLines = 4
	cmdLines := strings.Split(wrappedCmd, "\n")
	if len(cmdLines) > maxCmdLines {
		cmdLines = cmdLines[:maxCmdLines]
		cmdLines = append(cmdLines, strings.Repeat(" ", labelWidth)+"...")
	}
	// Apply syntax highlighting to each line
	for i, line := range cmdLines {
		cmdLines[i] = highlightCommand(line)
	}
	b.WriteString(strings.Join(cmdLines, "\n"))
	b.WriteString("\n")

	// Environment variables (if any)
	envVars := job.ParseExportVars()
	if len(envVars) > 0 {
		b.WriteString(labelStyle.Render("Env"))
		b.WriteString(valueStyle.Render(strings.Join(envVars, ", ")))
		b.WriteString("\n")
	}

	if job.Status == db.StatusQueued || job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
		b.WriteString(labelStyle.Render("CPU"))
		b.WriteString(valueStyle.Render(m.formatCPUAllotmentDisplay(job)))
		b.WriteString("\n")
	}
	if cpuUsage := m.formatCPUUsageDisplay(job); cpuUsage != "" {
		b.WriteString(labelStyle.Render("CPU Usage"))
		b.WriteString(valueStyle.Render(cpuUsage))
		b.WriteString("\n")
	}

	_ = m.writeJobTimingSection(&b, job, labelStyle, valueStyle)

	// Exit status
	if job.Status == db.StatusCompleted && job.ExitCode != nil {
		b.WriteString(labelStyle.Render("Exit"))
		if *job.ExitCode == 0 {
			b.WriteString(completedStyle.Render("0 (success)"))
		} else {
			b.WriteString(failedStyle.Render(fmt.Sprintf("%d (failed)", *job.ExitCode)))
		}
		b.WriteString("\n")
	} else if job.Status == db.StatusKilled {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(deadStyle.Render("killed"))
		b.WriteString("\n")
	} else if job.Status == db.StatusCanceled {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(deadStyle.Render("canceled"))
		b.WriteString("\n")
	} else if job.Status == db.StatusDead {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(failedStyle.Render("failed to start"))
		b.WriteString("\n")
		if job.ErrorMessage != "" {
			b.WriteString(labelStyle.Render("Error"))
			b.WriteString(errorStyle.Render(job.ErrorMessage))
			b.WriteString("\n")
		}
	} else if job.Status == db.StatusFailed {
		b.WriteString(labelStyle.Render("Exit"))
		b.WriteString(failedStyle.Render("crashed"))
		b.WriteString("\n")
	}

	// Process stats and progress section for running jobs
	// Reserve a fixed number of lines to prevent log preview from jumping
	if job.Status == db.StatusRunning {
		const statsReservedLines = 7 // header + CPU + Memory + Threads + 2 GPUs + Progress
		linesWritten := 0

		b.WriteString("\n")
		b.WriteString(sectionStyle.Render("Process Stats"))
		b.WriteString("\n")
		linesWritten++ // header

		if m.processStats != nil && m.processStatsJobID == job.ID {
			// CPU: percentage and cumulative time
			if m.processStats.CPUPct > 0 || m.processStats.CPUUser != "" {
				b.WriteString(labelStyle.Render("CPU"))
				cpuVal := ""
				if m.processStats.CPUPct > 0 {
					cpuVal = fmt.Sprintf("%.1f%%", m.processStats.CPUPct)
				}
				if m.processStats.CPUUser != "" {
					if cpuVal != "" {
						cpuVal += " "
					}
					cpuVal += fmt.Sprintf("(%s user, %s sys)", m.processStats.CPUUser, m.processStats.CPUSys)
				}
				b.WriteString(valueStyle.Render(cpuVal))
				b.WriteString("\n")
				linesWritten++
			}

			// Memory: absolute and percentage
			if m.processStats.MemoryRSS != "" {
				b.WriteString(labelStyle.Render("Memory"))
				mem := m.processStats.MemoryRSS
				if m.processStats.MemoryPct != "" {
					mem += " (" + m.processStats.MemoryPct + ")"
				}
				b.WriteString(valueStyle.Render(mem))
				b.WriteString("\n")
				linesWritten++
			}

			// Threads
			if m.processStats.Threads > 0 {
				b.WriteString(labelStyle.Render("Threads"))
				b.WriteString(valueStyle.Render(fmt.Sprintf("%d", m.processStats.Threads)))
				b.WriteString("\n")
				linesWritten++
			}

			// GPUs with utilization and memory
			if len(m.processStats.GPUs) > 0 {
				for _, gpu := range m.processStats.GPUs {
					b.WriteString(labelStyle.Render(fmt.Sprintf("GPU %d", gpu.Index)))
					gpuVal := ""
					if gpu.Utilization > 0 {
						gpuVal += fmt.Sprintf("%d%% util, ", gpu.Utilization)
					}
					gpuVal += gpu.MemUsed
					b.WriteString(valueStyle.Render(gpuVal))
					b.WriteString("\n")
					linesWritten++
				}
			}
		} else {
			// Stats not loaded yet - show placeholder
			b.WriteString(dimStyle.Render("Loading..."))
			b.WriteString("\n")
			linesWritten++
		}

		// Progress (on same reserved block)
		if prog, ok := m.jobProgress[job.ID]; ok {
			pct := prog.DisplayPercent()
			if pct >= 0 || prog.Total > 0 {
				b.WriteString(labelStyle.Render("Progress"))
				b.WriteString("  ")
				if pct >= 0 {
					b.WriteString(renderProgressBar(pct, 20))
					b.WriteString(fmt.Sprintf(" %d%%", pct))
				}
				if prog.Total > 0 {
					b.WriteString(fmt.Sprintf(" (%d/%d)", prog.Current, prog.Total))
				}
				b.WriteString("\n")
				linesWritten++
			}
		}

		// Pad with blank lines to reach reserved count
		for linesWritten < statsReservedLines {
			b.WriteString("\n")
			linesWritten++
		}
	}

	// Log preview section - show last few lines of log if available
	logPreview := ""
	if m.logContent != "" {
		logPreview = m.logContent
	} else if cached, ok := m.logCache[job.ID]; ok {
		logPreview = cached
	}
	if logPreview != "" {
		logPreview = processCarriageReturns(logPreview)

		const maxPreviewLines = 5
		allLines := strings.Split(strings.TrimSpace(logPreview), "\n")
		var previewLines []string
		for i := len(allLines) - 1; i >= 0 && len(previewLines) < maxPreviewLines; i-- {
			line := strings.TrimSpace(allLines[i])
			if line != "" {
				previewLines = append([]string{line}, previewLines...)
			}
		}

		if len(previewLines) > 0 {
			b.WriteString("\n")
			b.WriteString(sectionStyle.Render("Log (last lines)"))
			b.WriteString("\n")
			for _, line := range previewLines {
				// Truncate long lines to fit panel width
				maxWidth := m.width - 10
				if maxWidth < 40 {
					maxWidth = 40
				}
				if len(line) > maxWidth {
					line = line[:maxWidth-3] + "..."
				}
				b.WriteString(dimStyle.Render(line))
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

func (m Model) writeJobTimingSection(b *strings.Builder, job *db.Job, labelStyle, valueStyle lipgloss.Style) bool {
	wrote := false
	if job.CreatedAt > 0 || job.StartTime > 0 || job.EndTime != nil {
		if job.CreatedAt > 0 && (job.StartTime == 0 || job.StartTime-job.CreatedAt > 60) {
			createdTime := time.Unix(job.CreatedAt, 0)
			label := "Created"
			if job.Status == db.StatusQueued {
				label = "Queued"
			}
			b.WriteString(labelStyle.Render(label))
			b.WriteString(valueStyle.Render(formatDetailTimestamp(createdTime)))
			b.WriteString("\n")
			wrote = true
		}

		startTimestamp := job.StartTime
		if startTimestamp == 0 && job.CreatedAt > 0 && db.IsTerminalStatus(job.Status) {
			startTimestamp = job.CreatedAt
		}

		if startTimestamp > 0 {
			startTime := time.Unix(startTimestamp, 0)
			var endTime time.Time
			hasEnd := job.EndTime != nil
			if hasEnd {
				endTime = time.Unix(*job.EndTime, 0)
			}

			sameDay := hasEnd && sameLocalDay(startTime, endTime)

			if sameDay {
				startStr, suffix := formatDetailTimeParts(startTime)
				endStr, _ := formatDetailTimeParts(endTime)
				line := fmt.Sprintf("%s – %s%s", startStr, endStr, suffix)
				b.WriteString(labelStyle.Render("Start/End"))
				b.WriteString(valueStyle.Render(line))
				b.WriteString("\n")
			} else {
				b.WriteString(labelStyle.Render("Started"))
				b.WriteString(valueStyle.Render(formatDetailTimestamp(startTime)))
				b.WriteString("\n")

				if hasEnd {
					b.WriteString(labelStyle.Render("Ended"))
					b.WriteString(valueStyle.Render(formatDetailTimestamp(endTime)))
					b.WriteString("\n")
				}
			}
			wrote = true

			if job.Status == db.StatusRunning {
				elapsed := time.Since(startTime)
				b.WriteString(labelStyle.Render("Elapsed"))
				b.WriteString(valueStyle.Render(formatDuration(elapsed)))
				b.WriteString("\n")
			} else if hasEnd {
				duration := endTime.Sub(startTime)
				b.WriteString(labelStyle.Render("Duration"))
				b.WriteString(valueStyle.Render(formatDuration(duration)))
				b.WriteString("\n")
			}
		} else if job.EndTime != nil {
			endTime := time.Unix(*job.EndTime, 0)
			b.WriteString(labelStyle.Render("Ended"))
			b.WriteString(valueStyle.Render(formatDetailTimestamp(endTime)))
			b.WriteString("\n")
			wrote = true
		}
	}
	return wrote
}

func (m Model) renderJobSummaryPanel(height int) string {
	sectionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true).Underline(true)
	headerStyle := lipgloss.NewStyle().Bold(true)

	var b strings.Builder
	b.WriteString(sectionStyle.Render("Hosts Overview"))
	b.WriteString("\n")

	if len(m.hosts) == 0 {
		b.WriteString(dimStyle.Render("No hosts have reported yet. Run a job to gather host data."))
	} else {
		header := fmt.Sprintf(" %-12s %-10s %-16s %-5s %-7s %-6s %-5s %-5s",
			"HOST", "STATUS", "ARCH", "RUN", "QUEUE", "LOAD", "CPU", "RAM")
		b.WriteString(headerStyle.Render(header))
		b.WriteString("\n")

		for _, host := range m.hosts {
			running, _ := m.jobCountsForHost(host.Name)
			load := host.LoadAvgShort()
			if load == "" {
				load = "-"
			}
			line := fmt.Sprintf(" %-12s %-10s %-16s %-5d %-7s %-6s %-5s %-5s",
				truncate(host.Name, 12),
				strings.TrimSpace(m.formatHostStatus(host)),
				truncate(host.Arch, 16),
				running,
				m.queueSummaryForHost(host),
				load,
				host.CPUUtilization(),
				host.RAMUtilization(),
			)
			b.WriteString(m.styleForHostStatus(host.Status).Render(line))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(sectionStyle.Render("System Summary"))
	b.WriteString("\n")
	b.WriteString(m.renderSystemSummaryLines(m.width - 6))

	return logPanelStyle.Width(m.width - 2).Height(height).Render(b.String())
}

func (m Model) renderSystemSummaryLines(width int) string {
	if width < 20 {
		width = 20
	}
	if len(m.hosts) == 0 {
		return dimStyle.Render("No hosts have reported yet.")
	}
	if !m.showHostSummaries {
		return dimStyle.Render("AI host summaries are disabled (press d in Hosts view to enable).")
	}
	if m.llmGenerator == nil {
		return dimStyle.Render("Host summaries unavailable (LLM generator not running).")
	}
	var lines []string
	for _, host := range m.hosts {
		summary := strings.TrimSpace(m.hostSummaries[host.Name])
		switch {
		case summary == "" && m.hostSummaryPending[host.Name]:
			summary = "Generating summary..."
		case summary == "":
			summary = "No summary available yet."
		}
		wrapped := wrapText(summary, width-4)
		segments := strings.Split(wrapped, "\n")
		for i, segment := range segments {
			prefix := "  "
			if i == 0 {
				prefix = fmt.Sprintf("%s ", lipgloss.NewStyle().Bold(true).Render(truncate(host.Name, 12)))
			}
			lines = append(lines, prefix+segment)
		}
	}
	return strings.Join(lines, "\n")
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

// formatInlineRangeDuration renders a duration compactly for inline Start/End rows.
func formatInlineRangeDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Truncate(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		if s == 0 {
			return fmt.Sprintf("0h%02dm", m)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatCompactDuration renders a duration using up to two time units (e.g. "3m20s")
func formatCompactDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Truncate(time.Second)

	var parts []string
	const day = 24 * time.Hour

	if d >= day {
		days := d / day
		parts = append(parts, fmt.Sprintf("%dd", days))
		d -= days * day
	}
	if len(parts) < 2 && d >= time.Hour {
		hours := d / time.Hour
		parts = append(parts, fmt.Sprintf("%dh", hours))
		d -= hours * time.Hour
	}
	if len(parts) < 2 && d >= time.Minute {
		minutes := d / time.Minute
		parts = append(parts, fmt.Sprintf("%dm", minutes))
		d -= minutes * time.Minute
	}
	if len(parts) < 2 && d >= time.Second {
		seconds := d / time.Second
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, "")
}

func formatDetailTimestamp(t time.Time) string {
	timeStr, suffix := formatDetailTimeParts(t)
	return timeStr + suffix
}

func formatDetailTimeParts(t time.Time) (string, string) {
	now := time.Now()
	loc := now.Location()
	tt := t.In(loc)
	timeStr := tt.Format("15:04:05")

	startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	startYesterday := startToday.Add(-24 * time.Hour)
	ttDay := time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, loc)

	switch {
	case !ttDay.Before(startToday):
		return timeStr, ""
	case !ttDay.Before(startYesterday):
		return timeStr, " (yesterday)"
	default:
		dateStr := tt.Format("01/02")
		if tt.Year() != now.Year() {
			dateStr += tt.Format("/2006")
		}
		return timeStr, " (" + dateStr + ")"
	}
}

func sameLocalDay(a, b time.Time) bool {
	a = a.In(time.Now().Location())
	b = b.In(time.Now().Location())
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
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

// wrapText wraps text to the given width without indentation
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}

	lineLen := 0
	for i, word := range words {
		wordLen := len(word)
		if i == 0 {
			result.WriteString(word)
			lineLen = wordLen
		} else if lineLen+1+wordLen <= width {
			result.WriteString(" ")
			result.WriteString(word)
			lineLen += 1 + wordLen
		} else {
			result.WriteString("\n")
			result.WriteString(word)
			lineLen = wordLen
		}
	}

	return result.String()
}

// Chroma syntax highlighting for shell commands
var (
	shellLexer     chroma.Lexer
	shellFormatter chroma.Formatter
	shellStyle     *chroma.Style
)

func init() {
	// Use bash lexer for shell command highlighting
	shellLexer = lexers.Get("bash")
	if shellLexer == nil {
		shellLexer = lexers.Fallback
	}
	shellLexer = chroma.Coalesce(shellLexer)

	// Use terminal256 formatter for ANSI output
	shellFormatter = formatters.Get("terminal256")
	if shellFormatter == nil {
		shellFormatter = formatters.Fallback
	}

	// Use pygments style (works well with light backgrounds)
	shellStyle = styles.Get("pygments")
	if shellStyle == nil {
		shellStyle = styles.Fallback
	}
}

// highlightCommand applies syntax coloring to a shell command using chroma.
func highlightCommand(cmd string) string {
	if cmd == "" {
		return cmd
	}

	iterator, err := shellLexer.Tokenise(nil, cmd)
	if err != nil {
		return cmd // Fall back to plain text on error
	}

	var buf bytes.Buffer
	err = shellFormatter.Format(&buf, shellStyle, iterator)
	if err != nil {
		return cmd // Fall back to plain text on error
	}

	// Remove trailing newline that chroma adds
	result := buf.String()
	result = strings.TrimSuffix(result, "\n")
	return result
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
	help := helpStyle.Render("?:help q:quit ↑/↓:nav space/b/t:page ←/→:views l:logs f:filter H:host o:sort r:refresh n:new e:edit R:restart k:kill d:draft P:prune")

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
	header := fmt.Sprintf(" %-12s %-16s %-16s %-6s %-5s %-5s %-8s",
		"HOST", "STATUS", "ARCH", "QUEUE", "CPU", "RAM", "DISK")
	rows = append(rows, headerStyle.Render(header))

	if len(m.hosts) == 0 {
		rows = append(rows, dimStyle.Render(" No hosts found. Run a job first."))
	} else {
		// Hosts
		contentHeight := height - 4 // Account for borders and header
		rowCount := 0
		recentlyOnlineThreshold := time.Hour // Only show stats for hosts online within past hour
		for i, host := range m.hosts {
			if rowCount >= contentHeight {
				break
			}

			status := m.formatHostStatus(host)
			queue := m.queueSummaryForHost(host)
			arch := truncate(host.Arch, 16)
			if arch == "" {
				arch = "-"
			}

			// Only show stats for recently online hosts (within past hour)
			recentlyOnline := host.Status == HostStatusOnline ||
				(!host.LastCheck.IsZero() && time.Since(host.LastCheck) < recentlyOnlineThreshold)

			cpu := "-"
			ram := "-"
			disk := "-"
			if recentlyOnline {
				cpu = host.CPUUtilization()
				ram = host.RAMUtilization()
				disk = host.DiskFreeSummary()
				if disk == "" {
					disk = "-"
				}

				// Style CPU/RAM in red if >90%
				cpuPct := host.CPUUtilizationPct()
				ramPct := host.RAMUtilizationPct()
				if cpuPct > 90 {
					cpu = failedStyle.Render(cpu)
				}
				if ramPct > 90 {
					ram = failedStyle.Render(ram)
				}

				// Style disk in red if low (<5MB)
				if host.HasLowDiskSpace() {
					disk = failedStyle.Render(disk)
				}
			}

			line := fmt.Sprintf(" %-12s %-16s %-16s %-6s %-5s %-5s %-8s",
				truncate(host.Name, 12), status, arch, queue, cpu, ram, disk)

			if i == m.selectedHostIdx {
				line = selectedStyle.Width(m.width - 4).Render(line)
			} else {
				line = m.styleForHostStatus(host.Status).Render(line)
			}

			rows = append(rows, line)
			rowCount++

			// Show AI summary if enabled
			if m.showHostSummaries {
				if summary, ok := m.hostSummaries[host.Name]; ok && summary != "" {
					// Wrap summary to fit width, indent and italicize
					summaryStyle := lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("243"))
					summaryLines := wrapText(summary, m.width-10)
					for _, sl := range strings.Split(summaryLines, "\n") {
						if rowCount >= contentHeight {
							break
						}
						summaryLine := "    " + sl
						if i == m.selectedHostIdx {
							// Combine selected background with italic
							selectedSummaryStyle := summaryStyle.Background(selectedBg)
							summaryLine = selectedSummaryStyle.Width(m.width - 4).Render(summaryLine)
						} else {
							summaryLine = summaryStyle.Render(summaryLine)
						}
						rows = append(rows, summaryLine)
						rowCount++
					}
				} else if m.hostSummaryPending[host.Name] {
					// Show loading indicator
					if rowCount < contentHeight {
						pendingLine := "    " + dimStyle.Render("Generating summary...")
						rows = append(rows, pendingLine)
						rowCount++
					}
				}
			}
		}
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

// hostIndexAtRow maps a host list row (excluding header/borders) to the host index.
// It mirrors renderHostList's logic so clicks on wrapped summary rows select the owner host.
func (m Model) hostIndexAtRow(row, height int) int {
	contentHeight := height - 4
	if row < 0 || contentHeight <= 0 || row >= contentHeight {
		return -1
	}

	rowCount := 0
	for idx, host := range m.hosts {
		if rowCount >= contentHeight {
			break
		}

		startRow := rowCount
		rowCount++ // Account for the host's main row

		if m.showHostSummaries {
			if summary, ok := m.hostSummaries[host.Name]; ok && summary != "" {
				summaryLines := wrapText(summary, m.width-10)
				for range strings.Split(summaryLines, "\n") {
					if rowCount >= contentHeight {
						break
					}
					rowCount++
				}
			} else if m.hostSummaryPending[host.Name] && rowCount < contentHeight {
				rowCount++
			}
		}

		if row >= startRow && row < rowCount {
			return idx
		}
	}
	return -1
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
				case db.StatusRunning, db.StatusStarting, db.StatusPaused:
					runningCount++
				case db.StatusQueued:
					queuedCount++
				case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
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
			if host.JqMissing {
				lines = append(lines, "  ⚠ jq missing: Queue runner cannot start")
				lines = append(lines, "    Install: curl -sL https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64 -o ~/.local/bin/jq && chmod +x ~/.local/bin/jq")
			} else if host.QueueRunnerActive {
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

func (m Model) renderHostCPUTop(height int) string {
	if len(m.hosts) == 0 || m.selectedHostIdx >= len(m.hosts) {
		return dimStyle.Render("No host selected")
	}
	host := m.hosts[m.selectedHostIdx]
	hostName := host.Name

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Top CPU processes on %s\n\n", hostName))

	switch {
	case m.hostCPUTopLoading && m.hostCPUTopRequestedHost == hostName:
		b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		b.WriteString("\n")
	case m.hostCPUTopError != "" && m.hostCPUTopDataHost == hostName:
		b.WriteString(errorStyle.Render(fmt.Sprintf("Failed to fetch: %s", m.hostCPUTopError)))
		b.WriteString("\n")
	case m.hostCPUTopDataHost == hostName && len(m.hostCPUTopEntries) > 0:
		userWidth := 10
		cpuWidth := 6
		jobWidth := 8
		panelWidth := m.width - 6
		if panelWidth < 40 {
			panelWidth = 40
		}
		processWidth := panelWidth - (userWidth + cpuWidth + jobWidth + 6)
		if processWidth < 20 {
			processWidth = 20
		}

		header := fmt.Sprintf(" %-*s %*s %-*s %s", userWidth, "User", cpuWidth, "%CPU", jobWidth, "Job", "Process")
		b.WriteString(lipgloss.NewStyle().Bold(true).Render(header))
		b.WriteString("\n")

		availableLines := height - 4
		maxRows := len(m.hostCPUTopEntries)
		if availableLines > 0 && maxRows > availableLines-2 {
			maxRows = availableLines - 2
		}
		if maxRows < 0 {
			maxRows = 0
		}

		totalCPU := 0.0
		for i, proc := range m.hostCPUTopEntries {
			if i >= maxRows {
				break
			}
			jobLabel := "—"
			if proc.JobID > 0 {
				jobLabel = fmt.Sprintf("#%d", proc.JobID)
			}
			process := proc.Command
			if process == "" {
				process = fmt.Sprintf("PID %d", proc.PID)
			}
			process = truncate(shortenCommandPath(process), processWidth)
			line := fmt.Sprintf(" %-*s %*.1f %-*s %s",
				userWidth, truncate(proc.User, userWidth),
				cpuWidth, proc.CPU,
				jobWidth, jobLabel,
				process,
			)
			b.WriteString(line)
			b.WriteString("\n")
			totalCPU += proc.CPU
		}

		coreEstimate := ""
		if totalCPU > 0 {
			coreEstimate = fmt.Sprintf("(~%.0f cores)", totalCPU/100.0)
		}
		totalLine := fmt.Sprintf(" %-*s %*.1f %-*s %s",
			userWidth, "TOTAL",
			cpuWidth, totalCPU,
			jobWidth, "",
			dimStyle.Render(coreEstimate),
		)
		b.WriteString(totalLine)
		b.WriteString("\n")

		remaining := len(m.hostCPUTopEntries) - maxRows
		if remaining > 0 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			b.WriteString("\n")
		}

		if !m.hostCPUTopUpdated.IsZero() {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("Updated %s ago", time.Since(m.hostCPUTopUpdated).Truncate(time.Second))))
		}
	case m.hostCPUTopDataHost == hostName && len(m.hostCPUTopEntries) == 0:
		b.WriteString(dimStyle.Render("No active processes reported"))
		b.WriteString("\n")
	default:
		if m.hostCPUTopRequestedHost != "" {
			b.WriteString(dimStyle.Render(m.spinner.View() + " Loading..."))
		} else {
			b.WriteString(dimStyle.Render("Select a host to load CPU data"))
		}
		b.WriteString("\n")
	}

	return b.String()
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

	// CPU tab
	cpuLabel := "CPU"
	if m.hostDetailTab == HostDetailTabCPU {
		tabs = append(tabs, activeTabStyle.Render(cpuLabel))
	} else {
		tabs = append(tabs, inactiveTabStyle.Render(cpuLabel))
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
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
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
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused
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
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		isRunning := job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused
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
		const (
			gpuHeaderFmt = "%-4s %-5s %-7s %-6s %-6s %-22s %s"
			gpuRowFmt    = "%-4d %-5d %-7d %-6s %-6s %-22s %s"
		)
		// Combined table with job counts and GPU stats
		lines = append(lines, fmt.Sprintf(gpuHeaderFmt, "GPU", "Run", "Queue", "Temp", "Util", "Memory", "Name"))
		lines = append(lines, fmt.Sprintf(gpuHeaderFmt, "───", "───", "─────", "────", "────", "──────────────────────", "────"))

		for _, gpuIdx := range gpuIndices {
			counts := gpuJobCounts[gpuIdx]
			var gpu *GPUInfo
			for i := range host.GPUs {
				if host.GPUs[i].Index == gpuIdx {
					gpu = &host.GPUs[i]
					break
				}
			}

			temp := "-"
			util := "-"
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
			lines = append(lines, fmt.Sprintf(gpuRowFmt,
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
		if job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused && job.Status != db.StatusQueued {
			continue
		}

		jobGPUs := parseGPUIndices(job.GetGPU())
		for _, idx := range jobGPUs {
			if idx == gpuIdx {
				if job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused {
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
			desc := truncate(job.EffectiveDescription(), 50)
			if job.Description == "" && job.GeneratedDescription != "" {
				desc = lipgloss.NewStyle().Italic(true).Render(desc)
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
			desc := truncate(job.EffectiveDescription(), 50)
			if job.Description == "" && job.GeneratedDescription != "" {
				desc = lipgloss.NewStyle().Italic(true).Render(desc)
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
	case m.hostDetailTab == HostDetailTabCPU:
		content = m.renderHostCPUTop(height - 3)
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
		status := "○ offline"
		if !host.LastCheck.IsZero() {
			elapsed := time.Since(host.LastCheck)
			status = fmt.Sprintf("%s %s", status, formatCompactDuration(elapsed))
		}
		return status
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
	// Check if job has a pending (target) status from three-way merge model
	if job.PendingStatus != nil {
		return m.formatPendingStatusDisplay(*job.PendingStatus, job.Status)
	}

	switch job.Status {
	case db.StatusRunning:
		stale := m.isJobStatusStale(job)
		// Show progress percentage if available
		if prog, ok := m.jobProgress[job.ID]; ok {
			pct := prog.DisplayPercent()
			if pct >= 0 {
				text := fmt.Sprintf("● %3d%%", pct)
				if stale {
					return lipgloss.NewStyle().Italic(true).Render(text)
				}
				return text
			}
		}
		if stale {
			return "● running?"
		}
		return "● running"
	case db.StatusPaused:
		return "⏸ paused"
	case db.StatusStarting:
		return "◐ starting"
	case db.StatusCompleted:
		if job.ExitCode == nil {
			return "✔ completed"
		}
		if *job.ExitCode == 0 {
			return "✔ succeeded"
		}
		return fmt.Sprintf("✖ failed (%d)", *job.ExitCode)
	case db.StatusQueued:
		return "… queued"
	case db.StatusDead:
		return "✖ start failed"
	case db.StatusFailed:
		return "✖ crashed"
	case db.StatusKilled:
		return "✖ killed"
	case db.StatusCanceled:
		return "✖ canceled"
	case db.StatusDraft:
		return "  Draft"
	default:
		return job.Status
	}
}

func (m Model) areJobDependenciesMet(job *db.Job) (bool, []string) {
	return true, nil
}

func (m Model) styleForJob(job *db.Job) lipgloss.Style {
	if job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode != 0 {
		return failedStyle
	}
	if job.Status == db.StatusRunning || job.Status == db.StatusPaused {
		host := m.findHostByName(job.Host)
		if host != nil {
			if cpuPct, ok := hostCPULoadPercent(host); ok && cpuPct > 100 {
				return failedStyle
			}
		}
	}
	return m.styleForStatus(job.Status)
}

func (m Model) styleForStatus(status string) lipgloss.Style {
	switch status {
	case db.StatusRunning:
		return runningStyle
	case db.StatusPaused:
		return pendingStyle
	case db.StatusCompleted:
		return completedStyle
	case db.StatusDead:
		return deadStyle
	case db.StatusQueued:
		return queuedStyle
	case db.StatusFailed:
		return failedStyle
	case db.StatusKilled:
		return deadStyle
	case db.StatusCanceled:
		return deadStyle
	case db.StatusStarting:
		return pendingStyle
	default:
		return lipgloss.NewStyle()
	}
}

// formatPendingStatusDisplay shows pending (target) status with current status context.
// Uses ⧗ (hourglass) to indicate an operation is pending.
func (m Model) formatPendingStatusDisplay(pendingStatus, currentStatus string) string {
	switch pendingStatus {
	case db.StatusDead, db.StatusKilled:
		// Show what we're transitioning from
		switch currentStatus {
		case db.StatusRunning:
			return "⧗ killing"
		case db.StatusQueued:
			return "⧗ canceling"
		default:
			return "⧗ killing"
		}
	case db.StatusCanceled:
		return "⧗ canceling"
	case db.StatusRunning:
		if currentStatus == db.StatusPaused {
			return "⧗ resuming"
		}
		return "⧗ starting"
	case db.StatusQueued:
		return "⧗ queuing"
	case db.StatusPaused:
		return "⧗ pausing"
	default:
		return "⧗ " + pendingStatus
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
	return tea.Tick(m.syncActiveInterval, func(t time.Time) tea.Msg {
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

func (m Model) startDBWatcher() tea.Cmd {
	dbFile := db.Path()
	if dbFile == "" {
		return nil
	}
	dir := filepath.Dir(dbFile)
	targets := make(map[string]struct{})
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
			return dbWatcherReadyMsg{err: err}
		}
		if err := watcher.Add(dir); err != nil {
			watcher.Close()
			return dbWatcherReadyMsg{err: err}
		}
		return dbWatcherReadyMsg{
			watcher: watcher,
			targets: targets,
		}
	}
}

func (m Model) waitForMonitorEvent() tea.Cmd {
	if m.monitor == nil || m.monitorEvents == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-m.monitorEvents
		if !ok {
			return nil
		}
		return monitorEventMsg{event: event}
	}
}

func (m Model) refreshJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := db.ListJobs(m.database, "", "", 1000, nil, "")
		if err != nil {
			return jobsRefreshedMsg{err: err}
		}
		// Also fetch job dependencies for queue view
		deps, err := db.GetJobDependencyInfo(m.database)
		if err != nil {
			// Non-fatal, just ignore it
		}
		return jobsRefreshedMsg{jobs: jobs, jobDependencies: deps}
	}
}

func (m Model) waitForDBEvent() tea.Cmd {
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
					return dbWatchEventMsg{err: fmt.Errorf("db watcher closed")}
				}
				if !isWatchedDBFile(event.Name, targets) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				return dbWatchEventMsg{}
			case err, ok := <-watcher.Errors:
				if !ok {
					return dbWatchEventMsg{err: fmt.Errorf("db watcher error channel closed")}
				}
				return dbWatchEventMsg{err: err}
			}
		}
	}
}

func isWatchedDBFile(name string, targets map[string]struct{}) bool {
	if name == "" {
		return false
	}
	clean := filepath.Clean(name)
	_, ok := targets[clean]
	return ok
}

func (m *Model) applyJobFilter() {
	prevSelectedID := int64(0)
	selectedIdx := m.jobList.Index()
	if len(m.jobs) > 0 && selectedIdx >= 0 && selectedIdx < len(m.jobs) {
		prevSelectedID = m.jobs[selectedIdx].ID
	}

	var filtered []*db.Job
	for _, job := range m.allJobs {
		if jobMatchesFilter(job, m.jobFilter) && m.jobMatchesHostFilter(job) {
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

	if m.selectedJob != nil && (!jobMatchesFilter(m.selectedJob, m.jobFilter) || !m.jobMatchesHostFilter(m.selectedJob)) {
		m.detailTab = DetailTabDetails
		m.selectedJob = nil
		m.logContent = ""
		m.logStale = false
	}
}

// sortJobs sorts m.jobs in place according to m.jobSort
func (m *Model) sortJobs() {
	if m.jobFilter == jobFilterRecent {
		sortRecentJobs(m.jobs, m.jobSort)
		return
	}

	sortJobsByMode(m.jobs, m.jobSort)
}

// sortJobsByMode sorts jobs strictly according to the selected sort mode.
func sortJobsByMode(jobs []*db.Job, mode jobSortMode) {
	sort.Slice(jobs, func(i, j int) bool {
		return jobLessBySortMode(jobs[i], jobs[j], mode)
	})
}

// sortRecentJobs sorts jobs for Recent/Newest views:
// 1. Running jobs by start time
// 2. Queued jobs by queue order (order they will start)
// 3. Draft jobs
// 4. Terminal state jobs by completion time
func sortRecentJobs(jobs []*db.Job, mode jobSortMode) {
	sort.SliceStable(jobs, func(i, j int) bool {
		ji, jj := jobs[i], jobs[j]
		pi, pj := recentStatusPriority(ji), recentStatusPriority(jj)
		if pi != pj {
			return pi < pj
		}

		// Within same status group, apply appropriate ordering
		switch ji.Status {
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			// Running: most recently started first (for Newest), oldest first (for Oldest)
			if mode == jobSortOldest {
				return ji.StartTime < jj.StartTime
			}
			return ji.StartTime > jj.StartTime

		case db.StatusQueued:
			// Queued: by queue order (earliest queued = runs first, so show first)
			if ji.QueuedAt > 0 && jj.QueuedAt > 0 {
				return ji.QueuedAt < jj.QueuedAt
			}
			// Fallback to ID for legacy jobs
			return ji.ID < jj.ID

		case db.StatusDraft:
			// Draft: by creation time
			return jobLessBySortMode(ji, jj, mode)

		default:
			// Terminal states: by end time
			ti, tj := int64(0), int64(0)
			if ji.EndTime != nil {
				ti = *ji.EndTime
			}
			if jj.EndTime != nil {
				tj = *jj.EndTime
			}
			if ti != tj {
				if mode == jobSortOldest {
					return ti < tj
				}
				return ti > tj
			}
			// Fallback to standard sort
			return jobLessBySortMode(ji, jj, mode)
		}
	})
}

func recentStatusPriority(job *db.Job) int {
	switch job.Status {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return 0
	case db.StatusQueued:
		return 1
	case db.StatusDraft:
		return 2
	default:
		return 3
	}
}

func jobLessBySortMode(i, j *db.Job, mode jobSortMode) bool {
	switch mode {
	case jobSortNewest:
		return getJobSortTime(i) > getJobSortTime(j)
	case jobSortOldest:
		return getJobSortTime(i) < getJobSortTime(j)
	case jobSortIDDesc:
		return i.ID > j.ID
	case jobSortIDAsc:
		return i.ID < j.ID
	case jobSortQueueOrder:
		return jobQueueOrderLess(i, j)
	default:
		return getJobSortTime(i) > getJobSortTime(j)
	}
}

// getJobSortTime returns the time to use for sorting (start time, created time, or ID)
func getJobSortTime(job *db.Job) int64 {
	if job.StartTime > 0 {
		return job.StartTime
	}
	// For jobs that never started, use created_at if available
	if job.CreatedAt > 0 {
		return job.CreatedAt
	}
	// For legacy jobs without created_at, use ID as a proxy (higher ID = more recent)
	return job.ID
}

// jobQueueOrderLess returns true if job i should come before job j in queue order
func jobQueueOrderLess(i, j *db.Job) bool {
	// Status priority: queued/draft first, then running/starting, then completed/dead/failed
	statusPriority := func(job *db.Job) int {
		switch job.Status {
		case db.StatusQueued, db.StatusDraft:
			return 0
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
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
	// - Queued/Draft: by queued_at (earlier = runs first), falling back to ID for legacy jobs
	// - Running: by start time (oldest first = running longest)
	// - Completed: by end time (most recent first)
	if i.Status == db.StatusQueued || i.Status == db.StatusDraft {
		// For queued/draft jobs, use queued_at for proper queue order
		if i.QueuedAt > 0 && j.QueuedAt > 0 {
			return i.QueuedAt < j.QueuedAt
		}
		// Fallback to ID for legacy jobs without queued_at
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
	tabs := []DetailTab{DetailTabDetails, DetailTabLogs, DetailTabCPU}
	currentIdx := 0
	for i, tab := range tabs {
		if tab == m.detailTab {
			currentIdx = i
			break
		}
	}

	if forward {
		currentIdx = (currentIdx + 1) % len(tabs)
	} else {
		currentIdx = (currentIdx - 1 + len(tabs)) % len(tabs)
	}

	return m.switchToJobTab(tabs[currentIdx])
}

// switchToLogsTab switches to the Logs tab and fetches log content
func (m *Model) switchToLogsTab() (Model, tea.Cmd) {
	m.detailTab = DetailTabLogs
	m.jobSelectionActive = true
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
		if m.monitor != nil {
			m.monitor.WatchJobLog(m.selectedJob)
			if m.selectedJob.Status == db.StatusRunning {
				m.monitor.WatchJobStats(m.selectedJob)
			}
		} else {
			return *m, m.fetchSelectedJobLog()
		}
		return *m, nil
	}
	return *m, nil
}

func (m *Model) switchToCPUTab() (Model, tea.Cmd) {
	m.detailTab = DetailTabCPU
	m.jobSelectionActive = true
	job := m.getTargetJob()
	if cmd := m.requestJobCPUTop(job); cmd != nil {
		return *m, cmd
	}
	return *m, nil
}

func (m *Model) switchToJobTab(tab DetailTab) (Model, tea.Cmd) {
	switch tab {
	case DetailTabLogs:
		return m.switchToLogsTab()
	case DetailTabCPU:
		return m.switchToCPUTab()
	default:
		m.detailTab = DetailTabDetails
		m.detailViewport.GotoTop()
		return *m, nil
	}
}

func (m *Model) switchToHostCPUTab() (Model, tea.Cmd) {
	m.hostDetailTab = HostDetailTabCPU
	if len(m.hosts) == 0 || m.selectedHostIdx < 0 || m.selectedHostIdx >= len(m.hosts) {
		m.hostCPUTopEntries = nil
		m.hostCPUTopError = ""
		m.hostCPUTopRequestedHost = ""
		m.hostCPUTopDataHost = ""
		m.hostCPUTopLoading = false
		return *m, nil
	}
	host := m.hosts[m.selectedHostIdx]
	if cmd := m.requestHostCPUTop(host.Name); cmd != nil {
		return *m, cmd
	}
	return *m, nil
}

func (m *Model) switchToHostTab(tab HostDetailTab) (Model, tea.Cmd) {
	switch tab {
	case HostDetailTabCPU:
		return m.switchToHostCPUTab()
	default:
		m.hostDetailTab = tab
		return *m, nil
	}
}

// cycleHostsTab cycles through host detail tabs (Info, CPU, GPUs, GPU 0, GPU 1, ...)
func (m *Model) cycleHostsTab(forward bool) (Model, tea.Cmd) {
	gpuIndices := m.getHostGPUIndices()
	order := []HostDetailTab{HostDetailTabInfo, HostDetailTabCPU, HostDetailTabGPUSummary}
	for i := range gpuIndices {
		order = append(order, HostDetailTabGPUBase+HostDetailTab(i))
	}
	if len(order) == 0 {
		order = []HostDetailTab{HostDetailTabInfo}
	}

	current := 0
	for i, tab := range order {
		if tab == m.hostDetailTab {
			current = i
			break
		}
	}

	if forward {
		current = (current + 1) % len(order)
	} else {
		current = (current - 1 + len(order)) % len(order)
	}

	return m.switchToHostTab(order[current])
}

func (m Model) startHostRefreshTicker() tea.Cmd {
	return tea.Tick(m.hostRefreshInterval, func(t time.Time) tea.Msg {
		return hostRefreshTickMsg(t)
	})
}

func (m Model) startHostSummaryTicker() tea.Cmd {
	return tea.Tick(hostSummaryTickerInterval, func(t time.Time) tea.Msg {
		return hostSummaryTickMsg(t)
	})
}

func (m Model) handleJobsRefreshed(msg jobsRefreshedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading jobs: %v", msg.err), true)
	}
	m.allJobs = msg.jobs
	if msg.jobDependencies != nil {
		m.jobDependencies = msg.jobDependencies
	}
	m.applyJobFilter()

	runningJobIDs := make(map[int64]bool)
	for _, job := range msg.jobs {
		if job.Status == db.StatusRunning {
			runningJobIDs[job.ID] = true
		}
	}
	for jobID := range m.jobProgress {
		if !runningJobIDs[jobID] {
			delete(m.jobProgress, jobID)
		}
	}

	if m.pendingSelectJobID > 0 {
		for i, job := range m.jobs {
			if job.ID == m.pendingSelectJobID {
				m.jobList.Select(i)
				break
			}
		}
		m.pendingSelectJobID = 0
	}

	var cmds []tea.Cmd

	// Trigger initial priority sync now that jobs are loaded
	if m.initialSyncNeeded && m.syncWorker != nil {
		m.initialSyncNeeded = false
		m.requestSyncAllHosts(true)
		cmds = append(cmds, m.checkSyncResults())
	}

	if cmd := m.fetchAllRunningJobsProgress(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.showHostSummaries && m.llmGenerator != nil {
		cmds = append(cmds, m.generateAllHostSummaries())
	}

	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleHostsLoaded(msg hostsLoadedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading hosts: %v", msg.err), true)
	}
	monitorHosts := map[string]*Host{}
	if m.monitor != nil {
		for _, host := range m.monitor.Hosts() {
			if host != nil && host.Name != "" {
				monitorHosts[host.Name] = host
			}
		}
	}
	var cmds []tea.Cmd
	for _, name := range msg.hostNames {
		found := false
		for _, h := range m.hosts {
			if h.Name == name {
				found = true
				break
			}
		}
		if !found {
			var host *Host
			if monitorHost, ok := monitorHosts[name]; ok {
				host = monitorHost
				m.hosts = append(m.hosts, host)
				continue
			}
			cachedInfo, err := db.LoadCachedHostInfo(m.database, name)
			if err == nil && cachedInfo != nil {
				host = hostFromCachedInfo(cachedInfo)
				cacheAge := time.Since(time.Unix(cachedInfo.LastUpdated, 0))
				if cacheAge > m.hostCacheDuration && m.monitor == nil {
					host.Status = HostStatusChecking
					m.requestHostInfoRefresh(name, false)
				}
			} else {
				host = &Host{
					Name:   name,
					Status: HostStatusChecking,
				}
				if m.monitor == nil {
					m.requestHostInfoRefresh(name, false)
				}
			}
			m.hosts = append(m.hosts, host)
		}
		if !m.hostsQueriedThisSession[name] {
			m.requestHostInfoRefresh(name, false)
		}
	}
	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleHostSyncTimesLoaded(msg hostSyncTimesLoadedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading host sync times: %v", msg.err), true)
	}
	if msg.times != nil {
		m.hostSyncTimes = msg.times
	}
	m.applyJobFilter()
	return m, nil
}

func (m Model) handleHostInfo(msg hostInfoMsg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	for i, h := range m.hosts {
		if h.Name == msg.hostName {
			// Hysteresis: require 3 consecutive failures before marking offline.
			// This prevents a single SSH timeout from wiping host metrics.
			if msg.info.Status == HostStatusOffline && h.Status == HostStatusOnline {
				m.hostFailCount[msg.hostName]++
				if m.hostFailCount[msg.hostName] < 3 {
					// Not enough failures yet; keep existing host data
					break
				}
			} else if msg.info.Status == HostStatusOnline {
				m.hostFailCount[msg.hostName] = 0
			}
			m.hosts[i].UpdateFrom(msg.info)

			// Warn if host has low disk space (only on first detection this session)
			if msg.info.HasLowDiskSpace() && !m.lowDiskWarnedHosts[msg.hostName] {
				m.lowDiskWarnedHosts[msg.hostName] = true
				cmds = append(cmds, m.setFlash(
					fmt.Sprintf("Warning: %s has low disk space (%s free)", msg.hostName, msg.info.DiskFreeSummary()),
					true,
				))
			}
			break
		}
	}
	m.hostsQueriedThisSession[msg.hostName] = true
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func (m Model) handleSyncCompleted(msg syncCompletedMsg) (Model, tea.Cmd) {
	// Legacy handler - update host sync times
	now := time.Now()
	if len(msg.hostsSynced) > 0 {
		for _, host := range msg.hostsSynced {
			m.lastHostSyncTimes[host] = now
			m.hostSyncTimes[host] = now
		}
	}
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Sync error: %v", msg.err), true)
	}
	// Only show flash messages for actual errors during periodic sync.
	// Don't flash "Synced N jobs" or "Started queue" - those are normal operations.
	var cmds []tea.Cmd
	if len(msg.queueRunnerErrors) > 0 {
		label := "error"
		if len(msg.queueRunnerErrors) > 1 {
			label = "errors"
		}
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Queue runner %s: %s", label, strings.Join(msg.queueRunnerErrors, "; ")), true))
	}
	if m.monitor == nil {
		cmds = append(cmds, m.refreshJobs(), m.loadHosts())
	}
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func (m Model) handleMonitorEvent(msg monitorEventMsg) (Model, tea.Cmd) {
	event := msg.event
	var cmd tea.Cmd
	switch event.Type {
	case monitor.EventJobsRefreshed:
		m, cmd = m.handleJobsRefreshed(jobsRefreshedMsg{
			jobs:            event.Jobs,
			jobDependencies: event.JobDependencies,
			err:             event.Err,
		})
	case monitor.EventHostsLoaded:
		m, cmd = m.handleHostsLoaded(hostsLoadedMsg{
			hostNames: event.HostNames,
			err:       event.Err,
		})
	case monitor.EventHostInfoUpdated:
		if event.Host != nil {
			m, cmd = m.handleHostInfo(hostInfoMsg{
				hostName: event.Host.Name,
				info:     event.Host,
			})
		}
	case monitor.EventHostSyncTimesLoaded:
		m, cmd = m.handleHostSyncTimesLoaded(hostSyncTimesLoadedMsg{
			times: event.HostSyncTimes,
			err:   event.Err,
		})
	case monitor.EventSyncCompleted:
		m, cmd = m.handleSyncCompleted(syncCompletedMsg{
			updated:           event.SyncResult.Updated,
			queuesStarted:     event.SyncResult.QueuesStarted,
			hostsSynced:       event.SyncResult.HostsSynced,
			queueRunnerErrors: event.SyncResult.QueueRunnerErrors,
			err:               event.Err,
		})
	case monitor.EventJobLogFetched:
		if event.JobLog != nil {
			m, cmd = m.handleMonitorJobLog(event.JobLog)
		}
	case monitor.EventProcessStatsFetched:
		if event.ProcessStats != nil {
			m, cmd = m.handleMonitorProcessStats(event.ProcessStats)
		}
	case monitor.EventError:
		if event.Err != nil {
			cmd = m.setFlash(fmt.Sprintf("Monitor error: %v", event.Err), true)
		}
	}
	waitCmd := m.waitForMonitorEvent()
	if cmd == nil {
		return m, waitCmd
	}
	if waitCmd == nil {
		return m, cmd
	}
	return m, tea.Batch(cmd, waitCmd)
}

func (m Model) handleMonitorJobLog(result *monitor.JobLogResult) (Model, tea.Cmd) {
	m.logLoading = false

	// Extract progress info
	if result.Err == nil {
		prog := progress.FindLastProgress(result.Content)
		if prog != nil {
			m.jobProgress[result.JobID] = prog
		}
	}

	if m.selectedJob == nil || result.JobID != m.selectedJob.ID {
		return m, nil
	}

	if result.Err != nil {
		if result.ConnError {
			// Try local cache on connection error
			if cached, err := logcache.Read(result.JobID); err == nil {
				m.logContent = cached
				m.logStale = true
				m.logCache[result.JobID] = cached
			} else if cached, ok := m.logCache[result.JobID]; ok {
				m.logContent = cached
				m.logStale = true
			} else {
				m.logContent = fmt.Sprintf("Host %s unreachable", m.selectedJob.Host)
				m.logStale = false
			}
		} else {
			combined := result.Content + result.Stderr
			if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
				msg := "No log file yet"
				if m.selectedJob.Status == db.StatusCompleted || m.selectedJob.Status == db.StatusFailed || m.selectedJob.Status == db.StatusDead || m.selectedJob.Status == db.StatusKilled || m.selectedJob.Status == db.StatusCanceled {
					msg = "Log file not found (may have been cleaned up)"
				}
				m.logContent = msg
			} else {
				m.logContent = fmt.Sprintf("Error: %s", strings.TrimSpace(combined))
			}
			m.logStale = false
		}
	} else {
		combined := result.Content
		if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
			msg := "No log file yet"
			if m.selectedJob.Status == db.StatusCompleted || m.selectedJob.Status == db.StatusFailed || m.selectedJob.Status == db.StatusDead || m.selectedJob.Status == db.StatusKilled || m.selectedJob.Status == db.StatusCanceled {
				msg = "Log file not found (may have been cleaned up)"
			}
			m.logContent = msg
			m.logStale = false
		} else {
			m.logCache[result.JobID] = result.Content
			m.logContent = result.Content
			m.logStale = false
		}
	}

	m.logViewport.SetContent(m.logContent)
	m.logViewport.GotoBottom()
	return m, nil
}

func (m Model) handleMonitorProcessStats(result *monitor.ProcessStatsResult) (Model, tea.Cmd) {
	targetJob := m.getTargetJob()
	if targetJob == nil || result.JobID != targetJob.ID || result.Stats == nil {
		return m, nil
	}

	stats := result.Stats
	if stats.Running || m.processStats == nil || m.processStatsJobID != result.JobID {
		if m.prevProcessStats != nil && m.processStatsJobID == result.JobID &&
			stats.Timestamp > m.prevProcessStats.Timestamp && stats.Running {
			deltaTicks := (stats.CPUUserTicks + stats.CPUSysTicks) -
				(m.prevProcessStats.CPUUserTicks + m.prevProcessStats.CPUSysTicks)
			deltaTime := stats.Timestamp - m.prevProcessStats.Timestamp
			if deltaTime > 0 {
				stats.CPUPct = float64(deltaTicks) / float64(deltaTime)
			}
		}
		if stats.Running {
			m.prevProcessStats = m.processStats
		}
		m.processStats = stats
		m.processStatsJobID = result.JobID
	}

	return m, nil
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

func (m Model) loadHostSyncTimes() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		times, err := db.LoadHostSyncTimes(database)
		if err != nil {
			return hostSyncTimesLoadedMsg{err: err}
		}
		return hostSyncTimesLoadedMsg{times: times}
	}
}

// requestHostInfoRefresh requests a host info refresh via the sync worker.
func (m Model) requestHostInfoRefresh(hostName string, priority bool) {
	if m.syncWorker != nil {
		m.syncWorker.Request(SyncRequest{
			Host:     hostName,
			Rate:     RateRunning,
			Priority: priority,
		})
	}
}

// getTargetJob returns the job to act on - either the selected job or the highlighted job
func (m Model) getTargetJob() *db.Job {
	if !m.jobSelectionActive {
		return nil
	}
	if m.detailTab == DetailTabLogs && m.selectedJob != nil {
		return m.selectedJob
	}
	idx := m.jobList.Index()
	if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
		return m.jobs[idx]
	}
	return nil
}

func (m *Model) clearJobSelection() {
	m.jobSelectionActive = false
	m.detailTab = DetailTabDetails
	m.selectedJob = nil
	m.logContent = ""
	m.logStale = false
	m.logLoading = false
	m.processStats = nil
	m.prevProcessStats = nil
	m.processStatsJobID = 0
	m.detailViewport.SetContent("")
	m.detailViewport.GotoTop()
	if m.monitor != nil {
		m.monitor.WatchJobLog(nil)
		m.monitor.WatchJobStats(nil)
	}
}

func (m *Model) jumpJobListToTop() tea.Cmd {
	if len(m.jobs) == 0 {
		m.clearJobSelection()
		return nil
	}

	prevIdx := m.jobList.Index()
	m.jobList.Select(0)
	m.jobList.Paginator.Page = 0
	m.jobSelectionActive = true
	if m.jobList.Index() != prevIdx {
		return m.handleSelectionChanged()
	}
	return nil
}

func (m *Model) pageJobList(direction int) tea.Cmd {
	if len(m.jobs) == 0 {
		m.clearJobSelection()
		return nil
	}
	pageSize := m.jobListContentHeight
	if pageSize <= 0 {
		pageSize = 10
	}
	idx := m.jobList.Index()
	if idx < 0 {
		idx = 0
	}
	newIdx := idx + direction*pageSize
	if newIdx < 0 {
		newIdx = 0
	} else if newIdx >= len(m.jobs) {
		newIdx = len(m.jobs) - 1
	}
	prevIdx := m.jobList.Index()
	m.jobList.Select(newIdx)
	m.jobSelectionActive = true
	if m.jobList.Index() != prevIdx {
		return m.handleSelectionChanged()
	}
	return nil
}

// handleSelectionChanged is called when the job list selection changes.
// It clears cached process stats and fetches logs/stats for the new selection.
func (m *Model) handleSelectionChanged() tea.Cmd {
	m.jobSelectionActive = true
	// Clear cached process stats when changing jobs
	m.processStats = nil
	m.prevProcessStats = nil
	m.processStatsJobID = 0

	idx := m.jobList.Index()
	if len(m.jobs) == 0 || idx < 0 || idx >= len(m.jobs) {
		return nil
	}

	job := m.jobs[idx]

	if m.detailTab == DetailTabDetails {
		m.detailViewport.GotoTop()
	}

	var cmds []tea.Cmd
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
		if m.monitor != nil {
			m.monitor.WatchJobLog(job)
			if job.Status == db.StatusRunning {
				m.monitor.WatchJobStats(job)
			}
		} else {
			cmds = append(cmds, m.fetchSelectedJobLog())
		}
		if len(cmds) > 0 {
			return tea.Batch(cmds...)
		}
		return nil
	}

	// Fetch CPU summary if that tab is active
	if m.detailTab == DetailTabCPU {
		if cmd := m.requestJobCPUTop(job); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Pre-fetch log for non-Logs tab (background cache warming)
	if m.monitor == nil {
		if _, ok := m.logCache[job.ID]; !ok || job.Status == db.StatusRunning {
			if cmd := m.fetchJobLog(job); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	// Update watched stats job for running jobs
	if m.monitor != nil {
		if job.Status == db.StatusRunning {
			m.monitor.WatchJobStats(job)
		} else {
			m.monitor.WatchJobStats(nil)
		}
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	return nil
}

func (m *Model) handleHostSelectionChanged() tea.Cmd {
	if len(m.hosts) == 0 || m.selectedHostIdx < 0 || m.selectedHostIdx >= len(m.hosts) {
		if m.hostDetailTab == HostDetailTabCPU {
			return m.requestHostCPUTop("")
		}
		return nil
	}
	host := m.hosts[m.selectedHostIdx]
	if m.hostDetailTab == HostDetailTabCPU {
		return m.requestHostCPUTop(host.Name)
	}
	return nil
}

func jobMatchesFilter(job *db.Job, mode jobFilterMode) bool {
	switch mode {
	case jobFilterRecent:
		// Active jobs (running, starting, queued, draft)
		if job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused || job.Status == db.StatusQueued || job.Status == db.StatusDraft {
			return true
		}
		return isRecentHistory(job)
	case jobFilterActive:
		return job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusPaused || job.Status == db.StatusQueued || job.Status == db.StatusDraft
	case jobFilterSucceeded:
		return job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0
	case jobFilterFailed:
		if job.Status == db.StatusFailed || job.Status == db.StatusDead || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
			return true
		}
		return job.Status == db.StatusCompleted && (job.ExitCode == nil || *job.ExitCode != 0)
	default:
		return true
	}
}

func (m Model) jobMatchesHostFilter(job *db.Job) bool {
	switch m.jobHostFilterMode {
	case hostFilterAll:
		return true
	case hostFilterSpecific:
		if m.jobHostFilterHost == "" {
			return true
		}
		return job.Host == m.jobHostFilterHost
	case hostFilterRecent:
		return m.isHostRecentlySynced(job.Host)
	default:
		return true
	}
}

func (m Model) isHostRecentlySynced(host string) bool {
	if len(m.hostSyncTimes) == 0 {
		return true
	}
	if !m.anyHostRecentlySynced() {
		return true
	}
	last, ok := m.hostSyncTimes[host]
	if !ok || last.IsZero() {
		return false
	}
	return time.Since(last) <= hostRecentSyncWindow
}

func (m Model) anyHostRecentlySynced() bool {
	for _, last := range m.hostSyncTimes {
		if !last.IsZero() && time.Since(last) <= hostRecentSyncWindow {
			return true
		}
	}
	return false
}

func isRecentHistory(job *db.Job) bool {
	const window = 24 * time.Hour
	windowSeconds := int64(window / time.Second)
	now := time.Now()
	if job.EndTime != nil {
		return now.Unix()-*job.EndTime < windowSeconds
	}
	if job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
		timestamp := job.StartTime
		if timestamp == 0 {
			timestamp = job.CreatedAt
		}
		if timestamp == 0 {
			return false
		}
		return now.Sub(time.Unix(timestamp, 0)) < window
	}
	return false
}

func jobFilterDescription(mode jobFilterMode) string {
	switch mode {
	case jobFilterRecent:
		return "Recent"
	case jobFilterActive:
		return "Active"
	case jobFilterSucceeded:
		return "Succeeded"
	case jobFilterFailed:
		return "Failed"
	default:
		return "All"
	}
}

func hostFilterDescription(m Model) string {
	switch m.jobHostFilterMode {
	case hostFilterRecent:
		return "Synced <2d"
	case hostFilterAll:
		return "All"
	case hostFilterSpecific:
		if m.jobHostFilterHost != "" {
			return m.jobHostFilterHost
		}
		return "Host"
	default:
		return "All"
	}
}

func (m *Model) cycleHostFilter() {
	hosts := m.hostFilterCandidates()
	switch m.jobHostFilterMode {
	case hostFilterRecent:
		m.jobHostFilterMode = hostFilterAll
		m.jobHostFilterHost = ""
	case hostFilterAll:
		if len(hosts) == 0 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		m.jobHostFilterMode = hostFilterSpecific
		m.jobHostFilterHost = hosts[0]
	case hostFilterSpecific:
		if len(hosts) == 0 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		index := -1
		for i, host := range hosts {
			if host == m.jobHostFilterHost {
				index = i
				break
			}
		}
		if index == -1 || index == len(hosts)-1 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		m.jobHostFilterHost = hosts[index+1]
	default:
		m.jobHostFilterMode = hostFilterRecent
		m.jobHostFilterHost = ""
	}
}

func (m Model) hostFilterCandidates() []string {
	hostSet := make(map[string]struct{})
	for _, host := range m.hosts {
		if host.Name != "" {
			hostSet[host.Name] = struct{}{}
		}
	}
	if len(hostSet) == 0 {
		for _, job := range m.allJobs {
			if job.Host != "" {
				hostSet[job.Host] = struct{}{}
			}
		}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	naturalSortStrings(hosts)
	return hosts
}

func (m Model) fetchJobLog(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	jobCopy := *job
	ctx := m.ctx
	return func() tea.Msg {
		job := jobCopy
		// For terminal jobs, try local cache first
		if job.Status == db.StatusCompleted || job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
			if cached, err := logcache.Read(job.ID); err == nil {
				// Apply tail -500 equivalent
				lines := strings.Split(cached, "\n")
				if len(lines) > 500 {
					lines = lines[len(lines)-500:]
				}
				content := strings.Join(lines, "\n")
				prog := progress.FindLastProgress(content)
				return logFetchedMsg{
					jobID:     job.ID,
					content:   content,
					progress:  prog,
					fromCache: true,
				}
			}
			// Fall through to remote fetch if not cached
		}

		logFile, _ := logfiles.Resolve(&job)

		// Fetch the log content
		// Don't quote path - it contains ~ which needs shell expansion
		stdout, stderr, err := remote.TryRunWithContext(ctx, job.Host, fmt.Sprintf("tail -500 %s 2>&1", logFile))
		if err != nil {
			// Pool busy or context cancelled - exit silently
			if errors.Is(err, remote.ErrPoolBusy) || err == context.Canceled {
				return nil
			}
			// Check if it's a connection error
			combined := stdout + stderr
			if remote.IsConnectionError(combined) {
				if cached, err := logcache.Read(job.ID); err == nil {
					return logFetchedMsg{
						jobID:     job.ID,
						content:   cached,
						connError: true,
						fromCache: true,
					}
				}
				return logFetchedMsg{
					jobID:     job.ID,
					content:   fmt.Sprintf("Host %s unreachable", job.Host),
					connError: true,
				}
			}
			// Check if log file doesn't exist
			if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
				msg := "No log file yet"
				if job.Status == db.StatusCompleted || job.Status == db.StatusFailed || job.Status == db.StatusDead || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
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
			if job.Status == db.StatusCompleted || job.Status == db.StatusFailed || job.Status == db.StatusDead || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
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

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}
	return m.fetchJobLog(m.selectedJob)
}

func (m Model) fetchTopProcesses(host string, jobView bool) tea.Cmd {
	if host == "" {
		return nil
	}
	return func() tea.Msg {
		procs, err := remote.GetTopProcesses(host, topProcessLimit)
		return cpuTopMsg{
			host:      host,
			processes: procs,
			err:       err,
			jobView:   jobView,
		}
	}
}

func (m *Model) requestJobCPUTop(job *db.Job) tea.Cmd {
	if job == nil {
		m.jobCPUTopEntries = nil
		m.jobCPUTopError = ""
		m.jobCPUTopRequestedHost = ""
		m.jobCPUTopDataHost = ""
		m.jobCPUTopLoading = false
		return nil
	}
	if m.jobCPUTopDataHost != job.Host {
		m.jobCPUTopEntries = nil
	}
	m.jobCPUTopLoading = true
	m.jobCPUTopError = ""
	m.jobCPUTopRequestedHost = job.Host
	return m.fetchTopProcesses(job.Host, true)
}

func (m *Model) requestHostCPUTop(hostName string) tea.Cmd {
	if hostName == "" {
		m.hostCPUTopEntries = nil
		m.hostCPUTopError = ""
		m.hostCPUTopRequestedHost = ""
		m.hostCPUTopDataHost = ""
		m.hostCPUTopLoading = false
		return nil
	}
	if m.hostCPUTopDataHost != hostName {
		m.hostCPUTopEntries = nil
	}
	m.hostCPUTopLoading = true
	m.hostCPUTopError = ""
	m.hostCPUTopRequestedHost = hostName
	return m.fetchTopProcesses(hostName, false)
}

// fetchQuickProgress quickly greps the log file for the last progress line
// This is faster than fetching the full log and is used for periodic refresh
func (m Model) fetchQuickProgress(job *db.Job) tea.Cmd {
	if job == nil || job.Status != db.StatusRunning {
		return nil
	}

	ctx := m.ctx
	return func() tea.Msg {
		// Find the log file via shared resolver
		logFile, _ := logfiles.Resolve(job)

		// Quick grep for last progress line (case-insensitive)
		grepCmd := fmt.Sprintf("grep -i 'Progress:' %s 2>/dev/null | tail -1", logFile)
		stdout, _, err := remote.RunWithContext(ctx, job.Host, grepCmd)
		if err != nil || strings.TrimSpace(stdout) == "" {
			return quickProgressMsg{jobID: job.ID, progress: nil}
		}

		prog := progress.ParseProgress(strings.TrimSpace(stdout))
		return quickProgressMsg{jobID: job.ID, progress: prog}
	}
}

// fetchAllRunningJobsProgress fetches progress for all running jobs quickly
func (m Model) fetchAllRunningJobsProgress() tea.Cmd {
	var cmds []tea.Cmd
	for _, job := range m.allJobs {
		if job.Status == db.StatusRunning && !job.Tombstoned {
			if cmd := m.fetchQuickProgress(job); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m Model) killJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}

	jobID := job.ID
	cancelled := job.Status == db.StatusQueued
	return func() tea.Msg {
		result, err := m.coreService.KillJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobKilledMsg{jobID: jobID, err: err, cancelled: cancelled}
		}
		return jobKilledMsg{
			jobID:     jobID,
			deferred:  result.Outcome.Deferred,
			cancelled: cancelled,
			message:   result.Outcome.Message,
		}
	}
}

func (m Model) draftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.DraftJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobDraftedMsg{jobID: jobID, err: err}
		}
		return jobDraftedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) queueDraftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.RequestStatus(jobID, db.StatusQueued, ops.TimeoutFast)
		if err != nil {
			return jobQueuedMsg{jobID: jobID, err: err}
		}
		return jobQueuedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) runDraftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.RequestStatus(jobID, db.StatusRunning, ops.TimeoutFast)
		if err != nil {
			return jobQueuedMsg{jobID: jobID, err: err}
		}
		return jobQueuedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) cancelQueuedJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}

	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.KillJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobKilledMsg{jobID: jobID, err: err, cancelled: true}
		}
		return jobKilledMsg{
			jobID:     jobID,
			deferred:  result.Outcome.Deferred,
			cancelled: true,
			message:   result.Outcome.Message,
		}
	}
}

func (m Model) pauseJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.PauseJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobPausedMsg{jobID: jobID, err: err}
		}
		return jobPausedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) resumeJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.ResumeJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobResumedMsg{jobID: jobID, err: err}
		}
		return jobResumedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) restartJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		workingDir := job.WorkingDir
		command := job.Command
		description := job.Description

		if workingDir == "" || command == "" {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("missing working directory or command")}
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

func (m Model) retryJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		if job.Host == "" {
			return jobRetriedMsg{oldJobID: job.ID, err: fmt.Errorf("job missing host")}
		}
		if job.Command == "" {
			return jobRetriedMsg{oldJobID: job.ID, err: fmt.Errorf("job missing command")}
		}
		workingDir := job.WorkingDir
		if workingDir == "" {
			if dir := job.EffectiveWorkingDir(); dir != "" {
				workingDir = dir
			} else {
				workingDir = "~"
			}
		}
		result, err := ops.QueueJob(database, ops.QueueJobParams{
			Host:        job.Host,
			WorkingDir:  workingDir,
			Command:     job.Command,
			Description: job.Description,
			EnvVars:     job.EnvVars,
		}, ops.DefaultOptions())
		if err != nil {
			return jobRetriedMsg{oldJobID: job.ID, err: err}
		}
		if err := rewireDependenciesForRetry(database, job.ID, result.JobID); err != nil {
			return jobRetriedMsg{oldJobID: job.ID, newJobID: result.JobID, err: err, deferred: result.Deferred}
		}
		return jobRetriedMsg{oldJobID: job.ID, newJobID: result.JobID, deferred: result.Deferred}
	}
}

func rewireDependenciesForRetry(database *sql.DB, oldID, newID int64) error {
	depJobs, err := db.ListQueuedJobsWithDependency(database, oldID)
	if err != nil {
		return err
	}
	for _, depJob := range depJobs {
		newSpec, changed := db.ReplaceDepSpecID(depJob.DepSpec, oldID, newID)
		if !changed {
			continue
		}
		if err := db.SetJobDepSpec(database, depJob.ID, newSpec); err != nil {
			return err
		}
		depJob.DepSpec = newSpec
		if err := ops.UpdateQueuedJobEntry(depJob, depJob.EnvVars, newSpec); err != nil {
			return err
		}
	}
	return nil
}

// startQueuedJobNow starts a queued job immediately, bypassing any dependencies
func (m Model) startQueuedJobNow(job *db.Job) tea.Cmd {
	if job == nil {
		return func() tea.Msg {
			return jobStartedNowMsg{err: fmt.Errorf("no job selected")}
		}
	}
	// Use EffectiveStatus to be consistent with the handler
	if job.EffectiveStatus() != db.StatusQueued {
		return func() tea.Msg {
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("job %d is %s, not queued", job.ID, job.EffectiveStatus())}
		}
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
	database := m.database
	return func() tea.Msg {
		moved, err := queuefile.MoveToFront(job.Host, job.ID)
		if err == nil && moved {
			// Update queued_at to be earlier than all other queued jobs
			_ = db.SetQueuedAtBefore(database, job.ID, job.Host)
		}
		return jobMovedToFrontMsg{jobID: job.ID, moved: moved, err: err}
	}
}

// regenerateDescription generates an AI description for a job
func (m Model) regenerateDescription(job *db.Job) tea.Cmd {
	if job == nil || m.llmGenerator == nil {
		return nil
	}
	return func() tea.Msg {
		desc, hash, err := m.llmGenerator.GenerateOne(job)
		if err != nil {
			return descriptionGeneratedMsg{jobID: job.ID, err: err}
		}
		// Save to database
		if err := db.UpdateJobGeneratedDescription(m.database, job.ID, desc, hash); err != nil {
			return descriptionGeneratedMsg{jobID: job.ID, err: err}
		}
		return descriptionGeneratedMsg{jobID: job.ID, description: desc}
	}
}

// hasEnoughRAM checks if the system has at least minBytes of available RAM
func hasEnoughRAM(minBytes uint64) bool {
	if runtime.GOOS == "darwin" {
		// Use vm_stat on macOS
		out, err := exec.Command("vm_stat").Output()
		if err != nil {
			return true // Assume OK if we can't check
		}
		// Parse pages - include free, inactive, and speculative (all reclaimable)
		var freePages, inactivePages, speculativePages uint64
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				numStr := strings.TrimSuffix(parts[len(parts)-1], ".")
				pages, err := strconv.ParseUint(numStr, 10, 64)
				if err != nil {
					continue
				}
				switch {
				case strings.HasPrefix(line, "Pages free:"):
					freePages = pages
				case strings.HasPrefix(line, "Pages inactive:"):
					inactivePages = pages
				case strings.HasPrefix(line, "Pages speculative:"):
					speculativePages = pages
				}
			}
		}
		// Available = free + inactive + speculative (all can be reclaimed)
		availablePages := freePages + inactivePages + speculativePages
		availableBytes := availablePages * 4096
		return availableBytes >= minBytes
	}
	return true // Default to allowing on other platforms
}

// generateAllHostSummaries triggers summary generation for the next host that needs one
// Only generates one at a time to avoid overwhelming the system
func (m Model) generateAllHostSummaries() tea.Cmd {
	if m.llmGenerator == nil || !m.llmGenerator.IsAvailable() {
		return nil
	}
	// Check if any summary is already being generated
	for _, pending := range m.hostSummaryPending {
		if pending {
			return nil // Wait for current generation to complete
		}
	}
	// Check available RAM before generating (skip if < 1GB available)
	if !hasEnoughRAM(1 * 1024 * 1024 * 1024) {
		return nil
	}
	// Find first host that needs a summary
	for _, host := range m.hosts {
		if cmd := m.maybeGenerateHostSummary(host.Name); cmd != nil {
			return cmd
		}
	}
	return nil
}

// maybeGenerateHostSummary generates a summary if hash changed and rate limit allows
func (m Model) maybeGenerateHostSummary(host string) tea.Cmd {
	// Check rate limit: no more than once per minute
	if lastTime, ok := m.hostSummaryTimes[host]; ok {
		if time.Since(lastTime) < time.Minute {
			return nil
		}
	}
	// Check if already generating
	if m.hostSummaryPending[host] {
		return nil
	}
	// Compute current hash
	newHash := m.computeHostJobsHash(host)
	if oldHash, ok := m.hostSummaryHashes[host]; ok && oldHash == newHash {
		return nil // No change
	}
	// Mark as pending and generate
	m.hostSummaryPending[host] = true
	return m.generateHostSummary(host, newHash)
}

// computeHostJobsHash computes a hash of job IDs and statuses for a host
func (m Model) computeHostJobsHash(host string) string {
	var parts []string
	for _, job := range m.allJobs {
		if job.Host == host && !job.Tombstoned {
			exitCode := ""
			if job.ExitCode != nil {
				exitCode = fmt.Sprintf(":%d", *job.ExitCode)
			}
			parts = append(parts, fmt.Sprintf("%d:%s%s", job.ID, job.Status, exitCode))
		}
	}
	// Simple hash: join and take first 16 chars of sha256
	h := sha256.New()
	h.Write([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// generateHostSummary generates an AI summary for a host
func (m Model) generateHostSummary(host, hash string) tea.Cmd {
	return func() tea.Msg {
		// Gather job data for this host
		var running, queued, completedOK, failed []string
		for _, job := range m.allJobs {
			if job.Host != host || job.Tombstoned {
				continue
			}
			desc := job.EffectiveDescription()
			if len(desc) > 40 {
				desc = desc[:40] + "..."
			}
			switch job.Status {
			case db.StatusRunning, db.StatusStarting, db.StatusPaused:
				running = append(running, desc)
			case db.StatusQueued:
				queued = append(queued, desc)
			case db.StatusCompleted:
				if job.ExitCode != nil && *job.ExitCode == 0 {
					completedOK = append(completedOK, desc)
				} else {
					failed = append(failed, desc)
				}
			}
		}

		// Build facts for the prompt (simplified format)
		var facts []string
		if len(running) > 0 {
			facts = append(facts, fmt.Sprintf("RUNNING: %s", strings.Join(running, ", ")))
		}
		if len(queued) > 0 {
			if len(queued) > 3 {
				facts = append(facts, fmt.Sprintf("QUEUED: %d jobs", len(queued)))
			} else {
				facts = append(facts, fmt.Sprintf("QUEUED: %s", strings.Join(queued, ", ")))
			}
		}
		if len(completedOK) > 0 {
			if len(completedOK) > 3 {
				completedOK = completedOK[:3]
			}
			facts = append(facts, fmt.Sprintf("OK: %s", strings.Join(completedOK, ", ")))
		}
		if len(failed) > 0 {
			if len(failed) > 3 {
				facts = append(facts, fmt.Sprintf("FAILED: %d jobs including %s", len(failed), failed[0]))
			} else {
				facts = append(facts, fmt.Sprintf("FAILED: %s", strings.Join(failed, ", ")))
			}
		}

		// No activity at all
		if len(facts) == 0 {
			return hostSummaryGeneratedMsg{host: host, summary: "No recent activity", hash: hash}
		}

		// Generate via ollama
		prompt := fmt.Sprintf(`Rewrite as a status summary (1-2 sentences). Be specific about what's running and any failures.

%s

Status:`, strings.Join(facts, "\n"))

		summary, err := m.llmGenerator.GenerateText(prompt)
		if err != nil {
			return hostSummaryGeneratedMsg{host: host, err: err}
		}
		return hostSummaryGeneratedMsg{host: host, summary: summary, hash: hash}
	}
}

func (m Model) jobCountsForHost(host string) (running, queued int) {
	for _, job := range m.allJobs {
		if job.Host != host || job.Tombstoned {
			continue
		}
		switch job.Status {
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			running++
		case db.StatusQueued:
			queued++
		}
	}
	return
}

func (m Model) pruneJobs() tea.Cmd {
	return func() tea.Msg {
		count, err := db.PruneJobs(m.database, false, nil)
		return pruneCompletedMsg{count: count, err: err}
	}
}

func (m Model) startQueue(host string) tea.Cmd {
	return func() tea.Msg {
		runner := queuerunner.NewRunner(host)
		started, err := runner.EnsureStarted("")
		if err != nil {
			return queueStartedMsg{host: host, err: err}
		}
		return queueStartedMsg{host: host, already: !started}
	}
}

// ensureQueueRunnerStartedTUI checks if queue runner is running and starts it if not.
// Returns (true, nil) if started, (false, nil) if already running, (false, err) on error.
func ensureQueueRunnerStartedTUI(host string) (bool, error) {
	// Check if jq is available (required for queue runner)
	jqAvailable, err := queuerunner.CheckJqAvailable(host)
	if err == nil && !jqAvailable {
		return false, fmt.Errorf("jq is required but not installed on %s", host)
	}

	// Deploy notify script and build env vars if Slack is configured
	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host)
	return runner.EnsureStarted(envVars)
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
	gpuInput := strings.TrimSpace(m.inputs[inputGPU].Value())
	cpuInput := strings.TrimSpace(m.inputs[inputCPUAllotment].Value())
	cpuAllotment, cpuErr := parseCPUAllotmentInput(cpuInput)

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
	envVars := parseEnvInput(envVarsStr)

	// Append env vars extracted from command (if we didn't use them above)
	if normalizedDir != "" {
		// We extracted cd, so also use the normalized env vars
		envVars = append(envVars, normalizedEnv...)
	} else if len(normalizedEnv) > 0 {
		// No cd prefix, but we have env vars in the command - use them
		command = normalizedCmd
		envVars = append(envVars, normalizedEnv...)
	}

	// Merge GPU field with env vars
	existingGPU, envVars := splitGPUEnvVars(envVars)
	if gpuInput == "" {
		gpuInput = existingGPU
	}
	envVars = mergeGPUEnvVars(envVars, gpuInput)

	return func() tea.Msg {
		if cpuErr != nil {
			return jobCreatedMsg{err: cpuErr}
		}
		// Queue job for sequential execution via queue runner
		result, err := ops.QueueJob(database, ops.QueueJobParams{
			Host:         host,
			WorkingDir:   workingDir,
			Command:      command,
			Description:  description,
			EnvVars:      envVars,
			CPUAllotment: cpuAllotment,
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
	envVarsStr := strings.TrimSpace(m.inputs[inputEnvVars].Value())
	gpuInput := strings.TrimSpace(m.inputs[inputGPU].Value())
	cpuInput := strings.TrimSpace(m.inputs[inputCPUAllotment].Value())

	return func() tea.Msg {
		newAllotment, err := parseCPUAllotmentInput(cpuInput)
		if err != nil {
			return jobEditedMsg{jobID: jobID, err: err}
		}
		// Get the current job to check status and get original host
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("get job: %w", err)}
		}
		if job == nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("job %d not found", jobID)}
		}
		if job.EffectiveStatus() != db.StatusQueued {
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

		// Prepare env vars and GPU
		envVars := parseEnvInput(envVarsStr)
		existingGPU, envVars := splitGPUEnvVars(envVars)
		if gpuInput == "" {
			gpuInput = existingGPU
		}
		envVars = mergeGPUEnvVars(envVars, gpuInput)

		operationalChange := newWorkingDir != job.WorkingDir || newCommand != job.Command || gpuInput != job.GPU || !equalEnvVars(envVars, job.EnvVars) || !equalCPUAllotment(newAllotment, job.CPUAllotment)

		// Update the remote queue file
		depSpec := m.editingJobDepSpec
		if depSpec == "" {
			depSpec = job.DepSpec
		}
		updatedJob := &db.Job{
			ID:           jobID,
			Host:         job.Host,
			Command:      newCommand,
			WorkingDir:   newWorkingDir,
			Description:  newDescription,
			CPUAllotment: newAllotment,
		}

		if !equalEnvVars(envVars, job.EnvVars) {
			if err := db.SetJobEnvVars(database, jobID, envVars); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update env vars: %w", err)}
			}
		}
		if gpuInput != job.GPU {
			if err := db.SetJobGPU(database, jobID, gpuInput); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update GPU: %w", err)}
			}
		}
		if !equalCPUAllotment(newAllotment, job.CPUAllotment) {
			if err := db.SetJobCPUAllotment(database, jobID, newAllotment); err != nil {
				return jobEditedMsg{jobID: jobID, err: fmt.Errorf("update CPU allotment: %w", err)}
			}
		}

		if operationalChange {
			if err := updateRemoteQueueEntry(job.Host, updatedJob, envVars, depSpec); err != nil {
				if strings.Contains(err.Error(), "host unreachable") {
					_ = db.SetPendingStatus(database, jobID, db.StatusQueued)
				}
				// Non-fatal - the local database was updated
			}
		}

		return jobEditedMsg{jobID: jobID}
	}
}

// updateRemoteQueueEntry updates a job's entry in the remote queue file.
// This is a thin wrapper around ops.UpdateQueueEntry.
func updateRemoteQueueEntry(host string, job *db.Job, envVars []string, depSpec string) error {
	return ops.UpdateQueueEntry(ops.UpdateQueueEntryParams{
		Host:    host,
		Job:     job,
		EnvVars: envVars,
		DepSpec: depSpec,
	})
}

func parseCPUAllotmentInput(input string) (*int, error) {
	trimmed := strings.TrimSpace(strings.ToLower(input))
	if trimmed == "" || trimmed == "default" {
		return nil, nil
	}
	trimmed = strings.TrimSuffix(trimmed, "%")
	value, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("CPU allotment must be 1-100 or default")
	}
	if value < 1 || value > 100 {
		return nil, fmt.Errorf("CPU allotment must be 1-100 or default")
	}
	return &value, nil
}

func formatCPUAllotmentInput(allotment *int) string {
	if allotment == nil {
		return ""
	}
	return strconv.Itoa(*allotment)
}

func equalCPUAllotment(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func formatPresetList(presets []int) string {
	parts := make([]string, 0, len(presets))
	for _, preset := range presets {
		parts = append(parts, fmt.Sprintf("%d", preset))
	}
	return strings.Join(parts, "/")
}

func fetchQueueEntryData(database *sql.DB, jobID int64) (*queueEntryData, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return &queueEntryData{}, nil
	}
	return &queueEntryData{
		envVars: job.EnvVars,
		depSpec: job.DepSpec,
	}, nil
}

type queueEntryData struct {
	envVars []string
	depSpec string
}

func parseEnvInput(input string) []string {
	var envVars []string
	if input == "" {
		return envVars
	}
	for _, ev := range strings.Split(input, ",") {
		ev = strings.TrimSpace(ev)
		if ev != "" {
			envVars = append(envVars, ev)
		}
	}
	return envVars
}

func formatEnvInput(envVars []string) string {
	if len(envVars) == 0 {
		return ""
	}
	return strings.Join(envVars, ", ")
}

func splitGPUEnvVars(envVars []string) (string, []string) {
	var (
		gpu   string
		rest  []string
		found bool
	)
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") && !found {
			gpu = strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
			found = true
			continue
		}
		rest = append(rest, ev)
	}
	return gpu, rest
}

func mergeGPUEnvVars(envVars []string, gpu string) []string {
	var filtered []string
	for _, ev := range envVars {
		if !strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			filtered = append(filtered, ev)
		}
	}
	if gpu != "" {
		filtered = append(filtered, "CUDA_VISIBLE_DEVICES="+gpu)
	}
	return filtered
}

func equalEnvVars(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func (m Model) fetchQueuedJobEnv(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	jobID := job.ID
	return func() tea.Msg {
		data, err := fetchQueueEntryData(database, jobID)
		if err != nil {
			return jobEnvLoadedMsg{jobID: jobID, err: err}
		}
		if data == nil {
			data = &queueEntryData{}
		}
		envCopy := append([]string(nil), data.envVars...)
		return jobEnvLoadedMsg{
			jobID:   jobID,
			envVars: envCopy,
			depSpec: data.depSpec,
		}
	}
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	return ansi.Truncate(s, max, "…")
}

func shortenCommandPath(cmd string) string {
	if cmd == "" {
		return ""
	}
	prefixes := []string{"/home/", "/usr/home/", "/Users/"}
	result := cmd
	for _, prefix := range prefixes {
		result = replaceHomePrefix(result, prefix)
	}
	return result
}

func replaceHomePrefix(s, prefix string) string {
	idx := strings.Index(s, prefix)
	for idx != -1 {
		start := idx + len(prefix)
		end := start
		for end < len(s) && isUsernameChar(s[end]) {
			end++
		}
		if end == start {
			break
		}
		username := s[start:end]
		replacement := "~" + username
		if localUserName != "" && strings.EqualFold(username, localUserName) {
			replacement = "~"
		}
		s = s[:idx] + replacement + s[end:]
		idx = strings.Index(s, prefix)
	}
	return s
}

func isUsernameChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '.'
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
	// Show end time for any job that has completed/terminated
	if job.EndTime != nil && job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused {
		return formatStartTime(*job.EndTime)
	}
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

// processCarriageReturns handles \r characters used by progress bars.
// For each line, it returns only the final segment after the last \r,
// simulating what the terminal would display.
func processCarriageReturns(content string) string {
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		// If line contains \r, take only the part after the last \r
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}
