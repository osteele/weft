package terminal

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

func TestListTUIDiagnoseKeyRunsForSelectedJob(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{{ID: 6255, Status: db.StatusQueued}},
	}
	next, cmd, handled := handleListKeyBinding(m, "z", listFlatKeyBindings())
	if !handled || cmd == nil {
		t.Fatalf("z handled=%v cmd=%v, want diagnose command", handled, cmd)
	}
	got := next.(listTUIModel)
	if !strings.Contains(got.statusMessage, "Diagnosing job #6255") {
		t.Fatalf("status = %q, want selected diagnosis", got.statusMessage)
	}
	if !got.jobDiagnosisLoading {
		t.Fatalf("jobDiagnosisLoading = false, want true")
	}
}

func TestListTUIDiagnoseKeyRequiresJobRow(t *testing.T) {
	m := listTUIModel{}
	next, cmd, handled := handleListKeyBinding(m, "z", listFlatKeyBindings())
	if !handled || cmd != nil {
		t.Fatalf("z handled=%v cmd=%v, want handled without command", handled, cmd)
	}
	got := next.(listTUIModel)
	if got.statusMessage != "Select a job row to diagnose" {
		t.Fatalf("status = %q", got.statusMessage)
	}
}

func TestListTUIDiagnosisOverlayShowsLoadedOutput(t *testing.T) {
	m := listTUIModel{
		jobs:                []*db.Job{{ID: 6255, Status: db.StatusQueued}},
		width:               80,
		height:              24,
		jobDiagnosisJobID:   6255,
		jobDiagnosisLoading: true,
	}
	msg := listJobDiagnosisLoadedMsg{jobID: 6255, output: "State: queued\nWhy: awaiting placement"}
	next, _ := m.Update(msg)
	got := next.(listTUIModel)
	if !got.showJobDiagnosis {
		t.Fatalf("showJobDiagnosis = false, want true")
	}
	if got.jobDiagnosisLoading {
		t.Fatalf("jobDiagnosisLoading = true, want false")
	}
	if len(got.jobDiagnosisLines) != 2 {
		t.Fatalf("jobDiagnosisLines = %v, want 2 lines", got.jobDiagnosisLines)
	}
	view := got.renderJobDiagnosisView()
	if !strings.Contains(view, "State: queued") {
		t.Fatalf("view missing diagnosis output:\n%s", view)
	}
	if !strings.Contains(view, "esc/q/z back") {
		t.Fatalf("view missing footer:\n%s", view)
	}
}

func TestListTUIDiagnosisOverlayClosesOnZ(t *testing.T) {
	m := listTUIModel{
		width:             80,
		height:            24,
		showJobDiagnosis:  true,
		jobDiagnosisJobID: 6255,
		jobDiagnosisLines: []string{"State: queued"},
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	got := next.(listTUIModel)
	if got.showJobDiagnosis {
		t.Fatalf("showJobDiagnosis = true, want false after z")
	}
}

func TestListTUIDiagnosisOverlayClosesOnEsc(t *testing.T) {
	m := listTUIModel{
		width:             80,
		height:            24,
		showJobDiagnosis:  true,
		jobDiagnosisJobID: 6255,
		jobDiagnosisLines: []string{"State: queued"},
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := next.(listTUIModel)
	if got.showJobDiagnosis {
		t.Fatalf("showJobDiagnosis = true, want false after esc")
	}
}

func TestListTUIDiagnosisOverlayScrolls(t *testing.T) {
	m := listTUIModel{
		width:             80,
		height:            5,
		showJobDiagnosis:  true,
		jobDiagnosisJobID: 6255,
		jobDiagnosisLines: []string{"line1", "line2", "line3", "line4", "line5", "line6"},
	}
	if max := m.jobDiagnosisMaxScroll(); max <= 0 {
		t.Fatalf("jobDiagnosisMaxScroll = %d, want > 0", max)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	got := next.(listTUIModel)
	if got.jobDiagnosisScroll != 1 {
		t.Fatalf("jobDiagnosisScroll = %d, want 1", got.jobDiagnosisScroll)
	}
}
