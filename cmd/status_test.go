package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestIsWaitTerminalStatus(t *testing.T) {
	waitTerminal := []string{
		db.StatusCompleted,
		db.StatusDead,
		db.StatusFailed,
		db.StatusKilled,
		db.StatusCanceled,
	}
	for _, s := range waitTerminal {
		if !isWaitTerminalStatus(s) {
			t.Errorf("isWaitTerminalStatus(%q) = false, want true", s)
		}
	}

	// Draft is terminal but NOT wait-terminal
	if isWaitTerminalStatus(db.StatusDraft) {
		t.Error("isWaitTerminalStatus(draft) = true, want false")
	}

	nonTerminal := []string{
		db.StatusRunning,
		db.StatusStarting,
		db.StatusQueued,
	}
	for _, s := range nonTerminal {
		if isWaitTerminalStatus(s) {
			t.Errorf("isWaitTerminalStatus(%q) = true, want false", s)
		}
	}
}

func TestShouldAttemptSync(t *testing.T) {
	syncable := []string{
		db.StatusRunning,
		db.StatusStarting,
		db.StatusPaused,
		db.StatusQueued,
	}
	for _, s := range syncable {
		if !shouldAttemptSync(s) {
			t.Errorf("shouldAttemptSync(%q) = false, want true", s)
		}
	}

	nonSyncable := []string{
		db.StatusCompleted,
		db.StatusDead,
		db.StatusFailed,
		db.StatusKilled,
		db.StatusDraft,
	}
	for _, s := range nonSyncable {
		if shouldAttemptSync(s) {
			t.Errorf("shouldAttemptSync(%q) = true, want false", s)
		}
	}
}

func TestFormatJobIDList(t *testing.T) {
	tests := []struct {
		name string
		ids  []int64
		want string
	}{
		{"sorts descending to ascending", []int64{3, 1, 2}, "wj1:wj3"},
		{"empty slice", []int64{}, ""},
		{"single element", []int64{42}, "wj42"},
		{"already sorted", []int64{10, 20, 30}, "wj10,wj20,wj30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ids.FormatJobIDListCompact(tt.ids)
			if got != tt.want {
				t.Errorf("FormatJobIDListCompact(%v) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}
}
