package tui

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Intervals for background operations
const (
	SyncInterval       = 15 * time.Second
	LogRefreshInterval = 3 * time.Second
)

// Key bindings
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Escape  key.Binding
	Kill    key.Binding
	Restart key.Binding
	Remove  key.Binding
	NewJob  key.Binding
	Prune   key.Binding
	Suspend key.Binding
	Quit    key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	Escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "clear"),
	),
	Kill: key.NewBinding(
		key.WithKeys("k", "delete"),
		key.WithHelp("k", "kill"),
	),
	Restart: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart"),
	),
	Remove: key.NewBinding(
		key.WithKeys("x"),
		key.WithHelp("x", "remove"),
	),
	NewJob: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "new job"),
	),
	Prune: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "prune"),
	),
	Suspend: key.NewBinding(
		key.WithKeys("ctrl+z"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
}

// Messages
type jobsRefreshedMsg struct {
	jobs []*db.Job
	err  error
}

type syncCompletedMsg struct {
	updated int
	err     error
}

type logFetchedMsg struct {
	jobID   int64
	content string
	err     error
}

type jobKilledMsg struct {
	jobID int64
	err   error
}

type jobRestartedMsg struct {
	oldJobID int64
	newJobID int64
	err      error
}

type pruneCompletedMsg struct {
	count int64
	err   error
}

type jobRemovedMsg struct {
	jobID int64
	err   error
}

type jobCreatedMsg struct {
	jobID int64
	err   error
}

type jobCreateProgressMsg struct {
	step string
}

type tickMsg time.Time
type logTickMsg time.Time
type createTickMsg time.Time

// Input field indices for new job form
const (
	inputHost = iota
	inputCommand
	inputDescription
	inputWorkingDir
)

// Model is the main TUI state
type Model struct {
	// Data
	jobs          []*db.Job
	selectedIndex int
	selectedJob   *db.Job

	// UI State
	logContent    string
	logLoading    bool
	statusMessage string
	errorMessage  string

	// Operation state
	restarting         bool
	restartingJobName  string
	pendingSelectJobID int64

	// New job input mode
	inputMode      bool
	inputFocus     int
	inputs         []textinput.Model
	creatingJob    bool
	createJobStart time.Time
	createJobStep  string

	// Layout
	width  int
	height int

	// Database connection
	database *sql.DB

	// Background sync state
	syncing      bool
	lastSyncTime time.Time
}

// NewModel creates a new TUI model
func NewModel(database *sql.DB) Model {
	// Create text inputs for new job form
	inputs := make([]textinput.Model, 4)

	inputs[inputHost] = textinput.New()
	inputs[inputHost].Placeholder = "e.g., cool30"
	inputs[inputHost].Prompt = ""
	inputs[inputHost].Width = 40
	inputs[inputHost].CharLimit = 64

	inputs[inputCommand] = textinput.New()
	inputs[inputCommand].Placeholder = "e.g., python train.py"
	inputs[inputCommand].Prompt = ""
	inputs[inputCommand].Width = 40
	inputs[inputCommand].CharLimit = 512

	inputs[inputDescription] = textinput.New()
	inputs[inputDescription].Placeholder = "(optional)"
	inputs[inputDescription].Prompt = ""
	inputs[inputDescription].Width = 40
	inputs[inputDescription].CharLimit = 256

	inputs[inputWorkingDir] = textinput.New()
	inputs[inputWorkingDir].Placeholder = "(optional, defaults to ~)"
	inputs[inputWorkingDir].Prompt = ""
	inputs[inputWorkingDir].Width = 40
	inputs[inputWorkingDir].CharLimit = 256

	return Model{
		database:      database,
		selectedIndex: 0,
		inputs:        inputs,
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshJobs(),
		m.startSyncTicker(),
		m.startLogTicker(),
	)
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.inputMode {
			return m.handleInputKeyPress(msg)
		}
		return m.handleKeyPress(msg)

	case jobsRefreshedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Error loading jobs: %v", msg.err)
		} else {
			m.jobs = msg.jobs
			m.errorMessage = ""

			// If there's a pending job selection, find and select it
			if m.pendingSelectJobID > 0 {
				for i, job := range m.jobs {
					if job.ID == m.pendingSelectJobID {
						m.selectedIndex = i
						break
					}
				}
				m.pendingSelectJobID = 0
			}

			// Keep selection in bounds
			if m.selectedIndex >= len(m.jobs) {
				m.selectedIndex = len(m.jobs) - 1
			}
			if m.selectedIndex < 0 {
				m.selectedIndex = 0
			}
		}
		return m, nil

	case syncCompletedMsg:
		m.syncing = false
		m.lastSyncTime = time.Now()
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Sync error: %v", msg.err)
		} else if msg.updated > 0 {
			m.statusMessage = fmt.Sprintf("Synced %d job(s)", msg.updated)
			return m, m.refreshJobs()
		}
		return m, nil

	case logFetchedMsg:
		m.logLoading = false
		if msg.err != nil {
			m.logContent = fmt.Sprintf("Error: %v", msg.err)
		} else if m.selectedJob != nil && msg.jobID == m.selectedJob.ID {
			m.logContent = msg.content
		}
		return m, nil

	case jobKilledMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Kill failed: %v", msg.err)
		} else {
			m.statusMessage = "Job killed"
		}
		return m, m.refreshJobs()

	case jobRestartedMsg:
		m.restarting = false
		m.restartingJobName = ""
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Restart failed: %v", msg.err)
			return m, nil
		}
		m.statusMessage = fmt.Sprintf("Job restarted (new ID: %d)", msg.newJobID)
		m.pendingSelectJobID = msg.newJobID
		return m, m.refreshJobs()

	case pruneCompletedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Prune failed: %v", msg.err)
		} else if msg.count > 0 {
			m.statusMessage = fmt.Sprintf("Pruned %d job(s)", msg.count)
		} else {
			m.statusMessage = "No jobs to prune"
		}
		return m, m.refreshJobs()

	case jobRemovedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Remove failed: %v", msg.err)
		} else {
			m.statusMessage = "Job removed"
			m.selectedJob = nil
			m.logContent = ""
		}
		return m, m.refreshJobs()

	case jobCreateProgressMsg:
		m.createJobStep = msg.step
		return m, nil

	case jobCreatedMsg:
		m.creatingJob = false
		m.createJobStep = ""
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Create failed: %v", msg.err)
		} else {
			m.statusMessage = fmt.Sprintf("Job %d started", msg.jobID)
			m.pendingSelectJobID = msg.jobID
			// Keep inputs for easy re-use (user can modify and submit again)
		}
		return m, m.refreshJobs()

	case tickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startSyncTicker())
		if !m.syncing {
			m.syncing = true
			cmds = append(cmds, m.performBackgroundSync())
		}
		return m, tea.Batch(cmds...)

	case logTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, m.startLogTicker())
		// Refresh logs if a running job is selected
		if m.selectedJob != nil && m.selectedJob.Status == db.StatusRunning {
			cmds = append(cmds, m.fetchSelectedJobLog())
		}
		return m, tea.Batch(cmds...)

	case createTickMsg:
		// Only continue ticking if still creating
		if m.creatingJob {
			return m, m.startCreateTicker()
		}
		return m, nil
	}

	return m, nil
}

