package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

// walkRunnableCommands visits every runnable command below root (help and
// completion included, matching what the gate resolves against).
func walkRunnableCommands(t *testing.T, visit func(c *cobra.Command)) {
	t.Helper()
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != rootCmd && c.Runnable() {
			visit(c)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// The classification table is the policy; the tree is the truth. A command
// added without a table entry fails here rather than falling through to
// whatever the gate's default would be on an edge.
func TestEdgeModeTableCoversEveryRunnableCommand(t *testing.T) {
	var missing []string
	walkRunnableCommands(t, func(c *cobra.Command) {
		path := edgeCommandPath(c)
		if _, ok := edgeCommandModes[path]; !ok {
			missing = append(missing, path)
		}
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("commands missing from edgeCommandModes:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestEdgeModeTableHasNoStaleEntries(t *testing.T) {
	var stale []string
	for path := range edgeCommandModes {
		resolved, _, err := rootCmd.Find(strings.Fields(path))
		if err != nil || resolved == nil || !resolved.Runnable() || edgeCommandPath(resolved) != path {
			stale = append(stale, path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("edgeCommandModes entries that resolve to no runnable command:\n  %s", strings.Join(stale, "\n  "))
	}
}

func TestEdgeModeDisabledEntriesHaveReasons(t *testing.T) {
	for path, entry := range edgeCommandModes {
		if entry.mode == edgeModeDisabled && strings.TrimSpace(entry.reason) == "" {
			t.Errorf("%s: disabled without a reason", path)
		}
		if entry.mode != edgeModeDisabled && entry.reason != "" {
			t.Errorf("%s: %s entry carries a reason, which is only printed for disabled", path, entry.mode)
		}
	}
}

// findCmd resolves a table path the way the gate does.
func findCmd(t *testing.T, path string) *cobra.Command {
	t.Helper()
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	resolved, _, err := rootCmd.Find(strings.Fields(path))
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return resolved
}

func edgeConfig(role string) *config.Config {
	cfg := &config.Config{}
	cfg.Edge.Role = role
	return cfg
}

// captureGate runs the gate with the command's output streams redirected.
func captureGate(t *testing.T, cfg *config.Config, args []string) (stdout, stderr string, err error) {
	t.Helper()
	// The gate's ledger refusal is process-lifetime; reset it afterwards so
	// it cannot leak into other tests in this package.
	resetRefusal := db.RefuseLocalLedger("edge-gate-test")
	defer resetRefusal()
	resolved := findCmd(t, strings.TrimSpace(strings.Join(args, " ")))
	var outBuf, errBuf bytes.Buffer
	resolved.SetOut(&outBuf)
	resolved.SetErr(&errBuf)
	defer func() {
		resolved.SetOut(nil)
		resolved.SetErr(nil)
	}()
	err = applyEdgeGate(cfg, args)
	return outBuf.String(), errBuf.String(), err
}

func TestEdgeGateDisabledCommand(t *testing.T) {
	stdout, stderr, err := captureGate(t, edgeConfig("edge"), []string{"daemon", "run"})
	if err == nil {
		t.Fatal("expected the gate to refuse daemon run on an edge")
	}
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error type = %T, want *EdgeGateError", err)
	}
	if gateErr.ExitCode != edgeExitDisabled {
		t.Errorf("exit code = %d, want %d", gateErr.ExitCode, edgeExitDisabled)
	}
	want := "disabled on an edge: the daemon runs on the hub\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestEdgeGateMirrorCommandBlocked(t *testing.T) {
	_, stderr, err := captureGate(t, edgeConfig("edge"), []string{"job", "list"})
	assertBlocked(t, err, stderr, edgeCauseNoViewReader)
}

func TestEdgeGateSubmitCommandBlocked(t *testing.T) {
	_, stderr, err := captureGate(t, edgeConfig("edge"), []string{"run", "echo hi"})
	assertBlocked(t, err, stderr, edgeCauseNoSubmitPath)
}

func assertBlocked(t *testing.T, err error, stderr, cause string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the gate to block the command on an edge")
	}
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error type = %T, want *EdgeGateError", err)
	}
	if gateErr.ExitCode != edgeExitBlocked {
		t.Errorf("exit code = %d, want %d", gateErr.ExitCode, edgeExitBlocked)
	}
	want := fmt.Sprintf("hub not reachable from this edge: %s\n", cause)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestEdgeGateLocalCommandRuns(t *testing.T) {
	stdout, stderr, err := captureGate(t, edgeConfig("edge"), []string{"version"})
	if err != nil {
		t.Fatalf("local command refused: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("gate wrote output for a local command: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestEdgeGateJSONOnDisabled(t *testing.T) {
	// daemon status declares --json.
	stdout, stderr, err := captureGate(t, edgeConfig("edge"), []string{"daemon", "status", "--json"})
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error type = %T, want *EdgeGateError", err)
	}
	want := `{"edge":{"outcome":"disabled","reason":"the daemon runs on the hub"}}` + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty in JSON mode", stderr)
	}
}

func TestEdgeGateJSONOnBlocked(t *testing.T) {
	// autopilot status declares --json and is a mirror command.
	stdout, _, err := captureGate(t, edgeConfig("edge"), []string{"autopilot", "status", "--json"})
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error type = %T, want *EdgeGateError", err)
	}
	want := `{"edge":{"outcome":"blocked","cause":"` + edgeCauseNoViewReader + `"}}` + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestEdgeGateJSONNotDeclaredFallsBackToText(t *testing.T) {
	// daemon run has no --json flag, so --json must not switch the output.
	stdout, stderr, err := captureGate(t, edgeConfig("edge"), []string{"daemon", "run", "--json"})
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("error type = %T, want *EdgeGateError", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty when --json is not declared", stdout)
	}
	if !strings.HasPrefix(stderr, "disabled on an edge: ") {
		t.Errorf("stderr = %q, want the text refusal", stderr)
	}
}

func TestEdgeGateHelpStillWorks(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"daemon", "run", "--help"},
		{"help", "daemon"},
	} {
		if _, _, err := captureGate(t, edgeConfig("edge"), args); err != nil {
			t.Errorf("help invocation %v refused: %v", args, err)
		}
	}
}

func TestEdgeGateNoOpOnHub(t *testing.T) {
	for _, role := range []string{"hub", ""} {
		stdout, stderr, err := captureGate(t, edgeConfig(role), []string{"daemon", "run"})
		if err != nil {
			t.Errorf("role %q: hub-role run of a disabled command refused: %v", role, err)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("role %q: gate wrote output on a hub: stdout=%q stderr=%q", role, stdout, stderr)
		}
	}
}

func TestEdgeHelpAnnotatesDisabledCommands(t *testing.T) {
	edgeHelpAnnotated = false
	defer func() { edgeHelpAnnotated = false }()
	annotateEdgeHelp()
	defer unannotateEdgeHelp()

	walkRunnableCommands(t, func(c *cobra.Command) {
		entry, ok := edgeCommandModes[edgeCommandPath(c)]
		if !ok {
			return
		}
		annotated := strings.HasPrefix(c.Short, "[disabled on an edge] ")
		if entry.mode == edgeModeDisabled && !annotated {
			t.Errorf("%s: disabled command not annotated in help", edgeCommandPath(c))
		}
		if entry.mode != edgeModeDisabled && annotated {
			t.Errorf("%s: %s command wrongly annotated as disabled", edgeCommandPath(c), entry.mode)
		}
	})
}

// unannotateEdgeHelp restores Short strings mutated by annotateEdgeHelp so
// tests do not leak display state into each other.
func unannotateEdgeHelp() {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Short = strings.TrimPrefix(c.Short, "[disabled on an edge] ")
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}
