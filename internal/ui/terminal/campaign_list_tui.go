package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

const campaignListSyncInterval = TerminalSyncInterval

// Aliases for shared TUI styles used in the campaign list TUI.
var (
	listTitleStyle    = tuiTitleStyle
	listCursorStyle   = tuiCursorStyle
	listActiveStyle   = tuiRunningStyle
	listTerminalStyle = tuiDimStyle
)

// campaignListItem is a row in the campaign list: either a date header or a campaign entry.
type campaignListItem struct {
	isHeader   bool
	headerText string
	campaign   *db.Campaign
	instances  []*db.Launch
	actualCost string
}

type campaignListModel struct {
	items           []campaignListItem
	cursor          int
	database        *sql.DB
	quitting        bool
	showHelp        bool
	syncEnabled     bool
	syncInProgress  bool
	statusMessage   string
	dbWatcher       *dbwatch.Source
	debounceActive  bool
	debouncePending bool
	focused         bool
}

type campaignListLoadedMsg struct {
	items []campaignListItem
	err   error
}

type campaignListSyncFinishedMsg struct {
	warnings []string
	full     bool
}

type campaignListDBWatcherReadyMsg struct {
	source *dbwatch.Source
	err    error
}

type campaignListDBWatchEventMsg struct {
	err error
}

type campaignListDBRefreshTriggeredMsg struct{}

type campaignListSyncTickMsg struct{}

func newCampaignListModel(database *sql.DB, campaigns []*db.Campaign, syncEnabled bool) campaignListModel {
	model := campaignListModel{
		database:       database,
		syncEnabled:    syncEnabled,
		syncInProgress: syncEnabled,
		focused:        true,
	}
	model.applyItems(buildCampaignListItems(database, campaigns))
	return model
}

func buildCampaignListItems(database *sql.DB, campaigns []*db.Campaign) []campaignListItem {
	var items []campaignListItem
	var lastDate string

	for _, c := range campaigns {
		date := time.Unix(c.CreatedAt, 0).Format("Jan 2, 2006")
		if date != lastDate {
			items = append(items, campaignListItem{isHeader: true, headerText: date})
			lastDate = date
		}
		instances, _ := db.GetCampaignInstances(database, c.ID)
		items = append(items, campaignListItem{
			campaign:   c,
			instances:  instances,
			actualCost: campaignActualCost(instances),
		})
	}

	return items
}

func (m *campaignListModel) applyItems(items []campaignListItem) {
	selectedID := int64(0)
	if m.cursor >= 0 && m.cursor < len(m.items) {
		if current := m.items[m.cursor]; !current.isHeader && current.campaign != nil {
			selectedID = current.campaign.ID
		}
	}

	m.items = items
	m.cursor = 0
	if selectedID == 0 {
		m.cursor = firstCampaignListSelectable(items)
		return
	}
	for i, item := range items {
		if item.isHeader || item.campaign == nil {
			continue
		}
		if item.campaign.ID == selectedID {
			m.cursor = i
			return
		}
	}
	m.cursor = firstCampaignListSelectable(items)
}

func firstCampaignListSelectable(items []campaignListItem) int {
	for i, item := range items {
		if !item.isHeader {
			return i
		}
	}
	return 0
}

func (m campaignListModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.startDBWatcher(), m.scheduleCampaignListSyncTick()}
	if m.syncEnabled {
		cmds = append(cmds, m.runBackgroundSync(false))
	}
	return tea.Batch(cmds...)
}

