package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// issuesBinary is the agent-issues CLI. Indirected for tests.
var issuesBinary = "issues"

// issuesBugTracker forwards `weft bug` to the shared agent-issues ledger.
//
// weft's bug history moved to that ledger on 2026-09-17, keeping every number,
// so `wb64` still resolves and every citation in a lab notebook still points at
// the same issue. This proxy exists so the `weft bug ...` commands that skills
// and muscle memory already use keep working while they are retired; it adds no
// behavior of its own beyond translating weft's job/host pair into the ledger's
// generic ref field.
type issuesBugTracker struct {
	component string
}

func newIssuesBugTracker(component string) issuesBugTracker {
	component = strings.TrimSpace(component)
	if component == "" {
		component = "weft"
	}
	return issuesBugTracker{component: component}
}

// runIssuesCLI executes the issues CLI. Stdout streams through so the caller
// sees the ledger's own wording rather than a re-rendered copy. Stderr is
// captured instead of streamed: on failure it becomes the returned error, so
// the problem is reported once rather than by both the child and cobra.
// Indirected so tests can assert the translated arguments.
var runIssuesCLI = func(args ...string) error {
	path, err := exec.LookPath(issuesBinary)
	if err != nil {
		return fmt.Errorf("the %s CLI is not on PATH: weft's issue ledger now lives in agent-issues (install it with `just install` in ~/code/agent-tools/agent-issues), or set `bug.tracker = \"local\"` to read the pre-migration ledger: %w", issuesBinary, err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	message := strings.TrimSpace(stderr.String())
	if runErr == nil {
		// A warning on a successful run still belongs on stderr.
		if message != "" {
			fmt.Fprintln(os.Stderr, message)
		}
		return nil
	}
	if message != "" {
		return errors.New(strings.TrimPrefix(message, "error: "))
	}
	return fmt.Errorf("run %s: %w", issuesBinary, runErr)
}

func (t issuesBugTracker) run(args ...string) error { return runIssuesCLI(args...) }

func (t issuesBugTracker) Report(report db.BugReport) error {
	args := []string{"report", "--component", t.component, "--title", report.Title}
	args = appendIssueFlag(args, "--kind", report.Kind)
	args = appendIssueFlag(args, "--scope", report.Scope)
	args = appendIssueFlag(args, "--likelihood", report.Likelihood)
	args = appendIssueFlag(args, "--severity", report.Severity)
	args = appendIssueFlag(args, "--fingerprint", report.Fingerprint)
	args = appendIssueFlag(args, "--ref", issueRef(report.JobID, report.Host))
	args = appendIssueFlag(args, "--summary", report.Summary)
	args = appendIssueFlag(args, "--detail", report.Detail)
	args = appendIssueFlag(args, "--note", report.Note)
	return t.run(args...)
}

func (t issuesBugTracker) Note(id, note string) error {
	return t.run("note", id, note)
}

func (t issuesBugTracker) List(all bool) error {
	args := []string{"list", "--component", t.component}
	if all {
		args = append(args, "--all")
	}
	return t.run(args...)
}

func (t issuesBugTracker) Show(id string) error { return t.run("show", id) }

func (t issuesBugTracker) Close(id, reason string) error {
	args := []string{"close", id}
	args = appendIssueFlag(args, "--reason", reason)
	return t.run(args...)
}

func (t issuesBugTracker) Reopen(id string) error { return t.run("reopen", id) }

// issueRef folds weft's job id and host into the ledger's single ref field,
// keeping the job id in the wj#### form it is cited as everywhere else.
func issueRef(jobID *int64, host string) string {
	var parts []string
	if jobID != nil {
		parts = append(parts, ids.FormatJobID(*jobID))
	}
	if host = strings.TrimSpace(host); host != "" {
		parts = append(parts, host)
	}
	return strings.Join(parts, " on ")
}

func appendIssueFlag(args []string, flag, value string) []string {
	if strings.TrimSpace(value) == "" {
		return args
	}
	return append(args, flag, value)
}
