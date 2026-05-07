package terminal

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/ids"
)

type LaunchProgressTUI struct {
	program *tea.Program
	done    chan struct{}

	closed atomic.Bool
	mu     sync.Mutex
	err    error
}

type launchProgressEventMsg struct {
	event campaign.LaunchEvent
}

type launchProgressCampaignMsg struct {
	campaignID      int64
	expectedWorkers int
}

type launchProgressCompleteMsg struct{}
type launchProgressTickMsg time.Time

type launchProgressRow struct {
	jobLabel   string
	constraint string

	phase          string
	phaseStartedAt time.Time

	assetsReady int
	assetsTotal int

	retryAttempt int
	retryMax     int

	failed bool
	reason string

	done       bool
	instanceID int64
	doneAt     time.Time
}

type launchProgressModel struct {
	startedAt time.Time

	campaignID      int64
	expectedWorkers int
	headerMsg       string

	rowOrder []string
	rows     map[string]*launchProgressRow

	width  int
	height int

	spin spinner.Model
}

func StartLaunchProgressTUI(campaignID int64, expectedWorkers int) *LaunchProgressTUI {
	s := spinner.New()
	s.Spinner = spinner.Dot
	model := launchProgressModel{
		startedAt:       time.Now(),
		campaignID:      campaignID,
		expectedWorkers: expectedWorkers,
		rows:            make(map[string]*launchProgressRow),
		spin:            s,
	}
	program := tea.NewProgram(model)
	tui := &LaunchProgressTUI{
		program: program,
		done:    make(chan struct{}),
	}
	go func() {
		err := program.Start()
		tui.mu.Lock()
		tui.err = err
		tui.mu.Unlock()
		tui.closed.Store(true)
		close(tui.done)
	}()
	return tui
}

func (t *LaunchProgressTUI) SetCampaign(campaignID int64, expectedWorkers int) {
	if t == nil || t.closed.Load() {
		return
	}
	t.program.Send(launchProgressCampaignMsg{campaignID: campaignID, expectedWorkers: expectedWorkers})
}

func (t *LaunchProgressTUI) SendEvent(event campaign.LaunchEvent) {
	if t == nil || t.closed.Load() {
		return
	}
	t.program.Send(launchProgressEventMsg{event: event})
}

func (t *LaunchProgressTUI) Stop() error {
	if t == nil {
		return nil
	}
	if !t.closed.Load() {
		t.program.Send(launchProgressCompleteMsg{})
	}
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (m launchProgressModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, launchProgressTickCmd())
}

func launchProgressTickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return launchProgressTickMsg(t)
	})
}

func (m launchProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend
		case "d", "l":
			return m, nil
		default:
			return m, nil
		}
	case launchProgressTickMsg:
		return m, launchProgressTickCmd()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case launchProgressCampaignMsg:
		if msg.campaignID > 0 {
			m.campaignID = msg.campaignID
		}
		if msg.expectedWorkers > 0 {
			m.expectedWorkers = msg.expectedWorkers
		}
		return m, nil
	case launchProgressEventMsg:
		m.applyEvent(msg.event)
		return m, nil
	case launchProgressCompleteMsg:
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m *launchProgressModel) applyEvent(event campaign.LaunchEvent) {
	now := time.Now()
	switch event.Kind {
	case campaign.LaunchEventCampaignStatus:
		m.headerMsg = strings.TrimSpace(event.Phase)
	case campaign.LaunchEventGroupPhase, campaign.LaunchEventGroupAssets, campaign.LaunchEventGroupRetry, campaign.LaunchEventGroupDone, campaign.LaunchEventGroupFailed:
		row := m.ensureRow(event.Group)
		switch event.Kind {
		case campaign.LaunchEventGroupPhase:
			row.setPhase(event.Phase, now)
		case campaign.LaunchEventGroupAssets:
			row.setPhase("staging", now)
			row.assetsReady = event.AssetsReady
			row.assetsTotal = event.AssetsTotal
		case campaign.LaunchEventGroupRetry:
			row.setPhase("creating instance", now)
			row.retryAttempt = event.RetryAttempt
			row.retryMax = event.RetryMax
		case campaign.LaunchEventGroupDone:
			row.done = true
			row.failed = false
			row.instanceID = event.InstanceID
			row.doneAt = now
		case campaign.LaunchEventGroupFailed:
			row.failed = true
			row.done = false
			row.reason = strings.TrimSpace(event.Reason)
			row.setPhase("failed", now)
		}
	}
}

func (m *launchProgressModel) ensureRow(group campaign.InstanceGroup) *launchProgressRow {
	key := campaign.LaunchGroupSignature(group)
	if row, ok := m.rows[key]; ok {
		return row
	}
	row := &launchProgressRow{
		jobLabel:       launchRowJobLabel(group),
		constraint:     strings.TrimSpace(group.GPUSpec()),
		phase:          "staging",
		phaseStartedAt: time.Now(),
	}
	m.rows[key] = row
	m.rowOrder = append(m.rowOrder, key)
	return row
}

func (r *launchProgressRow) setPhase(phase string, now time.Time) {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return
	}
	if r.phase != phase || r.phaseStartedAt.IsZero() {
		r.phase = phase
		r.phaseStartedAt = now
	}
}

