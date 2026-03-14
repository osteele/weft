package cmd

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestLaunchModelView_ShowsPartialFailures(t *testing.T) {
	m := launchModel{
		done:          true,
		campaignID:    49,
		instanceIDs:   []int64{108},
		partialErrors: []string{"RTX3090 >=20GB: create instance: quota", "A100 >=60GB: create instance: capacity"},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Campaign 49: launched instances: 108",
		"2 planned launch(es) failed:",
		"RTX3090",
		"A100",
		"Press Enter, Esc, or q to continue.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelView_ShowsCostPlaceholderWhileLoadingOffers(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "A100",
			GPUMemGB: 80,
			Jobs: []*db.Job{
				{ID: 7, Description: "train", WorkingDir: "/tmp/project-alpha", Project: "exp-042"},
			},
		},
	}
	items, selected, cursor := buildItemsFromGroups(groups)
	m := launchModel{
		groups:      groups,
		items:       items,
		selected:    selected,
		cursor:      cursor,
		loading:     true,
		instanceIDs: nil,
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"── Cost Estimate",
		"Awaiting offers...",
		"exp-042",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestLaunchModelUpdate_OffersErrorQuits(t *testing.T) {
	model, cmd := launchModel{}.Update(offersLoadedMsg{err: fmt.Errorf("vastai: DNS lookup failed")})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "DNS lookup failed") {
		t.Fatalf("expected stored error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_ReconcileNoJobsQuits(t *testing.T) {
	model, cmd := launchModel{reconciling: true}.Update(reconcileDoneMsg{groups: []campaign.InstanceGroup{}})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "all placed during reconciliation") {
		t.Fatalf("expected reconciliation error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_LaunchErrorQuits(t *testing.T) {
	model, cmd := launchModel{launching: true}.Update(instancesLaunchedMsg{err: fmt.Errorf("runpod: HTTP 503")})
	got := model.(launchModel)
	if got.err == nil || !strings.Contains(got.err.Error(), "HTTP 503") {
		t.Fatalf("expected stored error, got %v", got.err)
	}
	assertQuitCmd(t, cmd)
}

func TestLaunchModelUpdate_PartialFailuresStayOpen(t *testing.T) {
	model, cmd := launchModel{launching: true}.Update(instancesLaunchedMsg{
		instanceIDs: []int64{42},
		errors:      []error{fmt.Errorf("quota exceeded")},
	})
	got := model.(launchModel)
	if !got.done {
		t.Fatal("expected done state")
	}
	if len(got.partialErrors) != 1 {
		t.Fatalf("expected 1 partial error, got %d", len(got.partialErrors))
	}
	if cmd != nil {
		t.Fatalf("expected no quit command for partial failures, got %T", cmd)
	}
}

func TestLaunchModelView_ErrorHasNoDismissPrompt(t *testing.T) {
	m := launchModel{
		err:       fmt.Errorf("vastai: auth failed"),
		fromWatch: true,
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Error: vastai: auth failed") {
		t.Fatalf("expected error output, got:\n%s", out)
	}
	if strings.Contains(out, "Press Enter") {
		t.Fatalf("unexpected dismiss prompt in error view:\n%s", out)
	}
}

func assertQuitCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}
