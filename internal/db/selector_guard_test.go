package db

import (
	"os"
	"strings"
	"testing"
)

// TestNoInlineAttemptSelector keeps attempt selection deliberate: the
// unfiltered "physical latest" subquery may live only in its named constant,
// so a new mutation cannot quietly bypass the open/authoritative filters by
// pasting its own ORDER BY attempt_number selector. It searches for the body
// of latestAttemptSubquery itself, so the guard tracks the constant if its
// text ever changes.
func TestNoInlineAttemptSelector(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, latestAttemptSubquery) {
				continue
			}
			if name == "attempts.go" && strings.Contains(line, "const latestAttemptSubquery =") {
				continue // the sanctioned definition
			}
			t.Errorf("%s:%d inlines the unfiltered attempt selector; use latestAttemptSubquery (or a narrower named selector) instead:\n\t%s",
				name, i+1, strings.TrimSpace(line))
		}
	}
}
