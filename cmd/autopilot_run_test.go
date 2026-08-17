package cmd

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/orchestration"
)

// The pass classification, timer policy, and wake snapshot are shared with the
// daemon and the TUI; they are tested in internal/orchestration/dispatch_test.go.
// What remains here is the headless runner's own presentation and wiring.

func TestAnyBlockedReason(t *testing.T) {
	if got := anyBlockedReason(nil); got != "" {
		t.Errorf("nil map: got %q, want empty", got)
	}
	if got := anyBlockedReason(map[int64]string{1: "  ", 2: ""}); got != "" {
		t.Errorf("blank reasons: got %q, want empty", got)
	}
	if got := anyBlockedReason(map[int64]string{1: " hello "}); got != "hello" {
		t.Errorf("trimmed: got %q, want %q", got, "hello")
	}
}

func TestClassifyAutopilotPassDelegatesToSharedPolicy(t *testing.T) {
	pausedWait := 42 * time.Second
	result := &orchestration.GroupedAutoPilotResult{Launched: 1}

	gotOutcome, gotWait := classifyAutopilotPass(result, nil, pausedWait)
	wantOutcome, wantWait := orchestration.ClassifyPass(result, nil, pausedWait)
	if gotOutcome != wantOutcome || gotWait != wantWait {
		t.Fatalf("classifyAutopilotPass = (%q, %s), want (%q, %s)",
			gotOutcome, gotWait, wantOutcome, wantWait)
	}
}