func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Allow cancelling job creation with Escape
	if m.creatingJob && key.Matches(msg, keys.Escape) {
		m.creatingJob = false
		m.createJobStep = ""
		m.statusMessage = "Job creation running in background..."
		return m, nil
	}

	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Suspend):
		return m, tea.Suspend

	case key.Matches(msg, keys.Up):
		if m.selectedIndex > 0 {
			m.selectedIndex--
			m.selectedJob = nil
			m.logContent = ""
		}
		return m, nil

	case key.Matches(msg, keys.Down):
		if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs)-1 {
			m.selectedIndex++
			m.selectedJob = nil
			m.logContent = ""
		}
		return m, nil

	case key.Matches(msg, keys.Enter):
		if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
			m.selectedJob = m.jobs[m.selectedIndex]
			m.logLoading = true
			return m, m.fetchSelectedJobLog()
		}
		return m, nil

	case key.Matches(msg, keys.Escape):
		m.selectedJob = nil
		m.logContent = ""
		m.statusMessage = ""
		m.errorMessage = ""
		return m, nil

	case key.Matches(msg, keys.Kill):
		job := m.getTargetJob()
		if job != nil && job.Status == db.StatusRunning {
			m.statusMessage = "Killing job..."
			return m, m.killJob(job)
		}
		return m, nil

	case key.Matches(msg, keys.Restart):
		job := m.getTargetJob()
		if job == nil {
			m.errorMessage = "No job selected"
			return m, nil
		}
		if m.restarting {
			m.statusMessage = "Restart already in progress..."
			return m, nil
		}
		m.restarting = true
		m.restartingJobName = fmt.Sprintf("job %d", job.ID)
		m.statusMessage = fmt.Sprintf("Restarting job %d...", job.ID)
		m.errorMessage = ""
		return m, m.restartJob(job)

	case key.Matches(msg, keys.Remove):
		job := m.getTargetJob()
		if job == nil {
			return m, nil
		}
		m.statusMessage = "Removing job..."
		return m, m.removeJob(job)

	case key.Matches(msg, keys.NewJob):
		m.inputMode = true
		m.inputFocus = 0
		m.inputs[inputHost].Focus()
		m.errorMessage = ""
		m.statusMessage = ""

		// Pre-populate from highlighted job if inputs are empty
		job := m.getTargetJob()
		if job != nil && m.inputs[inputHost].Value() == "" {
			m.inputs[inputHost].SetValue(job.Host)
			m.inputs[inputCommand].SetValue(job.Command)
			// Don't pre-populate description - it may contain error messages from failed jobs
			// and descriptions are usually different for each job anyway
			m.inputs[inputWorkingDir].SetValue(job.WorkingDir)
		}
		return m, nil

	case key.Matches(msg, keys.Prune):
		m.statusMessage = "Pruning completed/dead jobs..."
		return m, m.pruneJobs()
	}

	return m, nil
}

