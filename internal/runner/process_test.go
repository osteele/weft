package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// assertEnvVar checks that result contains exactly one entry for key with the given value.
func assertEnvVar(t *testing.T, result []string, key, wantVal string) {
	t.Helper()
	prefix := key + "="
	count := 0
	var gotVal string
	for _, ev := range result {
		if strings.HasPrefix(ev, prefix) {
			count++
			gotVal = strings.TrimPrefix(ev, prefix)
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 %s, got %d", key, count)
	}
	if gotVal != wantVal {
		t.Errorf("expected %s=%s, got %s=%s", key, wantVal, key, gotVal)
	}
}

func TestMergeEnvVars_LastWriterWins(t *testing.T) {
	base := []string{
		"HOME=/home/user",
		"PATH=/usr/bin",
		"CUDA_VISIBLE_DEVICES=0,1,2,3,4,5,6,7,8,9",
	}
	overlay := []string{
		"MY_VAR=hello",
		"CUDA_VISIBLE_DEVICES=0",
	}

	result := mergeEnvVars(base, overlay)

	assertEnvVar(t, result, "CUDA_VISIBLE_DEVICES", "0")
	assertEnvVar(t, result, "HOME", "/home/user")
	assertEnvVar(t, result, "PATH", "/usr/bin")
	assertEnvVar(t, result, "MY_VAR", "hello")
}

func TestMergeEnvVars_OverlayDuplicates(t *testing.T) {
	// When overlay itself has duplicates, last one wins
	base := []string{"A=1"}
	overlay := []string{
		"CUDA_VISIBLE_DEVICES=7", // from dotenv
		"B=2",
		"CUDA_VISIBLE_DEVICES=0", // from GPU class resolution
	}

	result := mergeEnvVars(base, overlay)

	assertEnvVar(t, result, "CUDA_VISIBLE_DEVICES", "0")
}

func TestMergeEnvVars_NoOverlap(t *testing.T) {
	base := []string{"A=1", "B=2"}
	overlay := []string{"C=3", "D=4"}

	result := mergeEnvVars(base, overlay)
	if len(result) != 4 {
		t.Errorf("expected 4 vars, got %d: %v", len(result), result)
	}
}

func TestMergeEnvVars_EmptyOverlay(t *testing.T) {
	base := []string{"A=1", "B=2"}
	result := mergeEnvVars(base, nil)
	if len(result) != 2 {
		t.Errorf("expected 2 vars, got %d", len(result))
	}
}

func TestWrapCommandWithExitCapture(t *testing.T) {
	wrapped := WrapCommandWithExitCapture("python train.py", "/tmp/test.status")
	// Must contain the original command
	if !strings.Contains(wrapped, "python train.py; EXIT_CODE=$?") {
		t.Errorf("wrapped command should embed the original command followed by EXIT_CODE=$?, got: %s", wrapped)
	}
	// Must capture exit code
	if !strings.Contains(wrapped, "EXIT_CODE=$?") {
		t.Errorf("wrapped command should capture EXIT_CODE, got: %s", wrapped)
	}
	// Must write footer to stdout (which Go redirects to log file)
	if !strings.Contains(wrapped, `echo "=== END exit=$EXIT_CODE`) {
		t.Errorf("wrapped command should write log footer, got: %s", wrapped)
	}
	// Must write status file (variable-quoted form)
	if !strings.Contains(wrapped, `echo "$EXIT_CODE" > "$_weft_status_file"`) {
		t.Errorf("wrapped command should write status file, got: %s", wrapped)
	}
	// Must install SIGTERM and SIGINT traps so kill flows produce a status file.
	if !strings.Contains(wrapped, `trap '_weft_on_signal 143 SIGTERM' TERM`) {
		t.Errorf("wrapped command should install SIGTERM trap, got: %s", wrapped)
	}
	if !strings.Contains(wrapped, `trap '_weft_on_signal 130 SIGINT' INT`) {
		t.Errorf("wrapped command should install SIGINT trap, got: %s", wrapped)
	}
	// Must propagate exit code
	if !strings.HasSuffix(wrapped, `exit "$EXIT_CODE"`) {
		t.Errorf("wrapped command should end with exit propagation, got: %s", wrapped)
	}
}

// TestWrapCommandWithExitCapture_SigtermWritesStatus runs the wrapper as a
// real bash subprocess, sends SIGTERM, and asserts that the status file gets
// written before the wrapper exits. This guards against regressing the
// SIGTERM trap that prevents the zombie-without-status state #1 was filed for.
func TestWrapCommandWithExitCapture_SigtermWritesStatus(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "test.status")

	// Long-running command so the wrapper is alive when we SIGTERM it.
	wrapped := WrapCommandWithExitCapture("sleep 30", statusFile)

	cmd := exec.Command("bash", "-c", wrapped)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	t.Cleanup(func() {
		// Ensure no leftover child if the test fails midway.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	// Give bash a moment to install the trap and enter sleep.
	time.Sleep(200 * time.Millisecond)

	// Send SIGTERM to the process group — that's how the killer reaches both
	// the wrapper bash and the foreground sleep child.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill -TERM: %v", err)
	}

	// Wrapper should exit shortly; wait with a generous deadline.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapper did not exit within 5s after SIGTERM")
	}

	// Status file must exist and contain a kill-style exit code.
	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("status file should have been written by the SIGTERM trap: %v", err)
	}
	got := strings.TrimSpace(string(data))
	// SIGTERM-killed sleep exits 143 (128 + SIGTERM); SIGINT would give 130.
	// The trap doesn't override the user command's exit code, so this should
	// reliably be 143 — the foreground sleep got SIGTERM via the pgroup.
	if got != "143" && got != "130" {
		t.Fatalf("status file should record signal exit (143 or 130), got %q", got)
	}
}

