package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestBugCommandsLifecycle(t *testing.T) {
	db.SetupTestBugDB(t)
	setupLocalBugTrackerConfig(t)
	restoreBugFlags(t)

	bugReportScope = "infrastructure"
	bugReportKind = "invariant"
	bugReportFingerprint = "test.bug.lifecycle"
	out := captureStdout(t, func() {
		if err := runBugReport(&cobra.Command{}, []string{"test", "bug"}); err != nil {
			t.Fatalf("runBugReport: %v", err)
		}
	})
	if !strings.Contains(out, "Reported wb1: test bug") {
		t.Fatalf("report output = %q", out)
	}

	out = captureStdout(t, func() {
		if err := runBugList(&cobra.Command{}, nil); err != nil {
			t.Fatalf("runBugList: %v", err)
		}
	})
	if !strings.Contains(out, "wb1") || !strings.Contains(out, "test bug") {
		t.Fatalf("list output = %q", out)
	}

	if err := runBugNote(&cobra.Command{}, []string{"wb1", "extra", "context"}); err != nil {
		t.Fatalf("runBugNote: %v", err)
	}
	out = captureStdout(t, func() {
		if err := runBugShow(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugShow: %v", err)
		}
	})
	if !strings.Contains(out, "Bug ID:      wb1") || !strings.Contains(out, "extra context") {
		t.Fatalf("show output = %q", out)
	}

	if err := runBugNote(&cobra.Command{}, []string{"wb1", "agent's", "note"}); err != nil {
		t.Fatalf("runBugNote with apostrophe: %v", err)
	}
	out = captureStdout(t, func() {
		if err := runBugShow(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugShow after apostrophe note: %v", err)
		}
	})
	if !strings.Contains(out, "agent's note") {
		t.Fatalf("show output missing apostrophe note = %q", out)
	}

	bugCloseReason = "fixed"
	out = captureStdout(t, func() {
		if err := runBugClose(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugClose: %v", err)
		}
	})
	if !strings.Contains(out, "Closed wb1") {
		t.Fatalf("close output = %q", out)
	}

	out = captureStdout(t, func() {
		if err := runBugReopen(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugReopen: %v", err)
		}
	})
	if !strings.Contains(out, "Reopened wb1") {
		t.Fatalf("reopen output = %q", out)
	}

	out = captureStdout(t, func() {
		if err := runBugList(&cobra.Command{}, nil); err != nil {
			t.Fatalf("runBugList after reopen: %v", err)
		}
	})
	if !strings.Contains(out, "wb1") || !strings.Contains(out, "open") {
		t.Fatalf("list after reopen output = %q", out)
	}
}