func (m Model) handleInputKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Cancel input mode
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		return m, nil

	case tea.KeyTab, tea.KeyShiftTab:
		// Cycle through inputs
		m.inputs[m.inputFocus].Blur()
		if msg.Type == tea.KeyShiftTab {
			m.inputFocus--
			if m.inputFocus < 0 {
				m.inputFocus = len(m.inputs) - 1
			}
		} else {
			m.inputFocus++
			if m.inputFocus >= len(m.inputs) {
				m.inputFocus = 0
			}
		}
		m.inputs[m.inputFocus].Focus()
		return m, nil

	case tea.KeyEnter:
		// Submit if we have required fields
		host := strings.TrimSpace(m.inputs[inputHost].Value())
		command := strings.TrimSpace(m.inputs[inputCommand].Value())

		if host == "" || command == "" {
			m.errorMessage = "Host and command are required"
			return m, nil
		}

		// Exit input mode and create job
		m.inputMode = false
		m.inputs[m.inputFocus].Blur()
		m.creatingJob = true
		m.createJobStart = time.Now()
		m.createJobStep = "Connecting..."
		m.statusMessage = ""
		return m, tea.Batch(m.createJob(), m.startCreateTicker())
	}

	// Forward other keys to the focused input
	var cmd tea.Cmd
	m.inputs[m.inputFocus], cmd = m.inputs[m.inputFocus].Update(msg)
	return m, cmd
}

// View renders the UI
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	// Calculate panel heights
	listHeight := int(float64(m.height) * 0.55)
	logHeight := int(float64(m.height) * 0.35)

	// Build panels
	listView := m.renderJobList(listHeight)
	logView := m.renderLogPanel(logHeight)
	statusView := m.renderStatusBar()

	mainView := lipgloss.JoinVertical(
		lipgloss.Left,
		listView,
		logView,
		statusView,
	)

	// Show modal overlay for long-running operations
	if m.restarting {
		return m.renderWithModal(mainView, fmt.Sprintf("Restarting %s...", m.restartingJobName))
	}

	if m.creatingJob {
		elapsed := time.Since(m.createJobStart).Truncate(time.Second)
		msg := fmt.Sprintf("Creating job... %s\n\n%s\n\nPress Esc to dismiss", elapsed, m.createJobStep)
		return m.renderWithModal(mainView, msg)
	}

	// Show input form
	if m.inputMode {
		return m.renderInputForm(mainView)
	}

	return mainView
}

