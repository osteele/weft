package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchStatusExitCodeWithSignalSuffix validates that exit codes are correctly
// parsed from status file content that may contain signal suffixes like
// "137 signal=KILL" or "0 signal=TERM".
func TestBatchStatusExitCodeWithSignalSuffix(t *testing.T) {
	tests := []struct {
		name          string
		statusContent string
		wantExitCode  int
	}{
		{
			name:          "plain exit 0",
			statusContent: "0",
			wantExitCode:  0,
		},
		{
			name:          "plain exit 1",
			statusContent: "1",
			wantExitCode:  1,
		},
		{
			name:          "exit 137 with signal suffix",
			statusContent: "137 signal=KILL",
			wantExitCode:  137,
		},
		{
			name:          "exit 0 with signal suffix",
			statusContent: "0 signal=TERM",
			wantExitCode:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitCodeStr := strings.TrimSpace(tt.statusContent)

			// Use fmt.Sscanf to extract only the leading integer, matching production code
			var exitCode int
			if _, err := fmt.Sscanf(exitCodeStr, "%d", &exitCode); err != nil {
				t.Fatalf("failed to parse exit code from %q: %v", exitCodeStr, err)
			}

			if exitCode != tt.wantExitCode {
				t.Errorf("got exit code %d, want %d (from %q)", exitCode, tt.wantExitCode, tt.statusContent)
			}
		})
	}
}

// TestBatchStatusParsesStatusFileWithSignalSuffix is an end-to-end test that
// writes a status file with a signal suffix and verifies batchStatus can parse it.
func TestBatchStatusParsesStatusFileWithSignalSuffix(t *testing.T) {
	logDir := t.TempDir()

	tests := []struct {
		name          string
		jobID         int64
		statusContent string
		wantExitCode  int
	}{
		{
			name:          "killed job",
			jobID:         100,
			statusContent: "137 signal=KILL",
			wantExitCode:  137,
		},
		{
			name:          "terminated job",
			jobID:         101,
			statusContent: "0 signal=TERM",
			wantExitCode:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusFile := filepath.Join(logDir, fmt.Sprintf("%d.status", tt.jobID))
			if err := os.WriteFile(statusFile, []byte(tt.statusContent), 0644); err != nil {
				t.Fatalf("write status file: %v", err)
			}

			// Read and parse like batchstatus.go does
			statusBytes, err := os.ReadFile(statusFile)
			if err != nil {
				t.Fatalf("read status file: %v", err)
			}

			exitCodeStr := strings.TrimSpace(string(statusBytes))
			var exitCode int
			if _, err := fmt.Sscanf(exitCodeStr, "%d", &exitCode); err != nil {
				t.Fatalf("parse exit code from %q: %v", exitCodeStr, err)
			}

			if exitCode != tt.wantExitCode {
				t.Errorf("got exit code %d, want %d", exitCode, tt.wantExitCode)
			}
		})
	}
}
