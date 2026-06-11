package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	srcsync "github.com/osteele/weft/internal/sync"
)

func gpuMemPtr(n int) *int { return &n }

func initTestRunner(t *testing.T) (*Runner, string) {
	t.Helper()

	baseDir := t.TempDir()
	queueDir := filepath.Join(baseDir, "queue")
	logDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	r := New(Config{
		QueueDir: queueDir,
		LogDir:   logDir,
	})
	r.state = NewState()
	r.cpuConfig = DefaultCPUConfig()
	r.cpuCount = 8

	oplogPath := filepath.Join(baseDir, "operations.log")
	if err := oplog.Init(oplogPath, 0); err != nil {
		t.Fatalf("init oplog: %v", err)
	}
	t.Cleanup(func() {
		if err := oplog.Close(); err != nil {
			t.Fatalf("close oplog: %v", err)
		}
	})

	return r, oplogPath
}

func writeFakeNvidiaSmi(t *testing.T, dir, output string) string {
	t.Helper()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir fake bin dir: %v", err)
	}

	path := filepath.Join(binDir, "nvidia-smi")
	script := "#!/bin/sh\ncat <<'EOF'\n" + output + "EOF\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake nvidia-smi: %v", err)
	}
	return binDir
}

func assertJobNotStarted(t *testing.T, logDir string, jobID int64) {
	t.Helper()

	paths := NewJobPaths(logDir, jobID)
	for _, path := range []string{paths.Log, paths.Meta, paths.Status, paths.PID, paths.PGID} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be absent, got err=%v", path, err)
		}
	}
}

func TestTryStartNextJob_GPUClassBlockedByExternalVRAM_RequeuesWithoutStarting(t *testing.T) {
	r, oplogPath := initTestRunner(t)

	fakeBinDir := writeFakeNvidiaSmi(t, filepath.Dir(oplogPath), "0, 28672, 81920\n1, 50176, 81920\n")
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r.gpuInv = &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
		hasNvidiaSmi: true,
	}

	jobID := int64(101)
	job := &opsqueue.CommandJob{
		ID:       jobID,
		Dir:      t.TempDir(),
		Cmd:      "echo should-not-run",
		GPUClass: "a100",
		GPUMem:   gpuMemPtr(60),
	}
	if err := writeJobFile(r.queueDir, job); err != nil {
		t.Fatalf("write job file: %v", err)
	}
	r.state.AddPending(jobID)

	r.tryStartNextJob()

	if len(r.state.Pending) != 1 || r.state.Pending[0] != jobID {
		t.Fatalf("pending = %v, want [%d]", r.state.Pending, jobID)
	}
	if got := r.state.RunningCount(); got != 0 {
		t.Fatalf("running count = %d, want 0", got)
	}

	assertJobNotStarted(t, r.logDir, jobID)

	entries, err := oplog.ReadEntries(oplogPath)
	if err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no oplog entries, got %v", entries)
	}
}

func TestTryStartNextJob_WaitsForPostJobWorkdirBeforeStartGates(t *testing.T) {
	r, oplogPath := initTestRunner(t)

	fakeBinDir := writeFakeNvidiaSmi(t, filepath.Dir(oplogPath), "0, 28672, 81920\n")
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.gpuInv = &GPUInventory{
		Devices:      []GPUInfo{{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80}},
		hasNvidiaSmi: true,
	}

	workDir := t.TempDir()
	manager := &recordingPostJobManager{}
	r.PostJobManager = manager

	jobID := int64(102)
	job := &opsqueue.CommandJob{
		ID:       jobID,
		Dir:      workDir,
		Cmd:      "echo should-not-run",
		GPUClass: "a100",
		GPUMem:   gpuMemPtr(60),
	}
	if err := writeJobFile(r.queueDir, job); err != nil {
		t.Fatalf("write job file: %v", err)
	}
	r.state.AddPending(jobID)

	r.tryStartNextJob()

	if manager.waitedWorkdir != workDir {
		t.Fatalf("waited workdir = %q, want %q", manager.waitedWorkdir, workDir)
	}
	assertJobNotStarted(t, r.logDir, jobID)
}

