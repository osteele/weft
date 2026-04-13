package runner

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

// TestStdoutSilenceKillsHangingJob reproduces the wj1134 hang: a job prints
// "Loading data..." once then sleeps forever. With a short silence timeout
// the watchdog should kill it and report exit code 126.
func TestStdoutSilenceKillsHangingJob(t *testing.T) {
	logDir := t.TempDir()

	start := time.Now()
	cfg := SingleJobConfig{
		JobID:                701,
		Job:                  opsqueue.CommandJob{Cmd: "echo 'Loading data...'; sleep 300"},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 2 * time.Second,
		WatchdogInitialGrace: 100 * time.Millisecond,
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 30*time.Second {
		t.Fatalf("watchdog did not fire promptly; elapsed=%s", elapsed)
	}
	if ei.ExitCode != ExitCodeStdoutSilenceKill {
		t.Errorf("exit code = %d, want %d (stdout-silence)", ei.ExitCode, ExitCodeStdoutSilenceKill)
	}

	paths := NewJobPaths(logDir, 701)
	reason, _ := os.ReadFile(paths.KillReason)
	if got := strings.TrimSpace(string(reason)); got != KillReasonStdoutSilence {
		t.Errorf("kill reason = %q, want %q", got, KillReasonStdoutSilence)
	}

	if got := DetectFailureReasonFromExitInfo(ei); got != FailureReasonStdoutSilence {
		t.Errorf("failure reason = %q, want %q", got, FailureReasonStdoutSilence)
	}
}

// TestGPUIdleKillsHangingGPUJob uses a fake GPU probe that always reports
// idle. The job prints every 100ms (so silence doesn't fire) and sleeps.
// Expect exit 125 with reason gpu-idle.
func TestGPUIdleKillsHangingGPUJob(t *testing.T) {
	logDir := t.TempDir()

	start := time.Now()
	cfg := SingleJobConfig{
		JobID: 702,
		Job: opsqueue.CommandJob{
			Cmd:      "while true; do echo tick; sleep 0.1; done",
			GPUClass: "test-gpu", // arms the GPU watchdog
		},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		GPUIdleTimeout:       2 * time.Second,
		StdoutSilenceTimeout: 0, // disable silence watchdog
		WatchdogInitialGrace: 100 * time.Millisecond,
		GPUActiveProbe:       func() bool { return false },
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("watchdog did not fire promptly")
	}
	if ei.ExitCode != ExitCodeGPUIdleKill {
		t.Errorf("exit code = %d, want %d (gpu-idle)", ei.ExitCode, ExitCodeGPUIdleKill)
	}

	paths := NewJobPaths(logDir, 702)
	reason, _ := os.ReadFile(paths.KillReason)
	if got := strings.TrimSpace(string(reason)); got != KillReasonGPUIdle {
		t.Errorf("kill reason = %q, want %q", got, KillReasonGPUIdle)
	}

	if got := DetectFailureReasonFromExitInfo(ei); got != FailureReasonGPUIdle {
		t.Errorf("failure reason = %q, want %q", got, FailureReasonGPUIdle)
	}
}

// TestStdoutActivityResetsSilenceTimer: a job that prints every 300ms for
// 2 seconds then exits must not be killed by a 1s silence timeout.
func TestStdoutActivityResetsSilenceTimer(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:                703,
		Job:                  opsqueue.CommandJob{Cmd: "for i in 1 2 3 4 5 6; do echo tick$i; sleep 0.3; done"},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 1 * time.Second,
		WatchdogInitialGrace: 50 * time.Millisecond,
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (job should complete normally)", ei.ExitCode)
	}
}

// TestGPUActivityResetsIdleTimer: a probe that flips true every few ticks
// should keep the GPU watchdog armed without killing.
func TestGPUActivityResetsIdleTimer(t *testing.T) {
	logDir := t.TempDir()

	var calls atomic.Int32
	cfg := SingleJobConfig{
		JobID: 704,
		Job: opsqueue.CommandJob{
			Cmd:      "sleep 2",
			GPUClass: "test-gpu",
		},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		GPUIdleTimeout:       1 * time.Second,
		StdoutSilenceTimeout: 0,
		WatchdogInitialGrace: 50 * time.Millisecond,
		// Alternating: returns true on odd calls, false on even
		GPUActiveProbe: func() bool { return calls.Add(1)%2 == 1 },
		SkipProbes:     true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", ei.ExitCode)
	}
}

// TestSilenceTimerRespectsInitialGrace: a job that stays silent for longer
// than the silence timeout but less than the initial grace should NOT be
// killed. The grace window lets a quiet setup phase finish.
func TestSilenceTimerRespectsInitialGrace(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:                706,
		Job:                  opsqueue.CommandJob{Cmd: "sleep 1; echo done"},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 500 * time.Millisecond,
		WatchdogInitialGrace: 2 * time.Second, // longer than the silent sleep
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (grace window should protect initial silence)", ei.ExitCode)
	}
}

// TestWatchdogDisabledWhenTimeoutZero: zero timeout means the watchdog does
// not fire even if the job would otherwise trigger it.
func TestWatchdogDisabledWhenTimeoutZero(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:                705,
		Job:                  opsqueue.CommandJob{Cmd: "echo hi; sleep 1"},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 0, // disabled
		GPUIdleTimeout:       0, // disabled
		WatchdogInitialGrace: 50 * time.Millisecond,
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (watchdogs disabled)", ei.ExitCode)
	}
}
