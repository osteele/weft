package terminal

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/orchestration"
)

func TestRebalancePreviewView_Empty(t *testing.T) {
	m := rebalancePreviewModel{active: true}
	out := stripANSI(m.View(100, 30))
	if !strings.Contains(out, "No rebalance moves found.") {
		t.Fatalf("expected empty message, got:\n%s", out)
	}
	if !strings.Contains(out, "[esc] dismiss") {
		t.Fatalf("expected dismiss footer, got:\n%s", out)
	}
}

func TestRebalancePreviewView_InBudgetMove(t *testing.T) {
	move := orchestration.QueueRebalanceMove{
		JobID:          12,
		FromInstanceID: 3,
		ToInstanceID:   4,
		CostRatio:      1.05,
		CostCeiling:    1.10,
		Reason:         "score -1.000, same class",
	}
	m := rebalancePreviewModel{active: true, moves: []orchestration.QueueRebalanceMove{move}}
	out := stripANSI(m.View(100, 30))
	for _, want := range []string{"JOB", "wj12", "wi3", "wi4", "1.05", "score -1.000", "[y] apply all", "[n]/[esc] cancel"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in view, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "over-budget") {
		t.Fatalf("in-budget move should not be flagged, got:\n%s", out)
	}
}

func TestRebalancePreviewView_OnPremMove(t *testing.T) {
	move := orchestration.QueueRebalanceMove{
		JobID:          12,
		FromInstanceID: 3,
		ToHost:         "studio",
		Reason:         "rental-to-onprem rebalance",
	}
	m := rebalancePreviewModel{active: true, moves: []orchestration.QueueRebalanceMove{move}}
	out := stripANSI(m.View(100, 30))
	for _, want := range []string{"wj12", "wi3", "studio", "on-prem", "rental-to-onprem"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in view, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "over-budget") {
		t.Fatalf("on-prem move should not be flagged over-budget, got:\n%s", out)
	}
}

func TestRebalancePreviewView_MixedBudgetMoves(t *testing.T) {
	inBudget := orchestration.QueueRebalanceMove{
		JobID:          12,
		FromInstanceID: 3,
		ToInstanceID:   4,
		CostRatio:      1.05,
		CostCeiling:    1.10,
		Reason:         "same class",
	}
	overBudget := orchestration.QueueRebalanceMove{
		JobID:          13,
		FromInstanceID: 3,
		ToInstanceID:   5,
		CostRatio:      1.25,
		CostCeiling:    1.10,
		Reason:         "cost increase within cap",
	}
	m := rebalancePreviewModel{active: true, moves: []orchestration.QueueRebalanceMove{inBudget, overBudget}}
	raw := m.View(100, 30)
	out := stripANSI(raw)
	if !strings.Contains(out, "wj13") || !strings.Contains(out, "over-budget") {
		t.Fatalf("expected over-budget row, got:\n%s", out)
	}
	styledLine := rebalanceOverBudgetStyle.Render(formatRebalanceMoveLine(overBudget))
	if !strings.Contains(raw, styledLine) {
		t.Fatalf("expected over-budget warning style to be applied")
	}
}

func TestRebalancePreviewView_LoadingAndApplying(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    rebalancePreviewModel
		want string
	}{
		{name: "loading", m: rebalancePreviewModel{active: true, loading: true}, want: "Planning rebalance moves"},
		{name: "applying", m: rebalancePreviewModel{active: true, applying: true}, want: "Applying rebalance moves"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := stripANSI(tc.m.View(100, 30))
			if !strings.Contains(out, tc.want) {
				t.Fatalf("expected %q, got:\n%s", tc.want, out)
			}
		})
	}
}
