package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	hostsync "github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/ids"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
	"github.com/osteele/weft/internal/util"
)

// hostsTUIModel renders a single-screen list of on-prem inventory hosts and
// currently-running cloud rentals. It reuses the dashboard's row rendering
// (status glyph + CPU/MEM/GPU bars) and shares the same hostsync.Worker for
// background refresh.
type hostsTUIModel struct {
	database   *sql.DB
	appConfig  *config.Config
	syncWorker *hostsync.Worker

	rows     []hostsRow
	cursor   int
	width    int
	height   int
	refresh  time.Duration
	lastTick time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// hostsRow is one displayed row — either an inventory host or a running rental.
type hostsRow struct {
	kind     hostsRowKind
	name     string // display name: "cool30" or "wi3122"
	host     *dashboard.Host
	provider string // for rentals: "vastai" / "runpod"
	gpuSpec  string // canonical GPU summary
	jobs     int    // running jobs known on this row
}

type hostsRowKind int

const (
	hostsRowInventory hostsRowKind = iota
	hostsRowRental
)

func newHostsTUIModel(database *sql.DB) hostsTUIModel {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, _ := config.Load()
	cloudClients, _ := buildCloudClients(cfg)
	r2Client, _ := buildR2Client(cfg)

	worker := hostsync.New(database, cloudClients, r2Client, cfg)
	worker.Start()

	return hostsTUIModel{
		database:   database,
		appConfig:  cfg,
		syncWorker: worker,
		refresh:    5 * time.Second,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (m *hostsTUIModel) shutdown() {
	if m.syncWorker != nil {
		m.syncWorker.Stop()
	}
	if m.cancel != nil {
		m.cancel()
	}
}

func (m hostsTUIModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadRows()}
	if m.syncWorker != nil {
		cmds = append(cmds,
			m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return hostsSyncResultMsg{result: r}
			}),
		)
	}
	cmds = append(cmds, m.tick())
	return tea.Batch(cmds...)
}

type hostsRowsLoadedMsg struct {
	rows []hostsRow
	err  error
}

type hostsSyncResultMsg struct {
	result hostsync.Result
}

type hostsTickMsg struct{}

func (m hostsTUIModel) tick() tea.Cmd {
	return tea.Tick(m.refresh, func(time.Time) tea.Msg { return hostsTickMsg{} })
}

func (m hostsTUIModel) loadRows() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		rows, err := buildHostsRows(database)
		return hostsRowsLoadedMsg{rows: rows, err: err}
	}
}

// buildHostsRows assembles one row per inventory host and per running rental.
// On-prem hosts are hydrated from cached host info; rentals use launch fields.
func buildHostsRows(database *sql.DB) ([]hostsRow, error) {
	// Inventory + cached hosts.
	cachedByName := map[string]*db.CachedHostInfo{}
	if cached, err := db.LoadAllCachedHosts(database); err == nil {
		for _, c := range cached {
			if c == nil || c.Name == "" || db.IsLaunchHost(c.Name) {
				continue
			}
			cachedByName[c.Name] = c
		}
	}

	names, err := db.ListHostsForTUI(database)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}

	var invRows, rentRows []hostsRow

	for _, name := range names {
		if name == "" || db.IsLaunchHost(name) {
			continue
		}
		host := hostFromCache(name, cachedByName[name])
		invRows = append(invRows, hostsRow{
			kind:    hostsRowInventory,
			name:    name,
			host:    host,
			gpuSpec: hostGPUSpec(host),
		})
	}

	sort.SliceStable(invRows, func(i, j int) bool {
		return util.NaturalLess(invRows[i].name, invRows[j].name)
	})

	// Running rentals.
	launches, lerr := db.ListRunningLaunches(database)
	if lerr == nil {
		for _, launch := range launches {
			if launch == nil || launch.ID <= 0 {
				continue
			}
			jobs := 0
			if js, err := db.GetLaunchJobs(database, launch.ID); err == nil {
				for _, j := range js {
					if j == nil {
						continue
					}
					switch j.EffectiveStatus() {
					case db.StatusRunning, db.StatusStarting, db.StatusPaused, db.StatusQueued:
						jobs++
					}
				}
			}
			rentRows = append(rentRows, hostsRow{
				kind:     hostsRowRental,
				name:     ids.FormatInstanceID(launch.ID),
				host:     hostFromLaunch(launch),
				provider: strings.TrimSpace(launch.Provider),
				gpuSpec:  strings.TrimSpace(launch.DisplayGPUSpec()),
				jobs:     jobs,
			})
		}
	}
	sort.SliceStable(rentRows, func(i, j int) bool {
		return util.NaturalLess(rentRows[i].name, rentRows[j].name)
	})

	rows := make([]hostsRow, 0, len(invRows)+len(rentRows))
	rows = append(rows, invRows...)
	rows = append(rows, rentRows...)

	// Annotate inventory rows with their running-job counts.
	if jobs, err := db.ListActiveOnPremJobs(database); err == nil {
		counts := map[string]int{}
		for _, j := range jobs {
			if j == nil || !j.HasInventoryHost() {
				continue
			}
			switch j.EffectiveStatus() {
			case db.StatusRunning, db.StatusStarting, db.StatusPaused, db.StatusQueued:
				counts[j.Host]++
			}
		}
		for i := range rows {
			if rows[i].kind == hostsRowInventory {
				rows[i].jobs = counts[rows[i].name]
			}
		}
	}

	return rows, nil
}

