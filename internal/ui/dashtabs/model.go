package dashtabs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/logging"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
)

// Options controls non-default behavior of the dashboard.
type Options struct {
	StartTabIndex   int           // 0-based; out-of-range clamps to 0
	StartCycle      time.Duration // 0 = disabled
	SpendTargetUSD  float64
	RefreshInterval time.Duration // min interval between snapshot reloads
	Logger          *slog.Logger
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() Options {
	return Options{
		RefreshInterval: 3 * time.Second,
		Logger:          logging.Discard(),
	}
}

// Model is the parent Bubble Tea model that owns tab state, cycling, help,
// focus, snapshot accumulation, and dispatch to the active view.
type Model struct {
	database        *sql.DB
	hosts           []*hostinfo.Host // optional static host inventory for Fleet view
	logger          *slog.Logger
	usage           *Logger
	loading         bool   // true while a snapshot reload is in flight
	processedFilter string // "", "processed", or "unprocessed" — toggled by 'u'

	views        []View
	active       int
	width        int
	height       int
	help         bool
	cycle        CycleState
	keys         Keys
	snapshot     Snapshot
	prevFailures int

	focused        bool
	focusReported  bool // ever received FocusMsg or BlurMsg
	refreshInt     time.Duration
	statusMessage  string
	statusMsgUntil time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// NewModel constructs a Model. database must be non-nil. hosts is optional
// and provides the Fleet view with on-prem host inventory; pass nil for a
// pure DB-driven view.
func NewModel(database *sql.DB, hosts []*hostinfo.Host, opts Options) *Model {
	if opts.Logger == nil {
		opts.Logger = logging.Discard()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Model{
		database:   database,
		hosts:      hosts,
		logger:     opts.Logger,
		usage:      NewLogger(),
		keys:       defaultKeys(),
		refreshInt: opts.RefreshInterval,
		focused:    true, // assume foregrounded until told otherwise
		snapshot: Snapshot{
			SpendTargetUSD: opts.SpendTargetUSD,
		},
		ctx:    ctx,
		cancel: cancel,
	}
	if opts.RefreshInterval <= 0 {
		m.refreshInt = 3 * time.Second
	}
	m.views = []View{
		newPulseView(),
		newTimelineView(),
		newFleetView(),
		newFocusView(),
		newTreeView(),
		newAlertsView(),
		newHistoryView(),
		newSankeyView(),
		newCostView(),
		newUsageView(),
	}
	if opts.StartTabIndex >= 0 && opts.StartTabIndex < len(m.views) {
		m.active = opts.StartTabIndex
	}
	if opts.StartCycle > 0 {
		m.cycle = CycleState{Enabled: true, Interval: opts.StartCycle, NextSwitchAt: time.Now().Add(opts.StartCycle)}
	} else {
		m.cycle = CycleState{Interval: defaultCycleInterval}
	}
	return m
}

// Close releases resources. Safe to call multiple times.
func (m *Model) Close() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.usage != nil {
		m.usage.Close()
	}
}

func (m *Model) Init() tea.Cmd {
	if m.usage != nil {
		m.usage.TabEnter(m.views[m.active].Title())
	}
	// Kick off the first snapshot load immediately so we don't show empty
	// data for the whole refresh interval on startup.
	cmds := []tea.Cmd{m.startSnapshotLoad()}
	for _, v := range m.views {
		if c := v.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	if m.cycle.Enabled {
		cmds = append(cmds, m.cycle.tick())
	}
	return tea.Batch(cmds...)
}

type refreshMsg time.Time

// snapshotLoadedMsg carries a freshly loaded Snapshot back from the
// goroutine started by startSnapshotLoad.
type snapshotLoadedMsg struct {
	snap Snapshot
}

func (m *Model) refreshCmd() tea.Cmd {
	d := m.refreshInt
	if !m.focused {
		d = d * 3
	}
	return tea.Tick(d, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

// startSnapshotLoad runs LoadSnapshotWithHosts in a goroutine and posts the
// result as a snapshotLoadedMsg. The event loop stays free to process key
// presses while the DB queries are in flight, so tab switches and key
// commands feel instant even when the load takes >100ms.
func (m *Model) startSnapshotLoad() tea.Cmd {
	if m.loading {
		// Already in flight — drop this request. The pending load will
		// produce a fresh snapshot when it finishes; the next scheduled
		// refresh tick will pick up after that.
		return nil
	}
	m.loading = true
	database := m.database
	hosts := m.hosts
	logger := m.logger
	opts := LoadOpts{
		SpendTargetUSD:  m.snapshot.SpendTargetUSD,
		ProcessedFilter: m.processedFilter,
	}
	return func() tea.Msg {
		snap := LoadSnapshotWithHosts(database, hosts, opts, logger)
		return snapshotLoadedMsg{snap: snap}
	}
}

// applySnapshot stores a freshly loaded Snapshot, advances the rolling
// history, and clears the loading flag.
func (m *Model) applySnapshot(snap Snapshot) {
	newFails := snap.Counts.Failed
	delta := newFails - m.prevFailures
	if delta < 0 {
		delta = 0
	}
	m.prevFailures = newFails
	snap.History = pushHistory(m.snapshot.History, snap.LoadedAt, snap, delta)
	m.snapshot = snap
	m.loading = false
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Forward to active view too.
		v, c := m.views[m.active].Update(msg)
		m.views[m.active] = v
		if c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case tea.FocusMsg:
		m.focused = true
		m.focusReported = true
		if m.usage != nil {
			m.usage.SetFocused(true)
		}
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		m.focusReported = true
		if m.usage != nil {
			m.usage.SetFocused(false)
		}
		return m, nil

	case refreshMsg:
		// Kick off async load; schedule next tick happens when the load
		// completes (snapshotLoadedMsg) to prevent overlapping reloads
		// on a slow DB.
		return m, m.startSnapshotLoad()

	case snapshotLoadedMsg:
		m.applySnapshot(msg.snap)
		return m, m.refreshCmd()

	case cycleTickMsg:
		if m.cycle.Enabled {
			m.activateTab((m.active+1)%len(m.views), "cycle")
			m.cycle.NextSwitchAt = time.Now().Add(m.cycle.Interval)
			cmds = append(cmds, m.cycle.tick())
		}
		return m, tea.Batch(cmds...)

	case autopilotResultMsg:
		var c tea.Cmd
		if msg.err != nil {
			m.setStatus("autopilot: " + msg.err.Error())
		} else {
			// Pick up the new autopilot state on the next render.
			c = m.startSnapshotLoad()
			if msg.paused {
				m.setStatus("autopilot paused")
			} else {
				m.setStatus("autopilot resumed")
			}
		}
		return m, c

	case tea.KeyMsg:
		// Help overlay swallows most keys; only its own close + quit get through.
		if m.help {
			switch {
			case key.Matches(msg, m.keys.CloseHelp):
				m.help = false
				return m, nil
			case key.Matches(msg, m.keys.Quit):
				return m, tea.Quit
			}
			return m, nil
		}
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Suspend):
			return m, tea.Suspend
		case key.Matches(msg, m.keys.Help):
			m.help = true
			return m, nil
		case key.Matches(msg, m.keys.Refresh):
			return m, m.startSnapshotLoad()
		case key.Matches(msg, m.keys.NextTab):
			m.activateTab((m.active+1)%len(m.views), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.PrevTab):
			m.activateTab((m.active-1+len(m.views))%len(m.views), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.RightInRow):
			rows := m.tabRows()
			m.activateTab(neighborInRow(rows, m.active, +1), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.LeftInRow):
			rows := m.tabRows()
			m.activateTab(neighborInRow(rows, m.active, -1), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.DownRow):
			rows := m.tabRows()
			m.activateTab(neighborInAdjacentRow(m.views, rows, m.active, +1), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.UpRow):
			rows := m.tabRows()
			m.activateTab(neighborInAdjacentRow(m.views, rows, m.active, -1), "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab1):
			m.activateTab(0, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab2):
			m.activateTab(1, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab3):
			m.activateTab(2, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab4):
			m.activateTab(3, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab5):
			m.activateTab(4, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab6):
			m.activateTab(5, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab7):
			m.activateTab(6, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab8):
			m.activateTab(7, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab9):
			m.activateTab(8, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Tab10):
			m.activateTab(9, "key")
			return m, m.bumpCycleIfActive()
		case key.Matches(msg, m.keys.Cycle):
			m.cycle.Enabled = !m.cycle.Enabled
			if m.cycle.Enabled {
				if m.cycle.Interval == 0 {
					m.cycle.Interval = defaultCycleInterval
				}
				m.cycle.NextSwitchAt = time.Now().Add(m.cycle.Interval)
				return m, m.cycle.tick()
			}
			return m, nil
		case key.Matches(msg, m.keys.CycleFaster):
			m.cycle.Interval = stepFaster(m.cycle.Interval)
			if m.cycle.Enabled {
				m.cycle.NextSwitchAt = time.Now().Add(m.cycle.Interval)
			}
			return m, nil
		case key.Matches(msg, m.keys.CycleSlower):
			m.cycle.Interval = stepSlower(m.cycle.Interval)
			if m.cycle.Enabled {
				m.cycle.NextSwitchAt = time.Now().Add(m.cycle.Interval)
			}
			return m, nil
		case key.Matches(msg, m.keys.Autopilot):
			return m, m.toggleAutopilot()
		case key.Matches(msg, m.keys.Unprocessed):
			if m.processedFilter == "unprocessed" {
				m.processedFilter = ""
				m.setStatus("showing all jobs")
			} else {
				m.processedFilter = "unprocessed"
				m.setStatus("filter: unprocessed only")
			}
			return m, m.startSnapshotLoad()
		case key.Matches(msg, m.keys.LaunchTUI):
			return m, emitLeaveToList()
		}
	}

	// Default: forward to the active view.
	v, c := m.views[m.active].Update(msg)
	m.views[m.active] = v
	if c != nil {
		cmds = append(cmds, c)
	}
	return m, tea.Batch(cmds...)
}

// tabRows computes the current row layout using the model's width and cycle
// state. Recomputed on every arrow-key press so a resize between presses
// doesn't strand the cursor in a stale row layout.
func (m *Model) tabRows() [][]int {
	avail := m.width
	if m.cycle.Enabled {
		// Match the gauge-width reservation in renderTabStrip.
		gauge := fmt.Sprintf("auto-rotate: %s", m.cycle.Interval)
		avail -= len(gauge) + 3
	}
	if avail < 4 {
		avail = 4
	}
	return tabRowLayout(m.views, avail)
}

func (m *Model) activateTab(i int, reason string) {
	if i == m.active {
		return
	}
	m.active = i
	if m.usage != nil {
		m.usage.TabEnter(m.views[i].Title())
	}
	_ = reason
}

func (m *Model) bumpCycleIfActive() tea.Cmd {
	if !m.cycle.Enabled {
		return nil
	}
	m.cycle.NextSwitchAt = time.Now().Add(m.cycle.Interval)
	return m.cycle.tick()
}

func (m *Model) setStatus(s string) {
	m.statusMessage = s
	m.statusMsgUntil = time.Now().Add(3 * time.Second)
}

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	// Top: tab strip (1 line).
	attn := m.attentionForTabs()
	header := renderHeader(m.width, m.snapshot, m.views, m.active, attn, m.cycle, time.Now())

	// Bottom bar: three lines — weather report, status, key hints.
	// Weather moved here from the top per the design.
	weatherLine := renderWeatherLine(m.snapshot, time.Now())
	transient := ""
	if m.statusMessage != "" && time.Now().Before(m.statusMsgUntil) {
		transient = m.statusMessage
	}
	statusLine := renderStatusLine(m.snapshot, m.cycle, m.loading, transient)
	helpLine := renderHelpLine(m.snapshot)

	// Reserve one blank line between the tab strip and the view body so
	// the tabs read as a separate band rather than crowding the title.
	const tabGap = 1
	bodyH := m.height -
		lipgloss.Height(header) -
		tabGap -
		lipgloss.Height(weatherLine) -
		lipgloss.Height(statusLine) -
		lipgloss.Height(helpLine)
	if bodyH < 1 {
		bodyH = 1
	}
	body := m.views[m.active].Render(m.width, bodyH, m.snapshot, m.focused)

	if m.help {
		body = overlay(body, renderHelp(m.keys, m.cycle, usageLogPath(m.usage)), m.width, bodyH)
	}

	// Pad the body to exactly bodyH lines so the bottom bar (weather +
	// status + keys) lands at the actual bottom of the terminal rather
	// than floating up against the body content.
	bodyLines := lipgloss.Height(body)
	if bodyLines < bodyH {
		body += strings.Repeat("\n", bodyH-bodyLines)
	}

	return header + "\n\n" + body + "\n" + weatherLine + "\n" + statusLine + "\n" + helpLine
}

func (m *Model) attentionForTabs() []Attention {
	out := make([]Attention, len(m.views))
	for i, v := range m.views {
		if r, ok := v.(AttentionReporter); ok {
			out[i] = r.Attention(m.snapshot)
		}
	}
	return out
}

// overlay places a help box near the top-right of the body region.
func overlay(body, box string, bodyW, bodyH int) string {
	bw := lipgloss.Width(box)
	bh := lipgloss.Height(box)
	col := bodyW - bw - 2
	row := 1
	if col < 1 {
		col = 1
	}
	_ = row
	_ = bh
	// Simple approach: render body, then render box on its own lines below.
	// A true overlay needs cell-level composition; for the prototype we
	// stack them vertically, which is good enough.
	return body + "\n" + lipgloss.NewStyle().MarginLeft(col).Render(box)
}

func usageLogPath(l *Logger) string {
	if l == nil {
		return ""
	}
	return l.path
}

type autopilotResultMsg struct {
	paused bool
	err    error
}

// LeaveToListMsg is emitted when the user presses the dashboard's "back to
// list" key (currently 'L'). A hosting router (e.g. internal/ui/terminal's
// watchRouterModel) translates this into its own switchToListMsg to mount
// the list TUI in place of the dashboard, sharing one alt-screen across
// the transition. Standalone invocations of `weft dashboard` without a
// host treat the message as a no-op.
type LeaveToListMsg struct{}

// emitLeaveToList returns a Cmd that produces a LeaveToListMsg. Used by the
// 'L' keybinding.
func emitLeaveToList() tea.Cmd {
	return func() tea.Msg { return LeaveToListMsg{} }
}

func (m *Model) toggleAutopilot() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		st, err := db.LoadAutopilotState(database)
		if err != nil {
			return autopilotResultMsg{err: err}
		}
		if st != nil && st.Paused {
			if _, err := db.ResumeAutopilot(database); err != nil {
				return autopilotResultMsg{err: err}
			}
			return autopilotResultMsg{paused: false}
		}
		who := os.Getenv("USER")
		if who == "" {
			who = "dashboard"
		}
		if _, err := db.PauseAutopilot(database, who, "dashboard"); err != nil {
			return autopilotResultMsg{err: err}
		}
		return autopilotResultMsg{paused: true}
	}
}
