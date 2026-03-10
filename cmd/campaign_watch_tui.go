package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// initialInstanceInfo holds pre-fetched DB data for instances that haven't
// received a channel update yet, avoiding repeated queries in View().
type initialInstanceInfo struct {
	ci   *db.CloudInstance
	jobs []*db.Job
}

type watchModel struct {
	instanceIDs []int64
	updates     map[int64]campaign.InstanceUpdate // latest update per instance
	channels    map[int64]<-chan campaign.InstanceUpdate
	initInfo    map[int64]initialInstanceInfo // pre-fetched data for pre-update display
	database    *sql.DB
	clients     map[int64]cloud.Client // per-instance client (looked up from DB provider)
	r2Client    *r2.Client             // R2 client for phase/bootstrap fetching (may be nil)
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
	watchRunningStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	watchCompletedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
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

func newWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())

	// Build per-instance clients from DB provider field
	clients := make(map[int64]cloud.Client)
	for _, id := range instanceIDs {
		clients[id] = clientForInstance(database, id)
	}

	// Pre-fetch DB data for initial display (avoids queries in View)
	initInfo := make(map[int64]initialInstanceInfo)
	for _, id := range instanceIDs {
		ci, _ := db.GetCloudInstance(database, id)
		jobs, _ := db.GetCloudInstanceJobsIncludingAttempts(database, id)
		initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs}
	}

	campaignID, launchedAt := campaignInfoFromInstances(database, instanceIDs)

	return watchModel{
		instanceIDs: instanceIDs,
		updates:     make(map[int64]campaign.InstanceUpdate),
		channels:    make(map[int64]<-chan campaign.InstanceUpdate),
		initInfo:    initInfo,
		database:    database,
		clients:     clients,
		r2Client:    r2Client,
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
		ch := campaign.WatchInstance(m.ctx, client, m.database, id, 2*time.Second, 10*time.Second, m.r2Client)
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
			// No channel update yet — show pre-fetched DB data
			info := m.initInfo[id]
			ci := info.ci
			jobs := info.jobs
			if ci != nil {
				header := fmt.Sprintf("Instance %d — %s — %s", ci.ID, ci.DisplayGPUSpec(), watchStatusStyle.Render("launching"))
				b.WriteString(watchTitleStyle.Render(header))
				b.WriteString(" " + m.spinner.View() + "\n")
				providerInstID := ci.EffectiveProviderID()
				if providerInstID != "" {
					b.WriteString(fmt.Sprintf("  %s: %s\n", ci.Provider, providerInstID))
				} else {
					b.WriteString(fmt.Sprintf("  %s: (provisioning...)\n", ci.Provider))
				}
				if len(jobs) > 0 {
					b.WriteString(fmt.Sprintf("  Jobs: 0/%d completed\n", len(jobs)))
					for _, j := range jobs {
						desc := j.Description
						if desc == "" {
							desc = campaign.TruncateCommand(j.Command, 50)
						}
						b.WriteString(fmt.Sprintf("    %4d  %s  %s\n",
							j.ID,
							watchDimStyle.Render(fmt.Sprintf("%-12s", j.Status)),
							desc,
						))
					}
				}
				b.WriteString("\n")
			} else {
				b.WriteString(m.spinner.View())
				b.WriteString(fmt.Sprintf(" Instance %d — waiting for data...\n\n", id))
			}
			continue
		}

		ci := u.CloudInstance
		statusLabel := ci.Status
		var stStyle lipgloss.Style
		switch ci.Status {
		case db.CloudInstanceStatusCompleted:
			stStyle = watchCompletedStyle
		case db.CloudInstanceStatusFailed, db.CloudInstanceStatusCancelled:
			stStyle = watchFailedStyle
		default:
			stStyle = watchStatusStyle
		}
		if label := ci.GraceStatusLabel(); label != "" {
			statusLabel = label
		}
		if ci.TerminationReason != "" && ci.TerminationReason != db.TerminationReasonCompleted {
			statusLabel += " (" + ci.TerminationReason + ")"
		}

		header := fmt.Sprintf("Instance %d — %s — %s", ci.ID, ci.DisplayGPUSpec(), stStyle.Render(statusLabel))
		b.WriteString(watchTitleStyle.Render(header))
		b.WriteString("\n")

		providerInstID := ci.EffectiveProviderID()
		if providerInstID != "" {
			instLine := fmt.Sprintf("  %s: %s", ci.Provider, providerInstID)
			if u.Instance != nil && !campaign.IsInstanceTerminal(ci.Status) {
				instLine += fmt.Sprintf(" (%s)", u.Instance.Status)
			}
			b.WriteString(instLine + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %s: (provisioning...)\n", ci.Provider))
		}

		if u.BootstrapStage != "" {
			b.WriteString(fmt.Sprintf("  Bootstrap: %s\n", campaign.BootstrapStageLabel(u.BootstrapStage)))
		}
		if u.InstancePhase != "" {
			b.WriteString(fmt.Sprintf("  Phase: %s\n", campaign.InstancePhaseLabel(u.InstancePhase)))
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
				case db.StatusRunning:
					jobStyle = watchRunningStyle
				case db.StatusCompleted:
					jobStyle = watchCompletedStyle
				case db.StatusFailed:
					jobStyle = watchFailedStyle
				default:
					jobStyle = watchDimStyle
				}
				statusText := j.Status
				if j.Status == db.StatusRunning && u.JobProgress >= 0 && u.JobProgressID == j.ID {
					statusText = fmt.Sprintf("running %3d%%", u.JobProgress)
				}
				b.WriteString(fmt.Sprintf("    %4d  %s  %s\n",
					j.ID,
					jobStyle.Render(fmt.Sprintf("%-12s", statusText)),
					desc,
				))
			}
		}
		b.WriteString("\n")
	}

	// Campaign cost total
	if len(m.instanceIDs) > 1 {
		var totalCost float64
		hasCost := false
		for _, id := range m.instanceIDs {
			u, ok := m.updates[id]
			if !ok || u.CloudInstance == nil || u.Instance == nil || u.CloudInstance.LaunchedAt == nil {
				continue
			}
			uptime := time.Since(time.Unix(*u.CloudInstance.LaunchedAt, 0)).Truncate(time.Minute)
			totalCost += uptime.Hours() * u.Instance.CostPerHour
			hasCost = true
		}
		if hasCost {
			b.WriteString(watchTitleStyle.Render(fmt.Sprintf("Total: $%.2f (%d instances)", totalCost, len(m.instanceIDs))))
			b.WriteString("\n\n")
		}
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
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	model := newWatchModel(database, instanceIDs, r2Client)

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