// hostFromCache constructs a dashboard.Host suitable for rendering from a
// CachedHostInfo row. Returns a placeholder Host (unknown status) when the
// host isn't in the cache yet, so the row still renders.
func hostFromCache(name string, cached *db.CachedHostInfo) *dashboard.Host {
	if cached == nil {
		return &dashboard.Host{Name: name, Status: dashboard.HostStatusUnknown}
	}
	host := hostinfo.HostFromCachedInfo(cached)
	if host == nil {
		return &dashboard.Host{Name: name, Status: dashboard.HostStatusUnknown}
	}
	host.Name = name
	return host
}

// hostFromLaunch builds a placeholder Host for a rental. We don't yet probe
// rentals live, so CPU/MEM/GPU percentages render as "--"; the status glyph
// reflects the launch's lifecycle.
func hostFromLaunch(launch *db.Launch) *dashboard.Host {
	h := &dashboard.Host{Name: ids.FormatInstanceID(launch.ID)}
	switch launch.Status {
	case db.LaunchStatusRunning:
		h.Status = dashboard.HostStatusOnline
	case db.LaunchStatusLaunching, db.LaunchStatusPaused, db.LaunchStatusGrace:
		h.Status = dashboard.HostStatusChecking
	default:
		h.Status = dashboard.HostStatusOffline
	}
	return h
}

func hostGPUSpec(host *dashboard.Host) string {
	if host == nil || len(host.GPUs) == 0 {
		return ""
	}
	// Compose a compact "<count>x <name>" summary; mixed-GPU hosts get a
	// comma-joined list.
	counts := map[string]int{}
	order := []string{}
	for _, g := range host.GPUs {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			name = "GPU"
		}
		if _, seen := counts[name]; !seen {
			order = append(order, name)
		}
		counts[name]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%dx %s", counts[name], name))
	}
	return strings.Join(parts, ", ")
}

func (m hostsTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case hostsRowsLoadedMsg:
		if msg.err == nil {
			m.rows = msg.rows
			if m.cursor >= len(m.rows) {
				m.cursor = len(m.rows) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			if m.syncWorker != nil {
				// Kick a refresh for every inventory row so the metrics bars
				// fill in. Rentals are skipped — we don't yet probe them.
				for _, r := range m.rows {
					if r.kind == hostsRowInventory && r.name != "" {
						m.syncWorker.Request(hostsync.Request{
							Host: r.name,
							Rate: hostsync.RateRunning,
						})
					}
				}
			}
		}
		return m, nil

	case hostsSyncResultMsg:
		// Replace the cached Host for the affected row so the next render
		// shows fresh metrics. The worker also persists to the cache table,
		// so a full reload would pick this up — but updating in-place is
		// cheaper and keeps the cursor stable.
		if msg.result.Host != "" && msg.result.HostFull != nil {
			for i := range m.rows {
				if m.rows[i].kind == hostsRowInventory && m.rows[i].name == msg.result.Host {
					m.rows[i].host = msg.result.HostFull
					m.rows[i].gpuSpec = hostGPUSpec(msg.result.HostFull)
					break
				}
			}
		}
		if m.syncWorker != nil {
			return m, m.syncWorker.WaitForResult(m.ctx, func(r hostsync.Result) tea.Msg {
				return hostsSyncResultMsg{result: r}
			})
		}
		return m, nil

	case hostsTickMsg:
		return m, tea.Batch(m.loadRows(), m.tick())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m hostsTUIModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.shutdown()
		return m, tea.Quit
	case "esc":
		m.shutdown()
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil
	case "J":
		// Switch to the jobs list (grouped by status, the "uj" landing).
		return m, func() tea.Msg { return switchToListMsg{groupedByStatus: true} }
	case "i":
		// Switch to the system watch view, mirroring the list-TUI's 'i'.
		return m, func() tea.Msg { return switchToSystemWatchMsg{} }
	case "r":
		// Force a reload.
		return m, m.loadRows()
	}
	return m, nil
}

func (m hostsTUIModel) View() string {
	if len(m.rows) == 0 {
		return dimStyle().Render("Loading hosts...\n")
	}

	var b strings.Builder
	header := fmt.Sprintf("%-6s  %-32s  %-7s  %s", "TYPE", "HOST", "JOBS", "GPUs")
	b.WriteString(headerStyle().Render(header))
	b.WriteString("\n")

	for i, row := range m.rows {
		summary := dashboard.RenderHostSummarySegment(row.host, dashboard.HostSummaryFull)

		typeLabel := "host"
		if row.kind == hostsRowRental {
			typeLabel = "rental"
		}

		jobsCol := "—"
		if row.jobs > 0 {
			jobsCol = fmt.Sprintf("%d", row.jobs)
		}

		gpus := row.gpuSpec
		if gpus == "" {
			gpus = "—"
		}

		line := fmt.Sprintf("%-6s  %-32s  %-7s  %s", typeLabel, summary, jobsCol, gpus)
		if i == m.cursor {
			line = selectedRowStyle().Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(dimStyle().Render("↑/↓ navigate · J jobs list · i instances · r reload · q quit"))
	return b.String()
}

// styles — small wrappers so we don't import dashboard's private styles. Kept
// local to keep the new panel self-contained.
func headerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
}

func dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
}

func selectedRowStyle() lipgloss.Style {
	return lipgloss.NewStyle().Reverse(true)
}
