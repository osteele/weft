package cmd

import (
	"database/sql"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// switchToCampaignWatchMsg is emitted by campaignListModel when the user
// presses enter on a campaign.
type switchToCampaignWatchMsg struct {
	instanceIDs []int64
}

// campaignListRouterModel wraps either a campaignListModel or a watchModel
// in a single tea.Program to avoid alt-screen flicker on transitions.
type campaignListRouterModel struct {
	active     tea.Model
	database   *sql.DB
	windowSize tea.WindowSizeMsg
}

func newCampaignListRouterModel(database *sql.DB, campaigns []*db.Campaign) campaignListRouterModel {
	list := newCampaignListModel(database, campaigns, true)
	return campaignListRouterModel{
		active:   list,
		database: database,
	}
}

func (m campaignListRouterModel) Init() tea.Cmd {
	return m.active.Init()
}

func (m campaignListRouterModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowSize = msg
		updated, cmd := m.active.Update(msg)
		m.active = updated
		return m, cmd

	case switchToCampaignWatchMsg:
		// Clean up list model
		if list, ok := m.active.(campaignListModel); ok {
			if list.dbWatcher != nil {
				_ = list.dbWatcher.Close()
			}
		}
		// Create watch model for the selected campaign's instances
		cfg, _ := config.Load()
		var r2Client *r2.Client
		if cfg != nil {
			r2Client, _ = buildR2Client(cfg)
		}
		watch := newWatchModel(m.database, msg.instanceIDs, r2Client, cfg)
		m.active = watch
		cmds := []tea.Cmd{watch.Init()}
		if m.windowSize.Width > 0 {
			cmds = append(cmds, func() tea.Msg { return m.windowSize })
		}
		return m, tea.Batch(cmds...)
	}

	// Delegate all other messages to the active model
	updated, cmd := m.active.Update(msg)
	m.active = updated
	return m, cmd
}

func (m campaignListRouterModel) View() string {
	return m.active.View()
}
