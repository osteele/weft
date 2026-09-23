package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

// humanizeFailureReason must collapse to a single line. failure_reason is a
// classification column (see specs/job-lifecycle.allium § FailureReason) and
// is rendered on single-line UI rows — embedded newlines from a buggy or
// legacy writer would corrupt the surrounding layout (TUI footer, `Reason:`
// in weft info / weft status).
func TestHumanizeFailureReason_StripsMultiline(t *testing.T) {
	multiline := "hf prewarm failed exit 124: setup command timed out after 1h0m0s\nprewarm log tail:\nlots of multi-line script output here\n"
	got := humanizeFailureReason(multiline)
	if strings.Contains(got, "\n") {
		t.Errorf("humanized output must be single-line; got %q", got)
	}
	if got == "" {
		t.Error("humanized output should not be empty for a non-empty reason")
	}
}

func TestHumanizeFailureReason_KnownReasons(t *testing.T) {
	tests := map[string]string{
		"":                    "",
		"gpu_oom":             "GPU out of memory",
		"oom":                 "host out of memory",
		"disk_full":           "disk full",
		"timeout":             "timed out",
		"cuda_driver_too_old": "cuda driver too old",
		db.FailureReasonInfraTorchPreflightFailed:       "CUDA probe failed before user code started",
		db.FailureReasonTorchPreflightEnvironmentFailed: "torch preflight could not start its Python environment",
		db.FailureReasonTorchPreflightImportFailed:      "torch preflight could not import torch",
	}
	for input, want := range tests {
		if got := humanizeFailureReason(input); got != want {
			t.Errorf("humanizeFailureReason(%q) = %q, want %q", input, got, want)
		}
	}
}

// A watchdog kill reason must say what the watchdog actually measured and
// how to override it: a GPU-idle kill that omits the log-activity half reads
// as "the GPU was idle", and operators then add progress prints the log
// already has, or change GPU work when output was what stopped.
func TestHumanizeFailureReason_WatchdogKillsNameSignalAndOverride(t *testing.T) {
	cases := []struct {
		reason, flag string
		gpu          bool
	}{
		{"killed_gpu_idle", "--gpu-idle-timeout", true},
		{"killed_stdout_silence", "--stdout-silence-timeout", false},
	}
	for _, c := range cases {
		got := humanizeFailureReason(c.reason)
		if !strings.Contains(got, "log growth") || !strings.Contains(got, "output-directory writes") {
			t.Errorf("%s: %q does not name the log/output activity signal", c.reason, got)
		}
		if !strings.Contains(got, c.flag) {
			t.Errorf("%s: %q does not name its override %s", c.reason, got, c.flag)
		}
		if strings.Contains(got, "GPU") != c.gpu {
			t.Errorf("%s: %q mentions GPU = %v, want %v", c.reason, got, !c.gpu, c.gpu)
		}
	}
}

func TestHumanizeFailureReason_SetupTimeoutNamesInfra(t *testing.T) {
	got := humanizeFailureReason("setup_timeout")
	if !strings.Contains(strings.ToLower(got), "retry") {
		t.Errorf("setup_timeout label should mention retry behavior; got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("setup_timeout label must be single-line; got %q", got)
	}
}

func TestHumanizeFailureReason_DistinguishesSetupAndRunTimeout(t *testing.T) {
	// FR2: setup-phase and run-phase timeouts must read differently and both
	// name the budget they exceeded.
	setup := humanizeFailureReason("setup_timeout")
	run := humanizeFailureReason("run_timeout")
	if setup == run || run == "run timeout" {
		t.Fatalf("run_timeout should have distinct, descriptive text; got setup=%q run=%q", setup, run)
	}
	if !strings.Contains(strings.ToLower(setup), "setup") || !strings.Contains(strings.ToLower(setup), "budget") {
		t.Errorf("setup_timeout text should name the setup budget; got %q", setup)
	}
	if !strings.Contains(strings.ToLower(run), "run") || !strings.Contains(strings.ToLower(run), "budget") {
		t.Errorf("run_timeout text should name the run budget; got %q", run)
	}
	wall := humanizeFailureReason("wall_timeout")
	if wall == setup || wall == run || !strings.Contains(strings.ToLower(wall), "wall-time") || !strings.Contains(strings.ToLower(wall), "setup") {
		t.Errorf("wall_timeout text should name the whole-job budget including setup; got %q", wall)
	}
}