type recordingPostJobManager struct {
	waitedWorkdir string
	captures      []PostJobCapture
}

func (m *recordingPostJobManager) WaitForWorkdir(workdir string) {
	m.waitedWorkdir = workdir
}

func (m *recordingPostJobManager) StartPostJob(capture PostJobCapture) {
	m.captures = append(m.captures, capture)
}

func TestTryStartNextJob_BenchmarkWarmupRecordsPendingReason(t *testing.T) {
	r, _ := initTestRunner(t)

	r.state.AddRunning("900", RunningJobState{
		StartedAt:   time.Now().Unix(),
		WarmupUntil: time.Now().Unix() + 60,
	})

	jobID := int64(123)
	job := &opsqueue.CommandJob{
		ID:   jobID,
		Dir:  t.TempDir(),
		Cmd:  "echo benchmark",
		Tags: []string{"benchmark-isolation"},
	}
	if err := writeJobFile(r.queueDir, job); err != nil {
		t.Fatalf("write job file: %v", err)
	}
	r.state.AddPending(jobID)

	r.tryStartNextJob()

	if len(r.state.Pending) != 1 || r.state.Pending[0] != jobID {
		t.Fatalf("pending = %v, want [%d]", r.state.Pending, jobID)
	}
	got := r.state.PendingReasons[fmt.Sprintf("%d", jobID)]
	want := "benchmark gate: waiting for propitious conditions (runner warmup)"
	if got != want {
		t.Fatalf("pending reason = %q, want %q", got, want)
	}
	assertJobNotStarted(t, r.logDir, jobID)
}

func TestStartJob_GPUResolutionFailure_DoesNotLogStartOrCreateArtifacts(t *testing.T) {
	r, oplogPath := initTestRunner(t)
	r.gpuInv = &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 28672, TotalMiB: 81920},
			"1": {UsedMiB: 50176, TotalMiB: 81920},
		},
	}

	jobID := int64(202)
	err := r.startJob(jobID, &opsqueue.CommandJob{
		ID:       jobID,
		Dir:      t.TempDir(),
		Cmd:      "echo should-not-run",
		GPUClass: "a100",
		GPUMem:   gpuMemPtr(60),
	}, nil)
	if err != errRequeue {
		t.Fatalf("startJob err = %v, want %v", err, errRequeue)
	}

	assertJobNotStarted(t, r.logDir, jobID)

	entries, err := oplog.ReadEntries(oplogPath)
	if err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no oplog entries, got %v", entries)
	}
}

func TestNewRunner_DefaultTelemetryDoesNotReuseCPUInterval(t *testing.T) {
	r, _ := initTestRunner(t)
	if r.telemetryConfig.Interval != time.Second {
		t.Fatalf("telemetry interval = %v, want %v", r.telemetryConfig.Interval, time.Second)
	}
	if r.cpuConfig.SampleInterval != 15 {
		t.Fatalf("cpu sample interval = %d, want 15", r.cpuConfig.SampleInterval)
	}
}

func TestTryStartNextJob_RequeuesUnreadableJobFile(t *testing.T) {
	r, _ := initTestRunner(t)

	jobID := int64(303)
	jobFile := filepath.Join(r.queueDir, "job-303.json")
	if err := os.WriteFile(jobFile, []byte("{not-json"), 0644); err != nil {
		t.Fatalf("write malformed job file: %v", err)
	}
	r.state.AddPending(jobID)

	r.tryStartNextJob()

	if got := r.state.Pending; len(got) != 1 || got[0] != jobID {
		t.Fatalf("pending = %v, want [%d]", got, jobID)
	}
	if got := r.state.RunningCount(); got != 0 {
		t.Fatalf("running count = %d, want 0", got)
	}
}

