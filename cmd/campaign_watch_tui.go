package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

type watchModel struct {
	instanceIDs []int64
	updates     map[int64]campaign.InstanceUpdate // latest update per instance
	channels    map[int64]<-chan campaign.InstanceUpdate
	database    *sql.DB
	clients     map[int64]cloud.Client // per-instance client (looked up from DB provider)
	spinner     spinner.Model
	done        bool
	err         error
	ctx         context.Context
	cancel      context.CancelFunc
	campaignID  int64     // campaign ID (0 if unknown)
	launchedAt  time.Time // campaign launch time
}

// Styles for the watch TUI (allocated once, not per-render).
var (
	watchTitleStyle     = lipgloss.NewStyle().Bold(true)
	watchStatusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	watchCompletedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	watchFailedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	watchDimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

type watchUpdateMsg struct {
	instanceID int64
	update     campaign.InstanceUpdate
	closed     bool // true if channel was closed
}

// watchSyncTickMsg triggers periodic cloud job result syncing.
type watchSyncTickMsg struct{}

func newWatchModel(database *sql.DB, instanceIDs []int64) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())

	// Build per-instance clients from DB provider field
	clients := make(map[int64]cloud.Client)
	for _, id := range instanceIDs {
		clients[id] = clientForInstance(database, id)
	}

	campaignID, launchedAt := campaignInfoFromInstances(database, instanceIDs)

	return watchModel{
		instanceIDs: instanceIDs,
		updates:     make(map[int64]campaign.InstanceUpdate),
		channels:    make(map[int64]<-chan campaign.InstanceUpdate),
		database:    database,
		clients:     clients,
		spinner:     s,
		ctx:         ctx,
		cancel:      cancel,
		campaignID:  campaignID,
		launchedAt:  launchedAt,
	}
}

func (m watchModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, m.spinner.Tick)

	for _, id := range m.instanceIDs {
		client := m.clients[id]
		ch := campaign.WatchInstance(m.ctx, client, m.database, id, 2*time.Second, 10*time.Second)
		m.channels[id] = ch
		cmds = append(cmds, waitForUpdate(id, ch))
	}

	// Start periodic cloud job sync
	cmds = append(cmds, scheduleSyncTick())

	return tea.Batch(cmds...)
}

// waitForUpdate reads the next value from an instance watch channel.
func waitForUpdate(instanceID int64, ch <-chan campaign.InstanceUpdate) tea.Cmd {
	return func() tea.Msg {
		update, ok := <-ch
		return watchUpdateMsg{instanceID: instanceID, update: update, closed: !ok}
	}
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.cancel()
			return m, tea.Quit
		}

	case watchUpdateMsg:
		if msg.closed {
			// Channel closed — instance reached terminal state
			return m, m.checkAllDone()
		}
		m.updates[msg.instanceID] = msg.update
		// Continue reading from the same channel
		ch := m.channels[msg.instanceID]
		return m, waitForUpdate(msg.instanceID, ch)

	case watchSyncTickMsg:
		// Sync cloud job results from R2 without blocking the UI
		return m, tea.Batch(
			func() tea.Msg {
				cfg, _ := config.Load()
				syncCloudJobResults(cfg, m.database, false)
				return nil
			},
			scheduleSyncTick(),
		)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m watchModel) checkAllDone() tea.Cmd {
	allDone := true
	for _, id := range m.instanceIDs {
		if u, ok := m.updates[id]; ok {
			if u.CloudInstance != nil && !campaign.IsInstanceTerminal(u.CloudInstance.Status) {
				allDone = false
				break
			}
		} else {
			allDone = false
			break
		}
	}
	if allDone {
		m.cancel()
		return tea.Quit
	}
	return nil
}

func (m watchModel) View() string {
	var b strings.Builder

	// Campaign header
	if m.campaignID > 0 {
		header := fmt.Sprintf("Campaign %d", m.campaignID)
		if !m.launchedAt.IsZero() {
			header += fmt.Sprintf(" — launched %s (%s ago)",
				m.launchedAt.Format("15:04"),
				time.Since(m.launchedAt).Truncate(time.Minute))
		}
		b.WriteString(watchTitleStyle.Render(header))
		b.WriteString("\n\n")
	}

	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if !ok {
			b.WriteString(m.spinner.View())
			b.WriteString(fmt.Sprintf(" Instance %d — waiting for data...\n\n", id))
			continue
		}

		ci := u.CloudInstance
		status := ci.Status
		var stStyle lipgloss.Style
		switch status {
		case db.CloudInstanceStatusCompleted:
			stStyle = watchCompletedStyle
		case db.CloudInstanceStatusFailed, db.CloudInstanceStatusCancelled:
			stStyle = watchFailedStyle
		default:
			stStyle = watchStatusStyle
		}

		header := fmt.Sprintf("Instance %d — %s — %s", ci.ID, ci.GPUSpec, stStyle.Render(status))
		b.WriteString(watchTitleStyle.Render(header))
		b.WriteString("\n")

		providerInstID := ci.EffectiveProviderID()
		if providerInstID != "" {
			instLine := fmt.Sprintf("  %s: %s", ci.Provider, providerInstID)
			if u.Instance != nil {
				instLine += fmt.Sprintf(" (%s)", u.Instance.Status)
			}
			b.WriteString(instLine + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %s: (provisioning...)\n", ci.Provider))
		}

		if u.Instance != nil && u.Instance.SSHHost != "" {
			b.WriteString(fmt.Sprintf("  SSH: %s\n", campaign.FormatSSHCommand(u.Instance)))
		}

		if u.BootstrapStage != "" {
			b.WriteString(fmt.Sprintf("  Bootstrap: %s\n", campaign.BootstrapStageLabel(u.BootstrapStage)))
		}

		if u.Instance != nil && ci.LaunchedAt != nil {
			uptime := time.Since(time.Unix(*ci.LaunchedAt, 0)).Truncate(time.Minute)
			cost := uptime.Hours() * u.Instance.CostPerHour
			b.WriteString(fmt.Sprintf("  Cost: $%.2f (uptime: %s)\n", cost, uptime))
		}

		// Jobs
		if len(u.Jobs) > 0 {
			completed := 0
			for _, j := range u.Jobs {
				if j.Status == db.StatusCompleted || j.Status == db.StatusFailed {
					completed++
				}
			}
			b.WriteString(fmt.Sprintf("  Jobs: %d/%d completed\n", completed, len(u.Jobs)))

			for _, j := range u.Jobs {
				desc := j.Description
				if desc == "" {
					desc = campaign.TruncateCommand(j.Command, 50)
				}
				var jobStyle lipgloss.Style
				switch j.Status {
				case db.StatusCompleted:
					jobStyle = watchCompletedStyle
				case db.StatusFailed:
					jobStyle = watchFailedStyle
				default:
					jobStyle = watchDimStyle
				}
				b.WriteString(fmt.Sprintf("    %4d  %s  %s\n",
					j.ID,
					jobStyle.Render(fmt.Sprintf("%-10s", j.Status)),
					desc,
				))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString(watchDimStyle.Render("ctrl-c to exit (instances continue in background)"))
	b.WriteString("\n")

	return b.String()
}

// scheduleSyncTick returns a command that fires a sync tick after 15 seconds.
func scheduleSyncTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

// watchInstances runs the interactive TUI watch for one or more cloud instances.
func watchInstances(database *sql.DB, instanceIDs []int64) error {
	model := newWatchModel(database, instanceIDs)
	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
