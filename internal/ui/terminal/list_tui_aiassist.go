package terminal

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/aiassist"
	"github.com/osteele/weft/internal/db"
)

// aiAssistPhase tracks where the assist overlay is in its lifecycle.
type aiAssistPhase int

const (
	aiAssistPhaseLoading   aiAssistPhase = iota // first claude call in flight
	aiAssistPhaseResult                         // showing summary (+ optional menu)
	aiAssistPhaseExecuting                      // second claude call in flight, streaming
	aiAssistPhaseDone                           // second call finished
)

// aiAssistState is the per-overlay state. Lives on listTUIModel as a pointer
// so the absence of an overlay is the zero value.
type aiAssistState struct {
	jobID     int64
	jobLabel  string
	kind      aiassist.Kind
	phase     aiAssistPhase
	result    aiassist.AssistResult
	selected  map[int]bool
	cursor    int
	execLines []string
	err       error
	cancel    context.CancelFunc
	chunkCh   chan aiAssistChunkMsg
}

type aiAssistResultMsg struct {
	jobID int64
	res   aiassist.AssistResult
	err   error
}

type aiAssistChunkMsg struct {
	jobID int64
	chunk string
}

type aiAssistDoneMsg struct {
	jobID int64
	err   error
}

// Pointer receiver: mutates m.aiAssist in place. Caller still returns m
// from Update so bubbletea sees the change.
func (m *listTUIModel) beginAIAssist(job *db.Job) tea.Cmd {
	kind, ok := aiassist.KindForJob(job)
	if !ok {
		m.statusMessage = fmt.Sprintf("AI assist not available for %s jobs", job.EffectiveStatus())
		return nil
	}
	resultCh := make(chan aiAssistResultMsg, 1)
	ctx, cancel := context.WithCancel(m.ctx)
	m.aiAssist = &aiAssistState{
		jobID:    job.ID,
		jobLabel: fmt.Sprintf("#%d %s", job.ID, job.DirectoryTailDisplay()),
		kind:     kind,
		phase:    aiAssistPhaseLoading,
		selected: map[int]bool{},
		cancel:   cancel,
	}
	go func() {
		res, err := aiassist.Assist(ctx, job, kind)
		resultCh <- aiAssistResultMsg{jobID: job.ID, res: res, err: err}
	}()
	return m.waitForAIAssistResult(resultCh)
}

func (m listTUIModel) waitForAIAssistResult(ch <-chan aiAssistResultMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m listTUIModel) waitForAIAssistChunk(ch <-chan aiAssistChunkMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m listTUIModel) waitForAIAssistDone(ch <-chan aiAssistDoneMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *listTUIModel) startAIAssistChoices(job *db.Job) tea.Cmd {
	if m.aiAssist == nil {
		return nil
	}
	var picked []aiassist.Choice
	for i, c := range m.aiAssist.result.Choices {
		if m.aiAssist.selected[i] {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		m.aiAssist.err = nil
		m.statusMessage = "Select at least one action with space, then enter"
		return nil
	}
	chunkCh := make(chan aiAssistChunkMsg, 32)
	doneCh := make(chan aiAssistDoneMsg, 1)
	ctx, cancel := context.WithCancel(m.ctx)
	if m.aiAssist.cancel != nil {
		m.aiAssist.cancel()
	}
	m.aiAssist.cancel = cancel
	m.aiAssist.phase = aiAssistPhaseExecuting
	m.aiAssist.execLines = nil
	m.aiAssist.chunkCh = chunkCh
	jobID := job.ID
	go func() {
		_, err := aiassist.ExecuteChoices(ctx, job, picked, func(line string) {
			select {
			case chunkCh <- aiAssistChunkMsg{jobID: jobID, chunk: line}:
			case <-ctx.Done():
			}
		})
		close(chunkCh)
		doneCh <- aiAssistDoneMsg{jobID: jobID, err: err}
	}()
	return tea.Batch(m.waitForAIAssistChunk(chunkCh), m.waitForAIAssistDone(doneCh))
}

func (m listTUIModel) handleAIAssistKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st := m.aiAssist
	if st == nil {
		return m, nil
	}
	switch msg.String() {
	case "esc":
		if st.cancel != nil {
			st.cancel()
		}
		m.aiAssist = nil
		return m, nil
	case "ctrl+c":
		if st.cancel != nil {
			st.cancel()
		}
		m.aiAssist = nil
		return m, nil
	}
	if st.phase != aiAssistPhaseResult || len(st.result.Choices) == 0 {
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if st.cursor > 0 {
			st.cursor--
		}
	case "down", "j":
		if st.cursor < len(st.result.Choices)-1 {
			st.cursor++
		}
	case " ":
		st.selected[st.cursor] = !st.selected[st.cursor]
	case "enter":
		job := m.jobByID(st.jobID)
		if job == nil {
			m.statusMessage = "Job no longer in list"
			return m, nil
		}
		cmd := m.startAIAssistChoices(job)
		return m, cmd
	}
	return m, nil
}

