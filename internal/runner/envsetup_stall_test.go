package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchSetupStallFiresOnQuietLog guards wb74: a setup command whose log
// stops growing is bounded by the stall window, not by the wall-clock budget.
func TestWatchSetupStallFiresOnQuietLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "job.log")
	writeFile(t, logPath, "Downloading file from Xet Storage..\n")
	fired := make(chan struct{})
	stop := watchSetupStall(logPath, 20*time.Millisecond, func() { close(fired) })
	defer stop()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("stall watch did not fire on a log that stopped growing")
	}
}

// A log that keeps growing is a live transfer, however slow, and must not be
// killed.
func TestWatchSetupStallToleratesSlowProgress(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "job.log")
	writeFile(t, logPath, "start\n")
	fired := make(chan struct{})
	stop := watchSetupStall(logPath, 300*time.Millisecond, func() { close(fired) })
	defer stop()

	deadline := time.After(900 * time.Millisecond)
	for {
		select {
		case <-fired:
			t.Fatal("stall watch fired while the log was still growing")
		case <-deadline:
			return
		case <-time.After(50 * time.Millisecond):
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("progress\n")
			_ = f.Close()
		}
	}
}

// An unreadable log is unknown evidence, not a stall: killing a live process
// on a failed stat would be worse than waiting out the wall-clock timeout.
func TestWatchSetupStallIgnoresUnstattableLog(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created.log")
	fired := make(chan struct{})
	stop := watchSetupStall(missing, 20*time.Millisecond, func() { close(fired) })
	defer stop()

	select {
	case <-fired:
		t.Fatal("stall watch fired on a log it could not stat")
	case <-time.After(300 * time.Millisecond):
	}
}

// A quiet command is killed at the stall window and reports ErrSetupStalled,
// so callers can retry it differently from a wall-clock expiry. Both surface
// exit code 124.
func TestRunSetupCommandWithStallTimeout_QuietCommandStalls(t *testing.T) {
	dir := t.TempDir()
	paths := NewJobPaths(t.TempDir(), 777)

	start := time.Now()
	ei, err := RunSetupCommandWithStallTimeout("sleep 300", 777, dir, nil, paths, 5*time.Minute, 200*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSetupStalled) {
		t.Fatalf("error = %v, want ErrSetupStalled", err)
	}
	if ei.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124 (timeout convention)", ei.ExitCode)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("stalled setup should be killed at the stall window, took %s", elapsed)
	}
}

// A wall-clock expiry must not be reported as a stall, even with the stall
// watch armed: the command was producing output right up to the deadline.
func TestRunSetupCommandWithStallTimeout_DeadlineExpiryIsNotAStall(t *testing.T) {
	dir := t.TempDir()
	paths := NewJobPaths(t.TempDir(), 778)

	ei, err := RunSetupCommandWithStallTimeout("while true; do echo working; sleep 0.05; done", 778, dir, nil, paths, time.Second, 10*time.Second)

	if err == nil {
		t.Fatal("expected an error from the timed-out setup command")
	}
	if errors.Is(err, ErrSetupStalled) {
		t.Fatalf("a chatty command that hit its deadline must not report a stall: %v", err)
	}
	if ei.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124 (timeout convention)", ei.ExitCode)
	}
}
