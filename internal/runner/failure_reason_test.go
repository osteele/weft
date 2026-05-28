package runner

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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

func writeFakeCommand(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write fake command %s: %v", name, err)
	}
}
