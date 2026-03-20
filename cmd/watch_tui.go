package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

type watchAllModel struct {
	database        *sql.DB
	config          *config.Config
	cloudInstances  []*db.CloudInstance
	instanceUpdates map[int64]campaign.InstanceUpdate
	watchChannels   map[int64]<-chan campaign.InstanceUpdate
	clients         map[int64]cloud.Client
	r2Client        *r2.Client
	onPremHosts     []onPremHostSummary
	unplacedJobs    []*db.Job
	cursor          int
	width           int
	height          int
	spinner         spinner.Model
	refreshing      bool
	flashMessage    string
	err             error
	ctx             context.Context
	cancel          context.CancelFunc
	jobProgressHWM  map[int64]int
	syncWorker      *tui.SyncWorker
}

type watchAllTickMsg struct{}

type watchAllRefreshedMsg struct {
	snapshot watchSystemSnapshot
	err      error
}

type watchSyncResultMsg struct {
	result tui.SyncResult
}

type watchOnPremRefreshedMsg struct {
	onPremHosts  []onPremHostSummary
	unplacedJobs []*db.Job
	err          error
}

type watchUnplaceDoneMsg struct {
	job     *db.Job
	message string
	err     error
}

type watchRenderRow struct {
	text       string
	selectable bool
}

var watchSelectedRowStyle = lipgloss.NewStyle().Background(lipgloss.Color("240"))

func newWatchAllModel(database *sql.DB, cfg *config.Config, flashMessage string) watchAllModel {
	s := spinner.New()
	s.Spinner = spinner.Dot

	ctx, cancel := context.WithCancel(context.Background())
	r2Client, _ := buildR2Client(cfg)

	sw := tui.NewSyncWorker(database, nil, r2Client, cfg)
	sw.Start()

	model := watchAllModel{
		database:        database,
		config:          cfg,
		instanceUpdates: map[int64]campaign.InstanceUpdate{},
		watchChannels:   map[int64]<-chan campaign.InstanceUpdate{},
		clients:         map[int64]cloud.Client{},
		r2Client:        r2Client,
		spinner:         s,
		flashMessage:    flashMessage,
		ctx:             ctx,
		cancel:          cancel,
		jobProgressHWM:  map[int64]int{},
		syncWorker:      sw,
	}

	snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
	if err != nil {
		model.err = err
		return model
	}
	model.cloudInstances = snapshot.CloudInstances
	model.instanceUpdates = snapshot.InstanceUpdates
	model.onPremHosts = snapshot.OnPremHosts
	model.unplacedJobs = snapshot.UnplacedJobs

	// Request initial syncs for on-prem hosts (starts queue runners, syncs status)
	for _, host := range model.onPremHosts {
		sw.Request(tui.SyncRequest{
			Host: host.Name,
			Rate: tui.GetHostSyncRate(host.Jobs),
		})
	}

	return model
}

