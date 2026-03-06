package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

// Styles for the launch TUI (allocated once, not per-render).
var (
	launchTitleStyle    = lipgloss.NewStyle().Bold(true)
	launchHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	launchSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	launchDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	launchCursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	launchErrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// listItem is a union type for the flat list of items in the launch selector.
type listItem struct {
	isHeader bool
	groupIdx int    // index into groups/groupOffers
	label    string // header text or job line
	jobID    int64  // only for job rows
}

type launchModel struct {
	groups      []campaign.InstanceGroup
	groupOffers []campaign.GroupOffer

	items    []listItem
	cursor   int
	selected map[int64]bool // job ID -> checked

	loading   bool
	launching bool
	done      bool
	err       error
	phase     string // launch progress phase

	instanceIDs []int64
	database    *sql.DB
	clients     []cloud.Client
	appConfig   *config.Config
	launchOpts  campaign.LaunchOpts

	spinner spinner.Model
	width   int
	height  int
}

// Messages
type offersLoadedMsg struct {
	offers []campaign.GroupOffer
	err    error
}

type instancesLaunchedMsg struct {
	instanceIDs []int64
	err         error
}

type launchPhaseMsg struct {
	phase string
}

func newLaunchModel(database *sql.DB, clients []cloud.Client, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts) launchModel {
	// Build flat list of items
	var items []listItem
	selected := make(map[int64]bool)

	for i, g := range groups {
		items = append(items, listItem{
			isHeader: true,
			groupIdx: i,
			label:    fmt.Sprintf("%s (%d jobs)", g.GPUSpec(), len(g.Jobs)),
		})
		for _, job := range g.Jobs {
			items = append(items, listItem{
				groupIdx: i,
				jobID:    job.ID,
				label:    campaign.FormatJobLine(job),
			})
			selected[job.ID] = true
		}
	}

	// Position cursor on first non-header item
	cursor := 0
	for i, item := range items {
		if !item.isHeader {
			cursor = i
			break
		}
	}

	s := spinner.New()
	s.Spinner = spinner.Dot

	return launchModel{
		groups:     groups,
		items:      items,
		cursor:     cursor,
		selected:   selected,
		loading:    true,
		database:   database,
		clients:    clients,
		appConfig:  cfg,
		launchOpts: opts,
		spinner:    s,
	}
}

func (m launchModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		m.fetchOffers(),
	)
}

func (m launchModel) fetchOffers() tea.Cmd {
	groups := m.groups
	clients := m.clients
	return func() tea.Msg {
		if len(clients) == 0 {
			return offersLoadedMsg{err: fmt.Errorf("no cloud providers available")}
		}
		offers := campaign.FetchGroupOffers(clients, groups)
		return offersLoadedMsg{offers: offers}
	}
}

func (m launchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case offersLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.groupOffers = msg.offers
		return m, nil

	case launchPhaseMsg:
		m.phase = msg.phase
		return m, nil

	case instancesLaunchedMsg:
		m.launching = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.done = true
		m.instanceIDs = msg.instanceIDs
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m launchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.launching {
		return m, nil // ignore keys while launching
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		m.cursor--
		for m.cursor >= 0 && m.items[m.cursor].isHeader {
			m.cursor--
		}
		if m.cursor < 0 {
			// Find first non-header
			for i, item := range m.items {
				if !item.isHeader {
					m.cursor = i
					break
				}
			}
		}
		return m, nil

	case "down", "j":
		m.cursor++
		for m.cursor < len(m.items) && m.items[m.cursor].isHeader {
			m.cursor++
		}
		if m.cursor >= len(m.items) {
			// Find last non-header
			for i := len(m.items) - 1; i >= 0; i-- {
				if !m.items[i].isHeader {
					m.cursor = i
					break
				}
			}
		}
		return m, nil

	case " ":
		if m.cursor >= 0 && m.cursor < len(m.items) && !m.items[m.cursor].isHeader {
			id := m.items[m.cursor].jobID
			m.selected[id] = !m.selected[id]
		}
		return m, nil

	case "a":
		for id := range m.selected {
			m.selected[id] = true
		}
		return m, nil

	case "n":
		for id := range m.selected {
			m.selected[id] = false
		}
		return m, nil

	case "enter":
		if m.loading {
			return m, nil
		}
		// Count selected
		count := 0
		for _, v := range m.selected {
			if v {
				count++
			}
		}
		if count == 0 {
			return m, tea.Quit
		}
		m.launching = true
		return m, tea.Batch(m.spinner.Tick, m.launchInstances())
	}

	return m, nil
}