func TestBugNoteTreatsFlagLikeTextAfterBugID(t *testing.T) {
	restoreBugFlags(t)
	var got []string
	cmd := &cobra.Command{
		Use:  "note [--stdin] <bug-id> [text...]",
		Args: usageArgs(validateBugNoteArgs),
		RunE: func(_ *cobra.Command, args []string) error {
			got = append([]string(nil), args...)
			return nil
		},
	}
	cmd.Flags().BoolVar(&bugNoteStdin, "stdin", false, "read note text from stdin")
	cmd.Flags().SetInterspersed(false)
	cmd.SetArgs([]string{"wb1", "--looks-like-a-flag", "value"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}
	want := []string{"wb1", "--looks-like-a-flag", "value"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestBugNoteStdin(t *testing.T) {
	db.SetupTestBugDB(t)
	setupLocalBugTrackerConfig(t)
	restoreBugFlags(t)

	if err := runBugReport(&cobra.Command{}, []string{"stdin", "bug"}); err != nil {
		t.Fatalf("runBugReport: %v", err)
	}

	bugNoteStdin = true
	withStdin(t, "line one\nline two\n", func() {
		if err := runBugNote(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugNote --stdin: %v", err)
		}
	})
	out := captureStdout(t, func() {
		if err := runBugShow(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugShow: %v", err)
		}
	})
	if !strings.Contains(out, "line one\nline two") {
		t.Fatalf("show output missing stdin note = %q", out)
	}
}

func TestValidateBugNoteArgs(t *testing.T) {
	restoreBugFlags(t)

	bugNoteStdin = false
	err := validateBugNoteArgs(&cobra.Command{}, []string{"wb1"})
	if err == nil || !strings.Contains(err.Error(), "bug note text is required") {
		t.Fatalf("missing note error = %v", err)
	}

	bugNoteStdin = true
	err = validateBugNoteArgs(&cobra.Command{}, []string{"wb1", "extra"})
	if err == nil || !strings.Contains(err.Error(), "--stdin expects exactly one bug id") {
		t.Fatalf("stdin plus text error = %v", err)
	}
}

func TestBugReportDoesNotOpenMainJobsDB(t *testing.T) {
	bugDB := db.SetupTestBugDB(t)
	setupLocalBugTrackerConfig(t)
	restoreBugFlags(t)

	jobsPath := filepath.Join(t.TempDir(), "jobs.db")
	if err := os.WriteFile(jobsPath, []byte("not a sqlite database"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	restoreJobsPath := db.SetDBPath(jobsPath)
	t.Cleanup(restoreJobsPath)

	out := captureStdout(t, func() {
		if err := runBugReport(&cobra.Command{}, []string{"schema", "mismatch"}); err != nil {
			t.Fatalf("runBugReport: %v", err)
		}
	})
	if !strings.Contains(out, "Reported wb1: schema mismatch") {
		t.Fatalf("report output = %q", out)
	}
	bugs, err := db.ListBugs(bugDB, false)
	if err != nil {
		t.Fatalf("ListBugs: %v", err)
	}
	if len(bugs) != 1 {
		t.Fatalf("len(bugs) = %d, want 1", len(bugs))
	}
}

func TestGitHubBugReportCreatesIssueWithMetadata(t *testing.T) {
	restoreBugFlags(t)
	var calls [][]string
	oldRun := runGitHubCLI
	runGitHubCLI = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "issue list"):
			return []byte(`[]`), nil
		case strings.HasPrefix(joined, "label create"):
			return []byte{}, nil
		case strings.HasPrefix(joined, "issue create"):
			return []byte("https://github.com/osteele/weft/issues/42\n"), nil
		default:
			t.Fatalf("unexpected gh call: %v", args)
			return nil, nil
		}
	}
	t.Cleanup(func() { runGitHubCLI = oldRun })

	report := db.BugReport{
		Title:       "github backed bug",
		Kind:        "invariant",
		Scope:       "infrastructure",
		Fingerprint: "unit.github.report",
		Detail:      "maintainer detail",
	}
	out := captureStdout(t, func() {
		if err := newGitHubBugTracker("osteele/weft").Report(report); err != nil {
			t.Fatalf("Report: %v", err)
		}
	})
	if !strings.Contains(out, "Reported #42: github backed bug") {
		t.Fatalf("report output = %q", out)
	}

	var createArgs []string
	for _, args := range calls {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "create" {
			createArgs = args
			break
		}
	}
	if createArgs == nil {
		t.Fatalf("missing issue create call: %#v", calls)
	}
	body := strings.Join(createArgs, "\n")
	for _, want := range []string{"<!-- weft-bug -->", "<!-- weft-bug-fingerprint:unit.github.report -->", "--repo\nosteele/weft"} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue create args missing %q:\n%v", want, createArgs)
		}
	}
}

func setupLocalBugTrackerConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	restore := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	t.Cleanup(restore)
	if err := config.SetBugTrackerSetting(config.BugTrackerLocal); err != nil {
		t.Fatalf("SetBugTrackerSetting: %v", err)
	}
}

func restoreBugFlags(t *testing.T) {
	t.Helper()
	oldReportTitle := bugReportTitle
	oldReportKind := bugReportKind
	oldReportScope := bugReportScope
	oldReportLikelihood := bugReportLikelihood
	oldReportSeverity := bugReportSeverity
	oldReportFingerprint := bugReportFingerprint
	oldReportJob := bugReportJob
	oldReportHost := bugReportHost
	oldReportSummary := bugReportSummary
	oldReportDetail := bugReportDetail
	oldReportNote := bugReportNote
	oldBugNoteStdin := bugNoteStdin
	oldListAll := bugListAll
	oldCloseReason := bugCloseReason
	t.Cleanup(func() {
		bugReportTitle = oldReportTitle
		bugReportKind = oldReportKind
		bugReportScope = oldReportScope
		bugReportLikelihood = oldReportLikelihood
		bugReportSeverity = oldReportSeverity
		bugReportFingerprint = oldReportFingerprint
		bugReportJob = oldReportJob
		bugReportHost = oldReportHost
		bugReportSummary = oldReportSummary
		bugReportDetail = oldReportDetail
		bugReportNote = oldReportNote
		bugNoteStdin = oldBugNoteStdin
		bugListAll = oldListAll
		bugCloseReason = oldCloseReason
	})
	bugReportTitle = ""
	bugReportKind = "bug"
	bugReportScope = "infrastructure"
	bugReportLikelihood = "unknown"
	bugReportSeverity = "notice"
	bugReportFingerprint = ""
	bugReportJob = ""
	bugReportHost = ""
	bugReportSummary = ""
	bugReportDetail = ""
	bugReportNote = ""
	bugNoteStdin = false
	bugListAll = false
	bugCloseReason = ""
}

func withStdin(t *testing.T, text string, fn func()) {
	t.Helper()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		_ = r.Close()
	}()

	if _, err := w.WriteString(text); err != nil {
		t.Fatalf("write stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close stdin pipe writer: %v", err)
	}

	fn()
}
