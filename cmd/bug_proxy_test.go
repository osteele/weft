package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

// captureIssuesCLI swaps the issues CLI for a recorder.
func captureIssuesCLI(t *testing.T) *[][]string {
	t.Helper()
	original := runIssuesCLI
	var calls [][]string
	runIssuesCLI = func(args ...string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { runIssuesCLI = original })
	return &calls
}
func TestIssuesTrackerRejectsUnstubbedCLI(t *testing.T) {
	// If the test guard is removed, the proxy can only invoke this harmless
	// executable, never the user's installed issue tracker.
	binary := filepath.Join(t.TempDir(), "issues")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	originalBinary := issuesBinary
	issuesBinary = binary
	t.Cleanup(func() { issuesBinary = originalBinary })

	defer func() {
		if recover() == nil {
			t.Fatal("unstubbed issue reporting was allowed to execute a subprocess")
		}
	}()
	_ = newIssuesBugTracker("weft").Report(db.BugReport{Title: "must remain isolated"})
}

func argValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestIssuesTrackerReportTranslatesJobAndHostToRef(t *testing.T) {
	calls := captureIssuesCLI(t)
	jobID := int64(6066)

	if err := newIssuesBugTracker("weft").Report(db.BugReport{
		Title:       "queue payload missing",
		Kind:        "invariant",
		Scope:       "infrastructure",
		Likelihood:  "likely",
		Severity:    "error",
		Fingerprint: "queue.missing_payload",
		JobID:       &jobID,
		Host:        "studio",
		Summary:     "queue state is inconsistent",
		Detail:      "raw evidence",
		Note:        "first sighting",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(*calls))
	}
	args := (*calls)[0]
	if args[0] != "report" {
		t.Fatalf("subcommand = %q, want report", args[0])
	}
	// The job id must keep the wj#### form it is cited as elsewhere.
	if ref, ok := argValue(args, "--ref"); !ok || ref != "wj6066 on studio" {
		t.Fatalf("--ref = %q (present=%v), want \"wj6066 on studio\"", ref, ok)
	}
	for flag, want := range map[string]string{
		"--component":   "weft",
		"--title":       "queue payload missing",
		"--kind":        "invariant",
		"--fingerprint": "queue.missing_payload",
		"--severity":    "error",
		"--summary":     "queue state is inconsistent",
		"--detail":      "raw evidence",
		"--note":        "first sighting",
	} {
		if got, ok := argValue(args, flag); !ok || got != want {
			t.Errorf("%s = %q (present=%v), want %q", flag, got, ok, want)
		}
	}
	// weft's own flag names must not leak through to the ledger.
	for _, absent := range []string{"--job", "--host"} {
		if _, ok := argValue(args, absent); ok {
			t.Errorf("%s should not be forwarded", absent)
		}
	}
}

func TestIssuesTrackerOmitsEmptyFlags(t *testing.T) {
	calls := captureIssuesCLI(t)
	if err := newIssuesBugTracker("weft").Report(db.BugReport{Title: "bare"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	args := (*calls)[0]
	// An empty ref must be omitted rather than passed as "", which would
	// overwrite a ref already recorded on a deduped issue.
	if _, ok := argValue(args, "--ref"); ok {
		t.Error("--ref should be omitted when there is no job or host")
	}
	for _, absent := range []string{"--summary", "--detail", "--note"} {
		if _, ok := argValue(args, absent); ok {
			t.Errorf("%s should be omitted when empty", absent)
		}
	}
}

func TestIssuesTrackerPassthroughCommands(t *testing.T) {
	calls := captureIssuesCLI(t)
	tracker := newIssuesBugTracker("weft")

	if err := tracker.Note("wb64", "seen again"); err != nil {
		t.Fatalf("Note: %v", err)
	}
	if err := tracker.List(true); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := tracker.List(false); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := tracker.Show("wb64"); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if err := tracker.Close("wb64", "fixed"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tracker.Reopen("wb64"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	got := make([]string, 0, len(*calls))
	for _, call := range *calls {
		got = append(got, strings.Join(call, " "))
	}
	want := []string{
		"note wb64 seen again",
		"list --component weft --all",
		"list --component weft",
		"show wb64",
		"close wb64 --reason fixed",
		"reopen wb64",
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIssuesTrackerDefaultsComponentToWeft(t *testing.T) {
	calls := captureIssuesCLI(t)
	if err := newIssuesBugTracker("  ").Report(db.BugReport{Title: "t"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if component, ok := argValue((*calls)[0], "--component"); !ok || component != "weft" {
		t.Fatalf("--component = %q, want weft", component)
	}
}
