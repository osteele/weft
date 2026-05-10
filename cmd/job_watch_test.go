package cmd

import "testing"

func TestJobWatchNeedsWritableDB(t *testing.T) {
	origGroupBy := listGroupBy
	t.Cleanup(func() {
		listGroupBy = origGroupBy
	})

	listGroupBy = "status"
	if !jobWatchNeedsWritableDB(true) {
		t.Fatal("expected grouped status TUI watch to require writable DB")
	}
	if jobWatchNeedsWritableDB(false) {
		t.Fatal("expected non-TUI watch to use read-only DB")
	}

	listGroupBy = ""
	if !jobWatchNeedsWritableDB(true) {
		t.Fatal("expected non-grouped TUI watch to require writable DB")
	}
}

func TestWatchJobIDsKeepPlainSummaryDefault(t *testing.T) {
	origFollow := watchFollow
	origTransitions := watchTransitionsOnly
	origJSONLines := watchJSONLines
	origUntilAnyTerminal := watchUntilAnyTerminal
	origTUI := watchTUI
	t.Cleanup(func() {
		watchFollow = origFollow
		watchTransitionsOnly = origTransitions
		watchJSONLines = origJSONLines
		watchUntilAnyTerminal = origUntilAnyTerminal
		watchTUI = origTUI
	})

	watchFollow = false
	watchTransitionsOnly = false
	watchJSONLines = false
	watchUntilAnyTerminal = false
	watchTUI = false
	if !watchOneShotSummary() {
		t.Fatal("expected explicit job-id watch default to use one-shot plain summary")
	}

	watchTUI = true
	if watchOneShotSummary() {
		t.Fatal("expected --tui to disable one-shot summary without changing job-id watch to TUI")
	}
}
