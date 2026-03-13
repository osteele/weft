package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// Styles for the campaign list TUI (allocated once, not per-render).
var (
	listTitleStyle    = lipgloss.NewStyle().Bold(true)
	listCursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	listActiveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	listTerminalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

// campaignListItem is a row in the campaign list: either a date header or a campaign entry.
type campaignListItem struct {
	isHeader   bool
	headerText string
	campaign   *db.Campaign
	instances  []*db.CloudInstance
	actualCost string
}

type campaignListModel struct {
	items    []campaignListItem
	cursor   int
	database *sql.DB
	quitting bool
	chosen   *db.Campaign // selected campaign to watch
}

func newCampaignListModel(database *sql.DB, campaigns []*db.Campaign) campaignListModel {
	// Group campaigns by date
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

	// Set cursor to first non-header item
	cursor := 0
	for i, item := range items {
		if !item.isHeader {
			cursor = i
			break
		}
	}

	return campaignListModel{
		items:    items,
		cursor:   cursor,
		database: database,
	}
}

func (m campaignListModel) Init() tea.Cmd {
	return nil
}

func (m campaignListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			m.cursor = m.prevSelectable(m.cursor)
		case "down", "j":
			m.cursor = m.nextSelectable(m.cursor)
		case "enter":
			if m.cursor >= 0 && m.cursor < len(m.items) && !m.items[m.cursor].isHeader {
				m.chosen = m.items[m.cursor].campaign
				return m, tea.Quit
			}
		}
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
	var b strings.Builder

	b.WriteString(listTitleStyle.Render("Campaigns"))
	b.WriteString("\n\n")

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
	b.WriteString(watchDimStyle.Render("↑/↓ navigate • enter to watch • q to quit"))
	b.WriteString("\n")

	return b.String()
}

// runCampaignListTUI launches the interactive campaign list TUI.
// Returns the selected campaign for watching, or nil if the user quit.
func runCampaignListTUI(database *sql.DB, campaigns []*db.Campaign) error {
	model := newCampaignListModel(database, campaigns)

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	m := finalModel.(campaignListModel)
	if m.chosen == nil {
		return nil
	}

	// Get instance IDs for the chosen campaign and enter watch mode
	instances, err := db.GetCampaignInstances(database, m.chosen.ID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		fmt.Printf("Campaign %d has no instances.\n", m.chosen.ID)
		return nil
	}

	var instanceIDs []int64
	for _, inst := range instances {
		instanceIDs = append(instanceIDs, inst.ID)
	}

	return watchInstances(database, instanceIDs)
}
