package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// attemptsListModel is a read-only drill-down screen pushed by the watch
// router when the user presses `a` on a selected job row.
type attemptsListModel struct {
	database *sql.DB
	jobID    int64
	job      *db.Job
	views    []attemptView
	err      error
	cursor   int
	width    int
}

// attemptView pairs an attempt with its launch (if any) and the derived
// lifecycle fields used across columns, so the per-row cascade over
// timestamp candidates runs once instead of once per column.
type attemptView struct {
	attempt db.JobAttempt
	launch  *db.Launch
	phase   attemptPhase
	// when is the timestamp that defines `phase`, used both for the When
	// column and as the duration start when the job never ran.
	when int64
}

type attemptPhase int

const (
	phaseNone attemptPhase = iota
	phaseQueued
	phaseRequest
	phaseLaunch
	phaseBooting
	phaseReady
	phaseRunning
	phaseEnded
)

func (p attemptPhase) Label() string {
	switch p {
	case phaseQueued:
		return "queued"
	case phaseRequest:
		return "request"
	case phaseLaunch:
		return "launch"
	case phaseBooting:
		return "booting"
	case phaseReady:
		return "ready"
	case phaseRunning:
		return "running"
	case phaseEnded:
		return "ended"
	}
	return "—"
}

type attemptsLoadedMsg struct {
	job   *db.Job
	views []attemptView
	err   error
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
		launchIDs := make([]int64, 0, len(attempts))
		for _, a := range attempts {
			if a.LaunchID != nil && *a.LaunchID > 0 {
				launchIDs = append(launchIDs, *a.LaunchID)
			}
		}
		launches, err := db.GetLaunchesByIDs(database, launchIDs)
		if err != nil {
			return attemptsLoadedMsg{job: job, err: fmt.Errorf("load launches: %w", err)}
		}
		views := make([]attemptView, len(attempts))
		for i, a := range attempts {
			v := attemptView{attempt: a}
			if a.LaunchID != nil {
				v.launch = launches[*a.LaunchID]
			}
			v.phase, v.when = derivePhase(a, v.launch)
			views[i] = v
		}
		return attemptsLoadedMsg{job: job, views: views}
	}
}

// derivePhase returns the furthest lifecycle checkpoint the attempt
// reached, along with the timestamp that marks it. This is the single
// source of truth the row columns key off.
func derivePhase(a db.JobAttempt, l *db.Launch) (attemptPhase, int64) {
	if ts := ptrVal(a.EndTime); ts > 0 {
		return phaseEnded, ts
	}
	if ts := ptrVal(a.StartTime); ts > 0 {
		return phaseRunning, ts
	}
	if l != nil {
		if ts := ptrVal(l.ReadyAt); ts > 0 {
			return phaseReady, ts
		}
		if ts := ptrVal(l.ProviderRunningAt); ts > 0 {
			return phaseBooting, ts
		}
		if ts := ptrVal(l.LaunchedAt); ts > 0 {
			return phaseLaunch, ts
		}
		if l.CreatedAt > 0 {
			return phaseRequest, l.CreatedAt
		}
	}
	if ts := ptrVal(a.QueuedAt); ts > 0 {
		return phaseQueued, ts
	}
	return phaseNone, 0
}