func (m Model) renderWithModal(background, message string) string {
	// Create modal box
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 3).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("229"))

	modal := modalStyle.Render(message)

	// Place modal centered on screen
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) renderInputForm(background string) string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(60)

	labelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("245"))
	focusedLabelStyle := lipgloss.NewStyle().Width(14).Foreground(lipgloss.Color("69")).Bold(true)

	var b strings.Builder
	b.WriteString("New Job\n\n")

	labels := []string{"Host:", "Command:", "Description:", "Working Dir:"}
	for i, input := range m.inputs {
		label := labelStyle
		if i == m.inputFocus {
			label = focusedLabelStyle
		}
		b.WriteString(label.Render(labels[i]))
		b.WriteString(input.View())
		b.WriteString("\n\n")
	}

	b.WriteString("\n")
	helpText := "Tab: next field • Enter: create job • Esc: cancel"
	if m.errorMessage != "" {
		helpText = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.errorMessage)
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(helpText))

	modal := modalStyle.Render(b.String())

	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("237")),
	)
}

func (m Model) renderJobList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-12s %-12s %s",
		"ID", "HOST", "STATUS", "STARTED", "COMMAND / DESCRIPTION")
	rows = append(rows, headerStyle.Render(header))

	// Jobs
	contentHeight := height - 4 // Account for borders and header
	for i, job := range m.jobs {
		if i >= contentHeight {
			break
		}

		status := m.formatStatus(job)
		started := formatStartTime(job.StartTime)

		// Show description if available, otherwise truncated command
		display := job.Description
		if display == "" {
			display = job.Command
		}
		if len(display) > 40 {
			display = display[:37] + "..."
		}

		line := fmt.Sprintf(" %-4d %-10s %-12s %-12s %s",
			job.ID, truncate(job.Host, 10),
			status, started, display)

		if i == m.selectedIndex {
			line = selectedStyle.Width(m.width - 4).Render(line)
		} else {
			line = m.styleForStatus(job.Status).Render(line)
		}

		rows = append(rows, line)
	}

	content := strings.Join(rows, "\n")
	return listPanelStyle.Width(m.width - 2).Height(height).Render(content)
}

func (m Model) renderLogPanel(height int) string {
	var content string
	var header string

	// Show details for highlighted job (even without pressing Enter)
	highlightedJob := m.getTargetJob()

	if highlightedJob == nil {
		content = dimStyle.Render("No jobs to display")
	} else {
		// Build job details header
		job := highlightedJob
		startTime := time.Unix(job.StartTime, 0)
		header = fmt.Sprintf("Job %d on %s\n", job.ID, job.Host)
		header += fmt.Sprintf("Started: %s (%s)\n", startTime.Format("2006-01-02 15:04:05"), formatStartTime(job.StartTime))
		header += fmt.Sprintf("Dir:     %s\n", job.WorkingDir)
		header += fmt.Sprintf("Cmd:     %s\n", job.Command)
		header += "───────────────────────────────────────────────────────────────\n"

		// Only show logs if job is selected (Enter pressed)
		if m.selectedJob == nil {
			content = dimStyle.Render("Press Enter to view logs")
		} else if m.logLoading {
			content = dimStyle.Render("Loading logs...")
		} else if m.logContent == "" {
			content = dimStyle.Render("No log content available")
		} else {
			// Take last N lines that fit (account for header lines)
			lines := strings.Split(m.logContent, "\n")
			maxLines := height - 10 // Account for borders, header, and padding
			if len(lines) > maxLines {
				lines = lines[len(lines)-maxLines:]
			}
			content = strings.Join(lines, "\n")
		}
	}

	// Title
	title := "Details"
	if highlightedJob == nil {
		title = "Logs"
	}

	panelContent := titleStyle.Render(title) + "\n"
	if header != "" {
		panelContent += header
	}
	panelContent += content

	return logPanelStyle.Width(m.width - 2).Height(height).Render(panelContent)
}

