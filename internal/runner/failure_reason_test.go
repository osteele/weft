package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestDetectFailureReason(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		want     string
	}{
		{"exit 137 is OOM", 137, "oom"},
		{"exit 139 is segfault", 139, "segfault"},
		{"exit 1 is generic error", 1, "error"},
		{"exit 2 is exit_2", 2, "exit_2"},
		{"exit 126 is exit_126", 126, "exit_126"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectFailureReason(tt.exitCode)
			if got != tt.want {
				t.Errorf("DetectFailureReason(%d) = %q, want %q", tt.exitCode, got, tt.want)
			}
		})
	}
}

func TestDetectFailureReasonFromExitInfo_SetupTimeout(t *testing.T) {
	got := DetectFailureReasonFromExitInfo(ExitInfo{ExitCode: ExitCodeSetupTimeout})
	if got != FailureReasonSetupTimeout {
		t.Errorf("DetectFailureReasonFromExitInfo(exit %d) = %q, want %q",
			ExitCodeSetupTimeout, got, FailureReasonSetupTimeout)
	}
}

func TestWriteAndReadFailureReasonFile(t *testing.T) {
	tmpDir := t.TempDir()
	paths := NewJobPaths(tmpDir, 123)

	// No file yet
	reason := ReadFailureReasonFile(paths.FailureReason)
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}

	// Write and read
	if err := WriteFailureReasonFile(paths, "oom"); err != nil {
		t.Fatalf("WriteFailureReasonFile: %v", err)
	}

	reason = ReadFailureReasonFile(paths.FailureReason)
	if reason != "oom" {
		t.Errorf("expected 'oom', got %q", reason)
	}

	// Verify file exists
	if _, err := os.Stat(paths.FailureReason); err != nil {
		t.Errorf("failure_reason file should exist: %v", err)
	}
}

func TestFailureReasonArtifactWritersSanitizeProse(t *testing.T) {
	tmpDir := t.TempDir()
	paths := NewJobPaths(tmpDir, 124)
	prose := "prewarm failed|source metadata\ncontinuation"
	if err := WriteFailureReasonFile(paths, prose); err != nil {
		t.Fatal(err)
	}
	if got := ReadFailureReasonFile(paths.FailureReason); got != db.FailureReasonError {
		t.Fatalf("failure reason file = %q, want %q", got, db.FailureReasonError)
	}
	if err := WriteCompletionRecord(paths, ExitInfo{ExitCode: 1}, RunningJobState{}, "", prose, 1, 2, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatal(err)
	}
	var record CompletionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.FailureReason != db.FailureReasonError {
		t.Fatalf("completion failure_reason = %q, want %q", record.FailureReason, db.FailureReasonError)
	}
}

func TestWriteFailureReasonFile_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	paths := NewJobPaths(tmpDir, 456)

	// Empty reason should not create file
	if err := WriteFailureReasonFile(paths, ""); err != nil {
		t.Fatalf("WriteFailureReasonFile: %v", err)
	}

	if _, err := os.Stat(paths.FailureReason); !os.IsNotExist(err) {
		t.Error("empty failure reason should not create file")
	}
}

func TestDetectFailureReason_DiskFullFromDmesg(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "dmesg", "#!/bin/sh\necho '[123] writeback: No space left on device'\n")
	writeFakeCommand(t, binDir, "df", "#!/bin/sh\necho 'Filesystem 1K-blocks Used Available Use% Mounted on'\necho '/dev/root 1000 100 900 10% /'\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := DetectFailureReason(1)
	if got != "disk_full" {
		t.Fatalf("DetectFailureReason(1) = %q, want disk_full", got)
	}
}

func TestDetectFailureReasonFromExitInfo_DiskFullOverridesSignal(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "dmesg", "#!/bin/sh\necho 'ENOSPC: write failed'\n")
	writeFakeCommand(t, binDir, "df", "#!/bin/sh\necho 'Filesystem 1K-blocks Used Available Use% Mounted on'\necho '/dev/root 1000 100 900 10% /'\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ei := ExitInfo{
		ExitCode: 137,
		Signaled: true,
		Signal:   syscall.SIGKILL,
	}
	got := DetectFailureReasonFromExitInfo(ei)
	if got != "disk_full" {
		t.Fatalf("DetectFailureReasonFromExitInfo() = %q, want disk_full", got)
	}
}

