package cmd

import (
	"strings"
	"testing"
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
		"":                      "",
		"gpu_oom":               "GPU out of memory",
		"oom":                   "host out of memory",
		"disk_full":             "disk full",
		"timeout":               "timed out",
		"killed_stdout_silence": "killed: no stdout output for the silence-watchdog timeout",
		"killed_gpu_idle":       "killed: GPU idle for the GPU-watchdog timeout",
		"cuda_driver_too_old":   "cuda driver too old",
	}
	for input, want := range tests {
		if got := humanizeFailureReason(input); got != want {
			t.Errorf("humanizeFailureReason(%q) = %q, want %q", input, got, want)
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
