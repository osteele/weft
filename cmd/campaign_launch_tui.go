package cmd

import (
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
	"github.com/osteele/weft/internal/predictor"
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
	groups        []campaign.InstanceGroup
	groupOffers   []campaign.GroupOffer
	costEstimates []campaign.CostEstimate
	predConfig    *predictor.Config

	items    []listItem
	cursor   int
	selected map[int64]bool // job ID -> checked

	showCostDetail bool

	loading          bool
	launching        bool
	done             bool
	err              error
	phase            string              // launch progress phase
	estimateProgress estimateProgressMsg // latest estimation progress
	progressCh       chan estimateProgressMsg

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

type estimatesLoadedMsg struct {
	estimates []campaign.CostEstimate
}

type estimateProgressMsg struct {
	phase           string // e.g., "Resolving model sizes", "Estimating job durations"
	resolved, total int
}

type instancesLaunchedMsg struct {
	instanceIDs []int64
	err         error
}

type launchPhaseMsg struct {
	phase string
}

func newLaunchModel(database *sql.DB, clients []cloud.Client, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, predCfg *predictor.Config) launchModel {
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
		predConfig: predCfg,
		progressCh: make(chan estimateProgressMsg, 1),
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

func (m launchModel) fetchEstimates() tea.Cmd {
	groupOffers := m.groupOffers
	predConfig := m.predConfig
	ch := m.progressCh
	return func() tea.Msg {
		onProgress := func(phase string, resolved, total int) {
			// Non-blocking send — if channel is full, skip this update
			select {
			case ch <- estimateProgressMsg{phase: phase, resolved: resolved, total: total}:
			default:
			}
		}
		estimates := campaign.EstimateCosts(groupOffers, predConfig, onProgress)
		return estimatesLoadedMsg{estimates: estimates}
	}
}

// waitForProgress returns a Cmd that reads one progress message from the channel.
func waitForProgress(ch chan estimateProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
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
		return m, tea.Batch(m.fetchEstimates(), waitForProgress(m.progressCh))

	case estimateProgressMsg:
		m.estimateProgress = msg
		if m.costEstimates == nil {
			return m, waitForProgress(m.progressCh)
		}
		return m, nil

	case estimatesLoadedMsg:
		m.costEstimates = msg.estimates
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
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
		return m, nil

	case " ":
		if m.cursor < len(m.items) {
			item := m.items[m.cursor]
			if item.isHeader {
				// Toggle all jobs in this group
				m.toggleGroup(item.groupIdx)
			} else {
				id := item.jobID
				m.selected[id] = !m.selected[id]
			}
		}
		return m, nil

	case "d":
		m.showCostDetail = !m.showCostDetail
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

// toggleGroup toggles all jobs in the given group. If any are unselected,
// select all; otherwise deselect all.
func (m launchModel) toggleGroup(groupIdx int) {
	// Check if all jobs in this group are selected
	allSelected := true
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			if !m.selected[item.jobID] {
				allSelected = false
				break
			}
		}
	}
	// Toggle: if all selected, deselect all; otherwise select all
	newState := !allSelected
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			m.selected[item.jobID] = newState
		}
	}
}

// groupCheckState returns "[x]" if all jobs selected, "[-]" if some, "[ ]" if none.
func (m launchModel) groupCheckState(groupIdx int) string {
	total, selected := 0, 0
	for _, item := range m.items {
		if !item.isHeader && item.groupIdx == groupIdx {
			total++
			if m.selected[item.jobID] {
				selected++
			}
		}
	}
	switch {
	case selected == total:
		return "[x]"
	case selected > 0:
		return "[-]"
	default:
		return "[ ]"
	}
}

// selectedCountByGroup returns the number of selected jobs per group.
// renderCostTable writes a CostTable to the builder, dimming lines as needed.
func renderCostTable(b *strings.Builder, table campaign.CostTable) {
	for _, cl := range table.Lines {
		if cl.Dimmed {
			b.WriteString(launchDimStyle.Render(cl.Text))
		} else {
			b.WriteString(cl.Text)
		}
		b.WriteString("\n")
	}
}