func TestDetectFailureReasonFromExitInfoAndLog_DiskFullFromQuotaExceeded(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "dmesg", "#!/bin/sh\nexit 1\n")
	writeFakeCommand(t, binDir, "df", "#!/bin/sh\necho 'Filesystem 1K-blocks Used Available Use% Mounted on'\necho '/dev/root 1000 100 900 10% /'\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name string
		log  string
	}{
		{
			name: "quota exceeded with os error",
			log: `error: Failed to install: sympy-1.13.1-py3-none-any.whl
  Caused by: failed to copy file: Quota exceeded (os error 122)
`,
		},
		{
			name: "bare EDQUOT",
			log:  "failed to hardlink lm_eval wheel: EDQUOT\n",
		},
		{
			name: "filesystem quota",
			log:  "filesystem quota during hardlink of lm_eval wheel\n",
		},
		{
			name: "os error 122 without quota text",
			log:  "failed to install package: operation failed (os error 122)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "job.log")
			if err := os.WriteFile(logPath, []byte(tt.log), 0o644); err != nil {
				t.Fatalf("write log: %v", err)
			}

			got := DetectFailureReasonFromExitInfoAndLog(ExitInfo{ExitCode: 2}, logPath)
			if got != "disk_full" {
				t.Fatalf("DetectFailureReasonFromExitInfoAndLog() = %q, want disk_full", got)
			}
		})
	}
}

func TestDetectFailureReasonFromExitInfoAndLog_DriverWarningNotClassified(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "dmesg", "#!/bin/sh\nexit 1\n")
	writeFakeCommand(t, binDir, "df", "#!/bin/sh\necho 'Filesystem 1K-blocks Used Available Use% Mounted on'\necho '/dev/root 1000 100 900 10% /'\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// torch emits this UserWarning at import even when the job later dies for an
	// unrelated reason (here a heartbeat-stale SIGTERM, exit 143). It must not be
	// classified as driver-too-old. Regression for wj3230.
	warnLog := "/x/torch/cuda/__init__.py:187: UserWarning: CUDA initialization: " +
		"The NVIDIA driver on your system is too old (found version 12020).\n" +
		"trainer killed\n"
	warnPath := filepath.Join(t.TempDir(), "warn.log")
	if err := os.WriteFile(warnPath, []byte(warnLog), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if got := DetectFailureReasonFromExitInfoAndLog(ExitInfo{ExitCode: 143}, warnPath); got == FailureReasonCUDADriverTooOld {
		t.Fatalf("benign driver UserWarning misclassified as %q", got)
	}

	// A fatal driver error (not a UserWarning line) still classifies.
	for _, fatal := range []string{
		"RuntimeError: The NVIDIA driver on your system is too old (found version 12020).\n",
		"CUDA error: CUDA driver version is insufficient for CUDA runtime version\n",
	} {
		logPath := filepath.Join(t.TempDir(), "fatal.log")
		if err := os.WriteFile(logPath, []byte(fatal), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}
		if got := DetectFailureReasonFromExitInfoAndLog(ExitInfo{ExitCode: 1}, logPath); got != FailureReasonCUDADriverTooOld {
			t.Fatalf("fatal driver error %q classified as %q, want %q", fatal, got, FailureReasonCUDADriverTooOld)
		}
	}
}

func TestDetectFailureReasonFromExitInfoAndLog_CUDAHardwareFault(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(logPath, []byte("torch.AcceleratorError: CUDA error: Invalid access of peer GPU memory over nvlink or a hardware error\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	got := DetectFailureReasonFromExitInfoAndLog(ExitInfo{ExitCode: 1}, logPath)
	if got != db.FailureReasonInfraCUDAHardwareFault {
		t.Fatalf("DetectFailureReasonFromExitInfoAndLog() = %q, want %q", got, db.FailureReasonInfraCUDAHardwareFault)
	}
}

func writeFakeCommand(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake command %s: %v", name, err)
	}
}