func TestStartJob_SourceProvenanceMismatchFails(t *testing.T) {
	r, _ := initTestRunner(t)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, srcsync.SourceMarkerFile), []byte("different\n"), 0644); err != nil {
		t.Fatalf("write source marker: %v", err)
	}

	jobID := int64(707)
	err := r.startJob(jobID, &opsqueue.CommandJob{
		ID:        jobID,
		Dir:       workDir,
		Cmd:       "echo should-not-run",
		SourceSHA: "expected-hash",
	}, nil)
	if err == nil {
		t.Fatal("expected provenance mismatch error")
	}
	if !strings.Contains(err.Error(), "source_provenance_mismatch") {
		t.Fatalf("error = %v, want source_provenance_mismatch", err)
	}

	paths := NewJobPaths(r.logDir, jobID)
	// Preflight rejection must NOT write a status file. A status file would
	// surface to sync as exit_code=1 / duration=0s, masking the
	// fact that the attempt never started.
	if _, statErr := os.Stat(paths.Status); statErr == nil {
		t.Fatal("preflight rejection wrote a status file; expected none")
	}
	// Preflight rejection MUST NOT write a meta file. The presence of
	// start_time in meta would make the attempt look like it ran.
	if _, statErr := os.Stat(paths.Meta); statErr == nil {
		t.Fatal("preflight rejection wrote a meta file; expected none")
	}
	// Preflight rejection MUST write the sentinel that batch-status reads.
	if _, statErr := os.Stat(paths.PreflightRejected); statErr != nil {
		t.Fatalf("preflight sentinel missing: %v", statErr)
	}
	// Preflight rejection MUST write the failure_reason file with the
	// structured token so explain.ForJob and the diagnose surface can
	// quote it back to the user.
	reason := ReadFailureReasonFile(paths.FailureReason)
	if !strings.HasPrefix(reason, "source_provenance_mismatch:") {
		t.Fatalf("failure_reason = %q, want source_provenance_mismatch prefix", reason)
	}
	if !strings.Contains(reason, "expected=expected-hash") {
		t.Fatalf("failure_reason = %q, want expected=expected-hash", reason)
	}
	if !strings.Contains(reason, "marker=different") {
		t.Fatalf("failure_reason = %q, want marker=different", reason)
	}
}

func TestStartJob_SourceProvenanceMatchPassesViaPerJobMarker(t *testing.T) {
	r, _ := initTestRunner(t)

	workDir := t.TempDir()
	// Write the per-job marker; the rolling marker is intentionally absent
	// so we know the runner used the per-job file (Layer C).
	jobID := int64(708)
	if err := os.WriteFile(
		filepath.Join(workDir, srcsync.PerJobSourceMarkerFile(jobID)),
		[]byte("matching-hash\n"), 0644,
	); err != nil {
		t.Fatalf("write per-job marker: %v", err)
	}

	err := r.startJob(jobID, &opsqueue.CommandJob{
		ID:        jobID,
		Dir:       workDir,
		Cmd:       "true",
		SourceSHA: "matching-hash",
	}, nil)
	if err != nil {
		t.Fatalf("startJob returned %v; expected nil (provenance match via per-job marker)", err)
	}
}