func (m launchModel) selectedCountByGroup() []int {
	counts := make([]int, len(m.groups))
	for _, item := range m.items {
		if !item.isHeader && m.selected[item.jobID] {
			counts[item.groupIdx]++
		}
	}
	return counts
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

	// Build offers slice, filtering out groups without offers
	var launchGroups []campaign.InstanceGroup
	var offers []cloud.Offer
	for i, fg := range filteredGroups {
		if i < len(filteredOffers) && filteredOffers[i].Offer != nil {
			launchGroups = append(launchGroups, fg)
			offers = append(offers, *filteredOffers[i].Offer)
		}
	}

	database := m.database
	clients := m.clients
	cfg := m.appConfig
	opts := m.launchOpts

	// Auto-derive budget limits from cached estimates if not set by CLI
	if (opts.MaxSpendCents == 0 || opts.MaxTimeSeconds == 0) && m.costEstimates != nil {
		// Filter cached estimates to selected groups
		var selectedEstimates []campaign.CostEstimate
		for i, est := range m.costEstimates {
			for _, fg := range filteredGroups {
				if i < len(m.groups) && m.groups[i].GPUClass == fg.GPUClass && m.groups[i].GPUMemGB == fg.GPUMemGB {
					// Scale estimate proportionally to selected job count
					scale := float64(len(fg.Jobs)) / float64(len(m.groups[i].Jobs))
					scaled := est
					scaled.TotalTime = time.Duration(float64(est.TotalTime) * scale)
					scaled.TotalCost = est.TotalCost * scale
					selectedEstimates = append(selectedEstimates, scaled)
					break
				}
			}
		}
		opts.ApplyAutoBudget(selectedEstimates)
	}

	return func() tea.Msg {
		r2Cfg := cfg.Vastai.R2.ToCloudR2Config()
		createOpts := cloud.DefaultCreateOpts(cfg.Vastai.DefaultImage)

		result, err := campaign.LaunchCampaign(
			clients, database, launchGroups, offers, opts, r2Cfg, createOpts, nil,
		)
		if err != nil {
			return instancesLaunchedMsg{err: err}
		}

		if len(result.InstanceIDs) == 0 && len(result.Errors) > 0 {
			return instancesLaunchedMsg{err: result.Errors[0]}
		}

		return instancesLaunchedMsg{instanceIDs: result.InstanceIDs}
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

	b.WriteString(launchTitleStyle.Render(fmt.Sprintf("Cloud GPU jobs (%d jobs, %d GPU groups)", totalJobs, len(m.groups))))
	b.WriteString("\n\n")

	selected := m.selectedCountByGroup()

	if m.showCostDetail && m.costEstimates != nil {
		// Detail view replaces job list
		b.WriteString(campaign.FormatCostBreakdown(m.costEstimates, selected))
	} else {
		// Job list with checkboxes
		for idx, item := range m.items {
			isCursor := idx == m.cursor

			if item.isHeader {
				checkbox := m.groupCheckState(item.groupIdx)
				line := fmt.Sprintf("%s %s", checkbox, item.label)
				if isCursor {
					b.WriteString(launchCursorStyle.Render("> " + line))
				} else {
					b.WriteString(launchHeaderStyle.Render("  " + line))
				}
				b.WriteString("\n")
				continue
			}

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
		if m.costEstimates != nil {
			b.WriteString("\n")
			b.WriteString(launchDimStyle.Render("── Cost Estimate ─────────────────────────────────────"))
			b.WriteString("\n")
			costTable := campaign.FormatCostTableSelected(m.costEstimates, selected)
			renderCostTable(&b, costTable)
		} else if m.groupOffers != nil {
			b.WriteString("\n")
			b.WriteString(launchDimStyle.Render("── Cost Estimate (rough) ─────────────────────────────"))
			b.WriteString("\n")
			costTable := campaign.FormatCostTable(m.groupOffers, selected)
			renderCostTable(&b, costTable)
			b.WriteString(m.spinner.View())
			if m.estimateProgress.phase != "" {
				b.WriteString(launchDimStyle.Render(fmt.Sprintf(" %s (%d/%d)...", m.estimateProgress.phase, m.estimateProgress.resolved, m.estimateProgress.total)))
			} else {
				b.WriteString(launchDimStyle.Render(" Computing estimates..."))
			}
			b.WriteString("\n")
		}
	}

	// Help line
	b.WriteString("\n")
	selectedCount := 0
	for _, v := range m.selected {
		if v {
			selectedCount++
		}
	}
	var help string
	if m.showCostDetail {
		help = "d back  q quit"
	} else if selectedCount == 0 {
		help = "↑/↓ navigate  space toggle  a all  n none  d details  enter quit  q quit"
	} else {
		help = "↑/↓ navigate  space toggle  a all  n none  d details  enter launch  q quit"
	}
	b.WriteString(launchDimStyle.Render(help))
	b.WriteString("\n")

	return b.String()
}