func (m campaignListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case tea.KeyMsg:
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "enter":
				m.showHelp = false
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			if m.dbWatcher != nil {
				_ = m.dbWatcher.Close()
			}
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend
		case "up", "k":
			m.cursor = m.prevSelectable(m.cursor)
		case "down", "j":
			m.cursor = m.nextSelectable(m.cursor)
		case "enter":
			if m.cursor >= 0 && m.cursor < len(m.items) && !m.items[m.cursor].isHeader {
				c := m.items[m.cursor].campaign
				instances, _ := db.GetCampaignInstances(m.database, c.ID)
				if len(instances) == 0 {
					m.statusMessage = fmt.Sprintf("Campaign %d has no instances.", c.ID)
					return m, nil
				}
				instanceIDs := make([]int64, len(instances))
				for i, inst := range instances {
					instanceIDs[i] = inst.ID
				}
				return m, func() tea.Msg {
					return switchToCampaignWatchMsg{instanceIDs: instanceIDs}
				}
			}
		case "?":
			m.showHelp = true
			return m, nil
		}
		return m, nil

	case campaignListLoadedMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("Refresh error: %v", msg.err)
			return m, nil
		}
		m.applyItems(msg.items)
		if len(m.items) > 0 && strings.HasPrefix(m.statusMessage, "No campaigns") {
			m.statusMessage = ""
		}
		return m, nil

	case campaignListSyncFinishedMsg:
		if !msg.full {
			if len(msg.warnings) > 0 {
				m.statusMessage = strings.Join(msg.warnings, " | ")
			} else {
				m.statusMessage = "Running full sync..."
			}
			return m, tea.Batch(m.reloadCampaigns(), m.runBackgroundSync(true))
		}

		m.syncInProgress = false
		if len(msg.warnings) > 0 {
			m.statusMessage = strings.Join(msg.warnings, " | ")
		} else if len(m.items) == 0 {
			m.statusMessage = "No campaigns yet."
		} else {
			m.statusMessage = ""
		}
		return m, m.reloadCampaigns()

	case campaignListDBWatcherReadyMsg:
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
			return m, nil
		}
		m.dbWatcher = msg.source
		return m, m.waitForDBEvent()

	case campaignListDBWatchEventMsg:
		cmds := []tea.Cmd{}
		if msg.err != nil {
			m.statusMessage = fmt.Sprintf("DB watch error: %v", msg.err)
		} else {
			if !m.debounceActive {
				m.debounceActive = true
				cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
					return campaignListDBRefreshTriggeredMsg{}
				}))
			} else {
				// Queue one trailing refresh so bursty DB writes don't leave stale rows.
				m.debouncePending = true
			}
		}
		if cmd := m.waitForDBEvent(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case campaignListDBRefreshTriggeredMsg:
		cmds := []tea.Cmd{m.reloadCampaigns()}
		if m.debouncePending {
			m.debouncePending = false
			m.debounceActive = true
			cmds = append(cmds, tea.Tick(listDBChangeDebounce, func(time.Time) tea.Msg {
				return campaignListDBRefreshTriggeredMsg{}
			}))
		} else {
			m.debounceActive = false
		}
		return m, tea.Batch(cmds...)

	case campaignListSyncTickMsg:
		cmds := []tea.Cmd{m.scheduleCampaignListSyncTick()}
		if !m.syncEnabled || m.syncInProgress {
			return m, tea.Batch(cmds...)
		}
		m.syncInProgress = true
		m.statusMessage = "Refreshing..."
		cmds = append(cmds, m.runBackgroundSync(true))
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m campaignListModel) prevSelectable(from int) int {
	for i := from - 1; i >= 0; i-- {
		if !m.items[i].isHeader {
			return i
		}
	}
	return from
}

func (m campaignListModel) nextSelectable(from int) int {
	for i := from + 1; i < len(m.items); i++ {
		if !m.items[i].isHeader {
			return i
		}
	}
	return from
}

