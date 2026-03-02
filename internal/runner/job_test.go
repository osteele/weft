package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/ops"
)

func TestJobPaths(t *testing.T) {
	paths := NewJobPaths("/tmp/logs", 42)
	if paths.Log != "/tmp/logs/42.log" {
		t.Errorf("log: %s", paths.Log)
	}
	if paths.Status != "/tmp/logs/42.status" {
		t.Errorf("status: %s", paths.Status)
	}
	if paths.PGID != "/tmp/logs/42.pgid" {
		t.Errorf("pgid: %s", paths.PGID)
	}
}

func TestArchiveExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Create some files
	logFile := filepath.Join(dir, "42.log")
	statusFile := filepath.Join(dir, "42.status")
	os.WriteFile(logFile, []byte("test"), 0644)
	os.WriteFile(statusFile, []byte("0"), 0644)

	ArchiveExistingFiles(dir, 42)

	// Originals should be gone
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Error("expected original log to be archived")
	}
	if _, err := os.Stat(statusFile); !os.IsNotExist(err) {
		t.Error("expected original status to be archived")
	}

	// Archived files should exist
	matches, _ := filepath.Glob(filepath.Join(dir, "42-*.log"))
	if len(matches) != 1 {
		t.Errorf("expected 1 archived log, got %d", len(matches))
	}
}

func TestWriteStatusFile_ReadStatusFile(t *testing.T) {
	dir := t.TempDir()
	paths := NewJobPaths(dir, 42)

	WriteStatusFile(paths, ExitInfo{ExitCode: 0})
	code, ok := ReadStatusFile(paths.Status)
	if !ok || code != 0 {
		t.Errorf("expected 0, got %d (ok=%v)", code, ok)
	}

	WriteStatusFile(paths, ExitInfo{ExitCode: 137, Signaled: true, Signal: 9})
	code, ok = ReadStatusFile(paths.Status)
	if !ok || code != 137 {
		t.Errorf("expected 137, got %d (ok=%v)", code, ok)
	}
}

func TestJobCompleted(t *testing.T) {
	dir := t.TempDir()

	// Not completed
	if JobCompleted(dir, 42) {
		t.Error("expected not completed")
	}

	// Write status file
	os.WriteFile(filepath.Join(dir, "42.status"), []byte("0"), 0644)
	if !JobCompleted(dir, 42) {
		t.Error("expected completed")
	}
}

func TestWriteRusageFile(t *testing.T) {
	dir := t.TempDir()
	paths := NewJobPaths(dir, 42)

	rs := RunningJobState{
		RusageUserCPU: "10.50",
		RusageSysCPU:  "2.30",
		RusagePeakRSS: "524288",
		RusageMaxGPU:  "8192",
		GPUDevices:    []string{"0", "1"},
	}

	err := WriteRusageFile(paths, rs)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(paths.Rusage)
	content := string(data)
	expected := []string{"user_cpu_secs=10.50", "sys_cpu_secs=2.30", "peak_rss_kb=524288", "max_gpu_mem_mib=8192", "gpu_devices=0,1"}
	for _, e := range expected {
		if !contains(content, e) {
			t.Errorf("expected %q in rusage file, got:\n%s", e, content)
		}
	}
}

func TestHasTag(t *testing.T) {
	job := &ops.CommandJob{Tags: []string{"exclusive", "gpu"}}
	if !HasTag(job, "exclusive") {
		t.Error("expected exclusive tag")
	}
	if HasTag(job, "benchmark") {
		t.Error("expected no benchmark tag")
	}
}

func TestGetJobGPUMem(t *testing.T) {
	// Explicit
	mem := 40
	job := &ops.CommandJob{GPUMem: &mem}
	if got := GetJobGPUMem(job, 20); got != 40 {
		t.Errorf("expected 40, got %d", got)
	}

	// Default with GPU
	job2 := &ops.CommandJob{GPU: "0"}
	if got := GetJobGPUMem(job2, 20); got != 20 {
		t.Errorf("expected 20, got %d", got)
	}

	// No GPU
	job3 := &ops.CommandJob{}
	if got := GetJobGPUMem(job3, 20); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
