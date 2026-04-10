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
	if jobWatchNeedsWritableDB(true) {
		t.Fatal("expected non-grouped TUI watch to use read-only DB")
	}
}