func (m Model) renderStatusBar() string {
	left := ""
	if m.errorMessage != "" {
		left = errorStyle.Render(m.errorMessage)
	} else if m.statusMessage != "" {
		left = statusMsgStyle.Render(m.statusMessage)
	}

	right := helpStyle.Render("↑/↓:nav Enter:logs n:new r:restart k:kill x:remove p:prune q:quit")

	if m.syncing {
		right = syncingStyle.Render("⟳ ") + right
	}

	// Calculate gap
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}

	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) formatStatus(job *db.Job) string {
	switch job.Status {
	case db.StatusRunning:
		return "● running"
	case db.StatusCompleted:
		if job.ExitCode == nil {
			return "✓ done"
		}
		if *job.ExitCode == 0 {
			return "✓ done"
		}
		return fmt.Sprintf("✗ exit %d", *job.ExitCode)
	case db.StatusDead:
		return "✗ dead"
	case db.StatusPending:
		return "○ pending"
	default:
		return job.Status
	}
}

func (m Model) styleForStatus(status string) lipgloss.Style {
	switch status {
	case db.StatusRunning:
		return runningStyle
	case db.StatusCompleted:
		return completedStyle
	case db.StatusDead:
		return deadStyle
	case db.StatusPending:
		return pendingStyle
	default:
		return lipgloss.NewStyle()
	}
}

// Commands

func (m Model) startSyncTicker() tea.Cmd {
	return tea.Tick(SyncInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) startLogTicker() tea.Cmd {
	return tea.Tick(LogRefreshInterval, func(t time.Time) tea.Msg {
		return logTickMsg(t)
	})
}

func (m Model) startCreateTicker() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return createTickMsg(t)
	})
}

func (m Model) refreshJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := db.ListJobs(m.database, "", "", 100)
		return jobsRefreshedMsg{jobs: jobs, err: err}
	}
}

// getTargetJob returns the job to act on - either the selected job or the highlighted job
func (m Model) getTargetJob() *db.Job {
	if m.selectedJob != nil {
		return m.selectedJob
	}
	if len(m.jobs) > 0 && m.selectedIndex < len(m.jobs) {
		return m.jobs[m.selectedIndex]
	}
	return nil
}

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	return func() tea.Msg {
		logFile := session.JobLogFile(job.ID, job.StartTime, job.SessionName)
		// First check if we can connect to the host
		// Don't quote path - it contains ~ which needs shell expansion
		stdout, stderr, err := ssh.Run(job.Host, fmt.Sprintf("tail -50 %s 2>&1", logFile))
		if err != nil {
			// Check if it's a connection error
			combined := stdout + stderr
			if ssh.IsConnectionError(combined) {
				return logFetchedMsg{
					jobID:   job.ID,
					content: fmt.Sprintf("Host %s unreachable", job.Host),
				}
			}
			// Other SSH error
			return logFetchedMsg{
				jobID:   job.ID,
				content: fmt.Sprintf("Error: %s", strings.TrimSpace(combined)),
			}
		}
		// Check if output indicates file not found
		if strings.Contains(stdout, "No such file") || strings.Contains(stdout, "cannot open") {
			return logFetchedMsg{
				jobID:   job.ID,
				content: "Log file not found (may have been cleaned up)",
			}
		}
		return logFetchedMsg{
			jobID:   job.ID,
			content: stdout,
		}
	}
}

func (m Model) performBackgroundSync() tea.Cmd {
	return func() tea.Msg {
		hosts, err := db.ListUniqueRunningHosts(m.database)
		if err != nil {
			return syncCompletedMsg{err: err}
		}

		var updated int
		for _, host := range hosts {
			jobs, err := db.ListRunning(m.database, host)
			if err != nil {
				continue
			}

			for _, job := range jobs {
				changed, err := syncJobQuick(m.database, job)
				if err != nil {
					continue
				}
				if changed {
					updated++
				}
			}
		}

		return syncCompletedMsg{updated: updated}
	}
}

