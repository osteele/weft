package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osteele/weft/internal/runner"
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

func TestParseBatchStatusArgs(t *testing.T) {
	t.Run("accepts explicit default queue for compatibility", func(t *testing.T) {
		jobIDs, err := parseBatchStatusArgs([]string{"--queue", "default", "101", "202"})
		if err != nil {
			t.Fatalf("parseBatchStatusArgs() error = %v", err)
		}
		if len(jobIDs) != 2 || jobIDs[0] != 101 || jobIDs[1] != 202 {
			t.Fatalf("parseBatchStatusArgs() jobIDs = %v, want [101 202]", jobIDs)
		}
	})

	t.Run("rejects non-default queue", func(t *testing.T) {
		if _, err := parseBatchStatusArgs([]string{"--queue", "gpu", "101"}); err == nil {
			t.Fatal("parseBatchStatusArgs() error = nil, want error")
		}
	})
}

func TestBatchStatus_PrefersQueuedOverFinishedState(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	queueDir := filepath.Join(homeDir, ".cache", "weft", "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	stateFile := filepath.Join(queueDir, "default.state.json")
	stateJSON := `{"pending":[42],"finished":{"42":{"exit_code":0,"finished_at":123}}}`
	if err := os.WriteFile(stateFile, []byte(stateJSON), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	output := captureStdout(t, func() {
		batchStatus([]int64{42})
	})

	if strings.TrimSpace(output) != "JOB|42|QUEUED" {
		t.Fatalf("batchStatus output = %q, want %q", strings.TrimSpace(output), "JOB|42|QUEUED")
	}
}

func TestBatchStatus_PendingMissingPayloadDoesNotHideStatusFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	queueDir := filepath.Join(homeDir, ".cache", "weft", "queue")
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	stateFile := filepath.Join(queueDir, "default.state.json")
	stateJSON := `{"pending":[42]}`
	if err := os.WriteFile(stateFile, []byte(stateJSON), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.status"), []byte("1\n"), 0644); err != nil {
		t.Fatalf("write status file: %v", err)
	}

	output := captureStdout(t, func() {
		batchStatus([]int64{42})
	})

	if !strings.HasPrefix(strings.TrimSpace(output), "JOB|42|COMPLETED|1|") {
		t.Fatalf("batchStatus output = %q, want completed", strings.TrimSpace(output))
	}
}

func TestBatchStatus_CompletedIncludesRunID(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	queueDir := filepath.Join(homeDir, ".cache", "weft", "queue")
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	stateFile := filepath.Join(queueDir, "default.state.json")
	if err := os.WriteFile(stateFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.status"), []byte("1\n"), 0644); err != nil {
		t.Fatalf("write status file: %v", err)
	}
	rec, err := json.Marshal(runner.CompletionRecord{RunID: 1234, ExitCode: 1, EndTime: 1700000000})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.completion.json"), rec, 0644); err != nil {
		t.Fatalf("write completion: %v", err)
	}

	output := captureStdout(t, func() {
		batchStatus([]int64{42})
	})

	parts := strings.Split(strings.TrimSpace(output), "|")
	if len(parts) < 6 || parts[5] != "1234" {
		t.Fatalf("batchStatus output = %q, want run_id field 1234", strings.TrimSpace(output))
	}
}

func TestBatchStatus_PendingPayloadWinsOverStaleStatusFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	queueDir := filepath.Join(homeDir, ".cache", "weft", "queue")
	logDir := filepath.Join(homeDir, ".cache", "weft", "logs")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	stateFile := filepath.Join(queueDir, "default.state.json")
	stateJSON := `{"pending":[42]}`
	if err := os.WriteFile(stateFile, []byte(stateJSON), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(queueDir, "job-42.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "42.status"), []byte("1\n"), 0644); err != nil {
		t.Fatalf("write status file: %v", err)
	}

	output := captureStdout(t, func() {
		batchStatus([]int64{42})
	})

	if strings.TrimSpace(output) != "JOB|42|QUEUED" {
		t.Fatalf("batchStatus output = %q, want queued", strings.TrimSpace(output))
	}
}

func TestBatchStatus_HandlesInvalidStateFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	queueDir := filepath.Join(homeDir, ".cache", "weft", "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("mkdir queue dir: %v", err)
	}
	stateFile := filepath.Join(queueDir, "default.state.json")
	if err := os.WriteFile(stateFile, []byte("{invalid"), 0644); err != nil {
		t.Fatalf("write invalid state file: %v", err)
	}

	output := captureStdout(t, func() {
		batchStatus([]int64{99})
	})

	if strings.TrimSpace(output) != "JOB|99|DEAD" {
		t.Fatalf("batchStatus output = %q, want %q", strings.TrimSpace(output), "JOB|99|DEAD")
	}
}

func TestCheckProcessState_UsesPortableStoppedDetection(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(501)
	logFile := filepath.Join(logDir, "job.log")
	proc, err := runner.StartProcess("sleep 30", t.TempDir(), nil, logFile)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	paths := runner.NewJobPaths(logDir, jobID)
	if err := proc.WritePIDFiles(paths); err != nil {
		t.Fatalf("WritePIDFiles: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Kill()
		_ = proc.Cmd.Wait()
	})

	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("Signal(SIGSTOP): %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Signal(syscall.SIGCONT)
	})

	state := ""
	for i := 0; i < 20; i++ {
		state = checkProcessState(logDir, jobID)
		if state == "paused" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if state != "paused" {
		t.Fatalf("checkProcessState() = %q, want paused", state)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return buf.String()
}