func (m launchProgressModel) View() string {
	var b strings.Builder
	now := time.Now()
	total := m.expectedWorkers
	if total == 0 {
		total = len(m.rowOrder)
	}
	b.WriteString(fmt.Sprintf("Launching %s · %s elapsed\n", pluralize(total, "instance", "instances"), formatMMSS(now.Sub(m.startedAt))))
	headerMsg := launchProgressHeaderMessage(m.headerMsg)
	if headerMsg != "" {
		b.WriteString("  " + headerMsg + "\n")
	}
	b.WriteString("\n")

	maxRows := len(m.rowOrder)
	footerLines := 2
	headerLines := 2
	if headerMsg != "" {
		headerLines = 3
	}
	if m.height > 0 {
		available := m.height - headerLines - footerLines
		if available < 0 {
			available = 0
		}
		if maxRows > available {
			maxRows = available
		}
	}
	overflow := len(m.rowOrder) - maxRows
	displayRows := maxRows
	if overflow > 0 && displayRows > 0 {
		displayRows--
	}
	for i := 0; i < displayRows; i++ {
		row := m.rows[m.rowOrder[i]]
		b.WriteString(m.renderRow(row, now))
		b.WriteString("\n")
	}
	if overflow > 0 {
		b.WriteString(fmt.Sprintf("  ... and %d more\n", overflow))
	}
	b.WriteString("\n")

	ready, failed := 0, 0
	for _, key := range m.rowOrder {
		row := m.rows[key]
		if row.done {
			ready++
		}
		if row.failed {
			failed++
		}
	}
	denom := total
	if denom == 0 {
		denom = len(m.rowOrder)
	}
	b.WriteString(fmt.Sprintf("  ready %d/%d · failed %d\n", ready, denom, failed))
	b.WriteString("  [q] quit  [d] details  [l] logs")
	return b.String()
}

func launchProgressHeaderMessage(phase string) string {
	phase = strings.TrimSpace(phase)
	if strings.EqualFold(phase, "launching worker instances") {
		return ""
	}
	return phase
}

func (m launchProgressModel) renderRow(row *launchProgressRow, now time.Time) string {
	job := fitText(row.jobLabel, 8)
	constraint := fitText(row.constraint, 18)

	phase := launchProgressRowPhase(row, now)
	spin := m.spin.View()
	phaseText := fmt.Sprintf("%s %s", spin, phase)
	if row.done {
		instanceText := "instance"
		if row.instanceID > 0 {
			instanceText = ids.FormatInstanceID(row.instanceID)
		}
		phaseText = fmt.Sprintf("✓ %s ready (%s)", instanceText, formatMMSS(row.doneAt.Sub(m.startedAt)))
	}
	if row.failed {
		phaseText = "✗ failed"
	}
	phaseCol := fitText(phaseText, 28)
	phaseCol = phaseStyleForRow(row).Render(phaseCol)

	progressCol := ""
	if !row.done && !row.failed && row.assetsTotal > 0 {
		progressCol = fmt.Sprintf("%s  assets %d/%d", renderBar(row.assetsReady, row.assetsTotal, 10), row.assetsReady, row.assetsTotal)
	}
	progressCol = fitText(progressCol, 24)

	badges := ""
	if !row.done && row.retryMax > 0 {
		badges += lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(fmt.Sprintf("retry %d/%d", row.retryAttempt, row.retryMax))
	}
	if row.failed && row.reason != "" {
		if badges != "" {
			badges += "  "
		}
		reason := fitText(row.reason, 28)
		badges += lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(reason)
	}

	return fmt.Sprintf("  %-8s  %-18s  %-28s  %-24s  %s", job, constraint, phaseCol, progressCol, badges)
}

func launchProgressRowPhase(row *launchProgressRow, now time.Time) string {
	phase := strings.TrimSpace(row.phase)
	if phase == "" {
		return ""
	}
	if row.done || row.failed || row.phaseStartedAt.IsZero() || now.Before(row.phaseStartedAt) {
		return phase
	}
	return fmt.Sprintf("%s %s", phase, formatMMSS(now.Sub(row.phaseStartedAt)))
}

func launchRowJobLabel(group campaign.InstanceGroup) string {
	minID := int64(0)
	count := 0
	for _, job := range group.Jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		count++
		if minID == 0 || job.ID < minID {
			minID = job.ID
		}
	}
	if minID == 0 {
		return "group"
	}
	if count > 1 {
		return ids.FormatJobID(minID) + "+"
	}
	return ids.FormatJobID(minID)
}

func fitText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= width {
		return string(runes)
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func renderBar(done, total, width int) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := int(float64(done) / float64(total) * float64(width))
	if done > 0 && filled == 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func formatMMSS(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", seconds/60, seconds%60)
}

func phaseStyleForRow(row *launchProgressRow) lipgloss.Style {
	if row.done {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	}
	if row.failed {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	}
	phase := strings.ToLower(strings.TrimSpace(row.phase))
	switch {
	case strings.HasPrefix(phase, "staging"):
		return lipgloss.NewStyle().Faint(true)
	case strings.HasPrefix(phase, "creating"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	case strings.HasPrefix(phase, "uploading"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	case strings.HasPrefix(phase, "running"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	default:
		return lipgloss.NewStyle()
	}
}

func UseLaunchProgressTUI(noTUI bool) bool {
	if noTUI {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	if isTruthy(os.Getenv("CI")) {
		return false
	}
	return term.IsTerminal(os.Stdout.Fd())
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
