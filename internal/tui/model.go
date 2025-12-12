package tui

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
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
	Prune   key.Binding
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
	Prune: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "prune"),
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

type tickMsg time.Time
type logTickMsg time.Time

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
	return Model{
		database:      database,
		selectedIndex: 0,
	}
}

// Close cleans up resources
func (m Model) Close() {
	if m.database != nil {
		m.database.Close()
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
		return m.handleKeyPress(msg)

	case jobsRefreshedMsg:
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Error loading jobs: %v", msg.err)
		} else {
			m.jobs = msg.jobs
			m.errorMessage = ""
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
		if msg.err != nil {
			m.errorMessage = fmt.Sprintf("Restart failed: %v", msg.err)
		} else {
			m.statusMessage = fmt.Sprintf("Job restarted (new ID: %d)", msg.newJobID)
		}
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
	}

	return m, nil
}

func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Up):
		if m.selectedIndex > 0 {
			m.selectedIndex--
			m.selectedJob = nil
			m.logContent = ""
		}
		return m, nil

	case key.Matches(msg, keys.Down):
		if m.selectedIndex < len(m.jobs)-1 {
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
		if m.selectedJob != nil && m.selectedJob.Status == db.StatusRunning {
			m.statusMessage = "Killing job..."
			return m, m.killSelectedJob()
		}
		return m, nil

	case key.Matches(msg, keys.Restart):
		if m.selectedJob != nil {
			m.statusMessage = "Restarting job..."
			return m, m.restartSelectedJob()
		}
		return m, nil

	case key.Matches(msg, keys.Prune):
		m.statusMessage = "Pruning completed/dead jobs..."
		return m, m.pruneJobs()
	}

	return m, nil
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

	return lipgloss.JoinVertical(
		lipgloss.Left,
		listView,
		logView,
		statusView,
	)
}

func (m Model) renderJobList(height int) string {
	var rows []string

	// Header
	header := fmt.Sprintf(" %-4s %-10s %-16s %-12s %-12s %s",
		"ID", "HOST", "SESSION", "STATUS", "STARTED", "DESCRIPTION")
	rows = append(rows, headerStyle.Render(header))

	// Jobs
	contentHeight := height - 4 // Account for borders and header
	for i, job := range m.jobs {
		if i >= contentHeight {
			break
		}

		status := m.formatStatus(job)
		started := time.Unix(job.StartTime, 0).Format("01/02 15:04")
		desc := job.Description
		if len(desc) > 25 {
			desc = desc[:22] + "..."
		}

		line := fmt.Sprintf(" %-4d %-10s %-16s %-12s %-12s %s",
			job.ID, truncate(job.Host, 10), truncate(job.SessionName, 16),
			status, started, desc)

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

	if m.selectedJob == nil {
		content = dimStyle.Render("Select a job and press Enter to view logs")
	} else if m.logLoading {
		content = dimStyle.Render("Loading logs...")
	} else if m.logContent == "" {
		content = dimStyle.Render("No log content available")
	} else {
		// Take last N lines that fit
		lines := strings.Split(m.logContent, "\n")
		maxLines := height - 4
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		content = strings.Join(lines, "\n")
	}

	// Title shows selected job info
	title := "Logs"
	if m.selectedJob != nil {
		title = fmt.Sprintf("Logs: %s@%s", m.selectedJob.SessionName, m.selectedJob.Host)
	}

	return logPanelStyle.Width(m.width - 2).Height(height).Render(
		titleStyle.Render(title) + "\n" + content,
	)
}

func (m Model) renderStatusBar() string {
	left := ""
	if m.errorMessage != "" {
		left = errorStyle.Render(m.errorMessage)
	} else if m.statusMessage != "" {
		left = statusMsgStyle.Render(m.statusMessage)
	}

	right := helpStyle.Render("↑/↓:nav Enter:logs r:restart k:kill p:prune q:quit")

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
		if job.ExitCode != nil && *job.ExitCode == 0 {
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

func (m Model) refreshJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := db.ListJobs(m.database, "", "", 100)
		return jobsRefreshedMsg{jobs: jobs, err: err}
	}
}

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	return func() tea.Msg {
		logFile := session.LogFile(job.SessionName)
		// First check if we can connect to the host
		stdout, stderr, err := ssh.Run(job.Host, fmt.Sprintf("tail -50 '%s' 2>&1", logFile))
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

func (m Model) killSelectedJob() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	return func() tea.Msg {
		err := ssh.TmuxKillSession(job.Host, job.SessionName)
		if err == nil {
			db.MarkDead(m.database, job.Host, job.SessionName)
		}
		return jobKilledMsg{jobID: job.ID, err: err}
	}
}

func (m Model) restartSelectedJob() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}

	job := m.selectedJob
	database := m.database
	return func() tea.Msg {
		// Read metadata from remote
		metadataFile := session.MetadataFile(job.SessionName)
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
		exists, _ := ssh.TmuxSessionExistsQuick(job.Host, job.SessionName)
		if exists {
			ssh.TmuxKillSession(job.Host, job.SessionName)
		}

		// Create file paths
		logFile := session.LogFile(job.SessionName)
		statusFile := session.StatusFile(job.SessionName)

		// Archive any existing log file
		archiveCmd := fmt.Sprintf("if [ -f '%s' ]; then mv '%s' '%s.$(date +%%Y%%m%%d-%%H%%M%%S).log'; fi",
			logFile, logFile, strings.TrimSuffix(logFile, ".log"))
		ssh.Run(job.Host, archiveCmd)

		// Remove old status file
		ssh.Run(job.Host, fmt.Sprintf("rm -f '%s'", statusFile))

		// Update metadata with new start time
		startTime := time.Now().Unix()
		newMetadata := session.FormatMetadata(workingDir, command, job.Host, description, startTime)
		metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, newMetadata)
		ssh.Run(job.Host, metadataCmd)

		// Create the wrapped command
		wrappedCommand := fmt.Sprintf(
			"cd '%s' && (%s) 2>&1 | tee '%s'; EXIT_CODE=\\${PIPESTATUS[0]}; echo \\$EXIT_CODE > '%s'",
			workingDir, command, logFile, statusFile)

		// Start tmux session
		tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", job.SessionName, wrappedCommand)
		if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("start session: %s", stderr)}
		}

		// Record in database
		newJobID, err := db.RecordStart(database, job.Host, job.SessionName, workingDir, command, startTime, description)
		if err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: err}
		}

		return jobRestartedMsg{oldJobID: job.ID, newJobID: newJobID}
	}
}

// syncJobQuick checks and updates a single job's status (no retry for TUI responsiveness)
func syncJobQuick(database *sql.DB, job *db.Job) (bool, error) {
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, job.SessionName)
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	statusFile := session.StatusFile(job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletion(database, job.Host, job.SessionName, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := db.MarkDead(database, job.Host, job.SessionName); err != nil {
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