func (m launchModel) launchInstances() tea.Cmd {
	// Build filtered groups with only selected jobs
	var filteredGroups []campaign.InstanceGroup
	var filteredOffers []campaign.GroupOffer

	for i, g := range m.groups {
		var selectedJobs []*db.Job
		for _, job := range g.Jobs {
			if m.selected[job.ID] {
				selectedJobs = append(selectedJobs, job)
			}
		}
		if len(selectedJobs) == 0 {
			continue
		}
		fg := campaign.InstanceGroup{
			GPUClass: g.GPUClass,
			GPUMemGB: g.GPUMemGB,
			Jobs:     selectedJobs,
		}
		filteredGroups = append(filteredGroups, fg)
		if m.groupOffers != nil && i < len(m.groupOffers) {
			filteredOffers = append(filteredOffers, m.groupOffers[i])
		}
	}

	database := m.database
	clients := m.clients
	cfg := m.appConfig
	opts := m.launchOpts

	return func() tea.Msg {
		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		createOpts := cloud.DefaultCreateOpts(cfg.Vastai.DefaultImage)

		// Create campaign batch record
		campaignRec := &db.Campaign{
			Status: db.CampaignStatusLaunching,
		}
		campaignID, err := db.CreateCampaign(database, campaignRec)
		if err != nil {
			return instancesLaunchedMsg{err: fmt.Errorf("create campaign: %w", err)}
		}

		// Launch instances in parallel
		var mu sync.Mutex
		var instanceIDs []int64
		var launchErrors []error
		var wg sync.WaitGroup

		for i, fg := range filteredGroups {
			var offer cloud.Offer
			if i < len(filteredOffers) && filteredOffers[i].Offer != nil {
				offer = *filteredOffers[i].Offer
			} else {
				mu.Lock()
				launchErrors = append(launchErrors, fmt.Errorf("no offer available for group %s", fg.GPUSpec()))
				mu.Unlock()
				continue
			}

			wg.Add(1)
			go func(group campaign.InstanceGroup, ofr cloud.Offer) {
				defer wg.Done()

				client := cloudClientForProvider(clients, ofr.Provider)
				if client == nil {
					mu.Lock()
					launchErrors = append(launchErrors, fmt.Errorf("no client for provider %s", ofr.Provider))
					mu.Unlock()
					return
				}

				cID, err := campaign.LaunchInstance(
					client, database, &campaignID, group, ofr, opts, r2Cfg, createOpts,
					func(phase string) {
						// Progress callback — prefix with GPU spec
					},
				)

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					launchErrors = append(launchErrors, fmt.Errorf("launch instance for %s: %w", group.GPUSpec(), err))
				} else {
					instanceIDs = append(instanceIDs, cID)
				}
			}(fg, offer)
		}

		wg.Wait()

		// Update campaign status based on results
		if len(instanceIDs) == 0 && len(launchErrors) > 0 {
			_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
			return instancesLaunchedMsg{err: launchErrors[0]}
		}
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusRunning)

		return instancesLaunchedMsg{instanceIDs: instanceIDs}
	}
}

func (m launchModel) View() string {
	var b strings.Builder

	if m.err != nil {
		b.WriteString(launchErrStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
		return b.String()
	}

	if m.done {
		b.WriteString("Launched instances: ")
		for i, id := range m.instanceIDs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%d", id))
		}
		b.WriteString("\n")
		return b.String()
	}

	if m.launching {
		b.WriteString(m.spinner.View())
		b.WriteString(" Launching instances...")
		if m.phase != "" {
			b.WriteString(fmt.Sprintf(" (%s)", m.phase))
		}
		b.WriteString("\n")
		return b.String()
	}

	// Loading state
	if m.loading {
		b.WriteString(m.spinner.View())
		b.WriteString(" Searching for GPU offers...\n\n")
	}

	// Count totals
	totalJobs := 0
	for _, g := range m.groups {
		totalJobs += len(g.Jobs)
	}

	b.WriteString(launchTitleStyle.Render(fmt.Sprintf("needs_rental jobs (%d jobs, %d GPU groups)", totalJobs, len(m.groups))))
	b.WriteString("\n\n")

	// Job list with checkboxes
	for _, item := range m.items {
		if item.isHeader {
			b.WriteString(launchHeaderStyle.Render(item.label))
			b.WriteString("\n")
			continue
		}

		isCursor := m.cursor >= 0 && m.cursor < len(m.items) && m.items[m.cursor].jobID == item.jobID
		checked := m.selected[item.jobID]

		var checkbox string
		if checked {
			checkbox = "[x]"
		} else {
			checkbox = "[ ]"
		}

		line := fmt.Sprintf("%s %s", checkbox, item.label)

		if isCursor {
			b.WriteString(launchCursorStyle.Render("> " + line))
		} else if checked {
			b.WriteString(launchSelectedStyle.Render("  " + line))
		} else {
			b.WriteString(launchDimStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}

	// Cost estimate table
	if m.groupOffers != nil {
		b.WriteString("\n")
		b.WriteString(launchDimStyle.Render("── Cost Estimate ─────────────────────────────────────"))
		b.WriteString("\n")
		b.WriteString(campaign.FormatCostTable(m.groupOffers))
	}

	// Help line
	b.WriteString("\n")
	selectedCount := 0
	for _, v := range m.selected {
		if v {
			selectedCount++
		}
	}
	help := "↑/↓ navigate  space toggle  a all  n none  enter launch  q quit"
	if selectedCount == 0 {
		help = "↑/↓ navigate  space toggle  a all  n none  enter quit  q quit"
	}
	b.WriteString(launchDimStyle.Render(help))
	b.WriteString("\n")

	return b.String()
}