func (m watchAllModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.spinner.Tick,
		scheduleWatchAllTick(),
		m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
			return watchSyncResultMsg{result: r}
		}),
	}
	for _, ci := range m.cloudInstances {
		if cmd := m.watchInstance(ci.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

func (m watchAllModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.cancel()
			return m, tea.Quit
		case "l":
			m.cancel()
			if m.syncWorker != nil {
				m.syncWorker.Stop()
			}
			return m, func() tea.Msg { return switchToLaunchMsg{} }
		case "r":
			if m.refreshing {
				return m, nil
			}
			m.refreshing = true
			return m, refreshWatchSystem(m.database, m.config)
		case "u":
			job := m.selectedOnPremJob()
			if job == nil || job.EffectiveStatus() != db.StatusQueued || !job.HasInventoryHost() {
				m.flashMessage = watchFailedStyle.Render("Select a queued on-prem job to unplace")
				return m, nil
			}
			return m, requestWatchJobUnplace(m.database, job.ID)
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			count := m.selectableRowCount()
			if count > 0 {
				m.cursor = count - 1
			}
		}
		return m, nil

	case watchUpdateMsg:
		if msg.closed {
			clearWatchJobProgressHWM(m.jobProgressHWM, m.instanceUpdates[msg.instanceID])
			delete(m.watchChannels, msg.instanceID)
			delete(m.clients, msg.instanceID)
			return m, nil
		}
		if m.jobProgressHWM == nil {
			m.jobProgressHWM = map[int64]int{}
		}
		prev := m.instanceUpdates[msg.instanceID]
		updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
		m.instanceUpdates[msg.instanceID] = msg.update
		return m, waitForUpdate(msg.instanceID, m.watchChannels[msg.instanceID])

	case watchAllTickMsg:
		if m.refreshing {
			return m, scheduleWatchAllTick()
		}
		m.refreshing = true
		return m, tea.Batch(
			refreshWatchSystem(m.database, m.config),
			scheduleWatchAllTick(),
		)

	case watchAllRefreshedMsg:
		m.refreshing = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.cloudInstances = msg.snapshot.CloudInstances
		m.onPremHosts = msg.snapshot.OnPremHosts
		m.unplacedJobs = msg.snapshot.UnplacedJobs
		cmds := m.mergeSnapshot(msg.snapshot)
		// Request on-prem syncs for newly refreshed hosts (starts queue runners)
		for _, host := range m.onPremHosts {
			m.syncWorker.Request(tui.SyncRequest{
				Host: host.Name,
				Rate: tui.GetHostSyncRate(host.Jobs),
			})
		}
		m.clampCursor()
		return m, tea.Batch(cmds...)

	case watchSyncResultMsg:
		// On-prem sync completed — reload on-prem data from DB, re-arm for next result
		return m, tea.Batch(
			refreshWatchOnPrem(m.database),
			m.syncWorker.WaitForResult(m.ctx, func(r tui.SyncResult) tea.Msg {
				return watchSyncResultMsg{result: r}
			}),
		)

	case watchOnPremRefreshedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.onPremHosts = msg.onPremHosts
		m.unplacedJobs = msg.unplacedJobs
		m.clampCursor()
		return m, nil

	case watchUnplaceDoneMsg:
		if msg.err != nil {
			m.flashMessage = watchFailedStyle.Render(msg.err.Error())
			return m, nil
		}
		if msg.job != nil {
			m.removeOnPremJob(msg.job.ID)
			m.upsertUnplacedJob(msg.job)
			m.clampCursor()
		}
		m.flashMessage = msg.message
		m.refreshing = true
		return m, refreshWatchSystem(m.database, m.config)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *watchAllModel) mergeSnapshot(snapshot watchSystemSnapshot) []tea.Cmd {
	active := make(map[int64]bool, len(snapshot.CloudInstances))
	var cmds []tea.Cmd

	for _, ci := range snapshot.CloudInstances {
		active[ci.ID] = true
		snapUpdate := snapshot.InstanceUpdates[ci.ID]
		if existing, ok := m.instanceUpdates[ci.ID]; ok {
			existing.CloudInstance = snapUpdate.CloudInstance
			existing.Jobs = snapUpdate.Jobs
			existing.JobAttemptOutcomes = snapUpdate.JobAttemptOutcomes
			m.instanceUpdates[ci.ID] = existing
		} else {
			m.instanceUpdates[ci.ID] = snapUpdate
		}
		if cmd := m.watchInstance(ci.ID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	for id := range m.instanceUpdates {
		if !active[id] {
			delete(m.instanceUpdates, id)
		}
	}

	return cmds
}

func (m *watchAllModel) watchInstance(instanceID int64) tea.Cmd {
	if _, ok := m.watchChannels[instanceID]; ok {
		return nil
	}
	client := clientForInstance(m.database, instanceID)
	ch := campaign.WatchInstance(m.ctx, client, m.database, instanceID, 2*time.Second, 10*time.Second, m.r2Client)
	m.clients[instanceID] = client
	m.watchChannels[instanceID] = ch
	return waitForUpdate(instanceID, ch)
}

func (m *watchAllModel) moveCursor(delta int) {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m *watchAllModel) clampCursor() {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m watchAllModel) selectedOnPremJob() *db.Job {
	index := m.cursor - len(m.cloudInstances)
	if index < 0 {
		return nil
	}
	for _, host := range m.onPremHosts {
		if index < len(host.Jobs) {
			return host.Jobs[index]
		}
		index -= len(host.Jobs)
	}
	return nil
}

func (m watchAllModel) selectedUnplacedJob() *db.Job {
	index := m.cursor - len(m.cloudInstances)
	if index < 0 {
		return nil
	}
	for _, host := range m.onPremHosts {
		if index < len(host.Jobs) {
			return nil
		}
		index -= len(host.Jobs)
	}
	if index < 0 || index >= len(m.unplacedJobs) {
		return nil
	}
	return m.unplacedJobs[index]
}

func (m *watchAllModel) removeOnPremJob(jobID int64) {
	filteredHosts := m.onPremHosts[:0]
	for _, host := range m.onPremHosts {
		jobs := host.Jobs[:0]
		for _, job := range host.Jobs {
			if job != nil && job.ID == jobID {
				continue
			}
			jobs = append(jobs, job)
		}
		if len(jobs) == 0 {
			continue
		}
		host.Jobs = jobs
		filteredHosts = append(filteredHosts, host)
	}
	m.onPremHosts = filteredHosts
}

func (m *watchAllModel) upsertUnplacedJob(job *db.Job) {
	if job == nil {
		return
	}
	for i, existing := range m.unplacedJobs {
		if existing != nil && existing.ID == job.ID {
			m.unplacedJobs[i] = job
			return
		}
	}
	m.unplacedJobs = append(m.unplacedJobs, job)
	sort.SliceStable(m.unplacedJobs, func(i, j int) bool {
		return m.unplacedJobs[i].ID < m.unplacedJobs[j].ID
	})
}

func (m watchAllModel) selectableRowCount() int {
	count := len(m.cloudInstances) + len(m.unplacedJobs)
	for _, host := range m.onPremHosts {
		count += len(host.Jobs)
	}
	return count
}

func (m watchAllModel) View() string {
	rows, selectedVisualIndex := m.renderRows()
	if len(rows) == 0 {
		return ""
	}

	footerParts := make([]string, 0, 3)
	footerPrefixWidth := 0
	if m.err != nil {
		errText := fmt.Sprintf("Error: %v", m.err)
		footerParts = append(footerParts, watchFailedStyle.Render(errText))
		footerPrefixWidth = lipgloss.Width(errText)
	} else if m.flashMessage != "" {
		footerParts = append(footerParts, m.flashMessage)
		footerPrefixWidth = lipgloss.Width(m.flashMessage)
	}
	if detail := m.selectedStatusDetail(); detail != "" {
		detail = m.truncateFooterDetail(detail, footerPrefixWidth)
		if detail != "" {
			footerParts = append(footerParts, watchDimStyle.Render(detail))
		}
	}
	footerParts = append(footerParts, watchDimStyle.Render("[u] unplace queued job  [l] launch  [r] refresh  [q] quit"))
	footer := strings.Join(footerParts, "  ")

	contentHeight := m.height - 1
	if contentHeight < 1 {
		contentHeight = len(rows)
	}
	start := watchScrollStart(len(rows), selectedVisualIndex, contentHeight)
	end := start + contentHeight
	if end > len(rows) {
		end = len(rows)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(rows[i].text)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	if end-start < contentHeight {
		padLine := strings.Repeat(" ", max(0, m.width))
		for i := end - start; i < contentHeight; i++ {
			b.WriteString("\n")
			b.WriteString(padLine)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(footer)
	return b.String()
}

func (m watchAllModel) renderRows() ([]watchRenderRow, int) {
	width := m.width
	if width <= 0 {
		width = 100
	}

	rows := make([]watchRenderRow, 0, 8+len(m.cloudInstances)+len(m.unplacedJobs))
	selectedVisualIndex := -1
	selectableIndex := 0

	addHeader := func(text string) {
		rows = append(rows, watchRenderRow{text: watchTitleStyle.Render(text)})
	}
	addSelectable := func(text string) {
		if selectableIndex == m.cursor {
			text = watchSelectedRowStyle.Render(padToWidth(text, width))
			selectedVisualIndex = len(rows)
		}
		rows = append(rows, watchRenderRow{text: text, selectable: true})
		selectableIndex++
	}
	addPlain := func(text string) {
		rows = append(rows, watchRenderRow{text: text})
	}

	title := fmt.Sprintf("System Watch")
	if m.refreshing {
		title += "  " + m.spinner.View() + " " + watchDimStyle.Render("refreshing")
	}
	addHeader(title)
	addPlain("")

	addHeader(fmt.Sprintf("Rental Instances (%d)", len(m.cloudInstances)))
	if len(m.cloudInstances) == 0 {
		addPlain(watchDimStyle.Render("  no active rental instances"))
	} else {
		views := make([]cloudInstanceView, 0, len(m.cloudInstances))
		for _, ci := range m.cloudInstances {
			update := normalizeWatchInstanceUpdate(m.instanceUpdates[ci.ID], ci)
			views = append(views, cloudInstanceView{
				CloudInstance: update.CloudInstance,
				Instance:      update.Instance,
			})
		}
		if summary := formatCloudAggregateSummary("  Summary:", summarizeCloudInstances(views, time.Now())); summary != "" {
			addPlain(summary)
			addPlain("")
		}
		for i, ci := range m.cloudInstances {
			if i > 0 {
				addPlain("")
			}
			update := normalizeWatchInstanceUpdate(m.instanceUpdates[ci.ID], ci)
			lines := formatWatchInstanceBlockLines(update, m.jobProgressHWM, watchInstanceBlockOptions{})
			if len(lines) == 0 {
				continue
			}
			addSelectable(truncate(lines[0], width))
			for _, line := range lines[1:] {
				addPlain(truncate(line, width))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Inventory Hosts (%d active)", len(m.onPremHosts)))
	if len(m.onPremHosts) == 0 {
		addPlain(watchDimStyle.Render("  no active inventory jobs"))
	} else {
		for _, host := range m.onPremHosts {
			addPlain(watchStatusStyle.Render("  " + host.Name))
			for _, job := range host.Jobs {
				addSelectable("    " + truncate(m.formatOnPremJobRow(job), width-4))
			}
		}
	}
	addPlain("")

	addHeader(fmt.Sprintf("Unplaced Jobs (%d)", len(m.unplacedJobs)))
	if len(m.unplacedJobs) == 0 {
		addPlain(watchDimStyle.Render("  no unplaced jobs"))
	} else {
		for _, job := range m.unplacedJobs {
			addSelectable("  " + truncate(m.formatUnplacedJobRow(job), width-2))
		}
	}

	return rows, selectedVisualIndex
}

func (m watchAllModel) formatOnPremJobRow(job *db.Job) string {
	status := job.EffectiveStatus()
	duration := "—"
	if job.StartTime > 0 {
		duration = db.FormatDuration(time.Now().Unix() - job.StartTime)
	}
	gpu := ""
	if gpuDev := job.GPUDevice(); gpuDev != "" {
		gpu = "  GPU " + gpuDev
	}
	return fmt.Sprintf("#%-4d %-12s %-28s %-9s %-8s%s",
		job.ID,
		campaign.JobProjectLabel(job),
		truncate(job.EffectiveDescription(), 28),
		status,
		duration,
		gpu,
	)
}

func (m watchAllModel) formatUnplacedJobRow(job *db.Job) string {
	return fmt.Sprintf("#%-4d %-12s %-28s %s",
		job.ID,
		campaign.JobProjectLabel(job),
		truncate(job.EffectiveDescription(), 28),
		formatWatchGPUConstraint(job),
	)
}

func (m watchAllModel) selectedStatusDetail() string {
	job := m.selectedUnplacedJob()
	if job == nil || len(job.PlacementReasons) == 0 {
		return ""
	}
	return fmt.Sprintf("#%d unplaced: %s", job.ID, strings.Join(job.PlacementReasons, " | "))
}

func (m watchAllModel) truncateFooterDetail(detail string, prefixWidth int) string {
	if m.width <= 0 {
		return detail
	}
	controlsWidth := lipgloss.Width("[u] unplace queued job  [l] launch  [r] refresh  [q] quit")
	available := m.width - controlsWidth
	if prefixWidth > 0 {
		available -= prefixWidth + lipgloss.Width("  ")
	}
	available -= lipgloss.Width("  ")
	if available <= 0 {
		return ""
	}
	if available < 16 {
		return truncate(detail, max(available, 3))
	}
	return truncate(detail, available)
}

func refreshWatchSystem(database *sql.DB, cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
		return watchAllRefreshedMsg{snapshot: snapshot, err: err}
	}
}

func requestWatchJobUnplace(database *sql.DB, jobID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("job %d not found", jobID)}
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return watchUnplaceDoneMsg{err: err}
		}
		updatedJob, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("reload job %d: %w", jobID, err)}
		}
		return watchUnplaceDoneMsg{job: updatedJob, message: result.Message}
	}
}

func refreshWatchOnPrem(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		onPremJobs, err := db.ListActiveOnPremJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			onPremHosts:  groupOnPremHosts(onPremJobs),
			unplacedJobs: unplacedJobs,
		}
	}
}

func scheduleWatchAllTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchAllTickMsg{}
	})
}

func watchScrollStart(totalRows, selectedIndex, viewportHeight int) int {
	if viewportHeight <= 0 || totalRows <= viewportHeight || selectedIndex < 0 {
		return 0
	}
	start := selectedIndex - viewportHeight/2
	if start < 0 {
		start = 0
	}
	maxStart := totalRows - viewportHeight
	if start > maxStart {
		start = maxStart
	}
	return start
}

func padToWidth(s string, width int) string {
	current := lipgloss.Width(s)
	if current >= width {
		return truncate(s, width)
	}
	return s + strings.Repeat(" ", width-current)
}
