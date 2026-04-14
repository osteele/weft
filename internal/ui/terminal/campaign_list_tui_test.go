package terminal

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

func TestCampaignListHelpOverlayOpensAndCloses(t *testing.T) {
	m := campaignListModel{
		items: []campaignListItem{
			{isHeader: true, headerText: "Apr 14, 2026"},
			{campaign: &db.Campaign{ID: 42, Status: db.CampaignStatusRunning}},
		},
		cursor: 1,
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got := next.(campaignListModel)
	if !got.showHelp {
		t.Fatal("expected help overlay to open")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Campaign List Keybindings") {
		t.Fatalf("expected campaign help title, got:\n%s", out)
	}
	if !strings.Contains(out, "enter watch selected campaign") {
		t.Fatalf("expected enter keybinding in help, got:\n%s", out)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got = next.(campaignListModel)
	if got.showHelp {
		t.Fatal("expected help overlay to close on Esc")
	}
}