func (m campaignListModel) View() string {
	if m.showHelp {
		return m.renderCampaignListHelpView()
	}

	var b strings.Builder
	sharedStatusLines := renderSharedTUIStatusLines(m.database, 0, 0)

	b.WriteString(listTitleStyle.Render("Campaigns"))
	if m.syncInProgress {
		b.WriteString("  ")
		b.WriteString(watchDimStyle.Render("syncing..."))
	}
	b.WriteString("\n\n")

	if len(m.items) == 0 {
		b.WriteString(watchDimStyle.Render(m.emptyStateText()))
		b.WriteString("\n\n")
	} else {
		for i, item := range m.items {
			if item.isHeader {
				b.WriteString(listTitleStyle.Render(item.headerText))
				b.WriteString("\n")
				continue
			}

			c := item.campaign
			cursor := "  "
			if i == m.cursor {
				cursor = listCursorStyle.Render("> ")
			}

			estCost := campaign.FormatEstimatedCostCents(c.EstimatedCostCents)
			created := time.Unix(c.CreatedAt, 0).Format("15:04")

			var statusStyle lipgloss.Style
			switch c.Status {
			case db.CampaignStatusRunning, db.CampaignStatusLaunching:
				statusStyle = listActiveStyle
			default:
				statusStyle = listTerminalStyle
			}

			line := fmt.Sprintf("%s#%-4d %s  %d inst  est %s  actual %s  %s",
				cursor,
				c.ID,
				statusStyle.Render(fmt.Sprintf("%-10s", c.Status)),
				len(item.instances),
				estCost,
				item.actualCost,
				created,
			)
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	for _, line := range sharedStatusLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	footer := []string{}
	if m.statusMessage != "" {
		footer = append(footer, m.statusMessage)
	}
	footer = append(footer, "↑/↓ navigate", "enter to watch", "? help", "q to quit")
	b.WriteString(watchDimStyle.Render(strings.Join(footer, "  ")))
	b.WriteString("\n")

	return b.String()
}

func (m campaignListModel) renderCampaignListHelpView() string {
	lines := []string{
		"Campaign List Keybindings",
		"",
		"Navigation:",
		"  up/down (or j/k) move selection",
		"  enter watch selected campaign",
		"",
		"General:",
		"  q or Esc quit campaign list",
		"",
		"Help:",
		"  ? toggle this help",
		"  q or Esc close help",
	}

	var b strings.Builder
	b.WriteString(listTitleStyle.Render(lines[0]))
	b.WriteString("\n")
	for i := 1; i < len(lines); i++ {
		b.WriteString(watchDimStyle.Render(lines[i]))
		b.WriteString("\n")
	}
	b.WriteString(watchDimStyle.Render("? close help"))
	return b.String()
}

func (m campaignListModel) emptyStateText() string {
	if m.syncInProgress {
		return "No campaigns yet. Waiting for startup sync and DB updates..."
	}
	if m.statusMessage != "" {
		return "No campaigns. " + m.statusMessage
	}
	return "No campaigns."
}

func (m campaignListModel) reloadCampaigns() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		campaigns, err := db.ListCampaigns(database)
		if err != nil {
			return campaignListLoadedMsg{err: err}
		}
		return campaignListLoadedMsg{items: buildCampaignListItems(database, campaigns)}
	}
}

func (m campaignListModel) runBackgroundSync(full bool) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		return campaignListSyncFinishedMsg{warnings: syncCampaignListTUIData(database, full), full: full}
	}
}

func syncCampaignListTUIData(database *sql.DB, full bool) []string {
	return syncCloudStateForTUI(database, full)
}

func (m campaignListModel) startDBWatcher() tea.Cmd {
	return func() tea.Msg {
		source, err := dbwatch.OpenChangeSource()
		if err != nil {
			return campaignListDBWatcherReadyMsg{err: err}
		}
		if source == nil {
			return nil
		}
		return campaignListDBWatcherReadyMsg{source: source}
	}
}

func (m campaignListModel) waitForDBEvent() tea.Cmd {
	if m.dbWatcher == nil {
		return nil
	}
	source := m.dbWatcher

	return func() tea.Msg {
		_, err := source.Wait(context.Background(), 0)
		if err != nil {
			return campaignListDBWatchEventMsg{err: err}
		}
		return campaignListDBWatchEventMsg{}
	}
}

func (m campaignListModel) scheduleCampaignListSyncTick() tea.Cmd {
	return tea.Tick(throttledInterval(campaignListSyncInterval, m.focused), func(time.Time) tea.Msg {
		return campaignListSyncTickMsg{}
	})
}

// runCampaignListTUI launches the interactive campaign list TUI.
// Uses a router model so that selecting a campaign transitions seamlessly
// to the watch TUI without exiting the alt screen.
func runCampaignListTUI(database *sql.DB, campaigns []*db.Campaign) error {
	router := newCampaignListRouterModel(database, campaigns)

	stdio := InstallTUIStdioCapture()

	p := tea.NewProgram(router, stdio.Option, tea.WithAltScreen(), tea.WithReportFocus())
	finalModel, err := p.Run()
	stdio.Restore()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	// Print exit report if we ended in watch mode
	if r, ok := finalModel.(campaignListRouterModel); ok {
		if w, ok := r.active.(watchModel); ok && w.done {
			if finalView := renderWatchExitSnapshot(w); finalView != "" {
				fmt.Print(finalView)
			}
			printWatchExitReport(database, w.instanceIDs)
		}
	}
	return nil
}
