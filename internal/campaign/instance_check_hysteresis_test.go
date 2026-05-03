package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// Regression: a flickering provider response (alive observation between dead
// observations) must reset the dead-confirm timer. Previously firstDeadAt was
// only cleared inside checkProviderDead, so a flicker would still reach the
// terminal action on the second dead observation if the first dead time had
// already aged past confirmTime.
func TestCheckInstance_LiveObservationResetsDeadConfirmTimer(t *testing.T) {
	r := NewReconciler()
	r.deadConfirmTime = 30 * time.Second
	ci := &db.Launch{
		ID:     42,
		Status: db.LaunchStatusRunning,
	}

	// 1) First pass: provider returns nil (instance not in list). Hysteresis
	//    starts the dead-confirm timer; no terminal action yet.
	action := r.CheckInstance(CheckInstanceParams{
		CI:           ci,
		ProviderInst: nil,
		ProviderErr:  nil,
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("first dead observation: action.Kind = %d, want ActionNone", action.Kind)
	}
	if _, seen := r.firstDeadAt[ci.ID]; !seen {
		t.Fatalf("first dead observation should have started the dead-confirm timer")
	}

	// 2) Second pass: provider returns the instance alive. The timer must be
	//    cleared so a future dead stretch starts fresh.
	action = r.CheckInstance(CheckInstanceParams{
		CI: ci,
		ProviderInst: &cloud.Instance{
			ProviderID: "abc",
			Status:     cloud.ProviderStatusRunning,
		},
		Now: time.Now().Add(20 * time.Second),
	})
	if _, seen := r.firstDeadAt[ci.ID]; seen {
		t.Fatalf("alive observation should clear firstDeadAt for instance %d", ci.ID)
	}
	_ = action // step 7 doesn't fire when instance is alive; result is ActionNone
}

// Regression: an instance that disappeared 20s ago, came back briefly, then
// disappeared again should NOT be marked dead just because cumulative wall
// time since the first absence exceeds confirmTime. The hysteresis reset on
// alive observations forces a fresh stretch of consecutive dead observations.
func TestCheckInstance_FlickerDoesNotMarkDeadPrematurely(t *testing.T) {
	r := NewReconciler()
	r.deadConfirmTime = 30 * time.Second
	ci := &db.Launch{
		ID:     43,
		Status: db.LaunchStatusRunning,
	}
	t0 := time.Now()

	// Dead observation at t=0
	r.CheckInstance(CheckInstanceParams{CI: ci, ProviderInst: nil, Now: t0})
	// Alive observation at t=20s — resets timer
	r.CheckInstance(CheckInstanceParams{
		CI:           ci,
		ProviderInst: &cloud.Instance{ProviderID: "abc", Status: cloud.ProviderStatusRunning},
		Now:          t0.Add(20 * time.Second),
	})
	// Dead again at t=40s. Since the timer was reset, this should be the
	// first observation of a new dead stretch — not 40s into the old one.
	action := r.CheckInstance(CheckInstanceParams{CI: ci, ProviderInst: nil, Now: t0.Add(40 * time.Second)})
	if action.Kind == ActionProviderDead {
		t.Fatalf("flicker (dead→alive→dead within 40s) should not mark instance dead; got %d", action.Kind)
	}
}
