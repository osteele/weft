package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/orchestration"
)

func TestClassifyAutopilotPass(t *testing.T) {
	pausedWait := 42 * time.Second

	tests := []struct {
		name        string
		result      *orchestration.GroupedAutoPilotResult
		err         error
		wantOutcome autopilotOutcome
		wantWait    time.Duration
	}{
		{
			name:        "paused",
			err:         orchestration.ErrAutopilotPaused,
			wantOutcome: outcomePaused,
			wantWait:    pausedWait,
		},
		{
			name:        "busy",
			err:         orchestration.ErrAutopilotBusy,
			wantOutcome: outcomeBusy,
			wantWait:    orchestration.AutopilotCooldownContend,
		},
		{
			name:        "error",
			err:         errors.New("boom"),
			wantOutcome: outcomeError,
			wantWait:    orchestration.AutopilotCooldownError,
		},
		{
			name:        "nil result is idle",
			result:      nil,
			wantOutcome: outcomeIdle,
			wantWait:    orchestration.AutopilotCooldownIdle,
		},
		{
			name:        "placed counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Placed: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "launched counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Launched: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "rebalanced counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Rebalanced: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "blocked-only with no progress",
			result:      &orchestration.GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: "no offer"}},
			wantOutcome: outcomeBlocked,
			wantWait:    orchestration.AutopilotCooldownBlocked,
		},
		{
			name:        "progress wins over blocked",
			result:      &orchestration.GroupedAutoPilotResult{Launched: 1, BlockedReasons: map[int64]string{1: "no offer"}},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "empty result is idle",
			result:      &orchestration.GroupedAutoPilotResult{},
			wantOutcome: outcomeIdle,
			wantWait:    orchestration.AutopilotCooldownIdle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOutcome, gotWait := classifyAutopilotPass(tc.result, tc.err, pausedWait)
			if gotOutcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", gotOutcome, tc.wantOutcome)
			}
			if gotWait != tc.wantWait {
				t.Errorf("wait = %s, want %s", gotWait, tc.wantWait)
			}
		})
	}
}

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
