package tui

import (
	"os/user"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
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
	Cloud           key.Binding
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
		Cloud: key.NewBinding(
			key.WithKeys("c"),
			key.WithHelp("c", "rental GPU options"),
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