func (m Model) killJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	database := m.database
	return func() tea.Msg {
		tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		err := ssh.TmuxKillSession(job.Host, tmuxSession)
		if err == nil {
			db.MarkDeadByID(database, job.ID)
		}
		return jobKilledMsg{jobID: job.ID, err: err}
	}
}

func (m Model) restartJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		// Read metadata from remote (for old jobs)
		metadataFile := session.JobMetadataFile(job.ID, job.StartTime, job.SessionName)
		content, err := ssh.ReadRemoteFile(job.Host, metadataFile)
		if err != nil || content == "" {
			// Fall back to database info
			content = ""
		}

		var workingDir, command, description string
		if content != "" {
			metadata := session.ParseMetadata(content)
			workingDir = metadata["working_dir"]
			command = metadata["command"]
			description = metadata["description"]
		}

		// Fall back to job info if metadata missing
		if workingDir == "" {
			workingDir = job.WorkingDir
		}
		if command == "" {
			command = job.Command
		}
		if description == "" {
			description = job.Description
		}

		if workingDir == "" || command == "" {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("missing working directory or command")}
		}

		// Kill existing session if running
		oldTmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		exists, _ := ssh.TmuxSessionExistsQuick(job.Host, oldTmuxSession)
		if exists {
			ssh.TmuxKillSession(job.Host, oldTmuxSession)
		}

		// Create new job record to get ID
		newJobID, err := db.RecordJobStarting(database, job.Host, workingDir, command, description)
		if err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("create job record: %w", err)}
		}

		// Get the new job to access start time
		newJob, err := db.GetJobByID(database, newJobID)
		if err != nil || newJob == nil {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("get new job: %w", err)}
		}

		// Generate new file paths from job ID
		newTmuxSession := session.TmuxSessionName(newJobID)
		logFile := session.LogFile(newJobID, newJob.StartTime)
		statusFile := session.StatusFile(newJobID, newJob.StartTime)
		newMetadataFile := session.MetadataFile(newJobID, newJob.StartTime)

		// Create log directory on remote
		mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
		if _, stderr, err := ssh.Run(job.Host, mkdirCmd); err != nil {
			db.UpdateJobFailed(database, newJobID, fmt.Sprintf("create log dir: %s", stderr))
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("create log directory: %s", stderr)}
		}

		// Save metadata
		newMetadata := session.FormatMetadata(newJobID, workingDir, command, job.Host, description, newJob.StartTime)
		// Don't quote path - it contains ~ which needs shell expansion
		metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", newMetadataFile, newMetadata)
		ssh.Run(job.Host, metadataCmd)

		// Create the wrapped command with better error capture
		// Note: file paths use ~ which must not be quoted to allow expansion
		wrappedCommand := fmt.Sprintf(
			`echo "=== START $(date) ===" > %s; `+
				`echo "job_id: %d" >> %s; `+
				`echo "cd: %s" >> %s; `+
				`echo "cmd: %s" >> %s; `+
				`echo "===" >> %s; `+
				`cd '%s' && (%s) 2>&1 | tee -a %s; `+
				`EXIT_CODE=${PIPESTATUS[0]}; `+
				`echo "=== END exit=$EXIT_CODE $(date) ===" >> %s; `+
				`echo $EXIT_CODE > %s`,
			logFile,
			newJobID, logFile,
			workingDir, logFile,
			command, logFile,
			logFile,
			workingDir, command, logFile,
			logFile,
			statusFile)

		// Escape single quotes for embedding in single-quoted string
		escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

		// Start tmux session - use single quotes to prevent shell expansion
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", newTmuxSession, escapedCommand)
		if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
			db.UpdateJobFailed(database, newJobID, fmt.Sprintf("start tmux: %s", stderr))
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("start session: %s", stderr)}
		}

		// Mark job as running
		if err := db.UpdateJobRunning(database, newJobID); err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: err}
		}

		return jobRestartedMsg{oldJobID: job.ID, newJobID: newJobID}
	}
}

// syncJobQuick checks and updates a single job's status (no retry for TUI responsiveness)
func syncJobQuick(database *sql.DB, job *db.Job) (bool, error) {
	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, tmuxSession)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	statusFile := session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletionByID(database, job.ID, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := db.MarkDeadByID(database, job.ID); err != nil {
		return false, err
	}
	return true, nil
}

