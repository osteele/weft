package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func captureAgentOutput(t *testing.T, target **os.File, fn func()) string {
	t.Helper()

	old := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	*target = w
	defer func() { *target = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func captureAgentStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureAgentOutput(t, &os.Stdout, fn)
}

func captureAgentStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureAgentOutput(t, &os.Stderr, fn)
}

func TestRunAgentSubcommandUnknownArgumentIsUsageError(t *testing.T) {
	var code int
	out := captureAgentStderr(t, func() {
		code = runAgentSubcommand("vresion", nil)
	})
	if code != 2 {
		t.Errorf("unknown subcommand exit code = %d, want 2", code)
	}
	if !strings.Contains(out, "vresion") {
		t.Errorf("usage error does not name the rejected argument, got:\n%s", out)
	}
	// Every dispatchable subcommand must appear in the usage text: one
	// missing here is a hard failure on whatever host or manifest uses it.
	for _, sub := range []string{
		"version", "--version", "run-queue", "run-job", "run-instance",
		"run-campaign", "grace-wait", "r2", "heartbeat-sidecar", "batch-status",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("usage text missing valid subcommand %q, got:\n%s", sub, out)
		}
	}
}

func TestBareInvocationIsUsageError(t *testing.T) {
	var code int
	out := captureAgentStderr(t, func() {
		code = runAgentArgs([]string{"weft-agent"})
	})
	if code != 2 {
		t.Errorf("bare invocation exit code = %d, want 2", code)
	}
	if !strings.Contains(out, "run-queue") {
		t.Errorf("bare invocation did not print usage, got:\n%s", out)
	}
}

func TestVersionSynonymPrintsSameStringAsDashed(t *testing.T) {
	var dashedCode, bareCode int
	dashed := captureAgentStdout(t, func() {
		dashedCode = runAgentSubcommand("--version", nil)
	})
	bare := captureAgentStdout(t, func() {
		bareCode = runAgentSubcommand("version", nil)
	})
	if dashedCode != 0 || bareCode != 0 {
		t.Errorf("version exit codes = (%d, %d), want (0, 0)", dashedCode, bareCode)
	}
	if bare == "" || bare != dashed {
		t.Errorf("bare `version` output %q differs from `--version` output %q", bare, dashed)
	}
	if !strings.HasPrefix(bare, "weft-agent ") {
		t.Errorf("version output %q does not match \"weft-agent <version>\"", bare)
	}
}