func (m attemptsListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case attemptsLoadedMsg:
		m.job = msg.job
		m.views = msg.views
		m.err = msg.err
		if m.cursor >= len(m.views) {
			m.cursor = max(0, len(m.views)-1)
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q", "ctrl+c", "backspace":
			return m, func() tea.Msg { return switchBackFromAttemptsMsg{} }
		case "ctrl+z":
			return m, tea.Suspend
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.views)-1 {
				m.cursor++
			}
			return m, nil
		case "g", "home":
			m.cursor = 0
			return m, nil
		case "G", "end":
			if len(m.views) > 0 {
				m.cursor = len(m.views) - 1
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

	if len(m.views) == 0 {
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
	// Title + status + project share the first line; the description goes on
	// its own line(s) below, wrapped to the terminal width so it doesn't get
	// truncated.
	firstLine := []string{title}
	status := m.job.EffectiveStatus()
	if status != "" {
		firstLine = append(firstLine, attemptStatusStyle(status).Render(status))
	}
	if m.job.Project != "" {
		firstLine = append(firstLine, tuiDimStyle.Render("project: "+m.job.Project))
	}
	header := strings.Join(firstLine, "  ")

	desc := m.job.EffectiveDescription()
	if desc == "" {
		return header
	}
	wrapWidth := m.width
	if wrapWidth <= 0 {
		wrapWidth = 100
	}
	wrapped := wrapDisplayWidth(desc, wrapWidth)
	descLines := make([]string, len(wrapped))
	for i, line := range wrapped {
		descLines[i] = tuiDimStyle.Render(line)
	}
	return header + "\n" + strings.Join(descLines, "\n")
}

type attemptColumn struct {
	title string
	width int
	value func(v attemptView, now int64) string
}

func attemptColumns() []attemptColumn {
	return []attemptColumn{
		{"#", 4, func(v attemptView, _ int64) string { return fmt.Sprintf("%d", v.attempt.AttemptNumber) }},
		{"Status", 10, func(v attemptView, _ int64) string { return v.attempt.Status }},
		{"Phase", 8, func(v attemptView, _ int64) string { return v.phase.Label() }},
		{"Host", 22, func(v attemptView, _ int64) string { return attemptHostCell(v) }},
		{"When", 24, func(v attemptView, _ int64) string { return attemptWhenCell(v) }},
		{"Duration", 10, func(v attemptView, now int64) string { return attemptDurationCell(v, now) }},
		{"Exit", 5, func(v attemptView, _ int64) string {
			if v.attempt.ExitCode == nil {
				return "—"
			}
			return fmt.Sprintf("%d", *v.attempt.ExitCode)
		}},
		{"Outcome/Reason", 40, func(v attemptView, _ int64) string { return attemptOutcomeText(v.attempt) }},
	}
}

func (m attemptsListModel) renderTable() string {
	cols := attemptColumns()
	now := time.Now().Unix()
	var b strings.Builder

	headerCells := make([]string, len(cols))
	for i, c := range cols {
		headerCells[i] = padOrTruncateDisplay(c.title, c.width, false)
	}
	b.WriteString(tuiAccentStyle.Render(strings.Join(headerCells, " ")))
	b.WriteString("\n")

	for i, v := range m.views {
		cells := make([]string, len(cols))
		for j, c := range cols {
			cells[j] = padOrTruncateDisplay(c.value(v, now), c.width, false)
		}
		row := strings.Join(cells, " ")
		styled := attemptStatusStyle(v.attempt.Status).Render(row)
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

func attemptHostCell(v attemptView) string {
	if v.attempt.Host != "" && !db.IsLaunchHost(v.attempt.Host) {
		return v.attempt.Host
	}
	if v.launch != nil {
		label := ids.FormatInstanceID(v.launch.ID)
		if gpu := v.launch.DisplayGPUBrief(); gpu != "" {
			label = label + " " + gpu
		}
		return label
	}
	return "—"
}

func attemptWhenCell(v attemptView) string {
	if v.when == 0 {
		return "—"
	}
	ts := formatAttemptTimestamp(v.when)
	// Running/ended attempts show a bare timestamp; earlier phases get a
	// short prefix so the column conveys both when *and* how far it got.
	switch v.phase {
	case phaseRunning, phaseEnded:
		return ts
	}
	return v.phase.Label() + " " + ts
}

func attemptDurationCell(v attemptView, now int64) string {
	start := ptrVal(v.attempt.StartTime)
	if start == 0 {
		start = v.when
	}
	if start == 0 {
		return "—"
	}
	end := ptrVal(v.attempt.EndTime)
	if end == 0 && v.launch != nil {
		end = ptrVal(v.launch.EndedAt)
	}
	if end == 0 {
		end = now
	}
	elapsed := end - start
	if elapsed < 0 {
		return "—"
	}
	return db.FormatDuration(elapsed)
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

func formatAttemptTimestamp(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func ptrVal(ts *int64) int64 {
	if ts == nil || *ts <= 0 {
		return 0
	}
	return *ts
}