func (m listTUIModel) applyAIAssistResult(msg aiAssistResultMsg) (tea.Model, tea.Cmd) {
	if m.aiAssist == nil || m.aiAssist.jobID != msg.jobID {
		return m, nil
	}
	if msg.err != nil {
		m.aiAssist.err = msg.err
		m.aiAssist.phase = aiAssistPhaseDone
		return m, nil
	}
	m.aiAssist.result = msg.res
	m.aiAssist.phase = aiAssistPhaseResult
	return m, nil
}

func (m listTUIModel) applyAIAssistChunk(msg aiAssistChunkMsg) (tea.Model, tea.Cmd) {
	if m.aiAssist == nil || m.aiAssist.jobID != msg.jobID {
		return m, nil
	}
	const (
		maxExecLines   = 2000
		maxChunkLength = 8 * 1024 // bound a single streamed event
	)
	chunk := msg.chunk
	if len(chunk) > maxChunkLength {
		chunk = chunk[:maxChunkLength] + "…"
	}
	m.aiAssist.execLines = append(m.aiAssist.execLines, chunk)
	if len(m.aiAssist.execLines) > maxExecLines {
		drop := len(m.aiAssist.execLines) - maxExecLines
		m.aiAssist.execLines = m.aiAssist.execLines[drop:]
	}
	if m.aiAssist.chunkCh != nil {
		return m, m.waitForAIAssistChunk(m.aiAssist.chunkCh)
	}
	return m, nil
}

func (m listTUIModel) applyAIAssistDone(msg aiAssistDoneMsg) (tea.Model, tea.Cmd) {
	if m.aiAssist == nil || m.aiAssist.jobID != msg.jobID {
		return m, nil
	}
	m.aiAssist.err = msg.err
	m.aiAssist.phase = aiAssistPhaseDone
	return m, nil
}

func (m listTUIModel) jobByID(id int64) *db.Job {
	for _, j := range m.jobs {
		if j != nil && j.ID == id {
			return j
		}
	}
	return nil
}

func (m listTUIModel) renderAIAssistOverlay() string {
	st := m.aiAssist
	if st == nil {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	title := fmt.Sprintf("Coding assistant · %s · job %s", st.kind.Label(), st.jobLabel)
	b.WriteString(listTUITitleStyle.Render(truncateDisplayWidth(title, width)))
	b.WriteString("\n\n")

	switch st.phase {
	case aiAssistPhaseLoading:
		b.WriteString(listTUIFooterStyle.Render("Asking Claude…  (esc cancels)"))
		b.WriteString("\n")
	case aiAssistPhaseResult:
		for _, line := range renderMarkdownBlock(st.result.Summary, width) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		if strings.TrimSpace(st.result.ETA) != "" {
			b.WriteString(listTUIFooterStyle.Render("ETA: " + st.result.ETA))
			b.WriteString("\n")
		}
		if len(st.result.Choices) == 0 {
			b.WriteString("\n")
			b.WriteString(listTUIFooterStyle.Render("No follow-up actions suggested.  [esc] dismiss"))
			break
		}
		b.WriteString("\n")
		b.WriteString(listTUIHeaderStyle.Render("Follow-up actions (space to toggle, enter to run):"))
		b.WriteString("\n\n")
		for i, c := range st.result.Choices {
			marker := "[ ]"
			if st.selected[i] {
				marker = "[x]"
			}
			line := fmt.Sprintf("%s %s", marker, c.Title)
			if i == st.cursor {
				line = listTUISelectedStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
			if strings.TrimSpace(c.Description) != "" {
				for _, dl := range renderMarkdownBlock(c.Description, max(10, width-6)) {
					b.WriteString(listTUIFooterStyle.Render("      " + dl))
					b.WriteString("\n")
				}
			}
		}
		b.WriteString("\n")
		b.WriteString(listTUIFooterStyle.Render("[j/k] move  [space] toggle  [enter] run  [esc] dismiss"))
	case aiAssistPhaseExecuting:
		b.WriteString(listTUIFooterStyle.Render("Running selected actions…  (esc cancels)"))
		b.WriteString("\n\n")
		writeExecLines(&b, st.execLines, width, max(1, m.height-8))
	case aiAssistPhaseDone:
		if st.err != nil {
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("160")).Render("Error: " + st.err.Error()))
			b.WriteString("\n\n")
		}
		writeExecLines(&b, st.execLines, width, max(1, m.height-6))
		b.WriteString("\n")
		b.WriteString(listTUIFooterStyle.Render("[esc] dismiss"))
	}
	return b.String()
}

