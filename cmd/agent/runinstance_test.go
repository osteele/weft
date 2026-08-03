package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestTerminalOutcomeForSequence(t *testing.T) {
	tests := []struct {
		name       string
		result     jobSequenceResult
		wantStatus string
		wantReason string
	}{
		{
			name:       "successful run",
			result:     jobSequenceResult{StartedJobCount: 1},
			wantStatus: db.LaunchStatusCompleted,
			wantReason: db.TerminationReasonCompleted,
		},
		{
			name:       "failed run",
			result:     jobSequenceResult{StartedJobCount: 1, AnyFailed: true},
			wantStatus: db.LaunchStatusFailed,
			wantReason: db.TerminationReasonJobFailure,
		},
		{
			name:       "infra failed run",
			result:     jobSequenceResult{StartedJobCount: 1, AnyFailed: true, AnyInfraFailed: true},
			wantStatus: db.LaunchStatusFailed,
			wantReason: db.TerminationReasonInfraFailure,
		},
		{
			name:       "canceled before any job started",
			result:     jobSequenceResult{AnyCanceled: true},
			wantStatus: db.LaunchStatusCancelled,
			wantReason: db.TerminationReasonCancelled,
		},
		{
			name:       "canceled after at least one job started",
			result:     jobSequenceResult{StartedJobCount: 1, AnyCanceled: true},
			wantStatus: db.LaunchStatusCompleted,
			wantReason: db.TerminationReasonCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotReason := terminalOutcomeForSequence(tt.result)
			if gotStatus != tt.wantStatus || gotReason != tt.wantReason {
				t.Fatalf("terminalOutcomeForSequence() = (%q, %q), want (%q, %q)",
					gotStatus, gotReason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

// rcloneTimeoutForBytes was the legacy size-derived timeout helper. It has
// been superseded by the stall-watchdog + ceiling drain gate in
// internal/r2upload; see TestComputeCeiling there for the equivalent
// regression coverage.

func TestUploadLiveFileSnapshotUsesRcatFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "telemetry.jsonl")
	want := "{\"event\":\"tick\"}\n"
	if err := os.WriteFile(sourcePath, []byte(want), 0o644); err != nil {
		t.Fatalf("write live file: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake rclone bin dir: %v", err)
	}
	capturePath := filepath.Join(dir, "capture")
	argsPath := filepath.Join(dir, "args")
	rcloneScript := `#!/bin/sh
printf '%s\n' "$*" > "$RCLONE_ARGS"
if [ "$1" != "rcat" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
if [ "$2" != "r2:test-bucket/live/key.jsonl" ]; then
  echo "unexpected rclone target: $2" >&2
  exit 1
fi
cat > "$RCLONE_CAPTURE"
`
	if err := os.WriteFile(filepath.Join(binDir, "rclone"), []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RCLONE_ARGS", argsPath)
	t.Setenv("RCLONE_CAPTURE", capturePath)

	uploadLiveFileSnapshot("test-bucket", 123, sourcePath, "live/key.jsonl", "live telemetry")

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured rclone stdin: %v", err)
	}
	if string(got) != want {
		t.Fatalf("uploaded stdin = %q, want %q", got, want)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured rclone args: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "rcat r2:test-bucket/live/key.jsonl" {
		t.Fatalf("rclone args = %q, want rcat target", got)
	}
}

func TestReadLogTail_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != content {
		t.Errorf("readLogTail() = %q, want %q", got, content)
	}
}

func TestReadLogTail_LargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write more than maxBytes
	var data []byte
	for i := 0; i < 200; i++ {
		data = append(data, []byte("this is a log line with some content\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 100)
	if len(got) > 100 {
		t.Errorf("readLogTail() returned %d bytes, want <= 100", len(got))
	}
	// Should contain the end of the file
	if got[len(got)-1] != '\n' {
		t.Error("readLogTail() should end with newline")
	}
}

func TestReadLogTail_MissingFile(t *testing.T) {
	got := readLogTail("/nonexistent/path/file.log", 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}

func TestReadLogTail_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readLogTail(path, 8192)
	if got != "" {
		t.Errorf("readLogTail() = %q, want empty string", got)
	}
}

func TestOutputUploadRcloneArgs(t *testing.T) {
	if got := outputUploadRcloneArgs(time.Time{}); len(got) != 1 || got[0] != "--update" {
		t.Errorf("outputUploadRcloneArgs(zero) = %v, want [--update]", got)
	}
	windowStart := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	got := outputUploadRcloneArgs(windowStart)
	want := []string{"--update", "--max-age", "2026-08-02T12:00:00Z"}
	if len(got) != len(want) {
		t.Fatalf("outputUploadRcloneArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outputUploadRcloneArgs() = %v, want %v", got, want)
		}
	}
}

// Regression: the upload walk must not count (or upload) files that predate
// the attempt — a prior same-workdir job's leftovers in output/ (spec:
// invariant Attribution mechanism (d) in specs/job-lifecycle.allium).
func TestMeasureUploadTreeSince_ExcludesPriorJobFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "prior-job.json")
	fresh := filepath.Join(dir, "this-job.json")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("new-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	files, bytes, ok := measureUploadTreeSince(dir, time.Now().Add(-time.Hour))
	if !ok {
		t.Fatal("measureUploadTreeSince() ok = false")
	}
	if files != 1 || bytes != int64(len("new-data")) {
		t.Errorf("measureUploadTreeSince() = (%d files, %d bytes), want (1, %d)", files, bytes, len("new-data"))
	}

	// Zero threshold counts everything.
	files, _, ok = measureUploadTreeSince(dir, time.Time{})
	if !ok || files != 2 {
		t.Errorf("measureUploadTreeSince(zero) = %d files, want 2", files)
	}
}
