package terminal

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/cloud"
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
	sessions []attemptSession
	err      error
	loaded   bool
	cursor   int
	width    int
	height   int
	expanded bool
}

// attemptSession groups consecutive attempts sharing an effective host.
// indices reference into attemptsListModel.views, newest-first.
type attemptSession struct {
	indices []int
	host    string
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

// groupAttemptSessions folds each contiguous run of attempts sharing an
// effective host into one session. Attempts with no host inherit from
// older neighbors so the (queued, canceled-superseded) intent pair lands
// in the same session.
func groupAttemptSessions(views []attemptView) []attemptSession {
	if len(views) == 0 {
		return nil
	}
	hosts := make([]string, len(views))
	for i, v := range views {
		h := attemptHostCell(v)
		if h == "—" {
			h = ""
		}
		hosts[i] = h
	}
	// Inherit empty hosts from the older neighbor, then forward-fill
	// from the newer side for any leading empties.
	for i := len(hosts) - 2; i >= 0; i-- {
		if hosts[i] == "" {
			hosts[i] = hosts[i+1]
		}
	}
	for i := 1; i < len(hosts); i++ {
		if hosts[i] == "" {
			hosts[i] = hosts[i-1]
		}
	}

	var sessions []attemptSession
	for i, h := range hosts {
		if i == 0 || h != hosts[i-1] || h == "" {
			sessions = append(sessions, attemptSession{host: h})
		}
		s := &sessions[len(sessions)-1]
		s.indices = append(s.indices, i)
	}
	return sessions
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

// rowCount returns the number of cursor-addressable rows for the current
// view mode.
func (m attemptsListModel) rowCount() int {
	if m.expanded {
		return len(m.views)
	}
	return len(m.sessions)
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
		m.height = msg.Height
		return m, nil
	case attemptsLoadedMsg:
		m.job = msg.job
		m.views = msg.views
		m.sessions = groupAttemptSessions(msg.views)
		m.err = msg.err
		m.loaded = true
		if n := m.rowCount(); m.cursor >= n {
			m.cursor = max(0, n-1)
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
			if m.cursor < m.rowCount()-1 {
				m.cursor++
			}
			return m, nil
		case "g", "home":
			m.cursor = 0
			return m, nil
		case "G", "end":
			if n := m.rowCount(); n > 0 {
				m.cursor = n - 1
			}
			return m, nil
		case "c":
			// Toggle between collapsed (one row per session) and expanded
			// (one row per attempt). Keep the cursor anchored on the same
			// underlying attempt across the toggle.
			m = m.toggleExpanded()
			return m, nil
		case "r":
			return m, m.loadAttempts()
		}
	}
	return m, nil
}

// toggleExpanded flips between collapsed and expanded modes, translating
// the cursor so the same underlying attempt stays selected.
func (m attemptsListModel) toggleExpanded() attemptsListModel {
	if len(m.views) == 0 {
		m.expanded = !m.expanded
		return m
	}
	if m.expanded {
		// Currently on a view index; find the session it belongs to.
		viewIdx := m.cursor
		for sIdx, s := range m.sessions {
			for _, idx := range s.indices {
				if idx == viewIdx {
					m.cursor = sIdx
					m.expanded = false
					return m
				}
			}
		}
		m.cursor = 0
	} else {
		// Currently on a session index; jump to its first (newest) view.
		if m.cursor >= 0 && m.cursor < len(m.sessions) {
			m.cursor = m.sessions[m.cursor].indices[0]
		} else {
			m.cursor = 0
		}
		m.expanded = true
	}
	return m
}

func (m attemptsListModel) View() string {
	var b strings.Builder

	header := m.renderHeader()
	b.WriteString(header)
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(tuiFailedStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
		b.WriteString(tuiDimStyle.Render("Press esc to go back, r to retry."))
		return b.String()
	}

	if !m.loaded {
		b.WriteString(tuiDimStyle.Render("Loading attempts…"))
		return padToHeight(b.String(), m.height)
	}
	if len(m.views) == 0 {
		b.WriteString(tuiDimStyle.Render("No attempts recorded for this job."))
		b.WriteString("\n\n")
		b.WriteString(tuiDimStyle.Render("esc/q back · r reload"))
		return padToHeight(b.String(), m.height)
	}

	footer := m.renderFooter()

	// Compute how many body rows fit. Reserve lines for header (variable
	// height because the description wraps), the blank line below, the
	// table column header, the blank line above the footer, and the footer.
	chrome := lipgloss.Height(header) + 1 + 1 + 1 + lipgloss.Height(footer)
	bodyHeight := m.height - chrome
	if bodyHeight < 1 || m.height == 0 {
		bodyHeight = m.rowCount()
	}

	b.WriteString(m.renderTable(bodyHeight))
	b.WriteString("\n")
	b.WriteString(footer)
	return padToHeight(b.String(), m.height)
}

// padToHeight appends blank lines so the rendered frame always occupies
// exactly `height` lines. Without this, narrowing the terminal can leave
// the previous (taller) frame's content visible below the new render.
func padToHeight(s string, height int) string {
	if height <= 0 {
		return s
	}
	have := lipgloss.Height(s)
	if have >= height {
		return s
	}
	return s + strings.Repeat("\n", height-have)
}

func (m attemptsListModel) renderFooter() string {
	mode := "collapsed"
	if m.expanded {
		mode = "expanded"
	}
	return tuiDimStyle.Render(fmt.Sprintf(
		"j/k move · g/G top/bottom · c %s · r reload · esc/q back",
		mode,
	))
}

func (m attemptsListModel) renderHeader() string {
	title := tuiTitleStyle.Render(fmt.Sprintf("Attempts for job %s", ids.FormatJobID(m.jobID)))
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
	out := header + "\n" + strings.Join(descLines, "\n")
	if chain := m.renderPlacementChain(wrapWidth); chain != "" {
		out += "\n" + chain
	}
	return out
}

// renderPlacementChain summarizes the host-bounce pattern. Hosts appearing
// in ≥2 non-adjacent sessions are "stable" (the source-of-record position
// that gets restored after each move-to-new); single-occurrence hosts are
// "transient" target attempts. Returns "" for trivial histories.
func (m attemptsListModel) renderPlacementChain(wrapWidth int) string {
	if len(m.sessions) < 3 {
		return ""
	}
	occur := map[string]int{}
	machines := map[string]string{}
	for _, s := range m.sessions {
		if s.host == "" {
			continue
		}
		occur[s.host]++
		if mid := machineForSession(m.views, s); mid != "" {
			machines[s.host] = mid
		}
	}
	var stable, transient []string
	for _, s := range m.sessions {
		if s.host == "" || slices.Contains(stable, s.host) || slices.Contains(transient, s.host) {
			continue
		}
		if occur[s.host] >= 2 {
			stable = append(stable, s.host)
		} else {
			transient = append(transient, s.host)
		}
	}
	if len(stable) == 0 && len(transient) < 3 {
		return ""
	}

	var parts []string
	if len(stable) > 0 {
		labels := make([]string, len(stable))
		for i, h := range stable {
			if mid := machines[h]; mid != "" {
				labels[i] = fmt.Sprintf("%s (%s, ×%d)", h, mid, occur[h])
			} else {
				labels[i] = fmt.Sprintf("%s (×%d)", h, occur[h])
			}
		}
		parts = append(parts, "stable: "+strings.Join(labels, ", "))
	}
	if len(transient) > 0 {
		parts = append(parts, fmt.Sprintf("bounced through: %s", strings.Join(transient, ", ")))
	}
	line := "Placement: " + strings.Join(parts, " · ")
	wrapped := wrapDisplayWidth(line, wrapWidth)
	out := make([]string, len(wrapped))
	for i, l := range wrapped {
		out[i] = tuiDimStyle.Render(l)
	}
	return strings.Join(out, "\n")
}

// machineForSession returns the first non-empty machine ID across the
// session's attempts, or "" if none of the underlying launches expose one.
func machineForSession(views []attemptView, s attemptSession) string {
	for _, idx := range s.indices {
		v := views[idx]
		if v.launch != nil && strings.TrimSpace(v.launch.MachineID) != "" {
			return v.launch.MachineID
		}
	}
	return ""
}

type attemptColumn struct {
	title string
	width int
	value func(v attemptView, now int64) string
}

func attemptColumns() []attemptColumn {
	return []attemptColumn{
		{"#", 7, func(v attemptView, _ int64) string { return fmt.Sprintf("%d", v.attempt.AttemptNumber) }},
		{"Status", 10, func(v attemptView, _ int64) string { return v.attempt.Status }},
		{"Phase", 8, func(v attemptView, _ int64) string { return v.phase.Label() }},
		{"Host", 10, func(v attemptView, _ int64) string { return attemptHostCell(v) }},
		{"Where", 18, func(v attemptView, _ int64) string { return attemptWhereCell(v) }},
		{"GPU", 18, func(v attemptView, _ int64) string { return attemptGPUCell(v) }},
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

func (m attemptsListModel) renderTable(bodyHeight int) string {
	cols := attemptColumns()
	now := time.Now().Unix()
	var b strings.Builder

	headerCells := make([]string, len(cols))
	for i, c := range cols {
		headerCells[i] = padOrTruncateDisplay(c.title, c.width, false)
	}
	b.WriteString(tuiAccentStyle.Render(strings.Join(headerCells, " ")))
	b.WriteString("\n")

	total := m.rowCount()
	if bodyHeight < 1 {
		bodyHeight = total
	}
	hintAbove, hintBelow := 0, 0
	if total > bodyHeight {
		hintAbove, hintBelow = 1, 1
	}
	rowsBudget := bodyHeight - hintAbove - hintBelow
	if rowsBudget < 1 {
		rowsBudget = 1
	}
	start, end := visibleWindow(m.cursor, total, rowsBudget)
	if start == 0 {
		hintAbove = 0
	}
	if end == total {
		hintBelow = 0
	}

	if hintAbove > 0 {
		b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  ▲ %d more above", start)))
		b.WriteString("\n")
	}
	for i := start; i < end; i++ {
		b.WriteString(m.renderRow(cols, i, now))
		b.WriteString("\n")
	}
	if hintBelow > 0 {
		b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  ▼ %d more below", total-end)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderRow formats one styled row for the current view mode (expanded =
// per-attempt, collapsed = per-session). Only called for visible rows so
// large histories don't materialize off-screen content.
func (m attemptsListModel) renderRow(cols []attemptColumn, i int, now int64) string {
	cells := make([]string, len(cols))
	var statusKey string
	if m.expanded || len(m.sessions) == 0 {
		v := m.views[i]
		for j, c := range cols {
			cells[j] = padOrTruncateDisplay(c.value(v, now), c.width, false)
		}
		statusKey = v.attempt.Status
	} else {
		s := m.sessions[i]
		latest := m.views[s.indices[0]]
		for j, c := range cols {
			switch c.title {
			case "#":
				cells[j] = padOrTruncateDisplay(sessionRangeLabel(m.views, s), c.width, false)
			case "Outcome/Reason":
				cells[j] = padOrTruncateDisplay(sessionOutcomeText(m.views, s), c.width, false)
			default:
				cells[j] = padOrTruncateDisplay(c.value(latest, now), c.width, false)
			}
		}
		statusKey = latest.attempt.Status
	}
	row := strings.Join(cells, " ")
	styled := attemptStatusStyle(statusKey).Render(row)
	if i == m.cursor {
		styled = tuiSelectedRowStyle.Render(styled)
	}
	return styled
}

// visibleWindow returns the [start, end) row range that keeps `cursor`
// visible inside a window of `height` rows.
func visibleWindow(cursor, total, height int) (int, int) {
	if total <= height {
		return 0, total
	}
	if cursor < height/2 {
		return 0, height
	}
	if cursor >= total-height/2 {
		return total - height, total
	}
	start := cursor - height/2
	return start, start + height
}

// sessionRangeLabel returns "96" for a singleton session or "14–96" for a
// multi-attempt run, using the underlying attempt numbers (not view
// indices).
func sessionRangeLabel(views []attemptView, s attemptSession) string {
	if len(s.indices) == 0 {
		return "—"
	}
	if len(s.indices) == 1 {
		return fmt.Sprintf("%d", views[s.indices[0]].attempt.AttemptNumber)
	}
	first := views[s.indices[0]].attempt.AttemptNumber
	last := views[s.indices[len(s.indices)-1]].attempt.AttemptNumber
	if first == last {
		return fmt.Sprintf("%d", first)
	}
	return fmt.Sprintf("%d–%d", last, first)
}

// sessionOutcomeText returns the latest attempt's outcome plus per-outcome
// counts of the folded older attempts.
func sessionOutcomeText(views []attemptView, s attemptSession) string {
	if len(s.indices) == 0 {
		return "—"
	}
	latest := attemptOutcomeText(views[s.indices[0]].attempt)
	if len(s.indices) == 1 {
		return latest
	}
	counts := map[string]int{}
	for _, idx := range s.indices[1:] {
		oc := views[idx].attempt.CloudOutcome
		if oc == "" {
			oc = "queued"
		}
		counts[oc]++
	}
	parts := make([]string, 0, len(counts))
	for k, n := range counts {
		parts = append(parts, fmt.Sprintf("%d× %s", n, k))
	}
	slices.Sort(parts)
	return latest + " · " + strings.Join(parts, ", ")
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

func attemptProviderCell(v attemptView) string {
	if v.launch != nil && v.launch.Provider != "" {
		if name := cloud.Provider(v.launch.Provider).DisplayName(); name != "" {
			return name
		}
		return v.launch.Provider
	}
	if v.attempt.Host != "" && !db.IsLaunchHost(v.attempt.Host) {
		return "on-prem"
	}
	return "—"
}

func attemptHostCell(v attemptView) string {
	if v.attempt.Host != "" && !db.IsLaunchHost(v.attempt.Host) {
		return v.attempt.Host
	}
	if v.launch != nil {
		return ids.FormatInstanceID(v.launch.ID)
	}
	return "—"
}

func attemptMachineCell(v attemptView) string {
	if v.launch == nil || strings.TrimSpace(v.launch.MachineID) == "" {
		return "—"
	}
	return v.launch.MachineID
}

// attemptWhereCell folds Provider and Machine into one cell so the table
// reads as e.g. "52305 @ Vast.ai" or "Vast.ai" when no machine is known.
// Falls back to "on-prem" for jobs running on weft inventory hosts.
func attemptWhereCell(v attemptView) string {
	provider := attemptProviderCell(v)
	machine := attemptMachineCell(v)
	switch {
	case machine != "—" && provider != "—":
		return machine + " @ " + provider
	case provider != "—":
		return provider
	case machine != "—":
		return machine
	default:
		return "—"
	}
}

func attemptGPUCell(v attemptView) string {
	if v.launch != nil {
		if gpu := v.launch.DisplayGPUBrief(); gpu != "" {
			return gpu
		}
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