func TestStartJob_R2IsolatedSourceFetchInvokesHook(t *testing.T) {
	r, _ := initTestRunner(t)

	workDir := t.TempDir()
	jobID := int64(709)

	var hookCalls int
	var seenR2Key string
	var seenPerJobDir string
	r.EnsureSourceFromR2 = func(id int64, r2Key, perJobDir string) error {
		hookCalls++
		seenR2Key = r2Key
		seenPerJobDir = perJobDir
		// Materialize a minimal per-job dir so the rest of startJob has
		// something to chdir into without errors.
		if err := os.MkdirAll(perJobDir, 0o755); err != nil {
			return err
		}
		return nil
	}

	err := r.startJob(jobID, &opsqueue.CommandJob{
		ID:          jobID,
		Dir:         workDir,
		Cmd:         "true",
		SourceR2Key: "sources/test-tarball.tar.gz",
		// SourceSHA is intentionally set to confirm that R2 mode wins —
		// the per-job marker check should be skipped when R2Key is set.
		SourceSHA: "would-fail-marker-check",
	}, nil)
	if err != nil {
		t.Fatalf("startJob returned %v; expected nil when EnsureSourceFromR2 succeeds", err)
	}
	if hookCalls != 1 {
		t.Fatalf("EnsureSourceFromR2 invoked %d times, want 1", hookCalls)
	}
	if seenR2Key != "sources/test-tarball.tar.gz" {
		t.Fatalf("hook saw r2Key=%q, want sources/test-tarball.tar.gz", seenR2Key)
	}
	if !strings.Contains(seenPerJobDir, "weft/jobs/") || !strings.HasSuffix(seenPerJobDir, "/source") {
		t.Fatalf("hook saw perJobDir=%q, want a path like .../weft/jobs/<id>/source", seenPerJobDir)
	}
}

func TestStartJob_R2IsolatedSourceFetchFailureRejectsPreflight(t *testing.T) {
	r, _ := initTestRunner(t)

	workDir := t.TempDir()
	jobID := int64(710)

	r.EnsureSourceFromR2 = func(_ int64, _, _ string) error {
		return errors.New("simulated download failure")
	}

	err := r.startJob(jobID, &opsqueue.CommandJob{
		ID:          jobID,
		Dir:         workDir,
		Cmd:         "echo should-not-run",
		SourceR2Key: "sources/missing.tar.gz",
	}, nil)
	if err == nil {
		t.Fatal("expected preflight rejection error")
	}
	if !strings.Contains(err.Error(), "r2_isolated_source_fetch_failed") {
		t.Fatalf("error = %v, want r2_isolated_source_fetch_failed", err)
	}

	paths := NewJobPaths(r.logDir, jobID)
	if _, statErr := os.Stat(paths.Status); statErr == nil {
		t.Fatal("R2 fetch failure wrote a status file; expected none")
	}
	if _, statErr := os.Stat(paths.PreflightRejected); statErr != nil {
		t.Fatalf("preflight sentinel missing: %v", statErr)
	}
	reason := ReadFailureReasonFile(paths.FailureReason)
	if !strings.HasPrefix(reason, "r2_isolated_source_fetch_failed") {
		t.Fatalf("failure_reason = %q, want r2_isolated_source_fetch_failed prefix", reason)
	}
}