func (m Model) pruneJobs() tea.Cmd {
	return func() tea.Msg {
		count, err := db.PruneJobs(m.database, false, nil)
		return pruneCompletedMsg{count: count, err: err}
	}
}

func (m Model) removeJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		err := db.DeleteJob(database, job.ID)
		return jobRemovedMsg{jobID: job.ID, err: err}
	}
}

func (m Model) createJob() tea.Cmd {
	database := m.database
	host := strings.TrimSpace(m.inputs[inputHost].Value())
	command := strings.TrimSpace(m.inputs[inputCommand].Value())
	description := strings.TrimSpace(m.inputs[inputDescription].Value())
	workingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())

	if workingDir == "" {
		workingDir = "~"
	}

	return func() tea.Msg {
		timeout := 30 * time.Second

		// Create job record to get ID
		jobID, err := db.RecordJobStarting(database, host, workingDir, command, description)
		if err != nil {
			return jobCreatedMsg{err: fmt.Errorf("create job record: %w", err)}
		}

		// Get the new job to access start time
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			return jobCreatedMsg{err: fmt.Errorf("get new job: %w", err)}
		}

		// Generate file paths from job ID
		tmuxSession := session.TmuxSessionName(jobID)
		logFile := session.LogFile(jobID, job.StartTime)
		statusFile := session.StatusFile(jobID, job.StartTime)
		metadataFile := session.MetadataFile(jobID, job.StartTime)

		// Create log directory on remote
		mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
		if _, _, err := ssh.RunWithTimeout(host, mkdirCmd, timeout); err != nil {
			db.UpdateJobFailed(database, jobID, fmt.Sprintf("create log dir: %v", err))
			return jobCreatedMsg{err: fmt.Errorf("connect to %s: %v", host, err)}
		}

		// Save metadata
		metadata := session.FormatMetadata(jobID, workingDir, command, host, description, job.StartTime)
		// Don't quote path - it contains ~ which needs shell expansion
		metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, metadata)
		ssh.RunWithTimeout(host, metadataCmd, timeout)

		// Create the wrapped command with better error capture
		// Note: file paths use ~ which must not be quoted to allow expansion
		wrappedCommand := fmt.Sprintf(
			`echo "=== START $(date) ===" > %s; `+
				`echo "job_id: %d" >> %s; `+
				`echo "cd: %s" >> %s; `+
				`echo "cmd: %s" >> %s; `+
				`echo "===" >> %s; `+
				`cd '%s' && (%s) 2>&1 | tee -a %s; `+
				`EXIT_CODE=${PIPESTATUS[0]}; `+
				`echo "=== END exit=$EXIT_CODE $(date) ===" >> %s; `+
				`echo $EXIT_CODE > %s`,
			logFile,
			jobID, logFile,
			workingDir, logFile,
			command, logFile,
			logFile,
			workingDir, command, logFile,
			logFile,
			statusFile)

		// Escape single quotes for embedding in single-quoted string
		escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)

		// Start tmux session - use single quotes to prevent shell expansion
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", tmuxSession, escapedCommand)
		if _, _, err := ssh.RunWithTimeout(host, tmuxCmd, timeout); err != nil {
			db.UpdateJobFailed(database, jobID, fmt.Sprintf("start tmux: %v", err))
			return jobCreatedMsg{err: fmt.Errorf("start session: %v", err)}
		}

		// Mark job as running
		if err := db.UpdateJobRunning(database, jobID); err != nil {
			return jobCreatedMsg{err: err}
		}

		return jobCreatedMsg{jobID: jobID}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// formatStartTime formats a start time as relative ("2h ago") for recent jobs
// or as absolute ("01/02 15:04") for older jobs
func formatStartTime(startTime int64) string {
	t := time.Unix(startTime, 0)
	elapsed := time.Since(t)

	if elapsed < 12*time.Hour {
		if elapsed < time.Minute {
			return "just now"
		} else if elapsed < time.Hour {
			return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
		}
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	}
	return t.Format("01/02 15:04")
}
