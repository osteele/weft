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

// TestOutputFileActivityResetsSilenceTimer: a job that is silent on stdout
// but writes to a file under outputs/ at a steady cadence must NOT be killed
// by the stdout-silence watchdog. This is the wj2131-style case in reverse:
// "I'm producing files, just not chatty on stdout."
func TestOutputFileActivityResetsSilenceTimer(t *testing.T) {
	workDir := t.TempDir()
	logDir := t.TempDir()
	if err := os.MkdirAll(workDir+"/outputs", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := SingleJobConfig{
		JobID: 750,
		Job: opsqueue.CommandJob{
			// One stdout line early (well within grace), then a 3-second run
			// that only updates a file under outputs/ every 250ms. A 1.5s stdout
			// silence threshold would fire ~3x without the keepalive.
			Cmd: "echo started; for i in 1 2 3 4 5 6 7 8 9 10 11 12; do printf \"%s\\n\" \"$i\" >> outputs/keepalive; sleep 0.25; done",
		},
		LogDir:               logDir,
		WorkingDir:           workDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 1500 * time.Millisecond,
		WatchdogInitialGrace: 100 * time.Millisecond,
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (output-file writes should reset silence timer)", ei.ExitCode)
	}
}

// TestSilentJobWithNoOutputFilesIsKilled: the wj2131 failure mode — a job
// that emits one stdout line during setup and then writes neither stdout
// nor any file under outputs/ for the entire silence window — must be
// killed by the watchdog. This is the case that should have fired on
// wi3165 (and didn't), so we lock the invariant in with a regression test.
func TestSilentJobWithNoOutputFilesIsKilled(t *testing.T) {
	workDir := t.TempDir()
	logDir := t.TempDir()
	if err := os.MkdirAll(workDir+"/outputs", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	start := time.Now()
	cfg := SingleJobConfig{
		JobID: 751,
		Job: opsqueue.CommandJob{
			// Burn CPU silently with no stdout and no output file writes —
			// mimics a CPU-bound deadlock where the GPU/CPU stays busy but
			// the job produces nothing observable.
			Cmd: "echo starting; while :; do :; done",
		},
		LogDir:               logDir,
		WorkingDir:           workDir,
		SampleInterval:       50 * time.Millisecond,
		StdoutSilenceTimeout: 2 * time.Second,
		WatchdogInitialGrace: 100 * time.Millisecond,
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("watchdog did not fire promptly for silent no-output job; elapsed=%s", time.Since(start))
	}
	if ei.ExitCode != ExitCodeStdoutSilenceKill {
		t.Errorf("exit code = %d, want %d (stdout-silence)", ei.ExitCode, ExitCodeStdoutSilenceKill)
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

// TestGPUIdleNotArmedWithoutExplicitRequest: a job that does not declare any
// GPU intent (no GPUClass, no GPU, no GPUMem) must not arm the GPU idle
// watchdog, even when GPUIdleTimeout is set and the probe reports idle. This
// protects compute-only jobs running on GPU rentals from being killed as
// gpu-idle; the stdout-silence watchdog remains the fallback stall signal.
func TestGPUIdleNotArmedWithoutExplicitRequest(t *testing.T) {
	logDir := t.TempDir()

	cfg := SingleJobConfig{
		JobID:                707,
		Job:                  opsqueue.CommandJob{Cmd: "for i in 1 2 3 4; do echo tick$i; sleep 0.2; done"},
		LogDir:               logDir,
		SampleInterval:       50 * time.Millisecond,
		GPUIdleTimeout:       500 * time.Millisecond, // would fire well within the job's runtime
		StdoutSilenceTimeout: 0,
		WatchdogInitialGrace: 50 * time.Millisecond,
		GPUActiveProbe:       func() bool { return false }, // always idle
		SkipProbes:           true,
	}

	ei, err := RunSingleJob(cfg)
	if err != nil {
		t.Fatalf("RunSingleJob: %v", err)
	}
	if ei.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (GPU watchdog must not arm without explicit GPU intent)", ei.ExitCode)
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

// TestCleanExitNotMislabeledAsSilenceKill: a job that exits 0 after the
// silence watchdog arms must be reported as a clean exit, not relabeled as a
// silence-kill. Regression test for the race where a watchdog
// tick landing just after the process exited could still set its fired flag;
// the fix records process-exit state under the same lock the watchdog uses
// before firing. Several iterations to give the (former) race a chance to
// manifest.
func TestCleanExitNotMislabeledAsSilenceKill(t *testing.T) {
	for i := 0; i < 5; i++ {
		logDir := t.TempDir()
		cfg := SingleJobConfig{
			JobID: 760,
			// Exits cleanly with no output after the watchdog arms.
			Job:                  opsqueue.CommandJob{Cmd: "sleep 0.2"},
			LogDir:               logDir,
			SampleInterval:       50 * time.Millisecond,
			StdoutSilenceTimeout: time.Second,
			WatchdogInitialGrace: 10 * time.Millisecond,
			SkipProbes:           true,
		}

		ei, err := RunSingleJob(cfg)
		if err != nil {
			t.Fatalf("iteration %d: RunSingleJob: %v", i, err)
		}
		if ei.ExitCode != 0 {
			t.Fatalf("iteration %d: exit code = %d, want 0 (clean exit mislabeled as watchdog kill)", i, ei.ExitCode)
		}
		paths := NewJobPaths(logDir, 760)
		if _, statErr := os.Stat(paths.KillReason); statErr == nil {
			t.Fatalf("iteration %d: kill-reason file written for a clean exit", i)
		}
	}
}
