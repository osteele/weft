package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/osteele/weft/internal/db"
)

func TestBugCommandsLifecycle(t *testing.T) {
	db.SetupTestDB(t)
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

	bugCloseReason = "fixed"
	out = captureStdout(t, func() {
		if err := runBugClose(&cobra.Command{}, []string{"wb1"}); err != nil {
			t.Fatalf("runBugClose: %v", err)
		}
	})
	if !strings.Contains(out, "Closed wb1") {
		t.Fatalf("close output = %q", out)
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
	bugListAll = false
	bugCloseReason = ""
}