// writeExecLines wraps and markdown-renders the trailing exec lines that
// will fit in `tail` display rows. Renders from the end so we don't pay for
// wrapping lines that would be dropped.
func writeExecLines(b *strings.Builder, raw []string, width, tail int) {
	rendered := make([]string, 0, tail+8)
	for i := len(raw) - 1; i >= 0 && len(rendered) < tail; i-- {
		block := renderMarkdownBlock(raw[i], width)
		// Prepend in display order.
		rendered = append(block, rendered...)
	}
	if len(rendered) > tail {
		rendered = rendered[len(rendered)-tail:]
	}
	for _, line := range rendered {
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// renderMarkdownBlock wraps a possibly-multi-paragraph block of markdown to
// the given display width, then renders inline emphasis (bold, italic, code)
// and basic block markers (headings, list bullets) using lipgloss styles.
// Wrapping happens before inline rendering so style spans never split across
// wrap boundaries.
func renderMarkdownBlock(text string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		prefix, body, blockStyle := splitBlockPrefix(para)
		avail := width - lipgloss.Width(prefix)
		if avail < 10 {
			avail = 10
		}
		wrapped := wrapDisplayWidth(body, avail)
		for i, line := range wrapped {
			rendered := renderInlineMarkdown(line)
			if blockStyle != nil {
				rendered = blockStyle.Render(rendered)
			}
			if i == 0 {
				out = append(out, prefix+rendered)
			} else {
				out = append(out, strings.Repeat(" ", lipgloss.Width(prefix))+rendered)
			}
		}
	}
	return out
}

// splitBlockPrefix peels off a leading list bullet or heading marker, so the
// wrapped continuation lines align under the body, not the marker.
func splitBlockPrefix(line string) (prefix, body string, style *lipgloss.Style) {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	switch {
	case strings.HasPrefix(trimmed, "### "):
		s := lipgloss.NewStyle().Bold(true)
		return indent, strings.TrimPrefix(trimmed, "### "), &s
	case strings.HasPrefix(trimmed, "## "):
		s := lipgloss.NewStyle().Bold(true)
		return indent, strings.TrimPrefix(trimmed, "## "), &s
	case strings.HasPrefix(trimmed, "# "):
		s := lipgloss.NewStyle().Bold(true).Underline(true)
		return indent, strings.TrimPrefix(trimmed, "# "), &s
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
		return indent + "• ", trimmed[2:], nil
	}
	return "", line, nil
}

// renderInlineMarkdown converts inline `**bold**`, `*italic*`, `_italic_`,
// and ` `code` ` markers into ANSI-styled spans. Any internal spaces inside
// a span are replaced with a non-break space so subsequent wrappers (none in
// our flow, since we wrap first) won't split styled words.
func renderInlineMarkdown(s string) string {
	s = renderSpan(s, "**", inlineBoldStyle)
	s = renderSpan(s, "`", inlineCodeStyle)
	s = renderSpan(s, "_", inlineItalicStyle)
	s = renderSpan(s, "*", inlineItalicStyle)
	return s
}

// renderSpan replaces matched delim-bounded spans with style.Render(inner).
// Greedy non-empty matches; unmatched delimiters are left alone.
func renderSpan(s, delim string, style lipgloss.Style) string {
	var out strings.Builder
	for {
		i := strings.Index(s, delim)
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])
		rest := s[i+len(delim):]
		j := strings.Index(rest, delim)
		if j < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		inner := rest[:j]
		if inner == "" {
			out.WriteString(s[i : i+2*len(delim)])
			s = rest[j+len(delim):]
			continue
		}
		out.WriteString(style.Render(inner))
		s = rest[j+len(delim):]
	}
}

var (
	inlineBoldStyle   = lipgloss.NewStyle().Bold(true)
	inlineItalicStyle = lipgloss.NewStyle().Italic(true)
	inlineCodeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)
