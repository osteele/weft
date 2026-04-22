package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

// attemptsListModel is a read-only drill-down screen pushed by the watch
// router when the user presses `a` on a selected job row.
type attemptsListModel struct {
	database *sql.DB
	jobID    int64
	job      *db.Job
	attempts []db.JobAttempt
	err      error
	cursor   int
}

type attemptsLoadedMsg struct {
	job      *db.Job
	attempts []db.JobAttempt
	err      error
}

func newAttemptsListModel(database *sql.DB, jobID int64) attemptsListModel {
	return attemptsListModel{database: database, jobID: jobID}
}

func (m attemptsListModel) Init() tea.Cmd {
	return m.loadAttempts()
}

func (m attemptsListModel) loadAttempts() tea.Cmd {
	database := m.database
	jobID := m.jobID
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return attemptsLoadedMsg{err: fmt.Errorf("load job: %w", err)}
		}
		attempts, err := db.ListAttempts(database, jobID)
		if err != nil {
			return attemptsLoadedMsg{job: job, err: fmt.Errorf("list attempts: %w", err)}
		}
		return attemptsLoadedMsg{job: job, attempts: attempts}
	}
}

func (m attemptsListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case attemptsLoadedMsg:
		m.job = msg.job
		m.attempts = msg.attempts
		m.err = msg.err
		if m.cursor >= len(m.attempts) {
			m.cursor = max(0, len(m.attempts)-1)
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q", "ctrl+c", "backspace":
			return m, func() tea.Msg { return switchBackFromAttemptsMsg{} }
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.attempts)-1 {
				m.cursor++
			}
			return m, nil
		case "g", "home":
			m.cursor = 0
			return m, nil
		case "G", "end":
			if len(m.attempts) > 0 {
				m.cursor = len(m.attempts) - 1
			}
			return m, nil
		case "r":
			return m, m.loadAttempts()
		}
	}
	return m, nil
}

func (m attemptsListModel) View() string {
	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(tuiFailedStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
		b.WriteString(tuiDimStyle.Render("Press esc to go back, r to retry."))
		return b.String()
	}

	if len(m.attempts) == 0 {
		b.WriteString(tuiDimStyle.Render("No attempts recorded for this job."))
		b.WriteString("\n\n")
		b.WriteString(tuiDimStyle.Render("esc/q back · r reload"))
		return b.String()
	}

	b.WriteString(m.renderTable())
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("j/k move · g/G top/bottom · r reload · esc/q back"))
	return b.String()
}

func (m attemptsListModel) renderHeader() string {
	title := tuiTitleStyle.Render(fmt.Sprintf("Attempts for job #%d", m.jobID))
	if m.job == nil {
		return title
	}
	parts := []string{title}
	if desc := m.job.EffectiveDescription(); desc != "" {
		parts = append(parts, tuiDimStyle.Render(truncate(desc, 80)))
	}
	status := m.job.EffectiveStatus()
	if status != "" {
		parts = append(parts, attemptStatusStyle(status).Render(status))
	}
	if m.job.Project != "" {
		parts = append(parts, tuiDimStyle.Render("project: "+m.job.Project))
	}
	return strings.Join(parts, "  ")
}

type attemptColumn struct {
	title string
	width int
	value func(a db.JobAttempt) string
}

func attemptColumns() []attemptColumn {
	return []attemptColumn{
		{"#", 4, func(a db.JobAttempt) string { return fmt.Sprintf("%d", a.AttemptNumber) }},
		{"Status", 12, func(a db.JobAttempt) string { return a.Status }},
		{"Host", 20, func(a db.JobAttempt) string {
			if a.Host == "" {
				return "—"
			}
			return a.Host
		}},
		{"Started", 19, func(a db.JobAttempt) string { return formatAttemptTime(a.StartTime) }},
		{"Duration", 10, func(a db.JobAttempt) string { return formatAttemptDuration(a.StartTime, a.EndTime) }},
		{"Exit", 5, func(a db.JobAttempt) string {
			if a.ExitCode == nil {
				return "—"
			}
			return fmt.Sprintf("%d", *a.ExitCode)
		}},
		{"Outcome/Reason", 40, func(a db.JobAttempt) string { return attemptOutcomeText(a) }},
	}
}

func (m attemptsListModel) renderTable() string {
	cols := attemptColumns()
	var b strings.Builder

	headerCells := make([]string, len(cols))
	for i, c := range cols {
		headerCells[i] = padOrTruncateDisplay(c.title, c.width, false)
	}
	b.WriteString(tuiAccentStyle.Render(strings.Join(headerCells, " ")))
	b.WriteString("\n")

	for i, a := range m.attempts {
		cells := make([]string, len(cols))
		for j, c := range cols {
			cells[j] = padOrTruncateDisplay(c.value(a), c.width, false)
		}
		row := strings.Join(cells, " ")
		styled := attemptStatusStyle(a.Status).Render(row)
		if i == m.cursor {
			styled = tuiSelectedRowStyle.Render(styled)
		}
		b.WriteString(styled)
		b.WriteString("\n")
	}
	return b.String()
}

func attemptStatusStyle(status string) lipgloss.Style {
	switch status {
	case db.StatusRunning:
		return tuiRunningStyle
	case db.StatusFailed, db.StatusDead, db.StatusKilled:
		return tuiFailedStyle
	case db.StatusCompleted, db.StatusCanceled:
		return tuiDimStyle
	default:
		return lipgloss.NewStyle()
	}
}

func attemptOutcomeText(a db.JobAttempt) string {
	if a.CloudOutcome != "" && a.CloudOutcome != db.AttemptOutcomeCompleted {
		if a.FailureReason != "" {
			return a.CloudOutcome + " — " + a.FailureReason
		}
		return a.CloudOutcome
	}
	if a.FailureReason != "" {
		return a.FailureReason
	}
	if a.ErrorMessage != "" {
		return a.ErrorMessage
	}
	if a.CloudOutcome == db.AttemptOutcomeCompleted {
		return db.AttemptOutcomeCompleted
	}
	return "—"
}

func formatAttemptTime(ts *int64) string {
	if ts == nil || *ts == 0 {
		return "—"
	}
	return time.Unix(*ts, 0).Format("2006-01-02 15:04:05")
}

func formatAttemptDuration(start, end *int64) string {
	if start == nil || *start == 0 {
		return "—"
	}
	endUnix := time.Now().Unix()
	if end != nil && *end != 0 {
		endUnix = *end
	}
	elapsed := endUnix - *start
	if elapsed < 0 {
		return "—"
	}
	return db.FormatDuration(elapsed)
}
