package runner

import (
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(err.Error(), "source provenance check failed") {
		t.Fatalf("error = %v, want provenance failure", err)
	}

	paths := NewJobPaths(r.logDir, jobID)
	status, readErr := os.ReadFile(paths.Status)
	if readErr != nil {
		t.Fatalf("read status file: %v", readErr)
	}
	if strings.TrimSpace(string(status)) != "1" {
		t.Fatalf("status = %q, want 1", strings.TrimSpace(string(status)))
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

func TestTelemetryPolicyForBenchmarkJobs(t *testing.T) {
	policy := TelemetryPolicyForJob(&opsqueue.CommandJob{Tags: []string{"benchmark"}})
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