// TestWrapCommandWithExitCapture_UserCatchesSigtermExitsZero confirms the
// SIGTERM trap does NOT override the user's intentional clean exit. The
// realistic scenario: user runs a child process (Python, training script,
// etc.) that installs its own SIGTERM handler that saves state and exits 0.
// SIGTERM goes to the pgroup, the child catches it and exits 0, bash's
// `wait` returns 0, and the wrapper's trap then fires with $?=0 — it must
// preserve the 0, not rewrite it as 143. Regression guard for a focused-fix
// review finding: the original trap had `if [ "$_e" = 0 ]; then _e=$1; fi`
// which clobbered the user's choice.
func TestWrapCommandWithExitCapture_UserCatchesSigtermExitsZero(t *testing.T) {
	dir := t.TempDir()
	statusFile := filepath.Join(dir, "test.status")

	// User command runs in a child bash that traps SIGTERM and exits 0
	// — modelling a Python script with a signal.signal handler. The
	// outer wrapper's trap stays installed because the user's trap is in
	// a different shell process.
	userCmd := `bash -c 'trap "exit 0" TERM; sleep 30'`
	wrapped := WrapCommandWithExitCapture(userCmd, statusFile)

	cmd := exec.Command("bash", "-c", wrapped)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill -TERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapper did not exit within 5s after SIGTERM")
	}

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("status file should exist: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != "0" {
		t.Fatalf("user caught SIGTERM and exited 0; status should record 0, got %q (trap is overriding the user's clean exit)", got)
	}
}

func TestMergeEnvVars_ExpandsLeadingTildeValues(t *testing.T) {
	base := []string{"HOME=/home/tester"}
	overlay := []string{
		"WEFT_ARTIFACT_MANIFEST=~/.cache/weft/artifacts/12.json",
		"KEEP_LITERAL=~artifact",
		"HOME=/srv/runner",
		"RJ_ARTIFACT_MANIFEST=~/artifacts/12.json",
	}

	result := mergeEnvVars(base, overlay)

	assertEnvVar(t, result, "HOME", "/srv/runner")
	assertEnvVar(t, result, "WEFT_ARTIFACT_MANIFEST", "/srv/runner/.cache/weft/artifacts/12.json")
	assertEnvVar(t, result, "RJ_ARTIFACT_MANIFEST", "/srv/runner/artifacts/12.json")
	assertEnvVar(t, result, "KEEP_LITERAL", "~artifact")
}

// TestKillProcessGroupWithGrace_AllowsSigtermHandlerToRun locks in the
// graceful-escalation contract that user-initiated kills (e.g. the cloud
// agent's kill poller) rely on: the SIGTERM lands immediately, but SIGKILL is
// deferred for the grace window, so a process that traps SIGTERM gets to
// flush state (and the bash exit-capture trap gets to record the exit)
// before being force-killed.
func TestKillProcessGroupWithGrace_AllowsSigtermHandlerToRun(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	marker := filepath.Join(dir, "flushed")
	logFile := filepath.Join(dir, "out.log")

	cmd := "trap 'echo done > " + marker + "; exit 143' TERM; " +
		"echo ready > " + ready + "; sleep 30 & wait"
	proc, err := StartProcess(cmd, dir, nil, logFile)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// Wait for the trap to be installed before signaling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(ready); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process never wrote readiness marker")
		}
		time.Sleep(10 * time.Millisecond)
	}

	paths := NewJobPaths(dir, 990)
	KillProcessGroupWithGrace(proc.PGID, 5*time.Second, paths, "test-reason")

	waited := make(chan error, 1)
	go func() { waited <- proc.Cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(4 * time.Second):
		t.Fatal("process did not exit from its SIGTERM trap within the grace window")
	}

	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("SIGTERM handler never ran (no flush marker): %v", statErr)
	}
	if got := ReadKillReasonFile(paths.KillReason); got != "test-reason" {
		t.Fatalf("kill reason = %q, want %q", got, "test-reason")
	}
}
