package runner

import (
	"os"
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
