package cmd

import (
	"strings"
	"testing"
)

// A dry run renders the task YAML and submits nothing. It must not create a
// ledger row: the row would carry backend=skypilot with no external binding,
// and SyncExternalExecutorJob is guarded on the binding existing, so the job
// could never advance and would sit queued forever.
func TestSkySubmitDryRunUsesPlaceholderName(t *testing.T) {
	// The placeholder exists precisely because no job id is allocated. If a
	// future change reintroduces a ledger write it will want the real name
	// back, and this constant is the tell.
	if !strings.HasPrefix(skyDryRunTaskName, "weft-") {
		t.Fatalf("skyDryRunTaskName = %q, want a weft- prefixed placeholder", skyDryRunTaskName)
	}
	if strings.Contains(skyDryRunTaskName, "wj") {
		t.Fatalf("skyDryRunTaskName = %q, must not imply an allocated job id", skyDryRunTaskName)
	}
}

// A stronger assertion — that a dry run creates no jobs row — would need a
// real database in package cmd, which has no such harness today (these tests
// stub collaborators rather than opening one). The ordering is enforced by
// construction instead: the dry-run branch returns before db.Open().