// TestRefreshRunningJobs_SkipsOrphanWhenWaiterExists verifies that
// refreshRunningJobs does not mark a job as orphaned when the waitForJob
// goroutine is still tracking the process (i.e., the process has exited
// but the status file hasn't been written yet).
func TestRefreshRunningJobs_SkipsOrphanWhenWaiterExists(t *testing.T) {
	r, _ := initTestRunner(t)

	jobID := int64(381)
	jobIDStr := "381"
	paths := NewJobPaths(r.logDir, jobID)

	// Create log dir for the job
	if err := os.MkdirAll(filepath.Dir(paths.Log), 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	// Write PID/PGID files pointing to a PID that doesn't exist (simulates
	// bash having exited). Use PID 1999999999 which won't exist.
	deadPID := 1999999999
	os.WriteFile(paths.PID, []byte("1999999999\n"), 0644)
	os.WriteFile(paths.PGID, []byte("1999999999\n"), 0644)
	_ = deadPID

	// Add the job to running state
	r.state.AddRunning(jobIDStr, RunningJobState{
		StartedAt: time.Now().Unix() - 10,
	})

	// Simulate waitForJob goroutine still tracking the process
	r.processesMu.Lock()
	r.processes[jobIDStr] = &Process{PID: deadPID, PGID: deadPID}
	r.processesMu.Unlock()

	// Run refreshRunningJobs — should skip because waiter exists
	r.refreshRunningJobs()

	// Job should still be in running state (not removed as orphan)
	if r.state.Running[jobIDStr].StartedAt == 0 {
		t.Fatal("job was removed from running state; should have been skipped because waiter exists")
	}

	// No status file should have been written
	if _, err := os.Stat(paths.Status); err == nil {
		t.Fatal("status file was written; refreshRunningJobs should not have treated this as an orphan")
	}

	// No completion record should exist
	if _, err := os.Stat(paths.Completion); err == nil {
		t.Fatal("completion record was written; refreshRunningJobs should not have treated this as an orphan")
	}
}

// TestRefreshRunningJobs_DetectsOrphanWhenNoWaiter verifies that orphan
// detection still works when there is genuinely no waitForJob goroutine
// (e.g., after a runner restart recovering state from disk).
func TestRefreshRunningJobs_DetectsOrphanWhenNoWaiter(t *testing.T) {
	r, _ := initTestRunner(t)

	jobID := int64(382)
	jobIDStr := "382"
	paths := NewJobPaths(r.logDir, jobID)

	if err := os.MkdirAll(filepath.Dir(paths.Log), 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	// Write PID/PGID files pointing to a dead PID
	os.WriteFile(paths.PID, []byte("1999999999\n"), 0644)
	os.WriteFile(paths.PGID, []byte("1999999999\n"), 0644)

	// Add the job to running state but do NOT add to r.processes
	// (simulates a runner restart where wait goroutines are gone)
	r.state.AddRunning(jobIDStr, RunningJobState{
		StartedAt: time.Now().Unix() - 10,
	})

	r.refreshRunningJobs()

	// Job should have been removed from running state
	if _, exists := r.state.Running[jobIDStr]; exists {
		t.Fatal("job should have been removed from running state as orphan")
	}

	// Status file should have been written
	if _, err := os.Stat(paths.Status); err != nil {
		t.Fatalf("status file should exist after orphan detection: %v", err)
	}
}

// TestRefreshRunningJobs_RecoversZombieWrapper exercises the path where the
// wrapper bash got SIGKILL'd (so no .status file was ever written) AND the
// agent re-execed (losing its Wait() goroutine), leaving the wrapper as a
// state-Z zombie. Before the fix, CheckPIDAlive returned true for zombies and
// refreshRunningJobs `continue`d, so the slot stayed occupied forever.
func TestRefreshRunningJobs_RecoversZombieWrapper(t *testing.T) {
	r, _ := initTestRunner(t)

	jobID := int64(383)
	jobIDStr := "383"
	paths := NewJobPaths(r.logDir, jobID)

	if err := os.MkdirAll(filepath.Dir(paths.Log), 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	// Fork a child that exits immediately but never wait for it -> zombie.
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zombie child: %v", err)
	}
	pid := cmd.Process.Pid
	// Give the kernel a moment to mark the child as Z.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if CheckProcessZombie(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckProcessZombie(pid) {
		t.Skip("could not produce a zombie process on this platform; skipping")
	}
	if !CheckPIDAlive(pid) {
		// Sanity: the original bug only manifests because kill -0 still
		// succeeds on zombies. If that's not true here, the test premise
		// doesn't hold.
		t.Skip("kill -0 does not see zombies on this platform; test premise invalid")
	}

	os.WriteFile(paths.PID, []byte(fmt.Sprintf("%d\n", pid)), 0644)
	os.WriteFile(paths.PGID, []byte(fmt.Sprintf("%d\n", pid)), 0644)

	r.state.AddRunning(jobIDStr, RunningJobState{
		StartedAt: time.Now().Unix() - 10,
	})

	r.refreshRunningJobs()

	if _, exists := r.state.Running[jobIDStr]; exists {
		t.Fatal("zombie wrapper should have been recovered out of running state")
	}
	if _, err := os.Stat(paths.Status); err != nil {
		t.Fatalf("status file should exist after zombie recovery: %v", err)
	}
}

func TestTelemetryPolicyForBenchmarkJobs(t *testing.T) {
	policy := TelemetryPolicyForJob(&opsqueue.CommandJob{Tags: []string{"benchmark-isolation"}})
	if policy.Interval != 5*time.Second {
		t.Fatalf("benchmark telemetry interval = %v, want %v", policy.Interval, 5*time.Second)
	}
	if !policy.CollectAdvancedGPU {
		t.Fatal("benchmark jobs should enable advanced GPU telemetry")
	}

	normal := TelemetryPolicyForJob(&opsqueue.CommandJob{})
	if normal.Interval != time.Second {
		t.Fatalf("normal telemetry interval = %v, want %v", normal.Interval, time.Second)
	}
	if !normal.CollectAdvancedGPU {
		t.Fatal("normal jobs should keep advanced GPU telemetry")
	}
}

// TestStartJob_SetupFailureWritesCompletionAndCleansDebris: when the setup
// phase fails, the queue runner must close out the attempt the same way
// RunSingleJob does — completion record and failure reason written, finished
// state recorded — and must not leave .pid/.pgid files or the job-*.json
// queue payload behind.
func TestStartJob_SetupFailureWritesCompletionAndCleansDebris(t *testing.T) {
	r, _ := initTestRunner(t)

	// A pixi.toml triggers the "pixi install" setup command; a fake pixi on
	// PATH makes it fail deterministically with exit 9.
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "pixi.toml"), []byte("[project]\n"), 0644); err != nil {
		t.Fatalf("write pixi.toml: %v", err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir fake bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pixi"), []byte("#!/bin/sh\necho 'setup boom' >&2\nexit 9\n"), 0755); err != nil {
		t.Fatalf("write fake pixi: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	jobID := int64(811)
	job := &opsqueue.CommandJob{
		ID:  jobID,
		Dir: workDir,
		Cmd: "echo should-not-run",
	}
	if err := writeJobFile(r.queueDir, job); err != nil {
		t.Fatalf("write job file: %v", err)
	}

	err := r.startJob(jobID, job, nil)
	if err == nil {
		t.Fatal("expected setup failure error")
	}
	if !strings.Contains(err.Error(), "setup command failed") {
		t.Fatalf("error = %v, want setup command failure", err)
	}

	paths := NewJobPaths(r.logDir, jobID)

	// Status file written by RunSetupCommand with the setup exit code.
	exitCode, ok := ReadStatusFile(paths.Status)
	if !ok || exitCode != 9 {
		t.Fatalf("status = (%d, %v), want (9, true)", exitCode, ok)
	}

	// Completion record and failure reason must exist (mirrors RunSingleJob).
	if _, statErr := os.Stat(paths.Completion); statErr != nil {
		t.Fatalf("completion record missing after setup failure: %v", statErr)
	}
	if reason := ReadFailureReasonFile(paths.FailureReason); reason == "" {
		t.Fatal("failure_reason file missing or empty after setup failure")
	}

	// PID/PGID files written for the setup process must be cleaned up.
	for _, path := range []string{paths.PID, paths.PGID} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("expected %s to be removed after setup failure, got err=%v", path, statErr)
		}
	}

	// Queue payload must be removed so the job is not re-attempted.
	if _, readErr := ReadJobFile(r.queueDir, jobID); readErr == nil {
		t.Fatal("queue job file still present after setup failure")
	}

	// Terminal state recorded.
	if f, found := r.state.Finished[fmt.Sprintf("%d", jobID)]; !found {
		t.Fatal("finished state not recorded after setup failure")
	} else if f.ExitCode != 9 {
		t.Fatalf("finished exit code = %d, want 9", f.ExitCode)
	}
}
