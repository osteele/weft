package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
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
		QueueName: "test",
		QueueDir:  queueDir,
		LogDir:    logDir,
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
	job := &ops.CommandJob{
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
	err := r.startJob(jobID, &ops.CommandJob{
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

func TestTelemetryPolicyForBenchmarkJobs(t *testing.T) {
	policy := TelemetryPolicyForJob(&ops.CommandJob{Tags: []string{"benchmark"}})
	if policy.Interval != 5*time.Second {
		t.Fatalf("benchmark telemetry interval = %v, want %v", policy.Interval, 5*time.Second)
	}
	if policy.CollectAdvancedGPU {
		t.Fatal("benchmark jobs should disable advanced GPU telemetry")
	}

	normal := TelemetryPolicyForJob(&ops.CommandJob{})
	if normal.Interval != time.Second {
		t.Fatalf("normal telemetry interval = %v, want %v", normal.Interval, time.Second)
	}
	if !normal.CollectAdvancedGPU {
		t.Fatal("normal jobs should keep advanced GPU telemetry")
	}
}
